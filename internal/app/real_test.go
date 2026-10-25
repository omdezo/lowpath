package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/gamesim"
)

type bootstrap struct {
	Demo     bool         `json:"demo"`
	Platform platformView `json:"platform"`
	Relays   []RelayView  `json:"relays"`
	Settings Settings     `json:"settings"`
	Games    []gameView   `json:"games"`
}

func TestBootstrapReportsPlatformAndRelays(t *testing.T) {
	fc := &fakeCapturer{avail: capture.ErrNeedAdmin}
	ts, _ := newRealServer(t, fc)
	var b bootstrap
	if code := call(t, ts, "GET", "/api/bootstrap", "", &b); code != 200 {
		t.Fatalf("bootstrap: %d", code)
	}
	if b.Demo {
		t.Error("real mode reports demo")
	}
	p := b.Platform
	if p.OS != runtime.GOOS || p.Admin || p.Capture.Available || p.Capture.Reason != "administrator rights are needed" {
		t.Errorf("platform %+v, want %s, not admin, capture unavailable for admin rights", p, runtime.GOOS)
	}
	if b.Relays == nil || len(b.Relays) != 0 {
		t.Errorf("relays %v, want an empty list (not null)", b.Relays)
	}
	if b.Settings.CaptureMode != defaultCaptureMode() || b.Settings.LoopbackRelay {
		t.Errorf("settings %+v, want default capture mode and loopback off", b.Settings)
	}
	for _, g := range b.Games {
		if g.Routable {
			t.Fatalf("%s routable with no relays", g.ID)
		}
	}

	// The loopback relay appears as a built-in relay and makes games routable.
	if code := call(t, ts, "PUT", "/api/settings", `{"loopback_relay":true}`, nil); code != 200 {
		t.Fatalf("enable loopback relay: %d", code)
	}
	b = bootstrap{}
	call(t, ts, "GET", "/api/bootstrap", "", &b)
	if len(b.Relays) != 1 || b.Relays[0].ID != "loopback" || !b.Relays[0].Builtin || !strings.HasPrefix(b.Relays[0].Addr, "127.0.0.1:") {
		t.Fatalf("relays %+v, want the built-in loopback relay", b.Relays)
	}
	if len(b.Games) == 0 || !b.Games[0].Routable {
		t.Error("games not routable with the loopback relay on")
	}
	call(t, ts, "PUT", "/api/settings", `{"loopback_relay":false}`, nil)
	var rl struct{ Relays []RelayView }
	call(t, ts, "GET", "/api/relays", "", &rl)
	if len(rl.Relays) != 0 {
		t.Errorf("loopback relay still listed after turning it off: %+v", rl.Relays)
	}
}

func TestSettingsValidateCaptureMode(t *testing.T) {
	ts, _ := newRealServer(t, &fakeCapturer{})
	if code := call(t, ts, "PUT", "/api/settings", `{"capture_mode":"tunnel"}`, nil); code != 400 {
		t.Errorf("unknown capture mode: got %d, want 400", code)
	}
	var s Settings
	if code := call(t, ts, "PUT", "/api/settings", `{"capture_mode":"proxy"}`, &s); code != 200 || s.CaptureMode != "proxy" {
		t.Errorf("proxy mode: %d %+v", code, s)
	}
}

