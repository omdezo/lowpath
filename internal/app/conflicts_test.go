package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/platform/fakeplatform"
)

var errNeedAdminForTest = fmt.Errorf("divert: open: %w", capture.ErrNeedAdmin)

// routeGame sets route mode with the loopback relay and adds a custom game.
func routeGame(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	call(t, ts, "POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	return g.ID
}

type applyReply struct {
	Error     string            `json:"error"`
	Message   string            `json:"message"`
	Conflicts []compat.Conflict `json:"conflicts"`
}

func applyRaw(t *testing.T, ts *httptest.Server, body string) (int, applyReply) {
	t.Helper()
	code, b := do(t, ts, "POST", "/api/apply", body, csrf)
	var r applyReply
	if code >= 300 {
		if err := json.Unmarshal([]byte(b), &r); err != nil {
			t.Fatalf("apply reply %q: %v", b, err)
		}
	}
	return code, r
}

var exitLagRunning = compat.Inventory{Processes: []string{"exitlag.exe"}, Programs: []string{"ExitLag"}}

// The acceptance case: with ExitLag running, Boost says so instead of
// starting a capture that fights ExitLag's driver; "Boost anyway" starts it.
func TestBoostRefusedWhileExitLagRuns(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	fp.Inventory.Set(exitLagRunning)
	ts, _ := newRealServer(t, fp)
	id := routeGame(t, ts)

	code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`)
	if code != 409 || r.Error != "conflict" || !strings.HasPrefix(r.Message, "ExitLag is running") || !strings.Contains(r.Message, "close it first") {
		t.Fatalf("apply next to ExitLag: %d %+v", code, r)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].ID != "exitlag" || r.Conflicts[0].Level != compat.LevelBlock || r.Conflicts[0].Why == "" {
		t.Fatalf("conflicts = %+v", r.Conflicts)
	}
	if fp.Capture.Last() != nil {
		t.Fatal("capture started next to ExitLag")
	}

	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart","ignore_conflicts":true}`); code != 200 {
		t.Fatalf("Boost anyway: %d %+v", code, r)
	}
	lastCapture(t, fp)
	call(t, ts, "POST", "/api/stop", "", nil)

	// ExitLag closed: Boost works again without asking.
	fp.Inventory.Set(compat.Inventory{Programs: []string{"ExitLag"}})
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); code != 200 {
		t.Fatalf("after closing ExitLag: %d %+v", code, r)
	}
	if n := len(fp.Capture.Sessions()); n != 2 {
		t.Errorf("%d capture sessions, want 2", n)
	}
}

// Restarting as administrator wouldn't help next to ExitLag, so the
// conflict is reported first.
func TestConflictBeforeAdminPrompt(t *testing.T) {
	fp := newFake(t)
	fp.Inventory.Set(exitLagRunning)
	fp.Capture.SetAvailable(errNeedAdminForTest)
	ts, _ := newRealServer(t, fp)
	id := routeGame(t, ts)
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); code != 409 || r.Error != "conflict" {
		t.Fatalf("unelevated next to ExitLag: %d %+v", code, r)
	}
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart","ignore_conflicts":true}`); code != 409 || r.Error != "needs_admin" {
		t.Fatalf("Boost anyway, unelevated: %d %+v", code, r)
	}
}

// Warnings (a full VPN, a leftover driver, an installed booster) never stop
// a boost, and proxy mode, which captures nothing, isn't refused.
func TestWarningsDontBlock(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	fp.Inventory.Set(compat.Inventory{
		Services:     []compat.Service{{Name: "ndisrd", Driver: true, Running: true}},
		Programs:     []string{"NoPing"},
		Adapters:     []compat.Adapter{{Index: 3, Name: "NordLynx", Description: "NordLynx Tunnel", Type: 53, Up: true}},
		DefaultRoute: 3,
	})
	ts, _ := newRealServer(t, fp)
	id := routeGame(t, ts)
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); code != 200 {
		t.Fatalf("apply with warnings: %d %+v", code, r)
	}
	call(t, ts, "POST", "/api/stop", "", nil)

	fp.Inventory.Set(exitLagRunning)
	call(t, ts, "PUT", "/api/settings", `{"capture_mode":"proxy"}`, nil)
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); r.Error == "conflict" {
		t.Fatalf("proxy mode refused: %d %+v", code, r)
	}
}

