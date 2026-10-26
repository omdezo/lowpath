package engine_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"lowpath/internal/demo"
	"lowpath/internal/engine"
	"lowpath/internal/netsim"
)

// gameClient sends n numbered packets to addr and reports how many distinct
// ones came back and how many came back more than once.
func gameClient(t *testing.T, addr netip.AddrPort, n int, every time.Duration) (got, dupes int) {
	t.Helper()
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(addr))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	seen := map[uint32]int{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 1500)
		for {
			c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
			m, err := c.Read(buf)
			if err != nil {
				return
			}
			if m >= 4 {
				seen[binary.BigEndian.Uint32(buf)]++
			}
		}
	}()
	pkt := make([]byte, 120) // typical game tick size
	for i := 1; i <= n; i++ {
		binary.BigEndian.PutUint32(pkt, uint32(i))
		c.Write(pkt)
		time.Sleep(every)
	}
	<-done
	for _, k := range seen {
		got++
		if k > 1 {
			dupes++
		}
	}
	return
}

func lossyPaths(loss float64) []demo.PathDef {
	var out []demo.PathDef
	for _, id := range []string{"a", "b", "c"} {
		out = append(out, demo.PathDef{
			Spec:    engine.PathSpec{ID: id, Label: "path " + id},
			Profile: netsim.Profile{Delay: 5 * time.Millisecond, Jitter: time.Millisecond, Loss: loss},
		})
	}
	return out
}

