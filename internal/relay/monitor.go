package relay

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

const (
	// A server leg slower than this is useless for gaming; count it as a miss.
	pingTimeout = time.Second
	// Stop pinging a server nobody has sent data or probes for in this long.
	serverIdle = 30 * time.Second
	// After this many consecutive misses the smoothed RTT is stale: report
	// "unknown" rather than an old number.
	maxMisses = 3
	// Upper bound on servers pinged at once (one goroutine each), so a client
	// spraying destinations cannot make the relay ping the whole internet.
	maxServers = 512
	// EWMA weight of a new sample: smooths single-ping noise but still follows
	// a route change within a few seconds.
	rttWeight = 0.25
)

// pingFunc sends one echo request and returns the round trip time (icmp.Ping).
type pingFunc func(dst netip.Addr, timeout time.Duration) (time.Duration, error)

// monitor pings the game servers this relay delivers to, so probe replies can
// tell clients the relay->server leg of their route.
type monitor struct {
	ping     pingFunc
	interval time.Duration
	ctx      context.Context
	stop     context.CancelFunc

	mu      sync.Mutex
	servers map[netip.Addr]*server
}

type server struct {
	lastUsed time.Time
	rtt      time.Duration // smoothed, meaningful only while known
	known    bool
	misses   int
}

func newMonitor(ping pingFunc, interval time.Duration) *monitor {
	ctx, stop := context.WithCancel(context.Background())
	return &monitor{ping: ping, interval: interval, ctx: ctx, stop: stop, servers: map[netip.Addr]*server{}}
}

// touch marks a as in use, starting its pinger if it is new.
func (m *monitor) touch(a netip.Addr) {
	m.mu.Lock()
	m.useLocked(a.Unmap(), time.Now())
	m.mu.Unlock()
}

// observe marks a as in use and returns its smoothed RTT; ok is false until
// the first reply, after maxMisses misses in a row, or when a cannot be
// monitored (limit reached, relay stopping).
func (m *monitor) observe(a netip.Addr) (rtt time.Duration, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.useLocked(a.Unmap(), time.Now())
	if s == nil || !s.known {
		return 0, false
	}
	return s.rtt, true
}

func (m *monitor) useLocked(a netip.Addr, now time.Time) *server {
	if s, ok := m.servers[a]; ok {
		s.lastUsed = now
		return s
	}
	if len(m.servers) >= maxServers || m.ctx.Err() != nil {
		return nil
	}
	s := &server{lastUsed: now}
	m.servers[a] = s
	go m.run(a, s)
	return s
}

// count is the number of servers currently being pinged.
func (m *monitor) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.servers)
}

// run pings a right away and then every interval until it goes idle.
func (m *monitor) run(a netip.Addr, s *server) {
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		rtt, err := m.ping(a, pingTimeout)
		if !m.record(a, s, rtt, err) {
			return
		}
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// record folds one ping result into s and reports whether a is still in use;
// an idle server is forgotten here, by its own pinger, so the map entry and
// the goroutine always go away together.
func (m *monitor) record(a netip.Addr, s *server, rtt time.Duration, err error) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		s.misses++
		if s.misses >= maxMisses {
			s.known = false
		}
	} else {
		s.misses = 0
		if s.known {
			s.rtt += time.Duration(rttWeight * float64(rtt-s.rtt))
		} else {
			s.rtt, s.known = rtt, true
		}
	}
	if time.Since(s.lastUsed) > serverIdle {
		delete(m.servers, a)
		return false
	}
	return true
}
