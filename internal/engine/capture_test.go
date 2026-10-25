package engine_test

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/demo"
	"lowpath/internal/engine"
	"lowpath/internal/gamesim"
	"lowpath/internal/icmp"
	"lowpath/internal/netsim"
	"lowpath/internal/proto"
)

// fakeRelay is a minimal v2 egress for wire-level checks: it records the token
// and type of every packet, echoes data straight back (standing in for both
// the relay and the game server), and answers probes that name a server with
// a fixed server leg, as a real egress does after pinging that server.
type fakeRelay struct {
	conn  *net.UDPConn
	leg   time.Duration // reported server leg; < 0 = unknown
	delay time.Duration // before every reply
	mute  atomic.Bool   // stop answering, like a dead relay

	mu        sync.Mutex
	tokens    map[uint64]int
	types     map[proto.Type]int
	probeDsts map[netip.AddrPort]int
}

func newFakeRelay(t *testing.T, leg, delay time.Duration) *fakeRelay {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	r := &fakeRelay{conn: c, leg: leg, delay: delay,
		tokens: map[uint64]int{}, types: map[proto.Type]int{}, probeDsts: map[netip.AddrPort]int{}}
	t.Cleanup(func() { c.Close() })
	go r.serve()
	return r
}

func (r *fakeRelay) addr() netip.AddrPort { return r.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (r *fakeRelay) serve() {
	buf := make([]byte, 65535)
	for {
		n, src, err := r.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		h, payload, err := proto.Parse(buf[:n])
		if err != nil {
			continue
		}
		r.mu.Lock()
		r.tokens[h.Token]++
		r.types[h.Type]++
		if h.Type == proto.TProbe {
			r.probeDsts[h.Addr]++
		}
		r.mu.Unlock()
		if r.mute.Load() {
			continue
		}
		var out []byte
		switch h.Type {
		case proto.TProbe:
			var body []byte
			if h.Addr.IsValid() {
				body = proto.AppendServerRTT(nil, r.leg)
			}
			h.Type = proto.TProbeReply
			out = proto.Append(nil, h, body)
		case proto.TData:
			out = proto.Append(nil, proto.Header{Type: proto.TData, Flow: h.Flow, Seq: h.Seq, Addr: h.Addr}, payload)
		default:
			continue
		}
		if r.delay > 0 {
			time.AfterFunc(r.delay, func() { r.conn.WriteToUDPAddrPort(out, src) })
		} else {
			r.conn.WriteToUDPAddrPort(out, src)
		}
	}
}

func (r *fakeRelay) count(typ proto.Type) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.types[typ]
}

func (r *fakeRelay) spec(id, egress, key string) engine.PathSpec {
	return engine.PathSpec{ID: id, Label: id, Addr: r.addr(), Egress: egress, Key: key}
}

// sink collects what the engine delivers back to the "game".
type sink struct {
	mu  sync.Mutex
	got []delivery
}

type delivery struct {
	local, remote netip.AddrPort
	payload       []byte
}

func (s *sink) deliver(local, remote netip.AddrPort, payload []byte) {
	s.mu.Lock()
	s.got = append(s.got, delivery{local, remote, append([]byte(nil), payload...)})
	s.mu.Unlock()
}

func (s *sink) all() []delivery {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]delivery(nil), s.got...)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

func startCapture(t *testing.T, cfg engine.Config) (*engine.Engine, *sink) {
	t.Helper()
	s := &sink{}
	cfg.Deliver = s.deliver
	e, err := engine.Start(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Stop)
	return e, s
}

var gameLocal = netip.MustParseAddrPort("127.0.0.1:50123") // the game's socket; capture never binds it

func TestCaptureModeRoundTripThroughEgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dn, err := demo.Start(ctx, lossyPaths(0), netsim.Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, s := startCapture(t, engine.Config{Paths: dn.Paths, DupLevel: 2})

	const n = 200
	pkt := make([]byte, 64)
	for i := 1; i <= n; i++ {
		binary.BigEndian.PutUint32(pkt, uint32(i))
		if !e.Send(gameLocal, dn.GameAddr(), pkt) {
			t.Fatal("Send refused a packet while routes are up")
		}
		time.Sleep(time.Millisecond)
	}
	waitFor(t, 2*time.Second, func() bool { return len(s.all()) >= n })

	seen := map[uint32]int{}
	for _, d := range s.all() {
		if d.local != gameLocal || d.remote != dn.GameAddr() {
			t.Fatalf("reply delivered as %v <- %v, want %v <- %v", d.local, d.remote, gameLocal, dn.GameAddr())
		}
		seen[binary.BigEndian.Uint32(d.payload)]++
	}
	for seq, k := range seen {
		if k > 1 {
			t.Errorf("packet %d delivered %d times; dedup failed", seq, k)
		}
	}
	if len(seen) < n*99/100 {
		t.Errorf("only %d/%d replies came back over a lossless network", len(seen), n)
	}

	snap := e.Snapshot()
	if snap.Mode != "capture" || snap.Listen != "" {
		t.Errorf("mode %q listen %q, want capture and no listen address", snap.Mode, snap.Listen)
	}
	if snap.Flows != 1 || len(snap.Servers) != 1 {
		t.Fatalf("flows %d servers %+v, want one of each", snap.Flows, snap.Servers)
	}
	srv := snap.Servers[0]
	if srv.Addr != dn.GameAddr() || !srv.Primary || srv.PktsUp != n || srv.PktsDown != uint64(len(seen)) {
		t.Errorf("server entry %+v, want primary %v with %d up / %d down", srv, dn.GameAddr(), n, len(seen))
	}
	if snap.Copies != 2 || snap.Dups == 0 {
		t.Errorf("copies %d dups %d, want 2 copies and dropped duplicates", snap.Copies, snap.Dups)
	}
}

