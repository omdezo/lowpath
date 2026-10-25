package engine

import (
	"net"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/dedup"
)

const (
	flowIdle      = time.Minute     // flows and servers silent this long are forgotten
	primaryWindow = 5               // seconds of upstream traffic that decide the primary server
	maxServers    = 8               // servers listed in a snapshot
	pingInterval  = time.Second / 5 // capture-mode "before" pings to the primary server
)

type flowKey struct{ local, remote netip.AddrPort }

// flow is one game socket talking to one server. Each gets its own random id,
// so the egress relay keeps them apart and dedups them independently.
type flow struct {
	id     uint64
	local  netip.AddrPort
	remote netip.AddrPort
	srv    *server
	seq    atomic.Uint32
	last   atomic.Int64 // unix nanos of the last packet in either direction

	mu     sync.Mutex
	down   dedup.Window
	direct *net.UDPConn // proxy-mode fallback route, opened lazily
}

func (f *flow) touch(now time.Time) { f.last.Store(now.UnixNano()) }

// server aggregates every flow to one remote address; it is what the UI lists
// as "game servers" and what the before/after measurements aim at.
type server struct {
	addr     netip.AddrPort
	up, down atomic.Uint64
	last     atomic.Int64

	// Upstream counter sampled once per second; only the selection loop
	// touches it.
	hist [primaryWindow]uint64
}

func (s *server) touch(now time.Time) { s.last.Store(now.UnixNano()) }

// ServerStatus is one entry of Snapshot.Servers.
type ServerStatus struct {
	Addr     netip.AddrPort `json:"addr"`
	PktsUp   uint64         `json:"pkts_up"`
	PktsDown uint64         `json:"pkts_down"`
	LastSeen time.Time      `json:"last_seen"`
	Primary  bool           `json:"primary"`
}

// flowTable is read on every packet and written only when a flow appears or
// expires, hence the RWMutex.
type flowTable struct {
	mu      sync.RWMutex
	flows   map[flowKey]*flow
	byID    map[uint64]*flow
	servers map[netip.AddrPort]*server
	primary atomic.Pointer[server]
	tick    int
}

func newFlowTable() *flowTable {
	return &flowTable{flows: map[flowKey]*flow{}, byID: map[uint64]*flow{}, servers: map[netip.AddrPort]*server{}}
}

// get returns the flow for (local, remote), creating it on first use, and
// marks it active. The touch happens under the lock so gc can't collect a flow
// between lookup and use.
func (t *flowTable) get(local, remote netip.AddrPort, now time.Time) *flow {
	k := flowKey{local, remote}
	t.mu.RLock()
	f := t.flows[k]
	if f != nil {
		f.touch(now)
		f.srv.touch(now)
	}
	t.mu.RUnlock()
	if f != nil {
		return f
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if f = t.flows[k]; f == nil {
		s := t.servers[remote]
		if s == nil {
			s = &server{addr: remote}
			t.servers[remote] = s
			// The first server the game talks to is the best guess until the
			// selection loop has a traffic count, so "before" starts at once.
			t.primary.CompareAndSwap(nil, s)
		}
		f = &flow{id: t.newID(), local: local, remote: remote, srv: s}
		t.flows[k] = f
		t.byID[f.id] = f
	}
	f.touch(now)
	f.srv.touch(now)
	return f
}

// newID picks a random flow id not in use; ids are random so a relay can't
// confuse our flows with another client's.
func (t *flowTable) newID() uint64 {
	for {
		id := randID()
		if _, taken := t.byID[id]; !taken && id != 0 {
			return id
		}
	}
}

func (t *flowTable) byFlowID(id uint64) *flow {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.byID[id]
}

// updatePrimary runs once per second. The primary server is the one with the
// most upstream packets over the last primaryWindow seconds; ties and quiet
// periods keep the current one so "before" doesn't hop between servers.
// It reports whether the primary changed.
func (t *flowTable) updatePrimary() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	slot := t.tick % primaryWindow
	t.tick++
	cur := t.primary.Load()
	var best *server
	var bestN, curN uint64
	for _, s := range t.servers {
		up := s.up.Load()
		n := up - s.hist[slot]
		s.hist[slot] = up
		if s == cur {
			curN = n
		}
		if n > bestN {
			best, bestN = s, n
		}
	}
	if best == nil || best == cur || bestN <= curN {
		return false
	}
	t.primary.Store(best)
	return true
}

// gc forgets flows and servers idle longer than flowIdle. It reports whether
// the primary server was among them.
func (t *flowTable) gc(now time.Time) (primaryGone bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-flowIdle).UnixNano()
	for k, f := range t.flows {
		if f.last.Load() < cutoff {
			f.mu.Lock()
			if f.direct != nil {
				f.direct.Close()
			}
			f.mu.Unlock()
			delete(t.flows, k)
			delete(t.byID, f.id)
		}
	}
	for a, s := range t.servers {
		if s.last.Load() < cutoff {
			delete(t.servers, a)
			if t.primary.CompareAndSwap(s, nil) {
				primaryGone = true
			}
		}
	}
	if primaryGone {
		// Fall back to whichever server is still in use, if any.
		var next *server
		for _, s := range t.servers {
			if next == nil || s.last.Load() > next.last.Load() {
				next = s
			}
		}
		if next != nil {
			t.primary.Store(next)
		}
	}
	return primaryGone
}

// list returns the flow count and up to maxServers servers, most recently
// active first. The primary is always included.
func (t *flowTable) list() (flows int, out []ServerStatus) {
	t.mu.RLock()
	flows = len(t.flows)
	all := make([]*server, 0, len(t.servers))
	for _, s := range t.servers {
		all = append(all, s)
	}
	t.mu.RUnlock()

	slices.SortFunc(all, func(a, b *server) int {
		la, lb := a.last.Load(), b.last.Load()
		switch {
		case la > lb:
			return -1
		case la < lb:
			return 1
		}
		return a.addr.Compare(b.addr)
	})
	primary := t.primary.Load()
	if i := slices.Index(all, primary); i >= maxServers {
		all[maxServers-1] = primary
	}
	out = make([]ServerStatus, 0, min(len(all), maxServers))
	for _, s := range all[:min(len(all), maxServers)] {
		out = append(out, ServerStatus{
			Addr:     s.addr,
			PktsUp:   s.up.Load(),
			PktsDown: s.down.Load(),
			LastSeen: time.Unix(0, s.last.Load()),
			Primary:  s == primary,
		})
	}
	return flows, out
}

// closeAll shuts every lazily opened direct socket (proxy-mode fallback).
func (t *flowTable) closeAll() {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, f := range t.flows {
		f.mu.Lock()
		if f.direct != nil {
			f.direct.Close()
		}
		f.mu.Unlock()
	}
}
