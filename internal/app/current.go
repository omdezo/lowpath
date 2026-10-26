package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/catalog"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/engine"
	"lowpath/internal/platform"
)

// The "current server" card: which server a game is talking to right now,
// whose network it is on, where it is (checked against the ping), and how
// the direct route to it behaves. It works for every game, catalog entry or
// not, because the server comes from the game's own traffic:
//
//   - during a boost, the session's flow table (Lowpath carries the packets);
//   - otherwise the OS's flow events (ETW on Windows), which need no packet
//     driver, so this also works next to anti-cheats that refuse WinDivert;
//   - otherwise the last server saved for the game ("last time").
const (
	// pidRefresh is how often the game PIDs are looked up while watching:
	// a game that starts is picked up within this time.
	pidRefresh = 2 * time.Second
	// watchLinger keeps the flow watch open after the last game exits, so a
	// game restarting (or a launcher handing over to the game) loses nothing.
	watchLinger = 30 * time.Second
	// learnedRecordEvery is how often the watched game's primary server is
	// saved, like route mode does (recordEvery).
	learnedRecordEvery = 5 * time.Second
	// minRecordPkts keeps a one-off packet (a NAT probe, a matchmaking ping)
	// from being saved as the game's server.
	minRecordPkts = 10
	// liveFor is how long after its last packet a server counts as current.
	liveFor   = 10 * time.Second
	maxOthers = 4
)

// Retry delays after a failed flow watch: missing rights don't come back
// while the process runs (elevation means a restart), other errors might.
const (
	watchRetryNeedAdmin = 5 * time.Minute
	watchRetryError     = 30 * time.Second
)

// errWatchEnded is the watch error when the OS stopped the flow watch
// without saying why.
var errWatchEnded = errors.New("the system stopped the flow watch; Lowpath tries again shortly")

type currentServers struct {
	flows   platform.FlowWatcher
	owners  platform.OwnerResolver
	nets    *netLookups
	locator Locator
	pinger  *catalog.LivePinger
	store   *Store
	log     *slog.Logger
	now     func() time.Time
	life    context.Context
	self    uint32

	// pids maps the PIDs of running games to their game id. It is read on
	// every flow event (any process on the PC), so it is swapped whole.
	pids atomic.Pointer[map[uint32]string]

	// learnMu guards learners; onFlow takes it on every game packet, so
	// nothing slow happens under it.
	learnMu  sync.Mutex
	learners map[string]*engine.FlowLearner

	mu         sync.Mutex
	watch      detect.FlowWatch
	watchErr   error
	retryAt    time.Time
	quietSince time.Time // when the last watched game stopped running
	pidsAt     time.Time
	recordAt   time.Time
	locateAt   time.Time
}

func newCurrentServers(flows platform.FlowWatcher, owners platform.OwnerResolver, lookup diag.NetworkLookup, locator Locator, ping diag.PingFunc, store *Store, life context.Context, log *slog.Logger) *currentServers {
	c := &currentServers{
		flows: flows, owners: owners, locator: locator, store: store, log: log, now: time.Now, life: life,
		nets:     newNetLookups(lookup, life),
		pinger:   catalog.NewLivePinger(icmpPinger{ping}, catalog.LiveOptions{}),
		self:     uint32(os.Getpid()),
		learners: map[string]*engine.FlowLearner{},
	}
	empty := map[uint32]string{}
	c.pids.Store(&empty)
	return c
}

