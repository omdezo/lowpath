// Package engine is the client side of Lowpath: it takes a game's UDP traffic,
// numbers each packet, sends a copy down several relay paths at once, drops
// duplicate replies, and continuously measures every path plus the direct
// (un-optimized) route so the UI can show before/after numbers.
//
// The game's packets reach the engine in one of two ways:
//
//   - proxy mode: the game (or a test client) sends to a local UDP port and
//     every flow goes to one configured game server;
//   - capture mode: a packet capture layer (WinDivert) hands each outbound
//     game packet to Send and puts the replies back with Config.Deliver, so
//     the game needs no settings changed and may talk to any server.
package engine

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/proto"
)

type Config struct {
	// Proxy mode (Listen != ""): the local address the game sends to. Every
	// flow goes to Target (the game server as the egress relay reaches it);
	// Direct is the un-optimized route used for the UDP-echo baseline and for
	// fallback (normally == Target).
	Listen string
	Target netip.AddrPort
	Direct netip.AddrPort
	// PingBefore makes proxy mode measure "before" with ICMP pings to Target
	// instead of UDP-echo probes along Direct. Only the demo's simulated
	// server echoes those; a real game server would just get junk packets.
	PingBefore bool

	// Capture mode (Listen == "" and Deliver != nil): replies for the game.
	// It is called from several goroutines at once and payload is only valid
	// during the call. No call is in progress once Stop returns.
	Deliver func(local, remote netip.AddrPort, payload []byte)

	Paths      []PathSpec // candidate relay chains
	Locked     string     // path ID when the user picked a specific route
	DupLevel   int        // 0 = auto, otherwise how many paths carry each packet
	AutoSwitch bool       // replace a degraded path with a better one
	KillSwitch bool       // when every path is down: drop (true) or go direct (false)
	Seed       map[string]Stats
}

type Event struct {
	TS   time.Time `json:"ts"`
	Kind string    `json:"kind"` // applied | switch | degraded | fallback | recovered
	Msg  string    `json:"msg"`
}

type PathStatus struct {
	PathSpec
	Stats
	Active bool `json:"active"`
}

type Snapshot struct {
	Mode           string         `json:"mode"`   // "proxy" | "capture"
	Listen         string         `json:"listen"` // "" in capture mode
	Before         Stats          `json:"before"`
	BeforeKnown    bool           `json:"before_known"` // false: the server ignores ping (or nothing to ping yet)
	After          Stats          `json:"after"`
	ServerLeg      float64        `json:"server_leg_ms"` // latest egress relay -> game server RTT
	ServerLegKnown bool           `json:"server_leg_known"`
	Servers        []ServerStatus `json:"servers"`
	Paths          []PathStatus   `json:"paths"`
	Copies         int            `json:"copies"`
	Flows          int            `json:"flows"`
	PktsUp         uint64         `json:"pkts_up"`
	PktsDown       uint64         `json:"pkts_down"`
	Dups           uint64         `json:"dups_dropped"`
	Fallback       bool           `json:"fallback"`
	Events         []Event        `json:"events"`

	// WireUp/WireDown are the bytes Lowpath itself put on (and took off)
	// the network for the game: every relay copy and the direct fallback,
	// with IPv4+UDP headers. The passive bufferbloat detector subtracts
	// them from the adapter's totals to find "other" traffic.
	WireUp   uint64 `json:"wire_bytes_up"`
	WireDown uint64 `json:"wire_bytes_down"`
}

const (
	ModeProxy   = "proxy"
	ModeCapture = "capture"
)

const (
	probeInterval = 100 * time.Millisecond
	swapHold      = 10 * time.Second // minimum time between optional route swaps
	legStale      = probeTimeout + 2*time.Second
	// startGrace bounds how long after Start a path in use is kept while
	// none of its probes has an outcome yet. Long enough for the first
	// probes to come back after a stall of a second or two; short enough
	// that routes which are all dead from the start still fall back within
	// a few seconds even if the probes themselves never got to run.
	startGrace = probeTimeout + 2*time.Second
)

// routing is the data plane's view of the path selection. It is replaced as
// a whole, so Send reads it with one atomic load instead of a lock.
type routing struct {
	active   []*path
	fallback bool
}

