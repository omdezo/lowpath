package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/demo"
	"lowpath/internal/diag"
	"lowpath/internal/engine"
	"lowpath/internal/relay"
)

// fakeHomeLine is a connection whose ping to the internet rises while the
// fake loader fills it; the gateway stays flat.
type fakeHomeLine struct {
	gw     netip.Addr
	loaded atomic.Bool
	rx, tx atomic.Uint64
	mu     sync.Mutex
	pinged map[netip.Addr]int
}

func newFakeHomeLine() *fakeHomeLine {
	return &fakeHomeLine{gw: netip.MustParseAddr("192.168.1.1"), pinged: map[netip.Addr]int{}}
}

func (f *fakeHomeLine) DefaultRoute() (diag.Route, error) {
	return diag.Route{Index: 7, Name: "Wi-Fi", Gateway: f.gw, Kind: diag.LinkWiFi}, nil
}

func (f *fakeHomeLine) Counters(int) (diag.Counters, error) {
	return diag.Counters{At: time.Now(), RxBytes: f.rx.Load(), TxBytes: f.tx.Load()}, nil
}

func (f *fakeHomeLine) ping(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	f.mu.Lock()
	f.pinged[dst]++
	f.mu.Unlock()
	if dst == f.gw {
		return 2 * time.Millisecond, nil
	}
	if f.loaded.Load() {
		return 95 * time.Millisecond, nil
	}
	return 25 * time.Millisecond, nil
}

func (f *fakeHomeLine) Endpoint() string { return "192.0.2.1:9" } // TEST-NET: never dialed (ICMP answers first)

func (f *fakeHomeLine) fill(ctx context.Context, add func(int64), c *atomic.Uint64) error {
	f.loaded.Store(true)
	defer f.loaded.Store(false)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(5 * time.Millisecond):
			add(25_000)
			c.Add(25_000)
		}
	}
}

func (f *fakeHomeLine) Download(ctx context.Context, add func(int64)) error {
	return f.fill(ctx, add, &f.rx)
}

func (f *fakeHomeLine) Upload(ctx context.Context, add func(int64)) error {
	return f.fill(ctx, add, &f.tx)
}

func fastBloat(c *diag.BloatConfig) {
	c.Phase, c.Ramp, c.Interval, c.Timeout, c.Pick = 300*time.Millisecond, 80*time.Millisecond, 15*time.Millisecond, 200*time.Millisecond, 500*time.Millisecond
}

func TestBloatAPIRunsAndSavesTheResult(t *testing.T) {
	line := newFakeHomeLine()
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
	})
	srv.bloat.tune = fastBloat
	csrf := map[string]string{"X-Lowpath": "1"}

	code, body := do(t, ts, "GET", "/api/diag/bloat", "", nil)
	if code != 200 || !strings.Contains(body, `"result":null`) || !strings.Contains(body, `"running":false`) {
		t.Fatalf("before any test: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 202 || !strings.Contains(body, `"running":true`) {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 409 || !strings.Contains(body, "bloat_running") {
		t.Fatalf("second start: %d %s", code, body)
	}
	var v bloatView
	deadline := time.Now().Add(10 * time.Second)
	sawProgress := false
	for {
		_, body := do(t, ts, "GET", "/api/diag/bloat", "", nil)
		v = bloatView{}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		sawProgress = sawProgress || (v.Progress != nil && v.Progress.Phase == diag.PhaseDownload)
		if !v.Running || time.Now().After(deadline) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	if v.Running || v.Result == nil {
		t.Fatalf("test did not finish: %+v", v)
	}
	r := v.Result
	if r.Grade != "C" || r.IdleMS == nil || *r.IdleMS != 25 || r.DownIncreaseMS == nil || *r.DownIncreaseMS != 70 {
		t.Fatalf("result: grade %q idle %v down %v note %q", r.Grade, r.IdleMS, r.DownIncreaseMS, r.Note)
	}
	if !strings.Contains(r.Target, "1.1.1.1") || !strings.Contains(r.Gateway, "192.168.1.1") || r.Link != diag.LinkWiFi {
		t.Fatalf("targets: %q / %q / %q", r.Target, r.Gateway, r.Link)
	}
	if !sawProgress {
		t.Error("never saw the download phase in the progress")
	}
	if srv.store.Bloat() == nil || srv.store.Bloat().Grade != "C" {
		t.Fatal("result not saved")
	}
	// The passive detector gets the new line rate.
	srv.line.mu.Lock()
	nl := srv.line.newLine
	srv.line.mu.Unlock()
	if nl == nil || nl.DownBps <= 0 {
		t.Fatalf("line rate not handed to the detector: %+v", nl)
	}
	// And the diagnostics export carries it.
	if _, body := do(t, ts, "GET", "/api/diagnostics", "", nil); !strings.Contains(body, `"bufferbloat":{`) {
		t.Fatal("diagnostics export lacks the bufferbloat result")
	}
}

func TestBloatAPIStop(t *testing.T) {
	line := newFakeHomeLine()
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
	})
	srv.bloat.tune = func(c *diag.BloatConfig) { c.Phase = 5 * time.Second }
	csrf := map[string]string{"X-Lowpath": "1"}
	if code, _ := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 202 {
		t.Fatal(code)
	}
	time.Sleep(300 * time.Millisecond)
	start := time.Now()
	code, body := do(t, ts, "DELETE", "/api/diag/bloat", "", csrf)
	if code != 200 || !strings.Contains(body, `"running":false`) || !strings.Contains(body, "stopped") {
		t.Fatalf("stop: %d %s", code, body)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("stop took too long")
	}
	if srv.store.Bloat() != nil {
		t.Fatal("a stopped test must not be saved as a result")
	}
}