func TestCaptureModeFlowPerRemote(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dn, err := demo.Start(ctx, lossyPaths(0), netsim.Profile{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var servers []netip.AddrPort
	for range 3 {
		g, err := gamesim.Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go g.Serve(ctx)
		servers = append(servers, g.Addr())
	}
	e, s := startCapture(t, engine.Config{Paths: dn.Paths, DupLevel: 2})

	// One game socket talks to three servers; the middle one is busiest and
	// must become the primary even though another was contacted first.
	weights := []int{1, 4, 1}
	sent := make([]int, len(servers))
	pkt := make([]byte, 32)
	for round := range 40 {
		for i, w := range weights {
			for range w {
				pkt[0] = byte(i)
				binary.BigEndian.PutUint32(pkt[1:], uint32(round))
				e.Send(gameLocal, servers[i], pkt)
				sent[i]++
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(t, 2*time.Second, func() bool { return len(s.all()) >= 240 })

	back := make([]int, len(servers))
	for _, d := range s.all() {
		i := int(d.payload[0])
		if d.local != gameLocal || d.remote != servers[i] {
			t.Fatalf("reply for server %d delivered as %v <- %v", i, d.local, d.remote)
		}
		back[i]++
	}
	for i := range servers {
		if back[i] < sent[i]*95/100 {
			t.Errorf("server %d: %d/%d replies; its flow was not relayed on its own", i, back[i], sent[i])
		}
	}

	time.Sleep(1200 * time.Millisecond) // one selection tick ranks the servers
	snap := e.Snapshot()
	if snap.Flows != 3 || len(snap.Servers) != 3 {
		t.Fatalf("flows %d servers %d, want 3 and 3", snap.Flows, len(snap.Servers))
	}
	for _, sv := range snap.Servers {
		if want := sv.Addr == servers[1]; sv.Primary != want {
			t.Errorf("server %v primary=%v, want primary only for the busiest %v", sv.Addr, sv.Primary, servers[1])
		}
	}
}

func TestSendFallsBackOnlyWithoutKillSwitch(t *testing.T) {
	dead := []engine.PathSpec{{ID: "dead", Label: "dead", Addr: netip.MustParseAddrPort("127.0.0.1:9")}}
	remote := netip.MustParseAddrPort("127.0.0.1:27015")
	for _, kill := range []bool{false, true} {
		e, s := startCapture(t, engine.Config{Paths: dead, KillSwitch: kill})
		if !e.Send(gameLocal, remote, []byte("hello")) {
			t.Errorf("kill=%v: Send refused a packet before any route was judged down", kill)
		}
		if !waitFor(t, 3*time.Second, func() bool { return e.Snapshot().Fallback }) {
			t.Fatalf("kill=%v: expected fallback state", kill)
		}
		if got := e.Send(gameLocal, remote, []byte("hello")); got != kill {
			t.Errorf("kill=%v: Send in fallback = %v, want %v", kill, got, kill)
		}
		if len(s.all()) != 0 {
			t.Errorf("kill=%v: something was delivered with every route down", kill)
		}
		if sv := e.Snapshot().Servers; len(sv) != 1 || sv[0].PktsUp != 2 {
			t.Errorf("kill=%v: servers %+v; traffic should still be tracked in fallback", kill, sv)
		}
		e.Stop()
		if e.Send(gameLocal, remote, []byte("hello")) {
			t.Errorf("kill=%v: Send after Stop claimed the packet", kill)
		}
	}
}

func TestTokenAndServerLegOnTheWire(t *testing.T) {
	const leg = 25 * time.Millisecond
	r := newFakeRelay(t, leg, 0)
	const key = "s3cret"
	e, s := startCapture(t, engine.Config{Paths: []engine.PathSpec{r.spec("r", "", key)}})

	remote := netip.MustParseAddrPort("127.0.0.1:27015")
	for range 20 {
		e.Send(gameLocal, remote, []byte("tick"))
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(800 * time.Millisecond)

	r.mu.Lock()
	tokens, types, dsts := r.tokens, r.types, r.probeDsts
	if want := proto.TokenFromKey(key); len(tokens) != 1 || tokens[want] == 0 {
		t.Errorf("tokens on the wire %v, want only %x", tokens, want)
	}
	if types[proto.TData] == 0 || types[proto.TProbe] == 0 {
		t.Errorf("packet types seen %v, want data and probes", types)
	}
	if dsts[remote] == 0 {
		t.Errorf("probes named servers %v, want the primary %v", dsts, remote)
	}
	r.mu.Unlock()
	if len(s.all()) < 19 {
		t.Errorf("%d/20 echoes delivered", len(s.all()))
	}

	snap := e.Snapshot()
	if !snap.ServerLegKnown || snap.ServerLeg < 24 || snap.ServerLeg > 26 {
		t.Errorf("server leg %.1fms known=%v, want 25ms", snap.ServerLeg, snap.ServerLegKnown)
	}
	if snap.After.RTT < 25 || snap.After.Server < 24 || snap.After.Server > 26 {
		t.Errorf("after %+v: the relay's server leg must be part of the RTT", snap.After)
	}
	if p := snap.Paths[0]; p.RTT < 25 || p.Server < 24 || p.Server > 26 {
		t.Errorf("path stats %+v: the relay's server leg must be part of the RTT", p.Stats)
	}
}

func TestProxyModeProbesNameTarget(t *testing.T) {
	r := newFakeRelay(t, 3*time.Millisecond, 0)
	target := netip.MustParseAddrPort("127.0.0.1:27016")
	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: target,
		Paths: []engine.PathSpec{r.spec("r", "", "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	time.Sleep(600 * time.Millisecond)
	r.mu.Lock()
	dsts, tokens := r.probeDsts, r.tokens
	if len(dsts) != 1 || dsts[target] == 0 {
		t.Errorf("probes named %v, want only the target %v", dsts, target)
	}
	if len(tokens) != 1 || tokens[0] == 0 {
		t.Errorf("tokens %v, want 0 for an open relay", tokens)
	}
	r.mu.Unlock()
	snap := e.Snapshot()
	if snap.Mode != "proxy" || snap.Listen == "" || !snap.ServerLegKnown {
		t.Errorf("mode %q listen %q leg known %v", snap.Mode, snap.Listen, snap.ServerLegKnown)
	}
}

// Proxy mode aimed at a real server pings it for "before" and sends it
// nothing directly: no UDP-echo probes, which it would only see as junk.
func TestProxyModePingBefore(t *testing.T) {
	icmpOrSkip(t)
	srv, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var direct atomic.Int32
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := srv.Read(buf); err != nil {
				return
			}
			direct.Add(1)
		}
	}()
	r := newFakeRelay(t, -1, 0)
	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: srv.LocalAddr().(*net.UDPAddr).AddrPort(), PingBefore: true,
		Paths: []engine.PathSpec{r.spec("r", "", "")},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	ok := waitFor(t, 3*time.Second, func() bool { return e.Snapshot().Before.Samples >= 3 })
	if s := e.Snapshot(); !ok || !s.BeforeKnown || !s.Before.Up || s.Before.RTT > 50 {
		t.Errorf("before %+v known=%v, want loopback ICMP to the target", s.Before, s.BeforeKnown)
	}
	if n := direct.Load(); n != 0 {
		t.Errorf("the server got %d packets directly; with PingBefore nothing may go to it but game traffic", n)
	}
}

func activeIDs(s engine.Snapshot) map[string]bool {
	m := map[string]bool{}
	for _, p := range s.Paths {
		if p.Active {
			m[p.ID] = true
		}
	}
	return m
}

func TestEgressGroupsRestrictDuplicationAndFailOver(t *testing.T) {
	// x1 is the best path and shares egress "x" with the slow x2; y1 sits in
	// between on another egress. Without groups the top two would be x1+y1;
	// with them, copies stay on egress x.
	x1 := newFakeRelay(t, -1, 0)
	x2 := newFakeRelay(t, -1, 30*time.Millisecond)
	y1 := newFakeRelay(t, -1, 15*time.Millisecond)
	specs := []engine.PathSpec{y1.spec("y1", "y", ""), x2.spec("x2", "x", ""), x1.spec("x1", "x", "")}
	e, _ := startCapture(t, engine.Config{Paths: specs, DupLevel: 2})

	ok := waitFor(t, 3*time.Second, func() bool {
		a := activeIDs(e.Snapshot())
		return a["x1"] && a["x2"] && !a["y1"]
	})
	if !ok {
		t.Fatalf("active %v, want x1+x2 (best path's egress group only)", activeIDs(e.Snapshot()))
	}
	remote := netip.MustParseAddrPort("127.0.0.1:27015")
	send := func(n int) {
		for range n {
			e.Send(gameLocal, remote, []byte("tick"))
			time.Sleep(2 * time.Millisecond)
		}
	}
	send(20)
	time.Sleep(100 * time.Millisecond)
	if x1.count(proto.TData) == 0 || x2.count(proto.TData) == 0 || y1.count(proto.TData) != 0 {
		t.Fatalf("data packets x1=%d x2=%d y1=%d, want copies only within egress x",
			x1.count(proto.TData), x2.count(proto.TData), y1.count(proto.TData))
	}

	// Egress x dies: fail over to y and say the server will see a new IP.
	x1.mute.Store(true)
	x2.mute.Store(true)
	ok = waitFor(t, 7*time.Second, func() bool {
		a := activeIDs(e.Snapshot())
		return a["y1"] && !a["x1"] && !a["x2"]
	})
	if !ok {
		t.Fatalf("active %v after egress x died, want y1", activeIDs(e.Snapshot()))
	}
	var failover bool
	for _, ev := range e.Snapshot().Events {
		failover = failover || ev.Kind == "switch" && strings.Contains(ev.Msg, "new IP")
	}
	if !failover {
		t.Errorf("no failover event mentioning the new IP: %+v", e.Snapshot().Events)
	}
	send(20)
	time.Sleep(100 * time.Millisecond)
	if y1.count(proto.TData) == 0 {
		t.Error("no data reached y1 after failover")
	}

	// x comes back and is faster, but the game server now knows y's IP.
	x1.mute.Store(false)
	x2.mute.Store(false)
	time.Sleep(2500 * time.Millisecond)
	if a := activeIDs(e.Snapshot()); !a["y1"] || a["x1"] || a["x2"] {
		t.Errorf("active %v, want to stay on egress y once the game uses it", a)
	}
}

func icmpOrSkip(t *testing.T) {
	t.Helper()
	if _, err := icmp.Ping(netip.MustParseAddr("127.0.0.1"), time.Second); err != nil {
		t.Skipf("ICMP not available here: %v", err)
	}
}

func TestCaptureModeBeforeIsICMPToPrimary(t *testing.T) {
	icmpOrSkip(t)
	r := newFakeRelay(t, -1, 0)
	e, _ := startCapture(t, engine.Config{Paths: []engine.PathSpec{r.spec("r", "", "")}})

	time.Sleep(700 * time.Millisecond)
	if s := e.Snapshot(); s.BeforeKnown || s.Before.Samples != 0 {
		t.Fatalf("before %+v known=%v with no game server yet; nothing should be pinged", s.Before, s.BeforeKnown)
	}
	if s := e.Snapshot(); s.ServerLegKnown || s.After.Samples == 0 {
		t.Fatalf("after %+v leg known=%v: probes without a server must not report a leg", s.After, s.ServerLegKnown)
	}
	e.Send(gameLocal, netip.MustParseAddrPort("127.0.0.1:27015"), []byte("hi"))
	ok := waitFor(t, 3*time.Second, func() bool { return e.Snapshot().Before.Samples >= 3 })
	s := e.Snapshot()
	if !ok || !s.BeforeKnown || !s.Before.Up || s.Before.RTT > 50 {
		t.Errorf("before %+v known=%v, want loopback pings to the primary server", s.Before, s.BeforeKnown)
	}
}

func TestAnalyzeAddsServerLeg(t *testing.T) {
	icmpOrSkip(t)
	ctx := context.Background()
	r := newFakeRelay(t, 20*time.Millisecond, 0)
	specs := []engine.PathSpec{r.spec("r", "", "k")}

	a, err := engine.Analyze(ctx, specs, engine.AnalyzeOptions{Server: netip.MustParseAddr("127.0.0.1")}, 800*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !a.DirectKnown || a.Direct.Samples == 0 || a.Direct.RTT > 50 {
		t.Errorf("direct %+v known=%v, want ICMP loopback baseline", a.Direct, a.DirectKnown)
	}
	if p := a.Paths["r"]; p.RTT < 20 || p.Server < 19 || p.Server > 21 {
		t.Errorf("path %+v, want the 20ms server leg included", p)
	}

	a, err = engine.Analyze(ctx, specs, engine.AnalyzeOptions{}, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if a.DirectKnown || a.Direct.Samples != 0 {
		t.Errorf("direct %+v known=%v with no server to measure", a.Direct, a.DirectKnown)
	}
	if p := a.Paths["r"]; !p.Up || p.Server != 0 || p.RTT >= 20 {
		t.Errorf("path %+v, want relay RTT only", p)
	}
}

// Concurrent senders that reuse their buffer right after Send returns: every
// echo must carry exactly what was sent, proving Send kept no reference.
func TestConcurrentSendDoesNotRetainPayload(t *testing.T) {
	r := newFakeRelay(t, -1, 0)
	e, s := startCapture(t, engine.Config{Paths: []engine.PathSpec{r.spec("r", "", "")}})

	const senders, each = 8, 100
	var wg sync.WaitGroup
	for g := range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			remote := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(27000+g%4))
			buf := make([]byte, 16)
			for i := range each {
				binary.BigEndian.PutUint32(buf, uint32(g))
				binary.BigEndian.PutUint32(buf[4:], uint32(i))
				copy(buf[8:], "lowpath!")
				e.Send(gameLocal, remote, buf)
				copy(buf, "XXXXXXXXXXXXXXXX")
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Wait()
	waitFor(t, 2*time.Second, func() bool { return len(s.all()) >= senders*each })

	seen := map[[2]uint32]bool{}
	for _, d := range s.all() {
		g, i := binary.BigEndian.Uint32(d.payload), binary.BigEndian.Uint32(d.payload[4:])
		if string(d.payload[8:]) != "lowpath!" || g >= senders || i >= each {
			t.Fatalf("corrupted echo %q: payload was retained or shared", d.payload)
		}
		if want := uint16(27000 + g%4); d.remote.Port() != want {
			t.Fatalf("echo of sender %d delivered from %v", g, d.remote)
		}
		seen[[2]uint32{g, i}] = true
	}
	if len(seen) < senders*each*95/100 {
		t.Errorf("%d/%d distinct echoes", len(seen), senders*each)
	}
	if snap := e.Snapshot(); snap.Flows != 4 {
		t.Errorf("%d flows, want 4 (one per remote)", snap.Flows)
	}
}

func TestSendDoesNotAllocate(t *testing.T) {
	r := newFakeRelay(t, -1, 0)
	r.mute.Store(true) // AllocsPerRun counts every goroutine; keep echoes out of it
	e, _ := startCapture(t, engine.Config{Paths: []engine.PathSpec{r.spec("a", "", "k"), r.spec("b", "", "k")}, DupLevel: 2})
	remote := netip.MustParseAddrPort("127.0.0.1:27015")
	payload := make([]byte, 120)
	e.Send(gameLocal, remote, payload) // creates the flow
	if allocs := testing.AllocsPerRun(500, func() { e.Send(gameLocal, remote, payload) }); allocs != 0 {
		t.Errorf("Send allocates %.0f times per packet", allocs)
	}
}