type Engine struct {
	cfg     Config
	capture bool
	start   time.Time
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	stopped atomic.Bool

	listener  *net.UDPConn // proxy mode only
	direct    *net.UDPConn // proxy mode only: UDP-echo baseline
	paths     []*path
	before    tracker
	after     tracker
	probeFlow uint64
	afterFlow uint64
	flows     *flowTable
	routing   atomic.Pointer[routing]
	relayed   atomic.Bool  // a game packet went out through the relays
	legUS     atomic.Int64 // latest server leg in µs, -1 = unknown
	legAt     atomic.Int64 // unix nanos when legUS was reported

	mu       sync.Mutex // control plane: selection state and events
	chosen   bool
	lastSwap time.Time
	group    string // egress group currently in use
	grouped  bool
	events   []Event

	pktsUp, pktsDown, dups atomic.Uint64
	wireUp, wireDown       atomic.Uint64
}

// udpOverhead is the IPv4 + UDP header around every datagram on the wire.
const udpOverhead = 28

func Start(cfg Config) (*Engine, error) {
	if len(cfg.Paths) == 0 {
		return nil, errors.New("engine: no relay paths configured")
	}
	if cfg.Listen == "" && cfg.Deliver == nil {
		return nil, errors.New("engine: set Listen (proxy mode) or Deliver (capture mode)")
	}
	e := &Engine{
		cfg:       cfg,
		capture:   cfg.Listen == "",
		start:     time.Now(),
		flows:     newFlowTable(),
		probeFlow: randID(),
		afterFlow: randID(),
	}
	e.routing.Store(&routing{})
	e.legUS.Store(-1)
	if !e.capture {
		if !e.cfg.Direct.IsValid() {
			e.cfg.Direct = cfg.Target
		}
		la, err := net.ResolveUDPAddr("udp", cfg.Listen)
		if err != nil {
			return nil, err
		}
		if e.listener, err = net.ListenUDP("udp", la); err != nil {
			return nil, err
		}
		if !cfg.PingBefore {
			if e.direct, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(e.cfg.Direct)); err != nil {
				e.closeSockets()
				return nil, err
			}
		}
	}
	for _, s := range cfg.Paths {
		p, err := dialPath(s)
		if err != nil {
			e.closeSockets()
			return nil, fmt.Errorf("path %s: %w", s.ID, err)
		}
		e.paths = append(e.paths, p)
	}

	e.choose(time.Now(), cfg.Seed)

	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	if !e.capture {
		e.spawn(e.readListener)
		if e.direct != nil {
			e.spawn(e.readDirectProbes)
		}
	}
	for _, p := range e.paths {
		e.spawn(func() { e.readPath(p) })
	}
	e.spawn(func() { e.probeLoop(ctx) })
	e.spawn(func() { e.beforeLoop(ctx) })
	e.spawn(func() { e.selectLoop(ctx) })
	return e, nil
}

func (e *Engine) spawn(f func()) {
	e.wg.Add(1)
	go func() { defer e.wg.Done(); f() }()
}

// Stop tears everything down; the game's traffic stops being relayed.
func (e *Engine) Stop() {
	e.stopped.Store(true)
	e.cancel()
	e.closeSockets()
	e.wg.Wait()
}

func (e *Engine) closeSockets() {
	if e.listener != nil {
		e.listener.Close()
	}
	for _, p := range e.paths {
		p.conn.Close()
	}
	if e.direct != nil {
		e.direct.Close()
	}
	e.flows.closeAll()
}

// Mode reports ModeProxy or ModeCapture.
func (e *Engine) Mode() string {
	if e.capture {
		return ModeCapture
	}
	return ModeProxy
}