func TestBloatRefusedDuringASession(t *testing.T) {
	ts, _ := newDemoServer(t)
	csrf := map[string]string{"X-Lowpath": "1"}
	if code, body := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"smart"}`, csrf); code != 200 {
		t.Fatalf("apply: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 409 || !strings.Contains(body, "session_active") {
		t.Fatalf("start during a session: %d %s", code, body)
	}
}

// newDemoBloatServer is a demo-mode server (apply works without admin or
// relays) whose bufferbloat test runs against the fake home line.
func newDemoBloatServer(t *testing.T, line *fakeHomeLine) (*httptest.Server, *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	sim, err := demo.Start(ctx, demo.DefaultPaths(), demo.DirectProfile(), discard)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Config{
		Store: openTestStore(t), Games: games, Network: DemoNetwork{N: sim}, Sim: sim, Platform: newFake(t).Platform(), Log: discard, Session: testSession,
		NetStats: line, BloatLoader: line, Ping: line.ping,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.bloat.tune = func(c *diag.BloatConfig) { c.Phase = 5 * time.Second }
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); ts.Close() })
	return ts, srv
}

func TestBoostStopsARunningTest(t *testing.T) {
	line := newFakeHomeLine()
	ts, srv := newDemoBloatServer(t, line)
	csrf := map[string]string{"X-Lowpath": "1"}
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	time.Sleep(200 * time.Millisecond)
	if code, body := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"smart"}`, csrf); code != 200 {
		t.Fatalf("apply: %d %s", code, body)
	}
	if v := srv.bloat.view(); v.Running || !strings.Contains(v.Error, "boost") {
		t.Fatalf("after a boost started: %+v", v)
	}
	if line.loaded.Load() {
		t.Fatal("the test's load is still running under the game")
	}
}

