// Package netsim is a bidirectional UDP forwarder with optional impairment
// (delay, jitter, loss, periodic lag spikes). With a zero Profile it is a plain
// relay hop (ingress / transit); with a non-zero one it simulates an ISP or
// carrier path so the whole system can be exercised on one machine.
package netsim

import (
	"context"
	"errors"
	"math/rand/v2"
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
}

type Link struct {
	listen  *net.UDPConn
	target  *net.UDPAddr
	profile atomic.Pointer[Profile]
	born    time.Time

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
	l := &Link{listen: c, target: net.UDPAddrFromAddrPort(target), born: time.Now(), peers: map[netip.AddrPort]*net.UDPConn{}}
	l.profile.Store(&p)
	return l, nil
}

func (l *Link) Addr() netip.AddrPort { return l.listen.LocalAddr().(*net.UDPAddr).AddrPort() }

func (l *Link) Profile() Profile { return *l.profile.Load() }

// SetProfile changes impairment at runtime (used to simulate a degrading route).
func (l *Link) SetProfile(p Profile) { l.profile.Store(&p) }

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
		l.send(pkt, func(b []byte) { up.Write(b) })
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
			l.send(pkt, func(b []byte) { l.listen.WriteToUDPAddrPort(b, src) })
		}
	}()
	return c
}

func (l *Link) send(pkt []byte, write func([]byte)) {
	p := l.profile.Load()
	loss := p.Loss
	delay := p.Delay
	if p.SpikeEvery > 0 && p.SpikeLen > 0 {
		phase := time.Since(l.born) % p.SpikeEvery
		if phase < p.SpikeLen {
			delay += p.SpikeExtra
			loss += p.SpikeLoss
		}
	}
	if loss > 0 && rand.Float64() < loss {
		return
	}
	if p.Jitter > 0 {
		delay += time.Duration(rand.NormFloat64() * float64(p.Jitter))
	}
	if delay <= 0 {
		write(pkt)
		return
	}
	time.AfterFunc(delay, func() { write(pkt) })
}