// ListenAddr is the proxy port the game sends to; zero in capture mode.
func (e *Engine) ListenAddr() netip.AddrPort {
	if e.listener == nil {
		return netip.AddrPort{}
	}
	return e.listener.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Send is the capture-mode entry point for one outbound game packet from local
// to remote. It is safe for concurrent use and does not keep payload. It
// returns false when the caller should send the packet directly itself: all
// routes are down and the kill switch is off, or the engine was stopped.
func (e *Engine) Send(local, remote netip.AddrPort, payload []byte) bool {
	if e.stopped.Load() {
		return false
	}
	_, ok := e.send(local, remote, payload)
	return ok
}

// Encode buffers for upstream packets, so Send doesn't allocate per packet.
var bufPool = sync.Pool{New: func() any {
	b := make([]byte, 0, 2048)
	return &b
}}

// send relays one game packet over the active paths. It returns the packet's
// flow and false if the packet was not consumed (fallback, kill switch off).
func (e *Engine) send(local, remote netip.AddrPort, payload []byte) (*flow, bool) {
	now := time.Now()
	e.pktsUp.Add(1)
	f := e.flows.get(local, remote, now)
	f.srv.up.Add(1)

	r := e.routing.Load()
	if r.fallback || len(r.active) == 0 {
		return f, e.cfg.KillSwitch
	}
	if !e.relayed.Load() {
		e.relayed.Store(true)
	}
	h := proto.Header{Type: proto.TData, Flow: f.id, Seq: f.seq.Add(1), TS: uint32(now.Sub(e.start).Milliseconds()), Addr: remote}
	bp := bufPool.Get().(*[]byte)
	out := *bp
	for _, p := range r.active {
		// Paths may belong to relays with different keys, so each copy is
		// framed with its own path's token.
		h.Token = p.token
		out = proto.Append(out[:0], h, payload)
		p.conn.Write(out)
		e.wireUp.Add(uint64(len(out) + udpOverhead))
	}
	*bp = out[:0]
	bufPool.Put(bp)
	return f, true
}

// readListener: game -> relays (proxy mode).
func (e *Engine) readListener() {
	buf := make([]byte, 65535)
	for {
		n, src, err := e.listener.ReadFromUDPAddrPort(buf)
		if err != nil {
			if isClosed(err) {
				return
			}
			continue
		}
		if f, ok := e.send(src, e.cfg.Target, buf[:n]); !ok {
			if c := e.directFor(f); c != nil {
				c.Write(buf[:n])
				e.wireUp.Add(uint64(n + udpOverhead))
			}
		}
	}
}

// readPath: relay -> game, plus probe replies for this path.
func (e *Engine) readPath(p *path) {
	buf := make([]byte, 65535+proto.MaxOverhead)
	for {
		n, err := p.conn.Read(buf)
		if err != nil {
			if isClosed(err) {
				return
			}
			continue // e.g. ICMP unreachable while the relay is down
		}
		now := time.Now()
		e.wireDown.Add(uint64(n + udpOverhead))
		h, payload, err := proto.Parse(buf[:n])
		if err != nil {
			continue
		}
		switch h.Type {
		case proto.TProbeReply:
			leg := replyLeg(payload)
			switch h.Flow {
			case e.probeFlow:
				p.probes.recvLeg(h.Seq, now, leg)
			case e.afterFlow:
				// First copy wins; the duplicates from other paths are ignored.
				if e.after.recvLeg(h.Seq, now, leg) {
					e.setServerLeg(leg, now)
				}
			}
		case proto.TData:
			f := e.flows.byFlowID(h.Flow)
			if f == nil {
				continue
			}
			f.mu.Lock()
			first := f.down.Accept(h.Seq)
			f.mu.Unlock()
			if !first {
				e.dups.Add(1)
				continue
			}
			e.pktsDown.Add(1)
			f.srv.down.Add(1)
			f.touch(now)
			f.srv.touch(now)
			e.deliver(f, payload)
		}
	}
}

func (e *Engine) deliver(f *flow, payload []byte) {
	if e.capture {
		e.cfg.Deliver(f.local, f.remote, payload)
		return
	}
	e.listener.WriteToUDPAddrPort(payload, f.local)
}

// directFor lazily opens the un-optimized route for a proxy-mode flow
// (fallback only). In capture mode the caller sends direct itself.
func (e *Engine) directFor(f *flow) *net.UDPConn {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.direct != nil {
		return f.direct
	}
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(e.cfg.Direct))
	if err != nil {
		return nil
	}
	f.direct = c
	e.spawn(func() {
		buf := make([]byte, 65535)
		for {
			n, err := c.Read(buf)
			if err != nil {
				if isClosed(err) {
					return
				}
				continue
			}
			e.pktsDown.Add(1)
			e.wireDown.Add(uint64(n + udpOverhead))
			f.srv.down.Add(1)
			e.listener.WriteToUDPAddrPort(buf[:n], f.local)
		}
	})
	return c
}

// probeAddr is the game server the probes ask the egress relay to measure:
// the configured target in proxy mode, the primary server in capture mode
// (zero until the game has sent something).
func (e *Engine) probeAddr() netip.AddrPort {
	if !e.capture {
		return e.cfg.Target
	}
	if s := e.flows.primary.Load(); s != nil {
		return s.addr
	}
	return netip.AddrPort{}
}