func TestRelayCRUDValidation(t *testing.T) {
	ts, srv := newRealServer(t, &fakeCapturer{})
	bad := map[string]string{
		"empty address":   `{"addr":""}`,
		"bad port":        `{"addr":"203.0.113.10:70000"}`,
		"zero port":       `{"addr":"203.0.113.10:0"}`,
		"unspecified":     `{"addr":"0.0.0.0:40000"}`,
		"multicast":       `{"addr":"239.1.1.1:40000"}`,
		"unknown egress":  `{"addr":"203.0.113.10:40000","egress":"nope"}`,
		"loopback egress": `{"addr":"203.0.113.10:40000","egress":"loopback"}`,
		"long label":      fmt.Sprintf(`{"addr":"203.0.113.10:40000","label":%q}`, strings.Repeat("x", 65)),
	}
	for name, body := range bad {
		if code := call(t, ts, "POST", "/api/relays", body, nil); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}

	var fra RelayView
	if code := call(t, ts, "POST", "/api/relays", `{"label":"Frankfurt 1","region":"EU-Central","continent":"Europe","addr":" 203.0.113.10:40000 ","key":"s3cret"}`, &fra); code != 200 {
		t.Fatalf("add relay: %d", code)
	}
	want := RelayView{ID: "frankfurt-1", Label: "Frankfurt 1", Region: "EU-Central", Continent: "Europe", Addr: "203.0.113.10:40000", HasKey: true}
	if fra != want {
		t.Errorf("added %+v, want %+v", fra, want)
	}
	if code := call(t, ts, "POST", "/api/relays", `{"label":"again","addr":"203.0.113.10:40000"}`, nil); code != 409 {
		t.Errorf("duplicate address: got %d, want 409", code)
	}
	// No port: the relay's default. Same label: a new id.
	var hop RelayView
	if code := call(t, ts, "POST", "/api/relays", `{"label":"Frankfurt 1","addr":"198.51.100.7","egress":"frankfurt-1"}`, &hop); code != 200 {
		t.Fatalf("add hop: %d", code)
	}
	if hop.ID != "frankfurt-1-2" || hop.Addr != "198.51.100.7:40000" || hop.Egress != "frankfurt-1" || hop.HasKey {
		t.Errorf("hop %+v", hop)
	}
	if code := call(t, ts, "POST", "/api/relays", `{"addr":"198.51.100.8:40000","egress":"frankfurt-1-2"}`, nil); code != 400 {
		t.Errorf("egress that is itself a hop: got %d, want 400", code)
	}

	// Keys never leave the service; both relays are paths, grouped by egress.
	_, body := do(t, ts, "GET", "/api/relays", "", nil)
	if strings.Contains(body, "s3cret") {
		t.Error("relay key returned by the API")
	}
	paths := srv.net.Paths("cs2")
	if len(paths) != 2 || paths[0].Key != "s3cret" || paths[0].Egress != "frankfurt-1" || paths[1].Egress != "frankfurt-1" {
		t.Errorf("paths %+v, want both in egress group frankfurt-1 with the key on the first", paths)
	}

	if code := call(t, ts, "DELETE", "/api/relays/frankfurt-1", "", nil); code != 409 {
		t.Errorf("delete an egress others forward to: got %d, want 409", code)
	}
	if code := call(t, ts, "DELETE", "/api/relays/loopback", "", nil); code != 400 {
		t.Errorf("delete loopback relay: got %d, want 400", code)
	}
	if code := call(t, ts, "DELETE", "/api/relays/nope", "", nil); code != 404 {
		t.Errorf("delete unknown relay: got %d, want 404", code)
	}
	for _, id := range []string{"frankfurt-1-2", "frankfurt-1"} {
		if code := call(t, ts, "DELETE", "/api/relays/"+id, "", nil); code != 200 {
			t.Errorf("delete %s: got %d", id, code)
		}
	}
	if n := len(srv.relays.Views()); n != 0 {
		t.Errorf("%d relays left after deleting all", n)
	}
}

func TestImportedRelaysAreReadOnly(t *testing.T) {
	imp := &RelayImport{Relays: []ImportedRelay{{ID: "ams", Label: "Amsterdam", Addr: netip.MustParseAddrPort("192.0.2.1:40000"), Key: "k"}}}
	ts, srv := newRealServer(t, &fakeCapturer{}, func(c *Config) {
		r, err := NewRelayNetwork(c.Store, imp, discard)
		if err != nil {
			t.Fatal(err)
		}
		c.Relays = r
	})
	v := srv.relays.Views()
	if len(v) != 1 || !v[0].Builtin || !v[0].HasKey {
		t.Fatalf("views %+v, want one built-in keyed relay", v)
	}
	if code := call(t, ts, "DELETE", "/api/relays/ams", "", nil); code != 400 {
		t.Errorf("delete imported relay: got %d, want 400", code)
	}
	var added RelayView
	call(t, ts, "POST", "/api/relays", `{"label":"ams","addr":"192.0.2.2:40000"}`, &added)
	if added.ID != "ams-2" {
		t.Errorf("stored relay got id %q, want ams-2 (ams is imported)", added.ID)
	}
}

