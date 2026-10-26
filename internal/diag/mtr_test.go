package diag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"lowpath/internal/icmp"
)

// fakeHop is one router of a simulated route.
type fakeHop struct {
	addrs []netip.Addr // answers round-robin; none: never answers
	rtt   time.Duration
	// limitPct of this router's own replies are dropped (rate limiting).
	limitPct int
	// lossPct of the packets that reach this hop are lost on the link into
	// it, so they never get further either.
	lossPct int
}

// fakeRoute answers TTL-limited echoes like a real path: the probe is lost
// on any link up to its TTL with that link's loss, else the router where
// the TTL runs out answers (unless it limits replies), or, past the last
// router, the destination does. Decisions follow each TTL's probe counter,
// so a run is deterministic.
type fakeRoute struct {
	target     netip.Addr
	hops       []fakeHop
	destSilent bool // the destination ignores ping
	destRTT    time.Duration
	err        error // every call fails with it

	mu    sync.Mutex
	calls map[int]int
}

func (f *fakeRoute) ping(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error) {
	if f.err != nil {
		return icmp.Reply{}, f.err
	}
	if dst != f.target {
		return icmp.Reply{}, fmt.Errorf("probe to %v, want %v", dst, f.target)
	}
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[int]int{}
	}
	k := f.calls[ttl]
	f.calls[ttl]++
	f.mu.Unlock()
	for j := 0; j < min(ttl, len(f.hops)); j++ {
		if k%100 < f.hops[j].lossPct {
			return icmp.Reply{}, icmp.ErrTimeout
		}
	}
	if ttl > len(f.hops) {
		if f.destSilent {
			return icmp.Reply{}, icmp.ErrTimeout
		}
		return icmp.Reply{From: f.target, RTT: f.destRTT, Kind: icmp.Echo}, nil
	}
	h := f.hops[ttl-1]
	if len(h.addrs) == 0 || k%100 < h.limitPct {
		return icmp.Reply{}, icmp.ErrTimeout
	}
	return icmp.Reply{From: h.addrs[k%len(h.addrs)], RTT: h.rtt, Kind: icmp.TimeExceeded}, nil
}

func (f *fakeRoute) callsAt(ttl int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ttl]
}

var (
	traceTarget = netip.MustParseAddr("203.0.113.50")
	homeRouter  = netip.MustParseAddr("192.168.1.1")
)

func ispHop(n byte, rtt time.Duration) fakeHop {
	return fakeHop{addrs: []netip.Addr{netip.AddrFrom4([4]byte{198, 51, 100, n})}, rtt: rtt}
}

// route: home router, three ISP routers, then the destination.
func testRoute() *fakeRoute {
	return &fakeRoute{
		target:  traceTarget,
		destRTT: 22 * time.Millisecond,
		hops: []fakeHop{
			{addrs: []netip.Addr{homeRouter}, rtt: time.Millisecond},
			ispHop(1, 6*time.Millisecond),
			ispHop(2, 9*time.Millisecond),
			ispHop(3, 20*time.Millisecond),
		},
	}
}

func fastTrace(f *fakeRoute, mut ...func(*TraceConfig)) *Trace {
	cfg := TraceConfig{Target: f.target, Host: "game.example", Ping: f.ping, MaxHops: 8, Count: 200, Interval: 2 * time.Millisecond, Timeout: 50 * time.Millisecond}
	for _, m := range mut {
		m(&cfg)
	}
	return NewTrace(cfg)
}

func runTrace(t *testing.T, tr *Trace) TraceResult {
	t.Helper()
	res, err := tr.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Running || res.FinishedAt.IsZero() {
		t.Fatalf("finished trace still marked running: %+v", res)
	}
	return res
}

func near(got, want float64) bool { return math.Abs(got-want) <= 1 }

