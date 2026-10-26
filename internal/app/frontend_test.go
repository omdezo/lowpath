package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lowpath/internal/platform"
	"lowpath/internal/platform/fakeplatform"
)

// testService is the background service's half of the split: a Server in
// service mode answering on the fake platform's service API, as
// cmd/lowpathsvc runs it.
type testService struct {
	srv *Server
	hs  *http.Server

	mu   sync.Mutex
	seen []http.Header // the headers of every request the service got
}

func startService(t *testing.T, fp *fakeplatform.Fake) *testService {
	t.Helper()
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Config{Store: openTestStore(t), Games: games, Platform: fp.Platform(), Log: discard, InService: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := &testService{srv: srv}
	ln, err := fp.ServiceAPI.Listen()
	if err != nil {
		t.Fatal(err)
	}
	h := srv.ServiceHandler()
	ts.hs = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.seen = append(ts.seen, r.Header.Clone())
		ts.mu.Unlock()
		h.ServeHTTP(w, r)
	})}
	go ts.hs.Serve(ln)
	t.Cleanup(ts.kill)
	return ts
}

// kill stops the service the way a crash does: listener and connections go
// at once.
func (s *testService) kill() {
	s.hs.Close()
	s.srv.Close()
}

func (s *testService) requests() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.seen...)
}

func newTestFrontend(t *testing.T, fp *fakeplatform.Fake) (*httptest.Server, *Frontend) {
	t.Helper()
	fe, err := NewFrontend(FrontendConfig{Dial: fp.ServiceAPI.Dial, Log: discard, Session: testSession})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(fe.Handler())
	t.Cleanup(func() { ts.Close(); fe.Close() })
	return ts, fe
}

// The window's API is the service's: bootstrap says so, and a change made
// through the window lands in the service's state.
func TestFrontendPassesAPIToService(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true) // the service runs as SYSTEM
	svc := startService(t, fp)
	ts, _ := newTestFrontend(t, fp)

	var b struct {
		Platform platformView `json:"platform"`
	}
	if code := call(t, ts, "GET", "/api/bootstrap", "", &b); code != 200 {
		t.Fatalf("bootstrap through the frontend: %d", code)
	}
	if b.Platform.Service.State != "in_use" || !b.Platform.Admin || b.Platform.CanElevate {
		t.Errorf("platform %+v, want the service in use, with admin rights and no restart button", b.Platform)
	}
	var rv struct {
		ID string `json:"id"`
	}
	if code := call(t, ts, "POST", "/api/relays", `{"label":"Amsterdam","addr":"203.0.113.10:40000","key":"k"}`, &rv); code != 200 || rv.ID == "" {
		t.Fatalf("add relay through the frontend: %d %+v", code, rv)
	}
	if views := svc.srv.relays.Views(); len(views) != 1 || views[0].Addr != "203.0.113.10:40000" {
		t.Errorf("service relays %+v", views)
	}
	// The browser's session secret stays with the window.
	for _, h := range svc.requests() {
		if h.Get("Cookie") != "" {
			t.Fatalf("the session cookie went on to the service: %q", h.Get("Cookie"))
		}
	}
	if len(svc.requests()) < 2 {
		t.Errorf("service saw %d requests", len(svc.requests()))
	}
}

// The window keeps the same local guard as the in-process app, and nothing
// it refuses reaches the service.
func TestFrontendGuard(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	ts, fe := newTestFrontend(t, fp)

	if code, _ := do(t, ts, "GET", "/api/bootstrap", "", map[string]string{"Cookie": ""}); code != 401 {
		t.Errorf("no session: %d, want 401", code)
	}
	if code, _ := do(t, ts, "POST", "/api/stop", "", nil); code != 403 {
		t.Errorf("no X-Lowpath: %d, want 403", code)
	}
	if code, _ := do(t, ts, "GET", "/api/bootstrap", "", map[string]string{"Host": "evil.example"}); code != 403 {
		t.Errorf("rebinding host: %d, want 403", code)
	}
	if n := len(svc.requests()); n != 0 {
		t.Errorf("refused requests reached the service: %d", n)
	}
	if !strings.HasPrefix(fe.LaunchPath(), "/api/session?t=") {
		t.Errorf("launch path %q", fe.LaunchPath())
	}
}

