// Package relay implements the egress relay: it receives duplicated overlay
// packets from clients (via any number of paths), keeps the first copy of each
// sequence number, forwards it to the game server, and sends the game server's
// replies back down every path the client is currently using.
//
// It also pings every game server it delivers to and answers path probes with
// that relay->server RTT, so clients can show the full route latency, and
// keeps per-lane delivery statistics of each flow (which return address
// delivered which sequence numbers), sent to the client once a second.
//
// Forwarder is the other kind of relay: a hop in front of an egress that
// gives clients another route to it.
package relay

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/dedup"
	"lowpath/internal/icmp"
	"lowpath/internal/proto"
	"lowpath/internal/tstats"
)

// A return address gets replies while it is no more than this much older than
// the flow's newest one, so addresses the client stopped using drop out.
const pathTTL = 3 * time.Second

// Every reply is copied to each of a flow's return addresses, so their number
// is capped: unbounded, one client could aim the relay at any number of
// spoofed sources. A client sends at most 4 copies of a packet.
const maxPaths = 8

// Each return address is a lane of the flow's statistics.
var _ [tstats.MaxLanes - maxPaths]struct{} // maxPaths <= tstats.MaxLanes

// Flows without upstream packets for this long are torn down.
const flowTTL = 60 * time.Second

// quietTTL ends flows early: ones the game server never answered (a server
// browser querying thousands of servers leaves a trail of them) and, when the
// flow table is full, ones quiet in both directions.
const quietTTL = 10 * time.Second

// Each flow holds a socket and a goroutine; cap them so a misbehaving client
// cannot exhaust the relay's file descriptors.
const maxFlows = 4096

// maxDatagram is the largest game server packet relayed back to clients. Game
// packets stay under the path MTU (about 1.5 KB); this leaves room for jumbo
// frames and fragmented bursts while keeping each flow's buffer small.
const maxDatagram = 16 << 10

// Interfaces can gain or lose addresses (DHCP, failover IPs); re-read them
// this often for the "never send to ourselves" rule.
const selfRefresh = 30 * time.Second

// Options configure an egress relay.
type Options struct {
	// Key is the access key clients must present. When set, every packet whose
	// header token is not proto.TokenFromKey(Key) is dropped. Empty = open.
	Key string
	// AllowPrivate lets clients reach loopback, private, link-local and other
	// non-public destinations. Only for relays on a trusted network (demo,
	// tests, the app's loopback test relay).
	AllowPrivate bool
	// Ping measures the relay->game-server leg; nil means icmp.Ping.
	Ping func(dst netip.Addr, timeout time.Duration) (time.Duration, error)
	// PingInterval is the time between pings to one server (default 1s).
	PingInterval time.Duration
	// StatsInterval is the time between a flow's lane statistics reports
	// (TStats) to its client (default 1s; negative = never).
	StatsInterval time.Duration
}

// Stats are cumulative counters since the relay started (Flows and Servers
// are current values).
type Stats struct {
	Flows       int    `json:"flows"`        // open flows
	Servers     int    `json:"servers"`      // game servers being pinged
	PacketsUp   uint64 `json:"packets_up"`   // first copies delivered to game servers
	PacketsDown uint64 `json:"packets_down"` // packets received from game servers
	Duplicates  uint64 `json:"duplicates"`   // extra upstream copies dropped by dedup
	Probes      uint64 `json:"probes"`       // probes answered
	TokenDrops  uint64 `json:"token_drops"`  // wrong or missing access token
	PolicyDrops uint64 `json:"policy_drops"` // data for a refused destination
	LimitDrops  uint64 `json:"limit_drops"`  // data refused because maxFlows was reached
	TooBig      uint64 `json:"too_big"`      // game server packets over maxDatagram, dropped
	Malformed   uint64 `json:"malformed"`    // not a valid overlay packet
	// Upstream lane statistics: sequence numbers settled (some lane was
	// carrying them), the ones no lane delivered, and TStats reports sent.
	Settled uint64 `json:"seqs_settled"`
	Missing uint64 `json:"seqs_missing"`
	Reports uint64 `json:"tstats_sent"`
}

type Egress struct {
	conn         *net.UDPConn
	log          *slog.Logger
	token        uint64 // expected header token; 0 = open relay
	allowPrivate bool
	statsEvery   time.Duration // 0 = no TStats reports
	mon          *monitor
	self         atomic.Pointer[addrSet]

	mu          sync.Mutex
	flows       map[uint64]*flow
	lastReclaim time.Time

	start     time.Time
	closeOnce sync.Once
	closeErr  error

	packetsUp, packetsDown, duplicates, probes          atomic.Uint64
	tokenDrops, policyDrops, limitDrops, tooBig, broken atomic.Uint64
	settled, missing, reports                           atomic.Uint64
}