func TestTraceCleanRoute(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[2].addrs = nil // a router that never answers trace probes
	res := runTrace(t, fastTrace(f))
	if !res.Reached || len(res.Hops) != 5 {
		t.Fatalf("want 5 hops ending at the destination, got reached=%v %d hops", res.Reached, len(res.Hops))
	}
	if res.Rounds != 200 || res.Planned != 200 || res.Stopped {
		t.Fatalf("rounds %d/%d stopped=%v", res.Rounds, res.Planned, res.Stopped)
	}
	last := res.Hops[4]
	if last.Addr != traceTarget.String() || last.Sent != 200 || last.Recv != 200 || last.LossPct != 0 {
		t.Fatalf("destination row %+v", last)
	}
	silent := res.Hops[2]
	if silent.Addr != "" || silent.Recv != 0 || silent.LossPct != 100 || silent.Limited || silent.LossStarts {
		t.Fatalf("silent hop %+v", silent)
	}
	if silent.PersistPct == nil || *silent.PersistPct != 0 {
		t.Fatalf("silent hop: loss that persists should be 0, got %v", silent.PersistPct)
	}
	if res.LossFrom != nil || res.DestLossPct == nil || *res.DestLossPct != 0 {
		t.Fatalf("clean route: loss from %v, destination loss %v", res.LossFrom, res.DestLossPct)
	}
	if !res.Hops[0].Private || res.Hops[1].Private {
		t.Fatalf("private flags: %v %v", res.Hops[0].Private, res.Hops[1].Private)
	}
	if !strings.Contains(res.Verdict, "clean") {
		t.Fatalf("verdict %q", res.Verdict)
	}
	// Probes past the destination stop once it has answered.
	if n := f.callsAt(8); n > 20 {
		t.Fatalf("TTL 8 probed %d times after the destination answered at TTL 5", n)
	}
	h := res.Hops[1]
	if h.BestMS == nil || !near(*h.AvgMS, 6) || *h.StDevMS != 0 || h.LastMS == nil {
		t.Fatalf("latency of hop 2: %+v", h)
	}
}

func TestTraceRateLimitedHopIsNotLoss(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[1].limitPct = 50
	res := runTrace(t, fastTrace(f))
	h := res.Hops[1]
	if !near(h.LossPct, 50) || !h.Limited || *h.PersistPct != 0 || h.LossStarts {
		t.Fatalf("rate-limited hop %+v", h)
	}
	if res.LossFrom != nil {
		t.Fatalf("a router limiting its replies is not loss, got loss from hop %d", *res.LossFrom)
	}
	if !strings.Contains(res.Verdict, "clean") || !strings.Contains(strings.Join(res.Notes, " "), "Hop 2 drops some replies") {
		t.Fatalf("verdict %q notes %q", res.Verdict, res.Notes)
	}
}

func TestTraceSeveralRateLimitedHops(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[0].limitPct, f.hops[2].limitPct = 30, 60
	res := runTrace(t, fastTrace(f))
	if res.LossFrom != nil || !res.Hops[0].Limited || !res.Hops[2].Limited {
		t.Fatalf("loss from %v, hops %+v", res.LossFrom, res.Hops)
	}
	if n := strings.Join(res.Notes, " "); !strings.Contains(n, "Hops 1 and 3 drop some replies, but the hops after them don't: those routers limit answers") {
		t.Fatalf("notes %q", n)
	}
}

func TestTraceLossThatPersists(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[1].limitPct = 30 // noise before the real problem
	f.hops[2].lossPct = 5   // the link into hop 3 loses 5%
	res := runTrace(t, fastTrace(f))
	if res.LossFrom == nil || *res.LossFrom != 3 {
		t.Fatalf("loss should start at hop 3, got %v", res.LossFrom)
	}
	if !near(*res.DestLossPct, 5) {
		t.Fatalf("destination loss %.1f, want 5", *res.DestLossPct)
	}
	for _, h := range res.Hops[2:] {
		if !near(*h.PersistPct, 5) {
			t.Fatalf("hop %d persisting loss %.1f, want 5", h.TTL, *h.PersistPct)
		}
	}
	if !res.Hops[2].LossStarts || res.Hops[3].LossStarts || res.Hops[1].LossStarts || !res.Hops[1].Limited {
		t.Fatalf("flags: %+v", res.Hops)
	}
	if !strings.Contains(res.Verdict, "hop 3 (198.51.100.2)") || !strings.Contains(res.Verdict, "persists") {
		t.Fatalf("verdict %q", res.Verdict)
	}
}

func TestTraceLossInHomeNetwork(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[0].lossPct = 4
	res := runTrace(t, fastTrace(f))
	if res.LossFrom == nil || *res.LossFrom != 1 || !strings.Contains(res.Verdict, "home network") {
		t.Fatalf("loss from %v, verdict %q", res.LossFrom, res.Verdict)
	}
}

