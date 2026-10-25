package relay

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/proto"
)

const (
	// A client address quiet in both directions this long loses its socket.
	peerTTL = 60 * time.Second
	// One the next hop never answered loses it sooner: spoofed sources,
	// scanners and clients with a wrong key (the egress drops their packets
	// without a word) would otherwise each hold a socket for a full peerTTL.
	unansweredPeerTTL = 10 * time.Second
	// Each client address costs a socket, a goroutine and a buffer.
	maxPeers = 4096
)

// Forwarder is a forwarding hop: it passes overlay packets between clients
// and one next hop (normally the egress relay) unchanged. It does not check
// access tokens; the egress does. Each client address gets its own socket
// toward the next hop, so the egress sees the client's paths as distinct
// return addresses and its replies come back to the right client.
type Forwarder struct {
	conn  *net.UDPConn
	next  *net.UDPAddr
	log   *slog.Logger
	limit int // maxPeers; lowered by tests

	mu         sync.Mutex
	peers      map[netip.AddrPort]*peer
	unanswered int // peers the next hop has never replied to

	closeOnce sync.Once
	closeErr  error

	up, down, malformed, limitDrops, evicted, tooBig atomic.Uint64
}

type peer struct {
	conn     *net.UDPConn // connected to the next hop
	last     atomic.Int64 // unix nanos of the last packet in either direction
	answered atomic.Bool  // the next hop has replied at least once (set under Forwarder.mu)
}

// ForwarderStats are cumulative counters since the hop started (Peers is the
// current value).
type ForwarderStats struct {
	Peers       int    `json:"peers"`        // client addresses with an open socket
	PacketsUp   uint64 `json:"packets_up"`   // client -> next hop
	PacketsDown uint64 `json:"packets_down"` // next hop -> client
	Malformed   uint64 `json:"malformed"`    // not an overlay packet; dropped
	LimitDrops  uint64 `json:"limit_drops"`  // new client refused: every slot in use by one the next hop answers
	Evicted     uint64 `json:"evicted"`      // unanswered clients dropped to make room
	TooBig      uint64 `json:"too_big"`      // packets from the next hop too big to relay
}

// ListenForwarder binds a forwarding hop on addr that passes packets to next.
func ListenForwarder(addr string, next netip.AddrPort, log *slog.Logger) (*Forwarder, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return &Forwarder{
		conn: c, next: net.UDPAddrFromAddrPort(next), log: log, limit: maxPeers,
		peers: map[netip.AddrPort]*peer{},
	}, nil
}

func (f *Forwarder) Addr() netip.AddrPort {
	return f.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Close stops the hop: Serve returns and every peer socket is closed.
func (f *Forwarder) Close() error {
	f.closeOnce.Do(func() {
		f.closeErr = f.conn.Close()
		f.mu.Lock()
		for src, p := range f.peers {
			f.removeLocked(src, p)
		}
		f.mu.Unlock()
	})
	return f.closeErr
}

// Stats returns a snapshot of the hop's counters.
func (f *Forwarder) Stats() ForwarderStats {
	f.mu.Lock()
	peers := len(f.peers)
	f.mu.Unlock()
	return ForwarderStats{
		Peers:       peers,
		PacketsUp:   f.up.Load(),
		PacketsDown: f.down.Load(),
		Malformed:   f.malformed.Load(),
		LimitDrops:  f.limitDrops.Load(),
		Evicted:     f.evicted.Load(),
		TooBig:      f.tooBig.Load(),
	}
}

// Serve runs until ctx is cancelled or the hop is closed.
func (f *Forwarder) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		f.Close()
	}()
	go f.gcLoop(ctx)

	buf := make([]byte, 65535)
	for {
		n, src, err := f.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue // transient (e.g. ICMP unreachable on Windows)
		}
		// Only overlay packets are passed on, so junk and scanners never get
		// a socket on the next hop.
		if _, _, err := proto.Parse(buf[:n]); err != nil {
			f.malformed.Add(1)
			continue
		}
		now := time.Now()
		p := f.peer(src, now)
		if p == nil {
			continue
		}
		p.last.Store(now.UnixNano())
		p.conn.Write(buf[:n])
		f.up.Add(1)
	}
}

// peer returns src's socket toward the next hop, opening one if needed.
func (f *Forwarder) peer(src netip.AddrPort, now time.Time) *peer {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.peers[src]; ok {
		return p
	}
	if len(f.peers) >= f.limit && !f.evictLocked() {
		f.limitDrops.Add(1)
		return nil
	}
	c, err := net.DialUDP("udp", nil, f.next)
	if err != nil {
		f.log.Warn("dial next hop", "next", f.next, "err", err)
		return nil
	}
	p := &peer{conn: c}
	p.last.Store(now.UnixNano())
	f.peers[src] = p
	f.unanswered++
	go f.downstream(src, p)
	return p
}

// evictLocked makes room by dropping one peer the next hop never answered.
// Peers that work are never pushed out, so a flood of spoofed sources only
// churns through the unanswered ones. f.mu must be held.
func (f *Forwarder) evictLocked() bool {
	if f.unanswered == 0 {
		return false
	}
	for src, p := range f.peers {
		if !p.answered.Load() {
			f.removeLocked(src, p)
			f.evicted.Add(1)
			return true
		}
	}
	return false
}

// removeLocked closes src's socket, which also ends its downstream goroutine.
// f.mu must be held.
func (f *Forwarder) removeLocked(src netip.AddrPort, p *peer) {
	delete(f.peers, src)
	if !p.answered.Load() {
		f.unanswered--
	}
	p.conn.Close()
}

// downstream relays next hop -> client for one client address.
func (f *Forwarder) downstream(src netip.AddrPort, p *peer) {
	// Room for the egress's largest packet; one byte more shows when a
	// packet was cut short.
	buf := make([]byte, proto.MaxOverhead+maxDatagram+1)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // ICMP unreachable while the next hop is down, or an oversized packet on Windows
		}
		if n >= len(buf) {
			f.tooBig.Add(1)
			continue
		}
		p.last.Store(time.Now().UnixNano())
		if !p.answered.Load() {
			f.markAnswered(src, p)
		}
		f.conn.WriteToUDPAddrPort(buf[:n], src)
		f.down.Add(1)
	}
}

func (f *Forwarder) markAnswered(src netip.AddrPort, p *peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !p.answered.Load() && f.peers[src] == p {
		f.unanswered--
	}
	p.answered.Store(true)
}

func (f *Forwarder) gcLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			f.expire(now)
		}
	}
}

// expire closes the sockets of peers that have gone quiet.
func (f *Forwarder) expire(now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for src, p := range f.peers {
		quiet := now.Sub(time.Unix(0, p.last.Load()))
		if quiet > peerTTL || !p.answered.Load() && quiet > unansweredPeerTTL {
			f.removeLocked(src, p)
		}
	}
}