func (e *Engine) probeLoop(ctx context.Context) {
	t := time.NewTicker(probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		dst := e.probeAddr()
		for _, p := range e.paths {
			p.probe(e.probeFlow, e.start, dst)
		}
		// The "after" probe rides every active path, exactly like game
		// packets do, so it measures what the game actually experiences.
		active := e.routing.Load().active
		if len(active) == 0 {
			continue // in fallback nothing rides the relays; don't count it as loss
		}
		now := time.Now()
		seq := e.after.send(now)
		for _, p := range active {
			p.sendProbe(e.afterFlow, seq, now, e.start, dst)
		}
	}
}

// setServerLeg records what the latest after-probe reply said about the egress
// relay's leg to the game server (leg < 0: unknown).
func (e *Engine) setServerLeg(leg time.Duration, now time.Time) {
	us := int64(-1)
	if leg >= 0 {
		us = leg.Microseconds()
	}
	e.legUS.Store(us)
	e.legAt.Store(now.UnixNano())
}

// serverLeg is the latest relay->server RTT reported with an after probe.
func (e *Engine) serverLeg(now time.Time) (ms float64, known bool) {
	us := e.legUS.Load()
	if us < 0 || now.Sub(time.Unix(0, e.legAt.Load())) > legStale {
		return 0, false
	}
	return float64(us) / 1000, true
}

func (e *Engine) selectLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	gc := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if e.flows.updatePrimary() {
				e.primaryChanged()
			}
			e.choose(now, nil)
			if gc++; gc%10 == 0 && e.flows.gc(now) {
				e.primaryChanged()
			}
		}
	}
}

// primaryChanged drops measurements that were aimed at the previous server.
func (e *Engine) primaryChanged() {
	if e.capture {
		e.before.reset()
	}
	e.legUS.Store(-1)
}

// copies decides how many paths carry each packet.
func (e *Engine) copies(best Stats) int {
	if e.cfg.Locked != "" {
		return 1
	}
	if e.cfg.DupLevel > 0 {
		return e.cfg.DupLevel
	}
	switch {
	case best.Loss >= 3:
		return 4
	case best.Loss >= 1 || best.Jitter >= 5:
		return 3
	default:
		return 2
	}
}

type ranked struct {
	p *path
	s Stats
}

// choose picks the active path set. seed (from a prior Analyze) is used before
// live probes have produced any numbers.
func (e *Engine) choose(now time.Time, seed map[string]Stats) {
	var cands []ranked
	for _, p := range e.paths {
		if e.cfg.Locked != "" && p.spec.ID != e.cfg.Locked {
			continue
		}
		s := p.probes.stats(now)
		if seed != nil {
			if ss, ok := seed[p.spec.ID]; ok {
				s = ss
			}
		}
		cands = append(cands, ranked{p, s})
	}
	slices.SortStableFunc(cands, func(a, b ranked) int {
		switch sa, sb := a.s.Score(), b.s.Score(); {
		case sa < sb:
			return -1
		case sa > sb:
			return 1
		}
		return 0
	})

	e.mu.Lock()
	defer e.mu.Unlock()
	cur := e.routing.Load()

	// With no measurements yet, trust every path; the first tick corrects it.
	optimistic := !e.chosen && seed == nil
	e.chosen = true
	// A path in use is judged on its probes' outcomes, not on their absence:
	// at the first tick its probes may still be in flight, or not even sent
	// on a machine that starved the engine, and falling back on that would
	// send the game direct for nothing. Paths not in use get no such credit.
	grace := now.Sub(e.start) < startGrace
	var up []ranked
	for _, c := range cands {
		unjudged := grace && !c.s.measured() && slices.Contains(cur.active, c.p)
		if optimistic || unjudged || c.s.Up && c.s.Loss < 50 {
			up = append(up, c)
		}
	}

	if len(up) == 0 {
		if !cur.fallback {
			e.routing.Store(&routing{fallback: true})
			if e.cfg.KillSwitch {
				e.event("fallback", "All routes are down - kill switch is blocking game traffic")
			} else {
				e.event("fallback", "All routes are down - sending game traffic directly")
			}
		}
		return
	}
	if cur.fallback {
		e.event("recovered", "Routes recovered - optimizing again")
	}

	up, failover := e.pickGroup(up)
	active := cur.active

	k := min(e.copies(up[0].s), len(up))
	next := make([]*path, 0, k)
	isUp := func(p *path) (Stats, bool) {
		for _, c := range up {
			if c.p == p {
				return c.s, true
			}
		}
		return Stats{}, false
	}
	// Keep current paths that are still healthy (hysteresis). Dead paths are
	// replaced at once; a merely worse path is swapped only when auto-switch
	// is on, the spare is clearly better, and we haven't swapped recently,
	// so the route set doesn't flap on probe noise. A path not judged yet
	// (grace above) is not kept here: it only fills a slot below, after every
	// path that has answered, so a spare known to work isn't held back by
	// paths that may be dead.
	var bestSpare *ranked
	for i := range up {
		if !slices.Contains(active, up[i].p) {
			bestSpare = &up[i]
			break
		}
	}
	canSwap := e.cfg.AutoSwitch && bestSpare != nil && now.Sub(e.lastSwap) >= swapHold
	for _, p := range active {
		s, ok := isUp(p)
		if !ok || !s.measured() || len(next) >= k {
			continue
		}
		if canSwap && s.Score() > bestSpare.s.Score()*1.2+8 {
			canSwap = false // at most one optional swap per tick
			e.lastSwap = now
			continue
		}
		next = append(next, p)
	}
	for _, c := range up {
		if len(next) >= k {
			break
		}
		if !slices.Contains(next, c.p) {
			next = append(next, c.p)
		}
	}

	switch added, removed := diff(active, next); {
	case failover:
		e.event("switch", "Every route to the current egress relay is down - failed over to "+labels(next)+
			". The game server will see a new IP address, so the game may reconnect")
	case active == nil:
		e.event("applied", "Routing via "+labels(next))
	case len(added)+len(removed) > 0:
		msg := "Switched routes"
		if len(removed) > 0 {
			msg += ": dropped " + labels(removed)
		}
		if len(added) > 0 {
			msg += ", added " + labels(added)
		}
		e.event("switch", msg)
	}
	e.routing.Store(&routing{active: next})
}