func TestTraceOnlyDestinationLoses(t *testing.T) {
	t.Parallel()
	// The destination answers only some pings, the routers all of them.
	dest := testRoute()
	ping := func(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error) {
		r, err := dest.ping(dst, ttl, timeout)
		if err == nil && r.Kind == icmp.Echo && dest.callsAt(ttl)%10 < 3 {
			return icmp.Reply{}, icmp.ErrTimeout
		}
		return r, err
	}
	res := runTrace(t, fastTrace(dest, func(c *TraceConfig) { c.Ping = ping }))
	if res.LossFrom == nil || *res.LossFrom != 5 || !strings.Contains(res.Verdict, "Only the destination itself") {
		t.Fatalf("loss from %v, verdict %q", res.LossFrom, res.Verdict)
	}
}

func TestTraceDestinationIgnoresPing(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.destSilent = true
	f.hops[1].limitPct = 20
	res := runTrace(t, fastTrace(f))
	if res.Reached || len(res.Hops) != 4 {
		t.Fatalf("want 4 hops and no destination, got reached=%v %d hops", res.Reached, len(res.Hops))
	}
	if res.DestLossPct != nil || res.LossFrom != nil || res.Hops[1].PersistPct != nil || res.Hops[1].Limited {
		t.Fatalf("nothing can be confirmed without the destination: %+v", res)
	}
	if !strings.Contains(res.Verdict, "didn't answer ping") || !strings.Contains(strings.Join(res.Notes, " "), "Hops 5 to 8 didn't answer") {
		t.Fatalf("verdict %q notes %q", res.Verdict, res.Notes)
	}
}

// udpEnd is a relay probe with a fixed loss pattern.
type udpEnd struct {
	mu      sync.Mutex
	n       int
	lossPct int
	rtt     time.Duration
	down    bool
}

func (u *udpEnd) Name() string { return "Relay Amsterdam 203.0.113.50:4000 (UDP probe)" }

func (u *udpEnd) Probe(ctx context.Context) (time.Duration, error) {
	u.mu.Lock()
	k := u.n
	u.n++
	u.mu.Unlock()
	if u.down || k%100 < u.lossPct {
		return 0, context.DeadlineExceeded
	}
	return u.rtt, nil
}

func TestTraceUDPMode(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.destSilent = true // the relay ignores ping, its UDP port answers
	end := &udpEnd{lossPct: 3, rtt: 24 * time.Millisecond}
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) { c.Mode, c.End = TraceUDP, end }))
	if !res.Reached || res.End == nil || res.End.Probe != "udp" || res.End.Sent != 200 || res.EndName != end.Name() {
		t.Fatalf("UDP destination row: reached=%v %+v", res.Reached, res.End)
	}
	if len(res.Hops) != 4 || *res.DestLossPct != 3 || res.LossFrom == nil || *res.LossFrom != 0 {
		t.Fatalf("%d hops, destination loss %v, loss from %v", len(res.Hops), res.DestLossPct, res.LossFrom)
	}
	if !strings.Contains(res.Verdict, "last link or at the relay") || !strings.Contains(strings.Join(res.Notes, " "), "doesn't answer ping") {
		t.Fatalf("verdict %q notes %q", res.Verdict, res.Notes)
	}
	if *res.Hops[3].PersistMS != 20 || *res.End.PersistMS != 24 {
		t.Fatalf("persisting delay: %v %v", *res.Hops[3].PersistMS, *res.End.PersistMS)
	}
}

func TestTraceUDPModeRelayDown(t *testing.T) {
	t.Parallel()
	f := testRoute()
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) { c.Mode, c.End = TraceUDP, &udpEnd{down: true} }))
	if res.Reached || res.DestLossPct != nil || !strings.Contains(res.Verdict, "relay didn't answer") {
		t.Fatalf("reached=%v verdict %q", res.Reached, res.Verdict)
	}
}

func TestTraceDelayThatPersists(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[1].rtt = 90 * time.Millisecond // slow to answer, but only for itself
	f.hops[2].rtt = 45 * time.Millisecond // the route really gets slower here
	f.hops[3].rtt = 46 * time.Millisecond
	f.destRTT = 47 * time.Millisecond
	res := runTrace(t, fastTrace(f))
	if res.JumpTTL == nil || *res.JumpTTL != 3 || !near(*res.JumpMS, 45-1) {
		t.Fatalf("jump at %v by %v", res.JumpTTL, res.JumpMS)
	}
	if *res.Hops[1].PersistMS != 45 {
		t.Fatalf("hop 2's own slow replies must not carry on: persisting %v", *res.Hops[1].PersistMS)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "biggest delay is added at hop 3") {
		t.Fatalf("notes %q", res.Notes)
	}
}

