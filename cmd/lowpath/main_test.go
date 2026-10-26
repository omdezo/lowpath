package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"lowpath/internal/app"
	"lowpath/internal/platform"
	"lowpath/internal/platform/fakeplatform"
)

func TestChildArgsReplaceWaitPID(t *testing.T) {
	saved := os.Args
	defer func() { os.Args = saved }()
	os.Args = []string{"Lowpath.exe", "-headless", "-wait-pid", "5", "--wait-pid=6", "-addr", "127.0.0.1:7879"}

	got := childArgs(options{data: `C:\Users\p\AppData\Roaming\Lowpath`})
	want := []string{"-headless", "-addr", "127.0.0.1:7879", "-data", `C:\Users\p\AppData\Roaming\Lowpath`, "-wait-pid", strconv.Itoa(os.Getpid())}
	if !slices.Equal(got, want) {
		t.Errorf("childArgs = %q, want %q", got, want)
	}

	// An explicit -data is passed through as given, not added twice.
	os.Args = []string{"Lowpath.exe", "-data", `D:\x`}
	got = childArgs(options{data: `D:\x`, dataFromArg: true})
	if strings.Count(strings.Join(got, " "), "-data") != 1 {
		t.Errorf("childArgs = %q, want -data once", got)
	}
}

func TestOpenLogStartsOverWhenLarge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lowpath.log")
	if err := os.WriteFile(path, make([]byte, maxLogSize+1), 0o600); err != nil {
		t.Fatal(err)
	}
	logger, closeLog, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("hello")
	closeLog()
	b, _ := os.ReadFile(path)
	if len(b) > 1000 || !strings.Contains(string(b), "hello") {
		t.Errorf("log is %d bytes after start; want it truncated and holding the new line", len(b))
	}

	// A small log is appended to.
	logger, closeLog, _ = openLog(dir)
	logger.Info("again")
	closeLog()
	b, _ = os.ReadFile(path)
	if !strings.Contains(string(b), "hello") || !strings.Contains(string(b), "again") {
		t.Errorf("small log not appended to: %q", b)
	}
}

func TestCheckLoopback(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:7878": true, "localhost:1": true, "[::1]:7878": true, "0.0.0.0:7878": false, "192.168.1.2:80": false} {
		if err := checkLoopback(addr); (err == nil) != ok {
			t.Errorf("checkLoopback(%s) = %v", addr, err)
		}
	}
}

// A second launch finds the running copy through the one API call that needs
// no session, and nothing else passes for it.
func TestExistingInstance(t *testing.T) {
	store, err := app.OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	plat := fakeplatform.New(t.TempDir()).Platform()
	srv, err := app.NewServer(app.Config{Store: store, Platform: plat, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Close()
	if _, ok := existingInstance(ts.Listener.Addr().String()); !ok {
		t.Error("running Lowpath not recognized")
	}

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"something else"}`))
	}))
	defer other.Close()
	if _, ok := existingInstance(other.Listener.Addr().String()); ok {
		t.Error("another program on the port taken for Lowpath")
	}
}

// signIn follows the one-time launch link so the client carries the session
// cookie, as the app window does.
func signIn(t *testing.T, ts *httptest.Server, srv *app.Server) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	res, err := c.Get(ts.URL + srv.LaunchPath())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	return c
}

// The whole app, as main wires it, runs on a fake platform on every OS: the
// bootstrap reports the platform layer's view, and the restart button is
// wired to the privilege helper with this process's arguments.
func TestAppOnFakePlatform(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	fp.OS = "testos"
	fp.Privileges.SetCanElevate(true)
	quit := make(chan struct{})
	o := options{data: t.TempDir(), addr: "127.0.0.1:0"}
	srv, err := buildServer(t.Context(), o, fp.Platform(), slog.New(slog.NewTextHandler(io.Discard, nil)), func() { close(quit) })
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Close()
	c := signIn(t, ts, srv)

	var b struct {
		Platform struct {
			OS         string `json:"os"`
			Admin      bool   `json:"admin"`
			CanElevate bool   `json:"can_elevate"`
			Capture    struct {
				Backend   string `json:"backend"`
				Available bool   `json:"available"`
			} `json:"capture"`
		} `json:"platform"`
	}
	res, err := c.Get(ts.URL + "/api/bootstrap")
	if err != nil {
		t.Fatal(err)
	}
	json.NewDecoder(res.Body).Decode(&b)
	res.Body.Close()
	p := b.Platform
	if p.OS != "testos" || p.Admin || !p.CanElevate || p.Capture.Backend != "fake" || !p.Capture.Available {
		t.Fatalf("bootstrap platform %+v", p)
	}

	req, _ := http.NewRequest("POST", ts.URL+"/api/elevate", nil)
	req.Header.Set("X-Lowpath", "1")
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("elevate: %d", res.StatusCode)
	}
	<-quit // the running copy hands over to the elevated one
	runs := fp.Privileges.Relaunches()
	if len(runs) != 1 || !slices.Contains(runs[0], "-wait-pid") || !slices.Contains(runs[0], o.data) {
		t.Fatalf("relaunch args %q, want -wait-pid and the data folder", runs)
	}
}

// Without a prompt to show (Linux and macOS today), there is no Elevate hook:
// the API answers 501 and the UI offers proxy mode instead.
func TestNoElevateWithoutPrompt(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	srv, err := buildServer(t.Context(), options{data: t.TempDir()}, fp.Platform(), slog.New(slog.NewTextHandler(io.Discard, nil)), func() {})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	defer srv.Close()
	c := signIn(t, ts, srv)
	req, _ := http.NewRequest("POST", ts.URL+"/api/elevate", nil)
	req.Header.Set("X-Lowpath", "1")
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 501 || len(fp.Privileges.Relaunches()) != 0 {
		t.Fatalf("elevate without a prompt: %d, relaunches %v", res.StatusCode, fp.Privileges.Relaunches())
	}
}

func TestWebViewDirPerElevation(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	plat := fp.Platform()
	user := webViewDir(plat)
	fp.Privileges.SetElevated(true)
	admin := webViewDir(plat)
	cache, _ := plat.Paths.CacheDir()
	if user == admin || filepath.Dir(user) != cache || filepath.Dir(admin) != cache {
		t.Errorf("webview folders %q and %q, want two folders under %q", user, admin, cache)
	}
}

type noDirs struct{ platform.Paths }

func (noDirs) DataDir() (string, error) { return "", errors.New("$HOME is not defined") }

func TestDefaultDataDir(t *testing.T) {
	paths := fakeplatform.New(t.TempDir()).Paths
	if d, _ := paths.DataDir(); defaultDataDir(paths) != d {
		t.Errorf("defaultDataDir = %q, want the platform's %q", defaultDataDir(paths), d)
	}
	if got := defaultDataDir(noDirs{paths}); got != ".lowpath" {
		t.Errorf("without a per-user folder: %q, want .lowpath", got)
	}
}