// tick runs once a second with the games running now (id → process names):
// it opens or closes the flow watch, refreshes which PIDs are games,
// advances the learners and saves what they found.
func (c *currentServers) tick(running map[string][]string) {
	now := c.now()
	var closing detect.FlowWatch
	c.mu.Lock()
	if c.watch != nil {
		select {
		case <-c.watch.Done():
			// The OS ended the watch by itself: say so, and try again
			// later instead of showing "on" while nothing arrives.
			err := errWatchEnded
			if e, ok := c.watch.(interface{ Err() error }); ok && e.Err() != nil {
				err = e.Err()
			}
			c.log.Warn("flow watch ended", "err", err)
			closing = c.detachWatchLocked()
			c.watchErr, c.retryAt = err, now.Add(watchRetryError)
		default:
		}
	}
	if len(running) > 0 {
		c.quietSince = time.Time{}
		if c.watch == nil && !now.Before(c.retryAt) {
			c.startWatchLocked(now)
		}
		if c.watch != nil && now.Sub(c.pidsAt) >= pidRefresh {
			c.pidsAt = now
			c.refreshPIDs(running)
		}
	} else if c.watch != nil {
		if c.quietSince.IsZero() {
			c.quietSince = now
		} else if now.Sub(c.quietSince) >= watchLinger {
			closing = c.detachWatchLocked()
		}
	}
	record := now.Sub(c.recordAt) >= learnedRecordEvery
	if record {
		c.recordAt = now
	}
	c.mu.Unlock()
	// Closing waits for the OS to deliver its last events, which call
	// onFlow: never with c.mu held.
	if closing != nil {
		c.closeWatch(closing)
	}

	saves := map[string]netip.AddrPort{}
	c.learnMu.Lock()
	for id, l := range c.learners {
		l.Tick(now)
		list := l.Servers()
		if len(list) == 0 && running[id] == nil {
			delete(c.learners, id)
			continue
		}
		// Only a UDP server is saved: the saved server is proxy mode's
		// endpoint and Analyze's baseline (RelayNetwork.Endpoint), which
		// are UDP, and a TCP primary is a lobby's chat or matchmaking.
		if p := primary(list); record && p != nil && p.Proto == engine.ProtoUDP && p.PktsUp >= minRecordPkts && now.Sub(p.LastSeen) < liveFor {
			saves[id] = p.Addr
		}
	}
	c.learnMu.Unlock()
	for id, a := range saves {
		if err := c.store.RecordServer(id, a); err != nil {
			c.log.Warn("record learned game server", "game", id, "err", err)
		}
	}
}

func (c *currentServers) startWatchLocked(now time.Time) {
	w, err := c.flows.Watch(c.onFlow)
	if err != nil {
		if c.watchErr == nil || c.watchErr.Error() != err.Error() {
			c.log.Info("flow watch unavailable", "err", err)
		}
		c.watchErr = err
		switch {
		case errors.Is(err, errors.ErrUnsupported):
			c.retryAt = now.Add(100 * 365 * 24 * time.Hour) // nothing will change while we run
		case errors.Is(err, detect.ErrNeedAdmin):
			c.retryAt = now.Add(watchRetryNeedAdmin)
		default:
			c.retryAt = now.Add(watchRetryError)
		}
		return
	}
	c.watch, c.watchErr = w, nil
	c.pidsAt = time.Time{}
	c.log.Info("watching game flows")
}

// detachWatchLocked takes the open watch for closeWatch.
func (c *currentServers) detachWatchLocked() detect.FlowWatch {
	w := c.watch
	c.watch = nil
	empty := map[uint32]string{}
	c.pids.Store(&empty)
	return w
}

func (c *currentServers) closeWatch(w detect.FlowWatch) {
	if w == nil {
		return
	}
	if err := w.Close(); err != nil {
		c.log.Warn("close flow watch", "err", err)
	}
}

// refreshPIDs finds the processes of the running games.
func (c *currentServers) refreshPIDs(running map[string][]string) {
	names, err := c.owners.ProcessNames()
	if err != nil {
		return // keep the last map; the next refresh tries again
	}
	byName := map[string]string{}
	for id, procs := range running {
		for _, p := range procs {
			byName[strings.ToLower(p)] = id
		}
	}
	m := map[uint32]string{}
	for pid, name := range names {
		if id, ok := byName[strings.ToLower(name)]; ok && pid != c.self && pid != 0 {
			m[pid] = id
		}
	}
	c.pids.Store(&m)
}

// onFlow is the flow watcher's callback, called for every packet of every
// process: it returns at once unless the packet is a game's.
func (c *currentServers) onFlow(ev detect.FlowEvent) {
	id, ok := (*c.pids.Load())[ev.PID]
	if !ok || !gameRemote(ev.Proto, ev.Remote) {
		return
	}
	c.learnMu.Lock()
	l := c.learners[id]
	if l == nil {
		l = engine.NewFlowLearner()
		c.learners[id] = l
	}
	c.learnMu.Unlock()
	remote := ev.Remote
	if !ev.Outbound && !l.Knows(remote) && l.Knows(ev.Local) {
		// The kernel named the ends the other way round for this receive.
		remote = ev.Local
	}
	l.Observe(ev.Proto, remote, ev.Outbound, c.now())
}