func TestMultipathBeatsLossyDirectRoute(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const loss = 0.05 // 5% each way on every path => ~10% round-trip
	net, err := demo.Start(ctx, lossyPaths(loss), netsim.Profile{Delay: 5 * time.Millisecond, Loss: loss}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const n = 1500
	directGot, _ := gameClient(t, net.DirectAddr(), n, 2*time.Millisecond)

	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: net.GameAddr(), Direct: net.DirectAddr(),
		Paths: net.Paths, DupLevel: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	viaGot, viaDupes := gameClient(t, e.ListenAddr(), n, 2*time.Millisecond)

	directLoss := 100 * float64(n-directGot) / n
	viaLoss := 100 * float64(n-viaGot) / n
	t.Logf("direct loss %.2f%%, multipath loss %.2f%% (3 copies)", directLoss, viaLoss)
	if directLoss < 5 {
		t.Errorf("simulated direct route should be lossy, got %.2f%%", directLoss)
	}
	if viaLoss > 1 {
		t.Errorf("multipath loss %.2f%% too high", viaLoss)
	}
	if viaDupes != 0 {
		t.Errorf("game saw %d duplicated packets; dedup failed", viaDupes)
	}
	s := e.Snapshot()
	if s.Copies != 3 || s.Dups == 0 {
		t.Errorf("snapshot: copies=%d dups=%d, want 3 copies and dropped dups", s.Copies, s.Dups)
	}
}

func TestPicksBestPathsAndMeasures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defs := []demo.PathDef{
		{Spec: engine.PathSpec{ID: "fast", Label: "fast"}, Profile: netsim.Profile{Delay: 5 * time.Millisecond}},
		{Spec: engine.PathSpec{ID: "ok", Label: "ok"}, Profile: netsim.Profile{Delay: 15 * time.Millisecond}},
		{Spec: engine.PathSpec{ID: "slow", Label: "slow"}, Profile: netsim.Profile{Delay: 60 * time.Millisecond}},
	}
	net, err := demo.Start(ctx, defs, netsim.Profile{Delay: 40 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, err := engine.Analyze(ctx, net.Paths, engine.AnalyzeOptions{Direct: net.DirectAddr()}, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("analysis: direct %.1fms fast %.1fms ok %.1fms slow %.1fms",
		a.Direct.RTT, a.Paths["fast"].RTT, a.Paths["ok"].RTT, a.Paths["slow"].RTT)
	if !a.DirectKnown {
		t.Error("UDP-echo baseline answered but DirectKnown is false")
	}
	// A busy machine only ever adds delay: a simulated link never delivers
	// sooner than its profile, but a starved goroutine can deliver much
	// later. So each RTT gets a floor (90% of what its links add up to) and
	// no ceiling, and the routes must come out in order, which is what
	// shows they were measured apart.
	prev := 0.0
	for _, r := range []struct {
		name       string
		rtt, floor float64
	}{
		{"fast path", a.Paths["fast"].RTT, 9},
		{"ok path", a.Paths["ok"].RTT, 27},
		{"direct route", a.Direct.RTT, 72},
		{"slow path", a.Paths["slow"].RTT, 108},
	} {
		if r.rtt < r.floor || r.rtt <= prev {
			t.Errorf("%s RTT %.1fms, want at least %.0fms and more than the faster route's %.1fms", r.name, r.rtt, r.floor, prev)
		}
		prev = r.rtt
	}

	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: net.GameAddr(), Direct: net.DirectAddr(),
		Paths: net.Paths, DupLevel: 2, AutoSwitch: true, Seed: a.Paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	// About 2 s of live probes, so a selection tick has ranked the paths on
	// live numbers rather than the seed. Counting samples instead of
	// sleeping keeps that true when the probe loop itself runs late.
	var s engine.Snapshot
	waitFor(t, 10*time.Second, func() bool {
		s = e.Snapshot()
		return s.After.Samples >= 20 && s.Before.Samples >= 20
	})
	if a := activeIDs(s); !a["fast"] || !a["ok"] || a["slow"] {
		t.Errorf("active paths %v, want fast+ok", a)
	}
	okRTT := 0.0
	for _, p := range s.Paths {
		if p.ID == "ok" {
			okRTT = p.Stats.RTT
		}
	}
	t.Logf("live: before %.1fms after %.1fms (%d samples), ok path %.1fms", s.Before.RTT, s.After.RTT, s.After.Samples, okRTT)
	// The first copy wins, so "after" follows the fast path: below the
	// direct route and below the second-best path it also rides.
	if s.After.Samples == 0 || s.After.RTT >= s.Before.RTT || s.After.RTT >= okRTT {
		t.Errorf("after %.1fms should beat before %.1fms and the ok path's %.1fms", s.After.RTT, s.Before.RTT, okRTT)
	}

	// Degrade the fast path badly: auto-switch must replace it with "slow".
	// It takes a few seconds of probes for the loss to show; how many
	// depends on how busy the machine is, so wait for the switch.
	net.Links["fast"].SetProfile(netsim.Profile{Delay: 5 * time.Millisecond, Loss: 0.6})
	switched := func(s engine.Snapshot) bool {
		return slices.ContainsFunc(s.Events, func(ev engine.Event) bool { return ev.Kind == "switch" })
	}
	waitFor(t, 15*time.Second, func() bool {
		s = e.Snapshot()
		return !activeIDs(s)["fast"] && switched(s)
	})
	if a := activeIDs(s); a["fast"] || !a["ok"] || !a["slow"] {
		t.Errorf("active paths %v after degrading fast, want ok+slow", a)
	}
	if !switched(s) {
		t.Errorf("no switch event: %+v", s.Events)
	}
}

// Near-identical noisy paths must not make the route set flap.
func TestNoFlappingOnProbeNoise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var defs []demo.PathDef
	for _, id := range []string{"a", "b", "c"} {
		defs = append(defs, demo.PathDef{
			Spec:    engine.PathSpec{ID: id, Label: id},
			Profile: netsim.Profile{Delay: 12 * time.Millisecond, Jitter: 2 * time.Millisecond, Loss: 0.01},
		})
	}
	net, err := demo.Start(ctx, defs, netsim.Profile{Delay: 20 * time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: net.GameAddr(), Direct: net.DirectAddr(),
		Paths: net.Paths, DupLevel: 2, AutoSwitch: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	time.Sleep(9 * time.Second)
	switches := 0
	for _, ev := range e.Snapshot().Events {
		if ev.Kind == "switch" {
			switches++
		}
	}
	if switches > 1 {
		t.Errorf("%d route switches in 9s on equivalent paths; want at most 1", switches)
	}
}

func TestFallbackWhenAllRelaysDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	net, err := demo.Start(ctx, nil, netsim.Profile{Delay: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dead := []engine.PathSpec{{ID: "dead", Label: "dead", Addr: netip.MustParseAddrPort("127.0.0.1:9")}}
	for _, kill := range []bool{false, true} {
		e, err := engine.Start(engine.Config{
			Listen: "127.0.0.1:0", Target: net.GameAddr(), Direct: net.DirectAddr(),
			Paths: dead, KillSwitch: kill,
		})
		if err != nil {
			t.Fatal(err)
		}
		// The first selection tick (1 s) finds the relay dead; a busy
		// machine can run it late.
		waitFor(t, 5*time.Second, func() bool { return e.Snapshot().Fallback })
		got, _ := gameClient(t, e.ListenAddr(), 50, 2*time.Millisecond)
		snap := e.Snapshot()
		e.Stop()
		if !snap.Fallback {
			t.Fatalf("kill=%v: expected fallback state", kill)
		}
		if kill && got != 0 {
			t.Errorf("kill switch on but %d packets leaked direct", got)
		}
		if !kill && got < 45 {
			t.Errorf("kill switch off but only %d/50 packets went direct", got)
		}
	}
}
