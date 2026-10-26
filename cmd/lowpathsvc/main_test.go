package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/platform"
	"lowpath/internal/platform/fakeplatform"
)

type testCLI struct {
	*cli
	fp       *fakeplatform.Fake
	out, err bytes.Buffer
}

func newTestCLI(t *testing.T) *testCLI {
	t.Helper()
	fp := fakeplatform.New(t.TempDir())
	tc := &testCLI{fp: fp}
	tc.cli = newCLI(fp.Platform(), &tc.out, &tc.err)
	tc.cli.exe = func() (string, error) { return `D:\Downloads\lowpathsvc.exe`, nil }
	return tc
}

// Unelevated, install asks for the rights once by starting itself again
// elevated, and passes this user's settings folder along: the administrator
// account that accepts the prompt may have another one.
func TestInstallElevatesOnce(t *testing.T) {
	c := newTestCLI(t)
	c.fp.Privileges.SetCanElevate(true)
	if err := c.dispatch("install", nil); err != nil {
		t.Fatal(err)
	}
	user, _ := c.fp.Paths.DataDir()
	runs := c.fp.Privileges.Relaunches()
	want := []string{"install", "-import", user, "-pause"}
	if len(runs) != 1 || !slices.Equal(runs[0], want) {
		t.Fatalf("relaunches %q, want %q", runs, want)
	}
	if len(c.fp.Service.Installs()) != 0 {
		t.Error("installed without the rights")
	}
	if c.pause {
		t.Error("the unelevated half waits for Enter")
	}
}

func TestInstallWithoutPrompt(t *testing.T) {
	c := newTestCLI(t) // no way to ask
	if err := c.dispatch("install", nil); err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("install without rights or prompt: %v", err)
	}
}

// Linux and macOS can show a prompt (the desktop app's polkit and
// administrator dialogs) but have no service manager to install into yet:
// install and uninstall say so without prompting. On Linux the prompt
// would restart the program as the same user, which would ask again.
func TestInstallWithoutServiceManager(t *testing.T) {
	for _, cmd := range []string{"install", "uninstall"} {
		c := newTestCLI(t)
		c.fp.Privileges.SetCanElevate(true)
		c.fp.Service.Set(platform.ServiceState{}, errors.ErrUnsupported)
		if err := c.dispatch(cmd, nil); !errors.Is(err, errors.ErrUnsupported) || !strings.Contains(err.Error(), "lowpathsvc run") {
			t.Errorf("%s without a service manager: %v", cmd, err)
		}
		if runs := c.fp.Privileges.Relaunches(); len(runs) != 0 {
			t.Errorf("%s prompted for rights: %q", cmd, runs)
		}
	}
}

// Elevated, install hands the service manager this program, the service's
// own data folder and the user's settings to start from.
func TestInstallElevated(t *testing.T) {
	c := newTestCLI(t)
	c.fp.Privileges.SetElevated(true)
	user := t.TempDir()
	state := []byte(`{"settings":{"capture_mode":"route"}}`)
	os.WriteFile(filepath.Join(user, "state.json"), state, 0o600)
	if err := c.dispatch("install", []string{"-import", user, "-pause"}); err != nil {
		t.Fatal(err)
	}
	ins := c.fp.Service.Installs()
	if len(ins) != 1 {
		t.Fatalf("installs %v", ins)
	}
	data, _ := c.fp.Paths.ServiceDataDir()
	got := ins[0]
	if got.Exe != `D:\Downloads\lowpathsvc.exe` || !slices.Equal(got.Args, []string{"-data", data}) || got.DataDir != data {
		t.Errorf("install config %+v", got)
	}
	if !bytes.Equal(got.Seed["state.json"], state) {
		t.Errorf("seed %q, want the user's state.json", got.Seed["state.json"])
	}
	if !c.pause || !strings.Contains(c.out.String(), "won't ask for administrator rights again") {
		t.Errorf("pause %v, output %q", c.pause, c.out.String())
	}

	// Nothing to import: the service starts with defaults.
	c2 := newTestCLI(t)
	c2.fp.Privileges.SetElevated(true)
	if err := c2.dispatch("install", []string{"-import", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if s := c2.fp.Service.Installs()[0].Seed; s != nil {
		t.Errorf("seed without a state file: %v", s)
	}
}

func TestSeedStateLimit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "state.json"), make([]byte, maxSeed+1), 0o600)
	if _, err := seedState(dir); err == nil {
		t.Error("an oversized state file was accepted")
	}
	if b, err := seedState(""); b != nil || err != nil {
		t.Errorf("no folder: %v, %v", b, err)
	}
}