// A second start of the app while the window runs in front of the service
// gets a fresh one-time link from the window, which holds the session, so
// it signs in even while the service is down.
func TestFrontendNewLaunchPath(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	ts, fe := newTestFrontend(t, fp)
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signIn := func(path string) int {
		res, err := noFollow.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	first := fe.LaunchPath()
	if code := signIn(first); code != http.StatusSeeOther {
		t.Fatalf("launch link: %d", code)
	}
	svc.kill()
	again := fe.NewLaunchPath()
	if again == first || !strings.HasPrefix(again, "/api/session?t=") || fe.NewLaunchPath() != again {
		t.Fatalf("new launch path %q (first %q)", again, first)
	}
	if code := signIn(again); code != http.StatusSeeOther {
		t.Errorf("fresh launch link with the service down: %d, want it to sign in", code)
	}
	if code, _ := do(t, ts, "GET", again, "", map[string]string{"Cookie": ""}); code != 403 {
		t.Errorf("fresh launch link reused: %d, want 403", code)
	}
}

// When the service dies the window says so (the UI shows "Lost connection
// to the Lowpath service. Reconnecting…"), still answers the version check
// a second launch uses, and picks up the restarted service by itself.
func TestFrontendServiceRestart(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	ts, fe := newTestFrontend(t, fp)
	if code := call(t, ts, "GET", "/api/relays", "", nil); code != 200 {
		t.Fatalf("relays: %d", code)
	}

	svc.kill()
	code, body := do(t, ts, "GET", "/api/relays", "", nil)
	if code != 503 || !strings.Contains(body, `"service_unavailable"`) || !strings.Contains(body, "normal connection") {
		t.Fatalf("service down: %d %s, want 503 service_unavailable", code, body)
	}
	if code, body := do(t, ts, "GET", "/api/version", "", map[string]string{"Cookie": ""}); code != 200 || !strings.Contains(body, Version) {
		t.Errorf("version while the service is down: %d %s", code, body)
	}
	if _, err := fe.Check(t.Context()); !errors.Is(err, platform.ErrServiceNotRunning) {
		t.Errorf("Check while down: %v, want ErrServiceNotRunning", err)
	}

	startService(t, fp)
	if code := call(t, ts, "GET", "/api/relays", "", nil); code != 200 {
		t.Fatalf("after the restart: %d", code)
	}
	if v, err := fe.Check(t.Context()); err != nil || v != Version {
		t.Errorf("Check = %q, %v", v, err)
	}
}

// The live view streams through the window event by event, and ends when
// the service goes away, so the UI notices at once.
func TestFrontendLiveStream(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	ts, _ := newTestFrontend(t, fp)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/live", nil)
	req.Header.Set("Cookie", sessionCookie+"="+testSession)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(nil, 1<<20)
	events := 0
	for events < 2 && sc.Scan() {
		if line, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var l liveView
			if err := json.Unmarshal([]byte(line), &l); err != nil || l.Status == "" {
				t.Fatalf("event %q: %v", line, err)
			}
			events++
		}
	}
	if events < 2 {
		t.Fatalf("got %d events before the stream ended: %v", events, sc.Err())
	}

	svc.kill()
	ended := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the live stream went on after the service died")
	}
}