// pickGroup restricts the live candidates (best first) to one egress group.
// Copies may only be duplicated within a group: two egress relays would each
// deliver the packet to the game server, from two different IPs. Once game
// traffic has gone out through a group we stay on it while any of its paths
// is up, because changing egress changes the IP the server sees. Before that
// the group follows the best path, with the same margin as route swaps so
// equivalent groups don't flap. failover reports a forced move off a group
// that carried game traffic.
func (e *Engine) pickGroup(up []ranked) (_ []ranked, failover bool) {
	group := up[0].p.spec.Egress
	if e.grouped && group != e.group {
		i := slices.IndexFunc(up, func(c ranked) bool { return c.p.spec.Egress == e.group })
		switch {
		case i < 0:
			failover = e.relayed.Load()
		case e.relayed.Load() || up[i].s.Score() <= up[0].s.Score()*1.2+8:
			group = e.group
		}
	}
	e.group, e.grouped = group, true
	return slices.DeleteFunc(up, func(c ranked) bool { return c.p.spec.Egress != group }), failover
}

func (e *Engine) event(kind, msg string) {
	e.events = append(e.events, Event{TS: time.Now(), Kind: kind, Msg: msg})
	if len(e.events) > 50 {
		e.events = e.events[len(e.events)-50:]
	}
}

func diff(old, new []*path) (added, removed []*path) {
	for _, p := range new {
		if !slices.Contains(old, p) {
			added = append(added, p)
		}
	}
	for _, p := range old {
		if !slices.Contains(new, p) {
			removed = append(removed, p)
		}
	}
	return
}

func labels(ps []*path) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.spec.Label)
	}
	return strings.Join(s, " + ")
}

func (e *Engine) Snapshot() Snapshot {
	now := time.Now()
	r := e.routing.Load()
	e.mu.Lock()
	events := slices.Clone(e.events)
	e.mu.Unlock()
	snap := Snapshot{
		Mode:     e.Mode(),
		Copies:   len(r.active),
		Fallback: r.fallback,
		Events:   events,
		Before:   e.before.statsN(now, e.beforeWindow()),
		After:    e.after.stats(now),
	}
	if e.listener != nil {
		snap.Listen = e.ListenAddr().String()
	}
	snap.BeforeKnown = snap.Before.Samples > 0
	snap.ServerLeg, snap.ServerLegKnown = e.serverLeg(now)
	snap.Flows, snap.Servers = e.flows.list()
	snap.PktsUp, snap.PktsDown, snap.Dups = e.pktsUp.Load(), e.pktsDown.Load(), e.dups.Load()
	snap.WireUp, snap.WireDown = e.wireUp.Load(), e.wireDown.Load()
	for _, p := range e.paths {
		snap.Paths = append(snap.Paths, PathStatus{PathSpec: p.spec, Stats: p.probes.stats(now), Active: slices.Contains(r.active, p)})
	}
	return snap
}

func randID() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])
}
