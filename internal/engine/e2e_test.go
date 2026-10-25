package engine_test

import (
	"context"
	"encoding/binary"
	"net"
	"net/netip"
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
	if r := a.Paths["fast"].RTT; r < 9 || r > 25 {
		t.Errorf("fast path RTT %.1fms, want ~10ms", r)
	}
	if !a.DirectKnown {
		t.Error("UDP-echo baseline answered but DirectKnown is false")
	}
	if r := a.Direct.RTT; r < 75 || r > 100 {
		t.Errorf("direct RTT %.1fms, want ~80ms", r)
	}

	e, err := engine.Start(engine.Config{
		Listen: "127.0.0.1:0", Target: net.GameAddr(), Direct: net.DirectAddr(),
		Paths: net.Paths, DupLevel: 2, AutoSwitch: true, Seed: a.Paths,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Stop()
	time.Sleep(2500 * time.Millisecond)
	s := e.Snapshot()
	active := map[string]bool{}
	for _, p := range s.Paths {
		if p.Active {
			active[p.ID] = true
		}
	}
	if !active["fast"] || !active["ok"] || active["slow"] {
		t.Errorf("active paths %v, want fast+ok", active)
	}
	t.Logf("live: before %.1fms after %.1fms", s.Before.RTT, s.After.RTT)
	if s.After.RTT >= s.Before.RTT || s.After.RTT > 25 {
		t.Errorf("after %.1fms should beat before %.1fms", s.After.RTT, s.Before.RTT)
	}

	// Degrade the fast path badly: auto-switch must replace it with "slow".
	net.Links["fast"].SetProfile(netsim.Profile{Delay: 5 * time.Millisecond, Loss: 0.6})
	time.Sleep(4 * time.Second)
	s = e.Snapshot()
	for _, p := range s.Paths {
		if p.ID == "fast" && p.Active {
			t.Errorf("degraded path still active: %+v", p.Stats)
		}
	}
	switched := false
	for _, ev := range s.Events {
		if ev.Kind == "switch" {
			switched = true
		}
	}
	if !switched {
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
		time.Sleep(1200 * time.Millisecond) // one selection tick
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