// A test start racing a boost start must never leave both running: either
// the test started first and the boost stopped it, or it was refused.
func TestBloatStartRacingABoost(t *testing.T) {
	line := newFakeHomeLine()
	ts, srv := newDemoBloatServer(t, line)
	csrf := map[string]string{"X-Lowpath": "1"}
	for i := range 8 {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			do(t, ts, "POST", "/api/diag/bloat", "", csrf)
		}()
		go func() {
			defer wg.Done()
			if code, body := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"smart"}`, csrf); code != 200 {
				t.Errorf("apply: %d %s", code, body)
			}
		}()
		wg.Wait()
		if srv.bloat.isRunning() {
			t.Fatalf("round %d: a bufferbloat test is running during a boost", i)
		}
		if code, body := do(t, ts, "POST", "/api/stop", "", csrf); code != 200 {
			t.Fatalf("stop: %d %s", code, body)
		}
	}
}

func TestFirstPerFamily(t *testing.T) {
	addrs := []netip.Addr{
		netip.MustParseAddr("2001:db8::6810:84e5"),
		netip.MustParseAddr("::ffff:198.51.100.29"),
		netip.MustParseAddr("2001:db8::6810:85e5"),
		netip.MustParseAddr("198.51.100.30"),
	}
	got := firstPerFamily(addrs, 443)
	if len(got) != 2 || got[0].String() != "198.51.100.29:443" || got[1].String() != "[2001:db8::6810:84e5]:443" {
		t.Fatalf("got %v, want the first IPv4 then the first IPv6", got)
	}
	if got := firstPerFamily([]netip.Addr{netip.MustParseAddr("2001:db8::2")}, 443); len(got) != 1 || !got[0].Addr().Is6() {
		t.Fatalf("IPv6 only: %v", got)
	}
	if got := resolveEndpoint(context.Background(), "[::ffff:192.0.2.1]:9"); len(got) != 1 || got[0].String() != "192.0.2.1:9" {
		t.Fatalf("literal: %v", got)
	}
}

func TestBloatTargetsSkipLocalRelays(t *testing.T) {
	line := newFakeHomeLine()
	b := &bloatRunner{
		net: line, loader: line, ping: line.ping, log: discard,
		relays: func() []engine.PathSpec {
			return []engine.PathSpec{
				{ID: "loop", Addr: netip.MustParseAddrPort("127.0.0.1:40000")},
				{ID: "lan", Addr: netip.MustParseAddrPort("192.168.1.20:40000")},
				{ID: "lan-mapped", Addr: netip.MustParseAddrPort("[::ffff:192.168.1.21]:40000")},
				{ID: "dxb", Label: "Dubai", Addr: netip.MustParseAddrPort("203.0.113.7:40000")},
			}
		},
	}
	cfg := b.config(context.Background())
	var names []string
	for _, c := range cfg.Internet {
		names = append(names, c.Prober.Name())
	}
	if len(cfg.Internet) != 3 || cfg.Internet[0].Tier != 0 || !strings.Contains(names[0], "Dubai") ||
		!strings.Contains(names[1], "1.1.1.1") || !strings.Contains(names[2], "192.0.2.1:9") {
		t.Fatalf("targets = %v", names)
	}
	if cfg.Gateway == nil || cfg.Route.Index != 7 {
		t.Fatalf("gateway %v route %+v", cfg.Gateway, cfg.Route)
	}
}

func TestRelayProberAgainstARealRelay(t *testing.T) {
	eg, err := relay.ListenEgressOpts("127.0.0.1:0", relay.Options{Key: "secret", AllowPrivate: true}, discard)
	if err != nil {
		t.Fatal(err)
	}
	go eg.Serve(context.Background())
	defer eg.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rtt, err := newRelayProber(engine.PathSpec{Label: "local", Addr: eg.Addr(), Key: "secret"}).Probe(ctx)
	if err != nil || rtt < 0 || rtt > time.Second { // 0 is possible: loopback is below the clock's resolution
		t.Fatalf("probe = %v, %v", rtt, err)
	}
	// A wrong key gets no answer: the probe times out instead of hanging.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := newRelayProber(engine.PathSpec{Addr: eg.Addr(), Key: "wrong"}).Probe(ctx2); err == nil {
		t.Fatal("wrong key answered")
	}
}

// fakeCounters is an adapter whose counters the test advances by hand.
type fakeCounters struct {
	rx, tx uint64
	at     time.Time
	err    error
}

func (f *fakeCounters) DefaultRoute() (diag.Route, error) {
	return diag.Route{Index: 3}, f.err
}

func (f *fakeCounters) Counters(int) (diag.Counters, error) {
	return diag.Counters{At: f.at, RxBytes: f.rx, TxBytes: f.tx}, f.err
}

func TestLineWatchFlagsADownloadDuringASession(t *testing.T) {
	nic := &fakeCounters{at: time.Unix(1000, 0)}
	w := newLineWatch(nic, nil, diag.LineRate{DownBps: 100e6 / 8, UpBps: 20e6 / 8})
	started := time.Unix(999, 0)
	snap := &engine.Snapshot{}
	step := func(rtt, server float64, rxMbps, gameKBps float64) *diag.PassiveState {
		nic.at = nic.at.Add(time.Second)
		nic.rx += uint64(rxMbps*1e6/8) + uint64(gameKBps*1e3)
		nic.tx += 10_000 + uint64(gameKBps*1e3)
		snap.WireDown += uint64(gameKBps * 1e3)
		snap.WireUp += uint64(gameKBps * 1e3)
		snap.After = engine.Stats{RTT: rtt, Server: server, Up: true, Samples: 50}
		w.tick(nic.at, lineInput{snap: snap, started: started})
		return w.view()
	}
	// No session: nothing to show.
	w.tick(nic.at, lineInput{})
	if w.view() != nil {
		t.Fatal("state without a session")
	}
	for range 10 { // play: 30 ms to the relay (+15 ms relay to server)
		if st := step(45, 15, 0.5, 20); st == nil || st.Active {
			t.Fatalf("normal play: %+v", st)
		}
	}
	st := w.view()
	if !st.Watching || st.BaselineMS == nil || *st.BaselineMS != 30 {
		t.Fatalf("after warm-up: %+v", st)
	}
	fired := false
	for range 8 { // a launcher download fills the line
		if st = step(95, 15, 90, 20); st.Active {
			fired = true
			break
		}
	}
	if !fired || st.Direction != "download" {
		t.Fatalf("download not flagged: %+v", st)
	}
	// While the active test runs, the watch stands aside.
	w.tick(nic.at.Add(time.Second), lineInput{snap: snap, started: started, bloatTest: true})
	if st := w.view(); st.Active || st.Watching || !strings.Contains(st.Reason, "test") {
		t.Fatalf("during the active test: %+v", st)
	}
	// A new session starts over.
	w.tick(nic.at.Add(2*time.Second), lineInput{snap: snap, started: started.Add(time.Hour)})
	if st := w.view(); st.Active || st.BaselineMS != nil {
		t.Fatalf("new session: %+v", st)
	}
}

func TestLineWatchUnreadableCounters(t *testing.T) {
	nic := &fakeCounters{at: time.Unix(0, 0), err: errors.New("no adapter")}
	w := newLineWatch(nic, nil, diag.LineRate{})
	snap := &engine.Snapshot{After: engine.Stats{RTT: 30, Up: true, Samples: 5}}
	for i := range 3 {
		w.tick(time.Unix(int64(i+1), 0), lineInput{snap: snap, started: time.Unix(0, 0)})
	}
	if st := w.view(); st == nil || st.Watching || st.Active || !strings.Contains(st.Reason, "counters") {
		t.Fatalf("state = %+v", st)
	}
}