func TestTraceLoadBalancedHop(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[2].addrs = append(f.hops[2].addrs, netip.MustParseAddr("198.51.100.20"))
	res := runTrace(t, fastTrace(f))
	h := res.Hops[2]
	if len(h.Others) != 1 || h.Recv != 200 {
		t.Fatalf("load-balanced hop %+v", h)
	}
	if !strings.Contains(strings.Join(res.Notes, " "), "Hop 3 answered from 2 addresses") {
		t.Fatalf("notes %q", res.Notes)
	}
}

func TestTraceCannotSend(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want string
	}{
		{icmp.ErrNoRoute, "no route"},
		{fmt.Errorf("icmp: raw socket: %w", os.ErrPermission), "root"},
		{fmt.Errorf("nope: %w", errors.ErrUnsupported), "aren't available"},
		{errors.New("socket exploded"), "socket exploded"},
	}
	for _, c := range cases {
		f := &fakeRoute{target: traceTarget, err: c.err}
		start := time.Now()
		res := runTrace(t, fastTrace(f, func(cfg *TraceConfig) { cfg.Interval = time.Second }))
		if !strings.Contains(res.Verdict, c.want) || len(res.Hops) != 0 || res.Reached {
			t.Errorf("%v: verdict %q, %d hops", c.err, res.Verdict, len(res.Hops))
		}
		if time.Since(start) > 500*time.Millisecond {
			t.Errorf("%v: took %v; a trace that can't send must end at once", c.err, time.Since(start))
		}
	}
}

func TestTraceStopKeepsWhatWasMeasured(t *testing.T) {
	t.Parallel()
	f := testRoute()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	var snaps []TraceResult
	var mu sync.Mutex
	res, err := fastTrace(f, func(c *TraceConfig) { c.Interval = 10 * time.Millisecond }).Run(ctx, func(r TraceResult) {
		mu.Lock()
		snaps = append(snaps, r)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stopped || res.Rounds == 0 || res.Rounds >= 200 || !strings.Contains(strings.Join(res.Notes, " "), "Stopped after") {
		t.Fatalf("stopped=%v rounds=%d notes %q", res.Stopped, res.Rounds, res.Notes)
	}
	if res.ThresholdPct <= 1 {
		t.Fatalf("a short trace needs a higher loss threshold, got %.1f", res.ThresholdPct)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(snaps) == 0 || !snaps[0].Running {
		t.Fatalf("want running snapshots, got %d", len(snaps))
	}
}

func TestTraceHostNames(t *testing.T) {
	t.Parallel()
	f := testRoute()
	names := func(ctx context.Context, a netip.Addr) string {
		if a == traceTarget {
			return "game.example"
		}
		return ""
	}
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) { c.Names = names }))
	if res.Hops[4].Name != "game.example" || res.Hops[1].Name != "" {
		t.Fatalf("names: %q %q", res.Hops[4].Name, res.Hops[1].Name)
	}
}