// A process that answers on the pipe but isn't the installed service gets
// no requests, and the UI says why.
func TestFrontendRefusesImpostor(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	fp.ServiceAPI.SetDialErr(fmt.Errorf("%w: served by process 4242", platform.ErrNotService))
	ts, _ := newTestFrontend(t, fp)
	code, body := do(t, ts, "GET", "/api/bootstrap", "", nil)
	if code != 503 || !strings.Contains(body, "not the Lowpath service") {
		t.Errorf("impostor: %d %s", code, body)
	}
	if n := len(svc.requests()); n != 0 {
		t.Errorf("service saw %d requests", n)
	}
}

func TestNewFrontendNeedsDial(t *testing.T) {
	if _, err := NewFrontend(FrontendConfig{}); err == nil {
		t.Error("frontend without a way to reach the service")
	}
}

// Bootstrap tells the UI whether a background service is there to use,
// and why the window doesn't use an installed one when it knows.
func TestPlatformServiceView(t *testing.T) {
	t.Run("the window's reason", func(t *testing.T) {
		for _, st := range []platform.ServiceState{{Installed: true}, {Installed: true, Running: true, PID: 7}} {
			fp := newFake(t)
			fp.Service.Set(st, nil)
			_, srv := newRealServer(t, fp, func(c *Config) { c.ServiceNote = "it isn't answering (timeout)" })
			if got := srv.platform().Service; got.Message != "it isn't answering (timeout)" {
				t.Errorf("%+v: service view %+v, want the window's reason", st, got)
			}
		}
		fp := newFake(t)
		_, srv := newRealServer(t, fp, func(c *Config) { c.ServiceNote = "stale" })
		if got := srv.platform().Service; got.State != "not_installed" || got.Message != "" {
			t.Errorf("not installed: %+v, want no reason", got)
		}
	})
	for _, tc := range []struct {
		name  string
		state platform.ServiceState
		err   error
		want  string
	}{
		{"none", platform.ServiceState{}, nil, "not_installed"},
		{"stopped", platform.ServiceState{Installed: true}, nil, "stopped"},
		{"running elsewhere", platform.ServiceState{Installed: true, Running: true, PID: 7}, nil, "running"},
		{"no service manager", platform.ServiceState{}, fmt.Errorf("x: %w", errors.ErrUnsupported), "unsupported"},
		{"manager error", platform.ServiceState{}, errors.New("access denied"), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFake(t)
			fp.Service.Set(tc.state, tc.err)
			_, srv := newRealServer(t, fp)
			got := srv.platform().Service
			if got.State != tc.want || (tc.want == "unknown") != (got.Message != "") {
				t.Errorf("service view %+v, want %s", got, tc.want)
			}
		})
	}
}

// Boost in route mode from the window: the service, which has the rights,
// starts the capture. When the service goes, the capture goes with it.
func TestFrontendRouteModeBoost(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	svc := startService(t, fp)
	ts, _ := newTestFrontend(t, fp)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	var lv liveView
	if code := call(t, ts, "POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, &lv); code != 200 {
		t.Fatalf("apply through the frontend: %d", code)
	}
	if lv.Status != "optimizing" || lv.Session == nil || lv.Session.CaptureMode != "route" {
		t.Fatalf("live after apply: %+v", lv)
	}
	c := lastCapture(t, fp)
	if c.Closed() {
		t.Fatal("capture closed right after Boost")
	}
	svc.kill()
	if !c.Closed() {
		t.Error("the capture outlived the service")
	}
}

// Closing the window stops the boost in the service, as it does when the
// engine runs in the window.
func TestFrontendStopSession(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	startService(t, fp)
	ts, fe := newTestFrontend(t, fp)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	if code := call(t, ts, "POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, nil); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	if err := fe.StopSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	var lv liveView
	call(t, ts, "GET", "/api/bootstrap", "", &struct {
		Live *liveView `json:"live"`
	}{&lv})
	if !lastCapture(t, fp).Closed() || lv.Status != "idle" {
		t.Errorf("after StopSession: capture closed %v, status %q", lastCapture(t, fp).Closed(), lv.Status)
	}
}