// gameRemote filters out what a game process talks to that isn't its game
// server: the web (HTTP, HTTPS and QUIC on 80/443), DNS, the PC itself, the
// LAN's discovery traffic and multicast.
func gameRemote(proto string, a netip.AddrPort) bool {
	ip := a.Addr().Unmap()
	if !ip.IsValid() || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
		ip.IsInterfaceLocalMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || a.Port() == 0 {
		return false
	}
	switch a.Port() {
	case 53, 80, 443, 853, 5353, 5355, 1900:
		return false
	}
	return proto == engine.ProtoUDP || proto == engine.ProtoTCP
}

func primary(list []engine.ServerStatus) *engine.ServerStatus {
	for i := range list {
		if list[i].Primary {
			return &list[i]
		}
	}
	return nil
}

func (c *currentServers) close() {
	c.mu.Lock()
	w := c.detachWatchLocked()
	c.retryAt = time.Now().Add(100 * 365 * 24 * time.Hour) // no new watch after shutdown
	c.mu.Unlock()
	c.closeWatch(w)
	c.pinger.Close()
}

// --- the view ---------------------------------------------------------

// Card states.
const (
	currentLive = "live" // the game is talking to this server now
	currentLast = "last" // the server it used last time
	currentNone = "none" // nothing known yet
)

// Where the server came from.
const (
	sourceBoost   = "boost"   // a session carries the game's packets
	sourceTraffic = "traffic" // the OS reported the game's packets
	sourceHistory = "history" // saved from an earlier session or watch
)

type currentView struct {
	State   string        `json:"state"`
	Source  string        `json:"source,omitempty"`
	Message string        `json:"message,omitempty"`
	Watch   watchView     `json:"watch"`
	Server  *serverView   `json:"server"`
	Others  []serverBrief `json:"others"`
	You     *youView      `json:"you"`
}

type watchView struct {
	// State: on (watching), idle (no game running), needs_admin,
	// unsupported, error.
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type youView struct {
	City    string `json:"city,omitempty"`
	Country string `json:"country,omitempty"`
}

type serverBrief struct {
	Addr     netip.AddrPort `json:"addr"`
	Proto    string         `json:"proto"`
	PktsUp   uint64         `json:"pkts_up"`
	PktsDown uint64         `json:"pkts_down"`
}

type serverView struct {
	Addr     netip.AddrPort `json:"addr"`
	Proto    string         `json:"proto,omitempty"` // "" when only the address was saved
	PktsUp   *uint64        `json:"pkts_up"`
	PktsDown *uint64        `json:"pkts_down"`
	LastSeen *time.Time     `json:"last_seen"`

	// Network is the registry's answer for the address (AS, ISP, prefix,
	// country, city); nil while it is looked up, for a LAN address, or when
	// the lookup failed (NetworkNote says which).
	Network     *diag.NetworkInfo `json:"network"`
	NetworkNote string            `json:"network_note,omitempty"`
	LookingUp   bool              `json:"looking_up"`
	LAN         bool              `json:"lan"`
	Geo         catalog.GeoCheck  `json:"geo"`

	PingMS   *float64 `json:"ping_ms"`
	JitterMS *float64 `json:"jitter_ms"`
	LossPct  *float64 `json:"loss_pct"`
	Sent     int      `json:"sent"`
	Received int      `json:"received"`
	PingNote string   `json:"ping_note,omitempty"`
}

// view builds the card for g. snap is the session's snapshot when a
// session carries g, else nil.
func (c *currentServers) view(g Game, snap *engine.Snapshot) currentView {
	v := currentView{Others: []serverBrief{}, Watch: c.watchView()}
	var list []engine.ServerStatus
	if snap != nil && len(snap.Servers) > 0 {
		v.Source, list = sourceBoost, snap.Servers
	} else {
		c.learnMu.Lock()
		l := c.learners[g.ID]
		c.learnMu.Unlock()
		if l != nil {
			v.Source, list = sourceTraffic, l.Servers()
		}
	}
	now := c.now()
	if p := primary(list); p != nil && now.Sub(p.LastSeen) < liveFor {
		v.State = currentLive
		up, down, seen := p.PktsUp, p.PktsDown, p.LastSeen
		v.Server = &serverView{Addr: p.Addr, Proto: p.Proto, PktsUp: &up, PktsDown: &down, LastSeen: &seen}
		for _, s := range list {
			if !s.Primary && len(v.Others) < maxOthers {
				v.Others = append(v.Others, serverBrief{s.Addr, s.Proto, s.PktsUp, s.PktsDown})
			}
		}
	} else if saved := c.store.Servers(g.ID); len(saved) > 0 {
		v.State, v.Source = currentLast, sourceHistory
		v.Server = &serverView{Addr: saved[0]}
	} else {
		v.State, v.Source = currentNone, ""
		v.Message = "Start " + g.Name + " and join a match: Lowpath learns its server from the game's own traffic."
		return v
	}
	v.You = c.fillServer(v.Server)
	return v
}

func (c *currentServers) watchView() watchView {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.watch != nil:
		return watchView{State: "on"}
	case c.watchErr == nil:
		return watchView{State: "idle", Message: "Starts watching when a game runs."}
	case errors.Is(c.watchErr, detect.ErrNeedAdmin):
		return watchView{State: "needs_admin", Message: "Without a boost, Lowpath sees which server a game uses only when it runs as administrator. Boost the game, or restart Lowpath as administrator."}
	case errors.Is(c.watchErr, errors.ErrUnsupported):
		return watchView{State: "unsupported", Message: "This system can't report a game's server without a boost yet: boost the game to see it."}
	default:
		return watchView{State: "error", Message: "Can't watch game traffic: " + c.watchErr.Error()}
	}
}

