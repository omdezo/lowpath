package capture

import (
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// socket is one local UDP endpoint and the process that owns it. IPv6
// entries matter too: a dual-stack socket bound to [::] sends IPv4 packets.
type socket struct {
	local netip.AddrPort
	pid   uint32
}

// owners decides whether a packet's local UDP endpoint belongs to a game.
//
// It keeps an immutable table of every UDP socket's owner, rebuilt every
// second in the background, so game packets are answered from memory. A
// socket missing from the table may simply be newer than it (that is the
// case for the very first packet a game sends), so a miss rebuilds the table
// before the packet is let out.
type owners struct {
	self    uint32          // our own PID: never captured
	names   map[string]bool // lower-case image names to capture
	pids    map[uint32]bool // exact PIDs to capture
	sockets func() ([]socket, error)
	procs   func() (map[uint32]string, error) // PID -> image name

	mu    sync.Mutex        // serializes rebuilds
	known map[uint32]string // last process list; guarded by mu
	cur   atomic.Pointer[ownerTable]
}

type ownerTable struct {
	at    time.Time                 // taken before the socket list was read
	exact map[netip.AddrPort]uint32 // sockets bound to one IPv4 address
	wild  map[uint16]uint32         // bound to 0.0.0.0
	wild6 map[uint16]uint32         // bound to [::], possibly dual-stack
	game  map[uint32]bool
	pids  []uint32 // running game PIDs, sorted

	// unowned holds endpoints this table was fresh for and still had no
	// owner (a socket closed with packets in flight, or traffic no local
	// socket sent). It saves a rebuild per packet and is dropped with the
	// table a second later.
	mu      sync.Mutex
	unowned map[netip.AddrPort]bool
}

func newOwners(processes []string, pids []uint32, self uint32,
	sockets func() ([]socket, error), procs func() (map[uint32]string, error)) (*owners, error) {
	o := &owners{
		self:    self,
		names:   map[string]bool{},
		pids:    map[uint32]bool{},
		sockets: sockets,
		procs:   procs,
	}
	for _, p := range processes {
		if p = normalizeName(p); p != "" {
			o.names[p] = true
		}
	}
	for _, p := range pids {
		o.pids[p] = true
	}
	if err := o.refresh(); err != nil {
		return nil, err
	}
	return o, nil
}

// normalizeName lower-cases an image name and adds ".exe" when there is no
// extension, so "CS2" matches cs2.exe.
func normalizeName(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s != "" && !strings.Contains(s, ".") {
		s += ".exe"
	}
	return s
}

// isGame reports whether the socket that sent from local belongs to a game
// process. err is set only when the owner could not be determined.
func (o *owners) isGame(local netip.AddrPort) (bool, error) {
	t := o.cur.Load()
	if g, known := t.verdict(local); known {
		return g, nil
	}
	t, err := o.refreshSince(time.Now())
	if err != nil {
		return false, err
	}
	if g, known := t.verdict(local); known {
		return g, nil
	}
	t.mu.Lock()
	t.unowned[local] = true
	t.mu.Unlock()
	return false, nil
}

func (t *ownerTable) verdict(local netip.AddrPort) (game, known bool) {
	pid, ok := t.exact[local]
	if !ok {
		pid, ok = t.wild[local.Port()]
	}
	if !ok {
		pid, ok = t.wild6[local.Port()]
	}
	if ok {
		return t.game[pid], true
	}
	t.mu.Lock()
	known = t.unowned[local]
	t.mu.Unlock()
	return false, known
}

// gamePIDs lists the running game processes.
func (o *owners) gamePIDs() []uint32 {
	return slices.Clone(o.cur.Load().pids)
}

// refresh rebuilds the table with a fresh process list, which catches games
// started since and PIDs reused by other programs. The process list takes
// milliseconds to read (the socket table ~0.1 ms), so it is read before
// taking the lock packet lookups may wait on.
func (o *owners) refresh() error {
	names, err := o.procs()
	if err != nil {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.known = names
	return o.rebuild()
}

// refreshSince returns a table read after seen, rebuilding it unless another
// goroutine already did while this one waited.
func (o *owners) refreshSince(seen time.Time) (*ownerTable, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if t := o.cur.Load(); t.at.After(seen) {
		return t, nil
	}
	if err := o.rebuild(); err != nil {
		return nil, err
	}
	return o.cur.Load(), nil
}

// rebuild reads the socket table and publishes a new ownerTable. The process
// list is re-read only if a socket has a PID it doesn't know yet.
func (o *owners) rebuild() error {
	at := time.Now()
	socks, err := o.sockets()
	if err != nil {
		return err
	}
	for _, s := range socks {
		if _, ok := o.known[s.pid]; !ok {
			names, err := o.procs()
			if err != nil {
				return err
			}
			o.known = names
			break
		}
	}

	t := &ownerTable{
		at:      at,
		exact:   make(map[netip.AddrPort]uint32, len(socks)),
		wild:    map[uint16]uint32{},
		wild6:   map[uint16]uint32{},
		game:    map[uint32]bool{},
		unowned: map[netip.AddrPort]bool{},
	}
	for _, s := range socks {
		a, port := s.local.Addr(), s.local.Port()
		switch {
		case a.Is4() && a.IsUnspecified():
			t.wild[port] = s.pid
		case a.Is4() || a.Is4In6():
			t.exact[netip.AddrPortFrom(a.Unmap(), port)] = s.pid
		case a.IsUnspecified():
			t.wild6[port] = s.pid
		}
	}
	for pid, name := range o.known {
		if pid != o.self && (o.pids[pid] || o.names[strings.ToLower(name)]) {
			t.game[pid] = true
			t.pids = append(t.pids, pid)
		}
	}
	slices.Sort(t.pids)
	o.cur.Store(t)
	return nil
}