func TestUninstall(t *testing.T) {
	c := newTestCLI(t)
	c.fp.Privileges.SetCanElevate(true)
	if err := c.dispatch("uninstall", nil); err != nil {
		t.Fatal(err)
	}
	if runs := c.fp.Privileges.Relaunches(); len(runs) != 1 || runs[0][0] != "uninstall" {
		t.Fatalf("relaunches %q", runs)
	}
	c.fp.Privileges.SetElevated(true)
	if err := c.dispatch("uninstall", nil); err != nil {
		t.Fatal(err)
	}
	if c.fp.Service.Uninstalls() != 1 {
		t.Error("not uninstalled")
	}
}

func TestDispatch(t *testing.T) {
	c := newTestCLI(t)
	if err := c.dispatch("bogus", nil); err == nil {
		t.Error("unknown command accepted")
	}
	// Double-clicked, outside the service manager: the usage, not a server.
	c = newTestCLI(t)
	if err := c.dispatch("", nil); err != nil || !strings.Contains(c.out.String(), "install the Lowpath service") {
		t.Errorf("no arguments: %v, %q", err, c.out.String())
	}
}

// runService runs the service on the fake platform until the test ends.
func runService(t *testing.T, fp *fakeplatform.Fake, data string) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- newService(fp.Platform(), runOptions{data: data}).serve(ctx) }()
	stopped := false
	stop = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the service did not stop")
			return nil
		}
	}
	t.Cleanup(func() { stop() })
	return stop
}

