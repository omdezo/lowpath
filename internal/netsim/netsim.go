// Package netsim is a bidirectional UDP forwarder with optional impairment
// (delay, jitter, loss, Gilbert-Elliott loss bursts, periodic lag spikes).
// With a zero Profile it is a plain relay hop (ingress / transit); with a
// non-zero one it simulates an ISP or carrier path so the whole system can
// be exercised on one machine. Links can sit behind a shared Segment, such
// as the player's Wi-Fi, whose losses hit all of them at once.
package netsim

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// Profile values are one-way: an RTT sees roughly 2x Delay and ~2x Loss.
type Profile struct {
	Delay  time.Duration `json:"delay"`
	Jitter time.Duration `json:"jitter"` // stddev of extra delay
	Loss   float64       `json:"loss"`   // 0..1 drop probability per packet

	// Every SpikeEvery, for SpikeLen, add SpikeExtra delay and SpikeLoss.
	SpikeEvery time.Duration `json:"spikeEvery"`
	SpikeLen   time.Duration `json:"spikeLen"`
	SpikeExtra time.Duration `json:"spikeExtra"`
	SpikeLoss  float64       `json:"spikeLoss"`

	// Burst adds Gilbert-Elliott burst loss on top of Loss; zero is off.
	Burst GilbertElliott `json:"burst,omitzero"`

	// Seed makes the random choices (loss, bursts, jitter) repeat exactly
	// from run to run, for tests; 0 picks a random one. Give each link and
	// segment its own: two with the same Seed make the same choices, so
	// their losses would line up.
	Seed uint64 `json:"seed,omitempty"`
}

type Link struct {
	listen  *net.UDPConn
	target  *net.UDPAddr
	profile atomic.Pointer[Profile]
	segment atomic.Pointer[Segment]
	clock   func() time.Duration // time since the link started; tests set their own
	imp     *impairer

	mu    sync.Mutex
	peers map[netip.AddrPort]*net.UDPConn
}

// Listen starts a link that accepts packets on addr and forwards them to target.
func Listen(addr string, target netip.AddrPort, p Profile) (*Link, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	l := &Link{listen: c, target: net.UDPAddrFromAddrPort(target), clock: sinceNow(), imp: newImpairer(p.Seed), peers: map[netip.AddrPort]*net.UDPConn{}}
	l.profile.Store(&p)
	return l, nil
}

func (l *Link) Addr() netip.AddrPort { return l.listen.LocalAddr().(*net.UDPAddr).AddrPort() }

func (l *Link) Profile() Profile { return *l.profile.Load() }

// SetProfile changes impairment at runtime (used to simulate a degrading route).
func (l *Link) SetProfile(p Profile) { l.profile.Store(&p) }

// SetSegment puts the link behind a shared hop, or takes it out with nil.
// Packets in both directions then cross the segment as well as the link.
// The segment is on the client's side (the address Listen took): packets
// from the client cross it first, replies once the link's delay is over.
func (l *Link) SetSegment(s *Segment) { l.segment.Store(s) }

func (l *Link) Segment() *Segment { return l.segment.Load() }

func (l *Link) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		l.listen.Close()
		l.mu.Lock()
		for _, c := range l.peers {
			c.Close()
		}
		l.mu.Unlock()
	}()
	buf := make([]byte, 65535)
	for {
		n, src, err := l.listen.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue // transient (e.g. ICMP unreachable on Windows)
		}
		up := l.peer(src)
		if up == nil {
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		l.send(pkt, false, func(b []byte) { up.Write(b) })
	}
}

func (l *Link) peer(src netip.AddrPort) *net.UDPConn {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c, ok := l.peers[src]; ok {
		return c
	}
	c, err := net.DialUDP("udp", nil, l.target)
	if err != nil {
		return nil
	}
	l.peers[src] = c
	go func() {
		buf := make([]byte, 65535)
		for {
			n, err := c.Read(buf)
			if err != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			l.send(pkt, true, func(b []byte) { l.listen.WriteToUDPAddrPort(b, src) })
		}
	}()
	return c
}

// send impairs one packet; reply is true for the target's direction. A
// reply meets the segment when it gets there, after the link's delay: two
// lanes' copies sent at once with different delays reach the Wi-Fi apart
// and don't share a fade.
func (l *Link) send(pkt []byte, reply bool, write func([]byte)) {
	drop, delay := l.imp.decide(l.profile.Load(), l.clock())
	s := l.segment.Load()
	if s != nil && (!reply || drop) {
		// Decided even when the link already drops the packet, so the
		// segment's state moves the same way whatever the link does.
		segDrop, segDelay := s.decide()
		drop = drop || segDrop
		delay += segDelay
		s = nil
	}
	if drop {
		return
	}
	after(delay, func() {
		if s == nil {
			write(pkt)
			return
		}
		if segDrop, segDelay := s.decide(); !segDrop {
			after(segDelay, func() { write(pkt) })
		}
	})
}

// after runs f once d has passed, at once when d isn't positive.
func after(d time.Duration, f func()) {
	if d <= 0 {
		f()
		return
	}
	time.AfterFunc(d, f)
}

// sinceNow is a clock that starts now (monotonic).
func sinceNow() func() time.Duration {
	born := time.Now()
	return func() time.Duration { return time.Since(born) }
}