// fillServer adds the network, the live ping and the geo check to sv, and
// returns where this PC is (for the card's "from").
func (c *currentServers) fillServer(sv *serverView) *youView {
	ip := sv.Addr.Addr().Unmap()
	if lanAddr(ip) {
		sv.LAN = true
		sv.NetworkNote = "A local or private address: a LAN server, or the game goes through a VPN or proxy."
	} else {
		info, note, pending := c.nets.get(ip)
		sv.Network, sv.NetworkNote, sv.LookingUp = info, note, pending
	}

	ping := c.pinger.Stats(ip)
	sv.Sent, sv.Received = ping.Sent, ping.Received
	if ping.Received > 0 {
		avg, jit, loss := msOf(ping.Avg), msOf(ping.Jitter), ping.LossPct()
		sv.PingMS, sv.JitterMS, sv.LossPct = &avg, &jit, &loss
	} else {
		sv.PingNote = ping.Note
	}

	// Where this PC is: the last location lookup (Settings → Network). Ask
	// for one in the background when there is none, at most every 2 min.
	loc, ok := c.locator.Last()
	if !ok {
		c.locateSoon()
	}
	var you *youView
	var here catalog.Geo
	if ok {
		if e := pickEndpoint(loc, ip); e != nil {
			you = &youView{City: e.City, Country: e.Country}
			if e.Lat != nil && e.Lon != nil {
				here = catalog.NewGeo(*e.Lat, *e.Lon)
			}
		}
	}
	var there catalog.Geo
	if n := sv.Network; n != nil && n.Lat != nil && n.Lon != nil {
		there = catalog.NewGeo(*n.Lat, *n.Lon)
	}
	var minRTT time.Duration
	if ping.Received > 0 {
		minRTT = ping.Min
	}
	sv.Geo = catalog.CheckGeo(here, there, minRTT)
	if sv.LAN {
		sv.Geo = catalog.GeoCheck{State: catalog.GeoCheckUnknown, Note: "No location for a private address."}
	} else if sv.Network == nil || sv.Network.City == "" {
		// Nothing to check: say what the ping alone shows, or nothing.
		g := catalog.CheckGeo(catalog.Geo{}, catalog.Geo{}, minRTT)
		if minRTT > 0 {
			sv.Geo = g
			if sv.Network != nil && sv.Network.Country != "" {
				// A country-only answer is common for anycast and cloud
				// ranges, and may be where the owner is registered.
				sv.Geo.Note = "The IP database knows only the country. " + g.Note
			}
		} else {
			sv.Geo = catalog.GeoCheck{State: catalog.GeoCheckUnknown}
		}
	}
	return you
}