func waitAnswer(t *testing.T, fe *app.Frontend) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := fe.Check(t.Context()); err == nil {
			return
		} else if time.Now().After(deadline) {
			t.Fatalf("service never answered: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The service serves the app's API on its channel, says it is the service,
// keeps its log in its own folder, and stops cleanly.
func TestServeAPI(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	fp.Privileges.SetElevated(true)
	data := t.TempDir()
	stop := runService(t, fp, data)

	fe, err := app.NewFrontend(app.FrontendConfig{Dial: fp.ServiceAPI.Dial, Session: "s"})
	if err != nil {
		t.Fatal(err)
	}
	defer fe.Close()
	waitAnswer(t, fe)
	ts := httptest.NewServer(fe.Handler())
	defer ts.Close()
	req, _ := http.NewRequest("GET", ts.URL+"/api/bootstrap", nil)
	req.Header.Set("Cookie", "lowpath_session=s")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var b struct {
		Platform struct {
			Admin   bool `json:"admin"`
			Service struct {
				State string `json:"state"`
			} `json:"service"`
		} `json:"platform"`
	}
	json.NewDecoder(res.Body).Decode(&b)
	res.Body.Close()
	if res.StatusCode != 200 || b.Platform.Service.State != "in_use" || !b.Platform.Admin {
		t.Fatalf("bootstrap through the service: %d %+v", res.StatusCode, b.Platform)
	}

	// status sees it answering.
	c := newTestCLI(t)
	c.cli.plat = fp.Platform()
	fp.Service.Set(platform.ServiceState{Installed: true, Running: true, PID: 9, Exe: "lowpathsvc.exe"}, nil)
	if err := c.dispatch("status", nil); err != nil || !strings.Contains(c.out.String(), "answers: Lowpath "+app.Version) {
		t.Errorf("status: %v, %q", err, c.out.String())
	}

	// An account the service turns away (the Unix socket's peer check)
	// learns why, and the service's log says who it was.
	fp.ServiceAPI.SetRefused(platform.PeerCred{UID: 65534, PID: 99}, fmt.Errorf("%w: uid 65534 has no login session", platform.ErrPeerRefused))
	fe.Close() // the check runs on new connections: one already served stays
	if _, err := fe.Check(t.Context()); !errors.Is(err, platform.ErrPeerRefused) {
		t.Errorf("check while refused: %v, want ErrPeerRefused", err)
	}
	c.out.Reset()
	if err := c.dispatch("status", nil); err == nil || !strings.Contains(err.Error(), "signed in at this computer") {
		t.Errorf("status while refused: %v", err)
	}
	fp.ServiceAPI.SetRefused(platform.PeerCred{}, nil)
	waitAnswer(t, fe)

	// A second copy can't take the API from a running one.
	if err := newService(fp.Platform(), runOptions{data: t.TempDir()}).serve(t.Context()); err == nil {
		t.Error("a second service started on a taken API")
	}

	if err := stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := fe.Check(t.Context()); err == nil {
		t.Error("still answering after stop")
	}
	if fi, err := os.Stat(filepath.Join(data, "lowpath.log")); err != nil || fi.Size() == 0 {
		t.Errorf("service log: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(data, "lowpath.log"))
	if !strings.Contains(string(log), "refused a connection to the API") || !strings.Contains(string(log), "uid 65534 (process 99)") {
		t.Errorf("the log doesn't name the refused account:\n%s", log)
	}
}

func TestStatus(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state platform.ServiceState
		want  string
	}{
		{"not installed", platform.ServiceState{}, "Not installed"},
		{"stopped", platform.ServiceState{Installed: true, Exe: "lowpathsvc.exe"}, "not running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestCLI(t)
			c.fp.Service.Set(tc.state, nil)
			if err := c.dispatch("status", nil); err != nil || !strings.Contains(c.out.String(), tc.want) {
				t.Errorf("status: %v, %q; want %q", err, c.out.String(), tc.want)
			}
		})
	}
	// Running, but nothing answers: an error a script can see.
	c := newTestCLI(t)
	c.fp.Service.Set(platform.ServiceState{Installed: true, Running: true, PID: 9}, nil)
	if err := c.dispatch("status", nil); err == nil {
		t.Error("status of a service that doesn't answer succeeded")
	}
}

// scmRunner plays the service manager: it runs the service at once, with a
// context that has already ended, so serve starts up and stops.
type scmRunner struct{ calls int }

func (r *scmRunner) Run(_ string, run func(ctx context.Context) error) (bool, error) {
	r.calls++
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return true, run(ctx)
}

// Started by the service manager, the service secures its data folder
// before it writes anything there, at every start; when it can't, it stops
// before serving anything.
func TestServiceSecuresDataFolder(t *testing.T) {
	c := newTestCLI(t)
	r := &scmRunner{}
	c.runner = r
	data := filepath.Join(t.TempDir(), "Lowpath")
	if err := c.dispatch("", []string{"-data", data}); err != nil {
		t.Fatal(err)
	}
	if got := c.fp.Service.Secured(); r.calls != 1 || len(got) != 1 || got[0].Dir != data || !got[0].Empty {
		t.Fatalf("secured %+v (runs %d), want %s once, before the log", got, r.calls, data)
	}
	if _, err := os.Stat(filepath.Join(data, "lowpath.log")); err != nil {
		t.Errorf("service log: %v", err)
	}

	c = newTestCLI(t)
	c.runner = &scmRunner{}
	c.fp.Service.SetSecureErr(platform.ErrNeedAdmin)
	data = filepath.Join(t.TempDir(), "Lowpath")
	if err := c.dispatch("", []string{"-data", data}); err == nil {
		t.Error("served with a data folder it couldn't secure")
	}
	if _, err := os.Stat(filepath.Join(data, "lowpath.log")); err == nil {
		t.Error("wrote its log into a data folder it couldn't secure")
	}
}