type flow struct {
	id       uint64
	token    uint64 // echoed on replies so clients can tell them apart per relay
	dst      netip.AddrPort
	upstream *net.UDPConn // connected to the game server

	mu       sync.Mutex
	up       dedup.Window
	downSeq  uint32
	paths    map[netip.AddrPort]time.Time // client-side return addresses
	lastSeen time.Time                    // last upstream packet
	lastDown time.Time                    // last packet from the game server; zero = never

	// Upstream lane statistics: each return address is a lane.
	lanes    map[netip.AddrPort]int
	used     uint8 // lane slots taken
	lanesUp  tstats.Tracker
	reports  uint32 // TStats reports sent (their header seq)
	reported tstats.Stats
}

// ListenEgress binds an open egress relay on addr (e.g. ":40000") that may
// deliver to any destination, private ones included. It is meant for the demo
// network, tests and the app's loopback test relay; servers on the internet
// should use ListenEgressOpts with a Key.
func ListenEgress(addr string, log *slog.Logger) (*Egress, error) {
	return ListenEgressOpts(addr, Options{AllowPrivate: true}, log)
}

// ListenEgressOpts binds an egress relay on addr with the given options.
func ListenEgressOpts(addr string, o Options, log *slog.Logger) (*Egress, error) {
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
	ping := o.Ping
	if ping == nil {
		ping = icmp.Ping
	}
	interval := o.PingInterval
	if interval <= 0 {
		interval = time.Second
	}
	statsEvery := o.StatsInterval
	switch {
	case statsEvery == 0:
		statsEvery = time.Second
	case statsEvery < 0:
		statsEvery = 0
	}
	e := &Egress{
		conn:         c,
		log:          log,
		token:        proto.TokenFromKey(o.Key),
		allowPrivate: o.AllowPrivate,
		statsEvery:   statsEvery,
		mon:          newMonitor(ping, interval),
		flows:        map[uint64]*flow{},
		start:        time.Now(),
	}
	self := localAddrs()
	e.self.Store(&self)
	return e, nil
}

func (e *Egress) Addr() netip.AddrPort {
	return e.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Close stops the relay: Serve returns, flows are torn down, pings stop.
func (e *Egress) Close() error {
	e.closeOnce.Do(func() {
		e.mon.stop()
		e.closeErr = e.conn.Close()
	})
	return e.closeErr
}

// Stats returns a snapshot of the relay's counters.
func (e *Egress) Stats() Stats {
	e.mu.Lock()
	flows := len(e.flows)
	e.mu.Unlock()
	return Stats{
		Flows:       flows,
		Servers:     e.mon.count(),
		PacketsUp:   e.packetsUp.Load(),
		PacketsDown: e.packetsDown.Load(),
		Duplicates:  e.duplicates.Load(),
		Probes:      e.probes.Load(),
		TokenDrops:  e.tokenDrops.Load(),
		PolicyDrops: e.policyDrops.Load(),
		LimitDrops:  e.limitDrops.Load(),
		TooBig:      e.tooBig.Load(),
		Malformed:   e.broken.Load(),
		Settled:     e.settled.Load(),
		Missing:     e.missing.Load(),
		Reports:     e.reports.Load(),
	}
}

// Serve runs until ctx is cancelled or the relay is closed.
func (e *Egress) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		e.Close()
	}()
	go e.gcLoop(ctx)
	if e.statsEvery > 0 {
		go e.statsLoop(ctx)
	}

	buf := make([]byte, 65535)
	out := make([]byte, 0, 65535)
	for {
		n, src, err := e.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue // transient (e.g. ICMP unreachable on Windows)
		}
		out = e.handle(buf[:n], src, out)
	}
}

// handle processes one packet from a client; out is scratch space for replies.
func (e *Egress) handle(pkt []byte, src netip.AddrPort, out []byte) []byte {
	h, payload, err := proto.Parse(pkt)
	if err != nil {
		e.broken.Add(1)
		return out
	}
	if e.token != 0 && h.Token != e.token {
		e.tokenDrops.Add(1) // silently: don't confirm to scanners that a relay is here
		return out
	}
	switch h.Type {
	case proto.TProbe:
		e.probes.Add(1)
		out = e.probeReply(out[:0], h)
		e.conn.WriteToUDPAddrPort(out, src)
	case proto.TData:
		e.data(h, payload, src)
	default:
		e.broken.Add(1)
	}
	return out
}

// probeReply echoes the probe header with the relay->server RTT for the
// server named in it, or "unknown" when there is none or it is not measured.
func (e *Egress) probeReply(dst []byte, h proto.Header) []byte {
	rtt := time.Duration(-1)
	if h.Addr.IsValid() && e.allowed(h.Addr.Addr()) {
		if d, ok := e.mon.observe(h.Addr.Addr()); ok {
			rtt = d
		}
	}
	h.Type = proto.TProbeReply
	var p [4]byte
	return proto.Append(dst, h, proto.AppendServerRTT(p[:0], rtt))
}