func TestTestRelay(t *testing.T) {
	ts, _ := newRealServer(t, &fakeCapturer{})
	call(t, ts, "PUT", "/api/settings", `{"loopback_relay":true}`, nil)
	var res relayTest
	if code := call(t, ts, "POST", "/api/relays/test", `{"id":"loopback"}`, &res); code != 200 || !res.OK || res.Loss > 50 {
		t.Errorf("test loopback relay: %d %+v", code, res)
	}
	if code := call(t, ts, "POST", "/api/relays/test", `{"id":"nope"}`, nil); code != 404 {
		t.Errorf("test unknown relay: got %d, want 404", code)
	}
	// Nothing listens on port 9 of loopback: no reply, said plainly.
	res = relayTest{}
	if code := call(t, ts, "POST", "/api/relays/test", `{"addr":"127.0.0.1:9","key":"k"}`, &res); code != 200 || res.OK || !strings.Contains(res.Message, "No reply") {
		t.Errorf("test dead address: %d %+v", code, res)
	}
}

func TestCustomGames(t *testing.T) {
	ts, _ := newRealServer(t, &fakeCapturer{})
	for name, body := range map[string]string{
		"no name":    `{"name":" ","process":"x.exe"}`,
		"no process": `{"name":"X","process":""}`,
		"bad chars":  `{"name":"X","process":"a?.exe"}`,
		"no letters": `{"name":"!!!","process":"???"}`,
	} {
		if code := call(t, ts, "POST", "/api/games/custom", body, nil); code != 400 {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	var g gameView
	if code := call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"C:\\tools\\lpcheck.exe"}`, &g); code != 200 {
		t.Fatalf("add custom game: %d", code)
	}
	if g.ID != "custom-lp-check" || !g.Custom || g.Name != "LP Check" || len(g.Processes) != 1 || g.Processes[0] != "lpcheck.exe" || g.Color == "" {
		t.Errorf("custom game %+v", g)
	}
	if code := call(t, ts, "POST", "/api/games/custom", `{"name":"lp check","process":"other.exe"}`, nil); code != 409 {
		t.Errorf("same name twice: got %d, want 409", code)
	}
	var b bootstrap
	call(t, ts, "GET", "/api/bootstrap", "", &b)
	found := false
	for _, x := range b.Games {
		if x.ID == g.ID {
			found = x.Custom
		} else if x.Custom {
			t.Errorf("catalog game %s marked custom", x.ID)
		}
	}
	if !found {
		t.Error("custom game missing from bootstrap")
	}
	call(t, ts, "POST", "/api/games/"+g.ID+"/favorite", "", nil)
	if code := call(t, ts, "DELETE", "/api/games/custom/"+g.ID, "", nil); code != 200 {
		t.Errorf("delete custom game: %d", code)
	}
	if code := call(t, ts, "DELETE", "/api/games/custom/"+g.ID, "", nil); code != 404 {
		t.Errorf("delete twice: got %d, want 404", code)
	}
	if code := call(t, ts, "DELETE", "/api/games/custom/cs2", "", nil); code != 404 {
		t.Errorf("delete a catalog game: got %d, want 404", code)
	}
}

func TestApplyErrors(t *testing.T) {
	fc := &fakeCapturer{avail: capture.ErrNeedAdmin}
	ts, _ := newRealServer(t, fc)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route"}`, nil)

	expect := func(what string, wantCode int, wantKind string) {
		t.Helper()
		code, body := do(t, ts, "POST", "/api/apply", `{"game_id":"cs2","mode":"smart"}`, csrf)
		if code != wantCode || !strings.Contains(body, `"error":"`+wantKind+`"`) {
			t.Errorf("%s: got %d %s, want %d %s", what, code, body, wantCode, wantKind)
		}
	}
	expect("no relays", 409, "no_relays")
	if code, body := do(t, ts, "POST", "/api/games/cs2/analyze", "", csrf); code != 409 || !strings.Contains(body, "no_relays") {
		t.Errorf("analyze without relays: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/mapping", "", csrf); code != 409 || !strings.Contains(body, "no_relays") {
		t.Errorf("mapping without relays: %d %s", code, body)
	}

	call(t, ts, "PUT", "/api/settings", `{"loopback_relay":true}`, nil)
	expect("route mode without admin", 409, "needs_admin")

	fc.avail = capture.ErrNoDriver
	expect("driver unavailable", 500, "capture_failed")
	fc.avail, fc.startErr = nil, errors.New("WinDivertOpen: the service cannot be started")
	expect("capture start fails", 500, "capture_failed")
	fc.startErr = fmt.Errorf("open: %w", capture.ErrNeedAdmin)
	expect("capture start needs admin", 409, "needs_admin")

	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"proxy"}`, nil)
	expect("proxy mode without a known server", 409, "no_endpoint")
}

func TestRouteModeSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	game, err := gamesim.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go game.Serve(ctx)

	fc := &fakeCapturer{admin: true}
	ts, srv := newRealServer(t, fc)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)

	var lv liveView
	if code := call(t, ts, "POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, &lv); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	if lv.Status != "optimizing" || lv.Session == nil || lv.Session.CaptureMode != "route" || lv.Session.Listen != "" {
		t.Fatalf("live after apply: %+v", lv)
	}
	c := fc.last(t)
	loop := srv.relays.Endpoints()
	if len(c.cfg.Processes) != 1 || c.cfg.Processes[0] != "lpcheck.exe" || len(c.cfg.Exclude) != 1 || c.cfg.Exclude[0] != loop[0] {
		t.Errorf("capture config processes %v exclude %v, want lpcheck.exe and the relay %v", c.cfg.Processes, c.cfg.Exclude, loop)
	}

	// The game sends; the packet is tunneled through the loopback relay to the
	// echo server and the reply comes back to be injected into the game.
	local := netip.MustParseAddrPort("127.0.0.1:50999")
	deadline := time.Now().Add(3 * time.Second)
	var got injected
	for i := 0; ; i++ {
		if !c.send(local, game.Addr(), fmt.Sprintf("tick %d", i)) {
			t.Fatal("engine refused a game packet with the relay up")
		}
		select {
		case got = <-c.injected:
		case <-time.After(20 * time.Millisecond):
		}
		if got.payload != "" || time.Now().After(deadline) {
			break
		}
	}
	if got.local != local || got.remote != game.Addr() || !strings.HasPrefix(got.payload, "tick ") {
		t.Fatalf("injected %+v, want an echo from %v to %v", got, game.Addr(), local)
	}

	// Keep the game talking until the engine names its primary server.
	for time.Now().Before(deadline.Add(3 * time.Second)) {
		c.send(local, game.Addr(), "tick")
		lv = srv.live()
		if lv.Snapshot != nil && len(lv.Snapshot.Servers) == 1 && lv.Snapshot.Servers[0].Primary {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lv.Capture == nil || lv.Capture.Mode != "route" || lv.Capture.Captured == 0 || lv.Snapshot.Mode != "capture" {
		t.Fatalf("live capture %+v snapshot mode %q", lv.Capture, lv.Snapshot.Mode)
	}
	if len(lv.Snapshot.Servers) != 1 || !lv.Snapshot.Servers[0].Primary {
		t.Fatalf("servers %+v, want the echo server as primary", lv.Snapshot.Servers)
	}

	if code := call(t, ts, "POST", "/api/stop", "", &lv); code != 200 || lv.Session != nil || lv.Capture != nil {
		t.Fatalf("stop: %d %+v", code, lv)
	}
	if !c.closed.Load() {
		t.Error("capture still open after stop")
	}
	if s := srv.store.Servers(g.ID); len(s) != 1 || s[0] != game.Addr() {
		t.Errorf("recorded servers %v, want %v", s, game.Addr())
	}

	// Analyze now pings the recorded server for its baseline.
	var a analysisResult
	if code := call(t, ts, "POST", "/api/games/"+g.ID+"/analyze", "", &a); code != 200 {
		t.Fatalf("analyze: %d", code)
	}
	if a.Server != "127.0.0.1" || !a.Paths["loopback"].Up {
		t.Errorf("analysis %+v, want baseline 127.0.0.1 and the loopback path up", a)
	}
}

func TestProcessesSortedBySockets(t *testing.T) {
	self := uint32(os.Getpid())
	fc := &fakeCapturer{procs: []capture.Process{
		{PID: 10, Name: "b.exe", UDPPorts: []uint16{1}},
		{PID: 11, Name: "C.exe", UDPPorts: []uint16{1, 2}},
		{PID: self, Name: "lowpath.exe", UDPPorts: []uint16{1, 2, 3}},
		{PID: 12, Name: "a.exe", UDPPorts: []uint16{5, 6}},
	}}
	ts, _ := newRealServer(t, fc)
	var res struct{ Processes []capture.Process }
	if code := call(t, ts, "GET", "/api/processes", "", &res); code != 200 {
		t.Fatalf("processes: %d", code)
	}
	var names []string
	for _, p := range res.Processes {
		names = append(names, p.Name)
	}
	if got := strings.Join(names, ","); got != "a.exe,C.exe,b.exe" {
		t.Errorf("order %s, want a.exe,C.exe,b.exe (most sockets first, then name; self left out)", got)
	}
}

func TestElevate(t *testing.T) {
	ts, _ := newRealServer(t, &fakeCapturer{})
	if code := call(t, ts, "POST", "/api/elevate", "", nil); code != 501 {
		t.Errorf("no Elevate hook: got %d, want 501", code)
	}

	quit := make(chan struct{})
	var elevErr error
	ts, _ = newRealServer(t, &fakeCapturer{}, func(c *Config) {
		c.Elevate = func() error { return elevErr }
		c.Quit = func() { close(quit) }
	})
	elevErr = fmt.Errorf("ShellExecute: %w", ErrElevateCancelled)
	if code, body := do(t, ts, "POST", "/api/elevate", "", csrf); code != 409 || !strings.Contains(body, "elevate_cancelled") {
		t.Errorf("cancelled UAC: %d %s", code, body)
	}
	elevErr = nil
	if code, body := do(t, ts, "POST", "/api/elevate", "", csrf); code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("elevate: %d %s", code, body)
	}
	select {
	case <-quit:
	case <-time.After(2 * time.Second):
		t.Error("app not asked to quit after starting the elevated copy")
	}
}

func TestStoreServersAndOldState(t *testing.T) {
	store := openTestStore(t)
	for i := range 7 {
		store.RecordServer("cs2", netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), uint16(27015+i)))
	}
	store.RecordServer("cs2", netip.MustParseAddrPort("192.0.2.1:27018"))
	s := store.Servers("cs2")
	if len(s) != maxServers || s[0].Port() != 27018 || s[1].Port() != 27021 {
		t.Errorf("servers %v, want 5 newest first with 27018 moved to the front", s)
	}

	path := t.TempDir() + "/state.json"
	os.WriteFile(path, []byte(`{"settings":{"dup_level":2,"theme":"light"}}`), 0o600)
	old, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if st := old.Settings(); st.CaptureMode != defaultCaptureMode() || st.DupLevel != 2 {
		t.Errorf("old state settings %+v, want capture mode defaulted and the rest kept", st)
	}
}

func TestLoadRelayImport(t *testing.T) {
	write := func(body string) string {
		p := t.TempDir() + "/relays.json"
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	imp, err := LoadRelayImport(write(`{"games":{"CS2":{"server":"192.0.2.50:27015"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if imp.Games["cs2"].Server != netip.MustParseAddrPort("192.0.2.50:27015") {
		t.Errorf("games %+v, want cs2 (lower-cased)", imp.Games)
	}
	for name, body := range map[string]string{
		"empty":          `{}`,
		"no addr":        `{"relays":[{"id":"a"}]}`,
		"duplicate id":   `{"relays":[{"id":"a","addr":"192.0.2.1:1"},{"id":"a","addr":"192.0.2.2:1"}]}`,
		"reserved id":    `{"relays":[{"id":"loopback","addr":"192.0.2.1:1"}]}`,
		"unknown egress": `{"relays":[{"id":"a","addr":"192.0.2.1:1","egress":"b"}]}`,
	} {
		if _, err := LoadRelayImport(write(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The example file in the repo stays loadable.
	if _, err := LoadRelayImport("../../relays.example.json"); err != nil {
		t.Errorf("relays.example.json: %v", err)
	}
}