// A capture failure next to a running conflict names it, so the failure
// isn't silent about the likely cause.
func TestCaptureFailureNamesConflict(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	fp.Capture.SetStartErr(errors.New("divert: the WinDivert driver is not available: another program has a different WinDivert version loaded"))
	fp.Inventory.Set(compat.Inventory{Services: []compat.Service{{Name: "WinDivert", Driver: true, Running: true, Path: `\??\C:\Tools\gdpi\WinDivert64.sys`}}})
	ts, srv := newRealServer(t, fp)
	id := routeGame(t, ts)
	srv.conflicts.scan()
	code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`)
	if code != 500 || r.Error != "capture_failed" || !strings.Contains(r.Message, "Another WinDivert program may be in the way") || len(r.Conflicts) != 1 {
		t.Fatalf("capture failure: %d %+v", code, r)
	}
}

// A scan that hangs must not hold up Boost: after the wait, the last report
// is used (none yet: nothing blocks) and the answer says the check didn't
// finish.
func TestHungScanFailsOpen(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	release := make(chan struct{})
	fp.Inventory.Set(exitLagRunning)
	fp.Inventory.Hang(release)
	ts, srv := newRealServer(t, fp)
	srv.conflicts.wait = 50 * time.Millisecond
	t.Cleanup(func() { close(release) })
	id := routeGame(t, ts)
	start := time.Now()
	if code, r := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); code != 200 {
		t.Fatalf("apply during a hung scan: %d %+v", code, r)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("apply took %v", d)
	}
	var rep compat.Report
	call(t, ts, "GET", "/api/conflicts?fresh=1", "", &rep)
	if len(rep.Unchecked) == 0 || !strings.Contains(rep.Unchecked[len(rep.Unchecked)-1], "didn't finish") {
		t.Errorf("report during a hung scan: %+v", rep)
	}
}

type panicky struct{ fakeplatform.Inventory }

func (*panicky) Services() ([]compat.Service, error) { panic("service manager exploded") }

func TestPanickingScanIsReported(t *testing.T) {
	fp := newFake(t)
	fp.Privileges.SetElevated(true)
	ts, srv := newRealServer(t, fp, func(c *Config) { c.Platform.Inventory = &panicky{} })
	r := srv.conflicts.scan()
	if len(r.Conflicts) != 0 || len(r.Unchecked) != 1 || !strings.Contains(r.Unchecked[0], "exploded") {
		t.Fatalf("report after a panic: %+v", r)
	}
	id := routeGame(t, ts)
	if code, rep := applyRaw(t, ts, `{"game_id":"`+id+`","mode":"smart"}`); code != 200 {
		t.Fatalf("apply after a panicking scan: %d %+v", code, rep)
	}
}

// The live feed carries the report for the banner; the API rescans on
// request; the diagnostics export leaves out user folder names.
func TestConflictsInLiveAPIAndDiagnostics(t *testing.T) {
	fp := newFake(t)
	ts, srv := newRealServer(t, fp)
	var rep compat.Report
	call(t, ts, "GET", "/api/conflicts", "", &rep)
	if rep.Conflicts == nil || len(rep.Conflicts) != 0 || rep.At.IsZero() {
		t.Fatalf("empty system: %+v", rep)
	}
	fp.Inventory.Set(compat.Inventory{
		Programs: []string{"ExitLag"},
		Services: []compat.Service{{Name: "WinDivert", Driver: true, Running: true, Path: `C:\Users\Example User\Downloads\tool\WinDivert64.sys`}},
	})
	fp.Inventory.SetErr("adapters", errors.New("access denied"))
	call(t, ts, "GET", "/api/conflicts", "", &rep)
	if len(rep.Conflicts) != 0 {
		t.Errorf("cached report changed without a scan: %+v", rep)
	}
	call(t, ts, "GET", "/api/conflicts?fresh=1", "", &rep)
	if len(rep.Conflicts) != 2 || len(rep.Unchecked) != 1 || !strings.HasPrefix(rep.Unchecked[0], "network adapters:") {
		t.Fatalf("fresh scan: %+v", rep)
	}
	if lv := srv.live(); lv.Conflicts == nil || len(lv.Conflicts.Conflicts) != 2 {
		t.Errorf("live = %+v", lv.Conflicts)
	}
	_, body := do(t, ts, "GET", "/api/diagnostics", "", nil)
	if !strings.Contains(body, `"conflicts"`) || !strings.Contains(body, "exitlag") || strings.Contains(body, "Example User") {
		t.Errorf("diagnostics: conflicts missing or user folder not cut: %s", body)
	}
}
