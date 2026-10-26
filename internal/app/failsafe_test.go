package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/owner"
	"lowpath/internal/platform/fakeplatform"
)

// withMarker gives the server a session marker file in a temp folder.
func withMarker(m *capture.MarkerFile) func(*Config) {
	return func(c *Config) { c.Marker = m }
}

func newMarker(t *testing.T) *capture.MarkerFile {
	t.Helper()
	return capture.NewMarkerFile(filepath.Join(t.TempDir(), "capture-session.json"))
}

// startRouteSession boosts a custom game in route mode on the fake platform.
func startRouteSession(t *testing.T, fp *fakeplatform.Fake, opts ...func(*Config)) (*Server, *fakeplatform.Session, string) {
	t.Helper()
	fp.Privileges.SetElevated(true)
	ts, srv := newRealServer(t, fp, opts...)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	if code := call(t, ts, "POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, nil); code != 200 {
		t.Fatalf("apply: %d", code)
	}
	return srv, lastCapture(t, fp), g.ID
}

func waitLive(t *testing.T, srv *Server, cond func(liveView) bool) liveView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		lv := srv.live()
		if cond(lv) {
			return lv
		}
		if time.Now().After(deadline) {
			t.Fatalf("live view never matched: %+v", lv)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// When the capture fails open on its own, the app ends the session, tells
// the UI why, and drops the on-disk marker (nothing is left to clean up).
func TestCaptureFailOpenEndsSession(t *testing.T) {
	fp := newFake(t)
	marker := newMarker(t)
	srv, c, gameID := startRouteSession(t, fp, withMarker(marker))
	m, ok, err := marker.Load()
	if !ok || err != nil || m.Game != gameID || m.Backend != "fake" || m.PID != uint32(os.Getpid()) || m.Exe == "" {
		t.Fatalf("marker during the session: %+v %v %v", m, ok, err)
	}

	c.Fail(errors.New("capture stopped: a packet was stuck in the handler for 500ms"))
	lv := waitLive(t, srv, func(lv liveView) bool { return lv.Session == nil })
	if lv.Status != "idle" || lv.Capture != nil {
		t.Fatalf("after failing open: %+v", lv)
	}
	f := lv.Failsafe
	if f == nil || f.Kind != "stopped" || f.GameID != gameID || f.Game != "LP Check" ||
		!strings.Contains(f.Message, "normal connection") || !strings.Contains(f.Detail, "stuck") {
		t.Fatalf("failsafe = %+v", f)
	}
	if _, ok, _ := marker.Load(); ok {
		t.Error("marker left after the session ended")
	}

	// The next boost starts clean.
	if err := srv.apply(applyReq{GameID: gameID, Mode: "smart"}); err != nil {
		t.Fatal(err)
	}
	if lv := srv.live(); lv.Failsafe != nil || lv.Session == nil {
		t.Fatalf("after a new boost: failsafe %+v session %+v", lv.Failsafe, lv.Session)
	}
	srv.stopSession()
}

// The banner names the cause: routing another program removed is not
// Lowpath's packet handling failing.
func TestFailOpenMessage(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("capture stopped: a packet was stuck in the handler for 500ms"), "packet handling for Example Game stopped working"},
		{fmt.Errorf("capture stopped: another program keeps removing the routing rule: %w", capture.ErrRouteRemoved), "another program (a network manager, VPN or firewall) removed the routing that sends Example Game"},
	} {
		if got := failOpenMessage(tc.err, "Example Game"); !strings.Contains(got, tc.want) || !strings.Contains(got, "normal connection") {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
	// End to end: the session ends with that message and the reason.
	fp := newFake(t)
	srv, c, _ := startRouteSession(t, fp)
	c.Fail(fmt.Errorf("capture stopped: another program removed the routing rule and putting it back failed: %w", capture.ErrRouteRemoved))
	lv := waitLive(t, srv, func(lv liveView) bool { return lv.Session == nil })
	if f := lv.Failsafe; f == nil || f.Kind != "stopped" || !strings.Contains(f.Message, "removed the routing") || !strings.Contains(f.Detail, "putting it back failed") {
		t.Fatalf("failsafe = %+v", f)
	}
}

// A plain Stop is not a failure.
func TestStopIsNotFailOpen(t *testing.T) {
	fp := newFake(t)
	marker := newMarker(t)
	srv, c, _ := startRouteSession(t, fp, withMarker(marker))
	srv.stopSession()
	if !c.Closed() {
		t.Fatal("capture not closed")
	}
	time.Sleep(50 * time.Millisecond) // the watcher would have run by now
	if lv := srv.live(); lv.Failsafe != nil {
		t.Fatalf("failsafe after a normal stop: %+v", lv.Failsafe)
	}
	if _, ok, _ := marker.Load(); ok {
		t.Error("marker left after Stop")
	}
}

// A capture that fails to start leaves no marker behind.
func TestCaptureStartFailureClearsMarker(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	fp.Capture.SetStartErr(errors.New("driver blocked"))
	marker := newMarker(t)
	ts, _ := newRealServer(t, fp, withMarker(marker))
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	if code := call(t, ts, "POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, nil); code != 500 {
		t.Fatalf("apply with a failing capture: %d", code)
	}
	if _, ok, _ := marker.Load(); ok {
		t.Error("marker left after a failed start")
	}
}

// A marker at start means the last run was killed or crashed mid-boost:
// the backend gets to clean up, the marker goes, and the UI is told.
func TestRecoverLeftoverAtStart(t *testing.T) {
	fp := newFake(t)
	marker := newMarker(t)
	m := capture.Marker{Backend: "fake", PID: 999999, Exe: "Lowpath.exe", Game: "cs2", Started: time.Now().Add(-time.Hour).UTC()}
	if err := marker.Save(m); err != nil {
		t.Fatal(err)
	}
	_, srv := newRealServer(t, fp, withMarker(marker))
	if got := fp.Capture.Recovered(); len(got) != 1 || got[0].PID != m.PID || got[0].Game != "cs2" {
		t.Fatalf("Recover got %+v", got)
	}
	if _, ok, _ := marker.Load(); ok {
		t.Error("marker still there")
	}
	f := srv.live().Failsafe
	if f == nil || f.Kind != "recovered" || f.Game != "Counter-Strike 2" || !strings.Contains(f.Message, "closed unexpectedly") ||
		!strings.Contains(f.Message, "went back to your normal connection") || f.Detail != "" {
		t.Fatalf("failsafe = %+v", f)
	}
}

// Dismissing the notice is remembered by the service, not just one window:
// a reload or a second UI doesn't bring it back.
func TestDismissFailsafe(t *testing.T) {
	fp := newFake(t)
	marker := newMarker(t)
	if err := marker.Save(capture.Marker{Backend: "fake", PID: 999999, Exe: "Lowpath.exe", Game: "cs2"}); err != nil {
		t.Fatal(err)
	}
	ts, srv := newRealServer(t, fp, withMarker(marker))
	if srv.live().Failsafe == nil {
		t.Fatal("no notice to dismiss")
	}
	// Behind the same local-API guard as every other change.
	if code, _ := do(t, ts, "DELETE", "/api/failsafe", "", nil); code != 403 || srv.live().Failsafe == nil {
		t.Fatalf("dismiss without X-Lowpath: %d", code)
	}
	var lv liveView
	if code := call(t, ts, "DELETE", "/api/failsafe", "", &lv); code != 200 || lv.Failsafe != nil {
		t.Fatalf("dismiss: %d %+v", code, lv.Failsafe)
	}
	if f := srv.live().Failsafe; f != nil {
		t.Fatalf("notice back after dismiss: %+v", f)
	}
	// Nothing to dismiss is fine too.
	if code := call(t, ts, "DELETE", "/api/failsafe", "", nil); code != 200 {
		t.Fatalf("second dismiss: %d", code)
	}
}

func TestRecoverLeftoverFailureIsReported(t *testing.T) {
	fp := newFake(t)
	fp.Capture.SetRecoverErr(errors.New("route 198.51.100.7/32 could not be removed"))
	marker := newMarker(t)
	marker.Save(capture.Marker{Backend: "fake", PID: 999999, Exe: "Lowpath.exe", Game: "custom-gone"})
	_, srv := newRealServer(t, fp, withMarker(marker))
	f := srv.live().Failsafe
	if f == nil || !strings.Contains(f.Detail, "could not be removed") || !strings.Contains(f.Message, "a game") ||
		strings.Contains(f.Message, "went back") || !strings.Contains(f.Message, "may not be back to normal") {
		t.Fatalf("failsafe = %+v", f)
	}
}

// A marker from a capture method this build doesn't have is not handed to
// the wrong backend, and the notice says it couldn't be cleaned up.
func TestRecoverLeftoverOtherBackend(t *testing.T) {
	fp := newFake(t)
	marker := newMarker(t)
	marker.Save(capture.Marker{Backend: "tun", PID: 999999, Exe: "Lowpath.exe", Game: "cs2"})
	_, srv := newRealServer(t, fp, withMarker(marker))
	if got := fp.Capture.Recovered(); len(got) != 0 {
		t.Fatalf("Recover called for another backend: %+v", got)
	}
	if f := srv.live().Failsafe; f == nil || !strings.Contains(f.Detail, `"tun"`) || strings.Contains(f.Message, "went back") {
		t.Fatalf("failsafe = %+v", f)
	}
}

// The Lowpath that wrote the marker is still running: leave its capture
// alone. A reused PID (another program, or this Lowpath after a reboot)
// does not count.
func TestRecoverLeftoverSkipsRunningOwner(t *testing.T) {
	for _, c := range []struct {
		name    string
		running string
		own     bool // the marker has this process's PID
		recover bool
	}{
		{"same program", "lowpath.exe", false, false},
		{"pid reused", "notepad.exe", false, true},
		{"own pid after a reboot", "lowpath.exe", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			fp := newFake(t)
			pid := fakePIDs(1)[0]
			if c.own {
				pid = uint32(os.Getpid())
			}
			fp.Owners.Set([]owner.Socket{}, map[uint32]string{pid: c.running})
			marker := newMarker(t)
			marker.Save(capture.Marker{Backend: "fake", PID: pid, Exe: "Lowpath.exe", Game: "cs2"})
			_, srv := newRealServer(t, fp, withMarker(marker))
			_, still, _ := marker.Load()
			recovered := len(fp.Capture.Recovered()) == 1
			if recovered != c.recover || still == c.recover || (srv.live().Failsafe != nil) != c.recover {
				t.Fatalf("recovered=%v marker kept=%v failsafe=%+v", recovered, still, srv.live().Failsafe)
			}
		})
	}
}

func TestBrokenMarkerIsDropped(t *testing.T) {
	fp := newFake(t)
	path := filepath.Join(t.TempDir(), "capture-session.json")
	os.WriteFile(path, []byte("{not json"), 0o600)
	_, srv := newRealServer(t, fp, withMarker(capture.NewMarkerFile(path)))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("broken marker kept")
	}
	if f := srv.live().Failsafe; f != nil || len(fp.Capture.Recovered()) != 0 {
		t.Fatalf("a broken marker was treated as a crash: %+v", f)
	}
}