// pickEndpoint is this PC's endpoint in the server's family, else the other.
func pickEndpoint(loc diag.Location, server netip.Addr) *diag.Endpoint {
	if server.Is4() {
		if loc.IPv4 != nil {
			return loc.IPv4
		}
		return loc.IPv6
	}
	if loc.IPv6 != nil {
		return loc.IPv6
	}
	return loc.IPv4
}

func (c *currentServers) locateSoon() {
	c.mu.Lock()
	now := c.now()
	due := now.Sub(c.locateAt) >= 2*time.Minute
	if due {
		c.locateAt = now
	}
	c.mu.Unlock()
	if !due {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.life, 15*time.Second)
		defer cancel()
		c.locator.Locate(ctx, false)
	}()
}

// lanAddr reports addresses no registry knows: private (RFC 1918, ULA),
// CGNAT (RFC 6598), loopback and link-local.
func lanAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsUnspecified() ||
		netip.MustParsePrefix("100.64.0.0/10").Contains(a)
}

func msOf(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// icmpPinger adapts diag.PingFunc (icmp.Ping) to catalog.Pinger.
type icmpPinger struct{ ping diag.PingFunc }

func (p icmpPinger) Ping(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	return catalog.ICMPPinger{Send: p.ping}.Ping(dst, timeout)
}

// --- network lookups, never blocking the card ------------------------------

// netLookups looks server addresses up in the background: the card shows
// "looking up" at once and the answer on its next poll.
type netLookups struct {
	lookup diag.NetworkLookup
	life   context.Context
	mu     sync.Mutex
	m      map[netip.Addr]*netEntry
}

type netEntry struct {
	info    *diag.NetworkInfo
	note    string
	done    bool
	expires time.Time
	added   time.Time
}

const (
	netLookupTTL    = time.Hour // the cache under it keeps answers 24 h
	netLookupErrTTL = time.Minute
	netLookupMax    = 64
)

func newNetLookups(l diag.NetworkLookup, life context.Context) *netLookups {
	return &netLookups{lookup: l, life: life, m: map[netip.Addr]*netEntry{}}
}

func (n *netLookups) get(a netip.Addr) (*diag.NetworkInfo, string, bool) {
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	e := n.m[a]
	if e != nil && (!e.done || now.Before(e.expires)) {
		if !e.done {
			return nil, "", true
		}
		return e.info, e.note, false
	}
	if e == nil && len(n.m) >= netLookupMax {
		var oldest netip.Addr
		var at time.Time
		for k, v := range n.m {
			if v.done && (at.IsZero() || v.added.Before(at)) {
				oldest, at = k, v.added
			}
		}
		delete(n.m, oldest)
	}
	n.m[a] = &netEntry{added: now}
	go n.run(a)
	return nil, "", true
}

func (n *netLookups) run(a netip.Addr) {
	ctx, cancel := context.WithTimeout(n.life, 12*time.Second)
	defer cancel()
	info, err := n.lookup.LookupNetwork(ctx, a)
	e := &netEntry{done: true, added: time.Now(), expires: time.Now().Add(netLookupTTL)}
	if err != nil {
		e.expires = time.Now().Add(netLookupErrTTL)
		e.note = "Network unknown: " + err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			e.note = "Network unknown: the lookup took too long."
		}
	} else {
		e.info = &info
	}
	n.mu.Lock()
	n.m[a] = e
	n.mu.Unlock()
}

// --- HTTP -----------------------------------------------------------------

// handleCurrentServer answers GET /api/games/{id}/current-server. It never
// waits on the network: lookups and pings run in the background and show
// up on the next poll (the card polls every 2 s while it is on screen,
// which is also what keeps the server pinged).
func (s *Server) handleCurrentServer(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "not_found", errors.New("unknown game"))
		return
	}
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	var snap *engine.Snapshot
	if sess != nil && sess.GameID == g.ID {
		sn := sess.eng.Snapshot()
		snap = &sn
	}
	writeJSON(w, 200, s.current.view(g, snap))
}

// runningGames is the process list of every game running now.
func (s *Server) runningGames() map[string][]string {
	s.mu.Lock()
	ids := make([]string, 0, len(s.running))
	for id := range s.running {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	out := map[string][]string{}
	for _, id := range ids {
		if g, ok := s.game(id); ok && len(g.Processes) > 0 {
			out[id] = g.Processes
		}
	}
	return out
}