func (e *Egress) data(h proto.Header, payload []byte, src netip.AddrPort) {
	if !h.Addr.IsValid() || h.Addr.Port() == 0 {
		e.broken.Add(1)
		return
	}
	if !e.allowed(h.Addr.Addr()) {
		e.policyDrops.Add(1)
		return
	}
	now := time.Now()
	f := e.getFlow(h.Flow, h.Token, h.Addr, now)
	if f == nil {
		return
	}
	f.mu.Lock()
	lane := f.addPath(src, now)
	f.lastSeen = now
	first := f.up.Accept(h.Seq)
	if lane >= 0 {
		// Every copy counts here, duplicates included: which lanes
		// delivered a number is what the statistics are about.
		f.lanesUp.Add(lane, h.Seq, h.TS, now.Sub(e.start))
	}
	f.mu.Unlock()
	if !first {
		e.duplicates.Add(1)
		return
	}
	e.packetsUp.Add(1)
	f.upstream.Write(payload)
	e.mon.touch(h.Addr.Addr())
}

// allowed applies the destination policy.
func (e *Egress) allowed(a netip.Addr) bool {
	if e.allowPrivate {
		return true
	}
	a = a.Unmap()
	if !IsPublic(a) {
		return false
	}
	_, own := (*e.self.Load())[a]
	return !own
}

func (e *Egress) getFlow(id, token uint64, dst netip.AddrPort, now time.Time) *flow {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.flows[id]; ok {
		if f.dst == dst {
			return f
		}
		e.broken.Add(1) // flow id reused for another destination; ignore
		return nil
	}
	if len(e.flows) >= maxFlows {
		e.reclaimLocked(now)
	}
	if len(e.flows) >= maxFlows {
		e.limitDrops.Add(1)
		return nil
	}
	up, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
	if err != nil {
		e.log.Warn("dial game server", "dst", dst, "err", err)
		return nil
	}
	f := &flow{
		id: id, token: token, dst: dst, upstream: up,
		// A random start keeps a restarted relay's numbers clear of the ones
		// the client's dedup window still holds from before the restart.
		downSeq:  rand.Uint32(),
		paths:    map[netip.AddrPort]time.Time{},
		lanes:    map[netip.AddrPort]int{},
		lastSeen: now,
	}
	e.flows[id] = f
	e.log.Info("flow open", "flow", id, "dst", dst)
	go e.downstream(f)
	return f
}

// reclaimLocked frees the slots of flows quiet in both directions for
// quietTTL, so a full table (say, after a server browser queried thousands of
// servers) doesn't refuse the server the player joins next. It walks every
// flow, so it runs at most once a second however fast new flows arrive.
// e.mu must be held.
func (e *Egress) reclaimLocked(now time.Time) {
	if now.Sub(e.lastReclaim) < time.Second {
		return
	}
	e.lastReclaim = now
	n := 0
	for id, f := range e.flows {
		f.mu.Lock()
		last := f.lastSeen
		if f.lastDown.After(last) {
			last = f.lastDown
		}
		f.mu.Unlock()
		if now.Sub(last) > quietTTL {
			f.upstream.Close()
			delete(e.flows, id)
			n++
		}
	}
	if n > 0 {
		e.log.Info("flow table full: closed quiet flows", "closed", n, "open", len(e.flows))
	}
}

// addPath records src as a return address for the flow and returns its
// lane. Past maxPaths, stale addresses make room; if none is stale, src
// still reaches the game server but gets no replies and no lane (-1).
// f.mu must be held.
func (f *flow) addPath(src netip.AddrPort, now time.Time) int {
	if _, ok := f.paths[src]; !ok {
		if len(f.paths) >= maxPaths {
			for a, t := range f.paths {
				if now.Sub(t) >= pathTTL {
					f.dropPath(a)
				}
			}
			if len(f.paths) >= maxPaths {
				return -1
			}
		}
		for i := range maxPaths {
			if f.used&(1<<i) == 0 {
				f.used |= 1 << i
				f.lanes[src] = i
				break
			}
		}
	}
	f.paths[src] = now
	return f.lanes[src]
}

// dropPath forgets a return address and its lane. f.mu must be held.
func (f *flow) dropPath(a netip.AddrPort) {
	delete(f.paths, a)
	if i, ok := f.lanes[a]; ok {
		delete(f.lanes, a)
		f.used &^= 1 << i
		f.lanesUp.Close(i)
	}
}