func TestTraceBadConfig(t *testing.T) {
	t.Parallel()
	f := testRoute()
	for name, mut := range map[string]func(*TraceConfig){
		"no target":    func(c *TraceConfig) { c.Target = netip.Addr{} },
		"no ping":      func(c *TraceConfig) { c.Ping = nil },
		"udp, no end":  func(c *TraceConfig) { c.Mode = TraceUDP },
		"unknown mode": func(c *TraceConfig) { c.Mode = "tcp" },
	} {
		if _, err := fastTrace(f, mut).Run(context.Background(), nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestTraceDefaults(t *testing.T) {
	t.Parallel()
	tr := NewTrace(TraceConfig{})
	if tr.cfg.MaxHops != 30 || tr.cfg.Count != 200 || tr.cfg.Interval != time.Second || tr.cfg.Mode != TraceICMP {
		t.Fatalf("defaults %+v", tr.cfg)
	}
	if d := tr.Duration(); d != 202*time.Second {
		t.Fatalf("duration %v", d)
	}
}

func TestLossThreshold(t *testing.T) {
	for sent, want := range map[int]float64{0: 1, 200: 1, 400: 1, 100: 2, 20: 10} {
		if got := lossThreshold(sent); got != want {
			t.Errorf("%d sent: %v, want %v", sent, got, want)
		}
	}
}

func TestTraceReportAndRedaction(t *testing.T) {
	t.Parallel()
	f := testRoute()
	f.hops[2].addrs = nil
	f.hops[1].addrs = append(f.hops[1].addrs, netip.MustParseAddr("198.51.100.77"))
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) {
		c.Mode, c.End = TraceUDP, &udpEnd{rtt: 23 * time.Millisecond}
		c.Names = func(ctx context.Context, a netip.Addr) string {
			if a == netip.MustParseAddr("198.51.100.3") {
				return "core1.example.net"
			}
			return ""
		}
	}))
	rep := res.Report()
	t.Log("\n" + rep)
	for _, want := range []string{
		"Lowpath route trace to game.example (203.0.113.50)",
		"Mode: UDP to the relay port", "200 of 200 rounds", "every 2 ms",
		"Loss%", "Persist%", "(no reply)", "also 198.51.100.77",
		"core1.example.net [198.51.100.3]", "UDP  Relay Amsterdam",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	red := res.Redacted()
	if red.Target != "203.0.113.0" || red.Hops[1].Addr != "198.51.100.0" || red.Hops[1].Others[0] != "198.51.100.0" || red.Hops[3].Name != "" {
		t.Fatalf("redacted: %+v", red.Hops)
	}
	if res.Hops[1].Others[0] != "198.51.100.77" || res.Hops[3].Name == "" {
		t.Fatal("Redacted changed the original")
	}
	if strings.Contains(red.EndName, "203.0.113.50") {
		t.Fatalf("relay address left in %q", red.EndName)
	}
}

func TestTraceDestinationRefusesPing(t *testing.T) {
	t.Parallel()
	// A firewall on the destination answers every echo with "unreachable"
	// (administratively prohibited): found, but nothing measures loss.
	f := testRoute()
	ping := func(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error) {
		r, err := f.ping(dst, ttl, timeout)
		if err == nil && r.Kind == icmp.Echo {
			r.Kind = icmp.Unreachable
		}
		return r, err
	}
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) { c.Ping = ping }))
	if res.Reached || res.DestLossPct != nil || res.LossFrom != nil {
		t.Fatalf("a destination that only refuses isn't reached: reached=%v loss=%v from=%v verdict %q", res.Reached, res.DestLossPct, res.LossFrom, res.Verdict)
	}
	if len(res.Hops) != 5 || res.Hops[4].Unreachable == 0 {
		t.Fatalf("want 5 hops ending at the refusing destination, got %+v", res.Hops)
	}
	if !strings.Contains(res.Verdict, "didn't answer ping") || !strings.Contains(strings.Join(res.Notes, " "), `"unreachable"`) {
		t.Fatalf("verdict %q, notes %q", res.Verdict, res.Notes)
	}
}

func TestTraceRedactedVerdictDropsHostNames(t *testing.T) {
	t.Parallel()
	// Reverse DNS names often spell out the address they belong to; the
	// verdict names the hop where loss starts by its name.
	f := testRoute()
	f.hops[2].lossPct = 5
	res := runTrace(t, fastTrace(f, func(c *TraceConfig) {
		c.Names = func(ctx context.Context, a netip.Addr) string {
			if a == netip.MustParseAddr("198.51.100.2") {
				return "198-51-100-2.dyn.example.net"
			}
			return ""
		}
	}))
	if !strings.Contains(res.Verdict, "198-51-100-2.dyn.example.net") {
		t.Fatalf("verdict %q should name the hop", res.Verdict)
	}
	red := res.Redacted()
	for _, s := range append([]string{red.Verdict}, red.Notes...) {
		if strings.Contains(s, "198-51-100-2") || strings.Contains(s, "example.net") {
			t.Fatalf("host name left in the redacted text %q", s)
		}
	}
	if !strings.Contains(red.Verdict, "hop 3 (198.51.100.0)") {
		t.Fatalf("redacted verdict %q", red.Verdict)
	}
}

func TestTraceReportCutsLongNamesByCharacter(t *testing.T) {
	t.Parallel()
	label := strings.Repeat("é", 50)
	rep := TraceResult{Mode: TraceUDP, EndName: label, End: &TraceHop{Probe: "udp", Sent: 1, Recv: 1}}.Report()
	if !utf8.ValidString(rep) || !strings.Contains(rep, strings.Repeat("é", 43)+"~") {
		t.Fatalf("report:\n%s", rep)
	}
}