// replyTargets appends the addresses that get the flow's replies: the ones
// used within pathTTL of the newest. Measuring from the newest rather than
// from now makes no difference while the game sends; when it goes quiet (a
// map load, a hitch) while the server keeps talking, the replies keep going
// to the paths it used last instead of to nobody. f.mu must be held.
func (f *flow) replyTargets(dst []netip.AddrPort) []netip.AddrPort {
	var newest time.Time
	for _, t := range f.paths {
		if t.After(newest) {
			newest = t
		}
	}
	for a, t := range f.paths {
		if newest.Sub(t) < pathTTL {
			dst = append(dst, a)
		}
	}
	return dst
}

// downstream relays game server -> client, duplicated over every live path.
func (e *Egress) downstream(f *flow) {
	// The payload is read in place behind room for the header, so one small
	// buffer per flow serves for reading and sending. The extra byte shows
	// when a packet was too big for it (and got cut short).
	hlen := len(proto.Append(nil, proto.Header{Addr: f.dst}, nil))
	buf := make([]byte, hlen+maxDatagram+1)
	var targets []netip.AddrPort
	for {
		n, err := f.upstream.Read(buf[hlen:])
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // ICMP port unreachable from the server, or an oversized packet on Windows
		}
		if n > maxDatagram {
			e.tooBig.Add(1)
			continue
		}
		e.packetsDown.Add(1)
		now := time.Now()
		f.mu.Lock()
		f.lastDown = now
		f.downSeq++
		seq := f.downSeq
		targets = f.replyTargets(targets[:0])
		f.mu.Unlock()
		h := proto.Header{Type: proto.TData, Token: f.token, Flow: f.id, Seq: seq, TS: uint32(now.Sub(e.start).Milliseconds()), Addr: f.dst}
		proto.Append(buf[:0], h, nil) // exactly hlen bytes, in front of the payload
		for _, a := range targets {
			e.conn.WriteToUDPAddrPort(buf[:hlen+n], a)
		}
	}
}

func (e *Egress) gcLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastSelf := time.Now()
	for {
		select {
		case <-ctx.Done():
			e.mu.Lock()
			for id, f := range e.flows {
				f.upstream.Close()
				delete(e.flows, id)
			}
			e.mu.Unlock()
			return
		case now := <-t.C:
			e.expireFlows(now)
			if !e.allowPrivate && now.Sub(lastSelf) >= selfRefresh {
				self := localAddrs()
				e.self.Store(&self)
				lastSelf = now
			}
		}
	}
}

func (e *Egress) expireFlows(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, f := range e.flows {
		f.mu.Lock()
		quiet := now.Sub(f.lastSeen)
		idle := quiet > flowTTL || f.lastDown.IsZero() && quiet > quietTTL
		for a, seen := range f.paths {
			if now.Sub(seen) > flowTTL {
				f.dropPath(a)
			}
		}
		f.mu.Unlock()
		if idle {
			f.upstream.Close()
			delete(e.flows, id)
			e.log.Info("flow closed", "flow", id)
		}
	}
}

// statsLoop sends every flow's upstream lane statistics to its client.
func (e *Egress) statsLoop(ctx context.Context) {
	t := time.NewTicker(e.statsEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			e.sendStats(now)
		}
	}
}

// sendStats sends each flow that settled new numbers since its last
// report a TStats packet down every live return address. Each copy names
// the lane it goes down, which is how the client learns which of its
// paths each lane is: the relay only knows the addresses they come from.
func (e *Egress) sendStats(now time.Time) {
	e.mu.Lock()
	flows := make([]*flow, 0, len(e.flows))
	for _, f := range e.flows {
		flows = append(flows, f)
	}
	e.mu.Unlock()
	at := now.Sub(e.start)
	var targets []netip.AddrPort
	var buf []byte
	for _, f := range flows {
		f.mu.Lock()
		s := f.lanesUp.Report(at)
		if s.Seqs == f.reported.Seqs {
			f.mu.Unlock()
			continue // nothing new: a quiet flow sends nothing
		}
		e.settled.Add(uint64(s.Seqs - f.reported.Seqs))
		e.missing.Add(uint64(s.Lost - f.reported.Lost))
		f.reported = s
		f.reports++
		h := proto.Header{Type: proto.TStats, Token: f.token, Flow: f.id, Seq: f.reports, TS: uint32(at.Milliseconds()), Addr: f.dst}
		targets = f.replyTargets(targets[:0])
		lanes := make([]int, len(targets))
		for i, a := range targets {
			lanes[i] = f.lanes[a]
		}
		f.mu.Unlock()
		for i, a := range targets {
			buf = s.Append(proto.Append(buf[:0], h, nil), lanes[i])
			e.conn.WriteToUDPAddrPort(buf, a)
			e.reports.Add(1)
		}
	}
}
