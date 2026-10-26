package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"lowpath/internal/platform"
	"lowpath/internal/platform/fakeplatform"
)

// fakeDesktop plays the app window: RunWindow runs the test's window
// function (which plays the user) and returns when it does, as a closed
// window would.
type fakeDesktop struct {
	window func(ctx context.Context, url string) error

	mu      sync.Mutex
	shown   []string // ShowWindow URLs
	opened  []string // OpenBrowser URLs
	errors  []string
	showErr error
	menu    []string
}

func (d *fakeDesktop) RunWindow(ctx context.Context, url string) error {
	if d.window == nil {
		return platform.ErrNoWindow
	}
	return d.window(ctx, url)
}

func (d *fakeDesktop) ShowWindow(url string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.showErr != nil {
		return d.showErr
	}
	d.shown = append(d.shown, url)
	return nil
}

func (d *fakeDesktop) OpenBrowser(url string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.opened = append(d.opened, url)
	return nil
}

func (d *fakeDesktop) ShowError(title, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.errors = append(d.errors, msg)
}

func (d *fakeDesktop) AddToAppMenu(exe string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.menu = append(d.menu, exe)
	return nil
}

func (d *fakeDesktop) browsed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.opened...)
}

// fakeInstance connects starts in one test process: the first Serve wins,
// Signal reaches it.
type fakeInstance struct {
	mu     sync.Mutex
	onShow func()
}

func (i *fakeInstance) Serve(onShow func()) (io.Closer, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onShow = onShow
	return closerFunc(func() error { i.mu.Lock(); i.onShow = nil; i.mu.Unlock(); return nil }), nil
}

func (i *fakeInstance) Signal() bool {
	i.mu.Lock()
	f := i.onShow
	i.mu.Unlock()
	if f == nil {
		return false
	}
	f()
	return true
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// freeAddr is a loopback address with a free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// windowClient signs a client in with the launch link, as the window does.
func windowClient(t *testing.T, launch string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	res, err := c.Get(launch)
	if err != nil {
		t.Error(err)
		return c
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("launch link: %d", res.StatusCode)
	}
	return c
}

func send(t *testing.T, c *http.Client, method, url, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("X-Lowpath", "1")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.Do(req)
	if err != nil {
		t.Error(err)
		return 0
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	return res.StatusCode
}

// The acceptance of issue #44: closing the window quits the app, and a
// boost running in route mode is stopped first, so the game's traffic is
// back on the normal route. A second start meanwhile brings the window
// forward with a fresh launch link instead of starting another copy.
func TestClosingTheWindowStopsTheBoost(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	fp.Privileges.SetElevated(true)
	inst := &fakeInstance{}
	addr := freeAddr(t)
	data, _ := os.MkdirTemp("", "lowpath-shell-")
	t.Cleanup(func() { time.Sleep(200 * time.Millisecond); os.RemoveAll(data) })
	o := options{addr: addr, data: data, logs: t.TempDir(), noElevate: true}

	second := &fakeDesktop{}
	desk := &fakeDesktop{}
	desk.window = func(ctx context.Context, launch string) error {
		c := windowClient(t, launch)
		base := "http://" + addr
		send(t, c, "PUT", base+"/api/settings", `{"capture_mode":"route","loopback_relay":true}`)
		var id struct{ ID string }
		req, _ := http.NewRequest("POST", base+"/api/games/custom", strings.NewReader(`{"name":"LP Check","process":"lpcheck.exe"}`))
		req.Header.Set("X-Lowpath", "1")
		if res, err := c.Do(req); err == nil {
			json.NewDecoder(res.Body).Decode(&id)
			res.Body.Close()
		}
		if code := send(t, c, "POST", base+"/api/apply", `{"game_id":"`+id.ID+`","mode":"smart"}`); code != 200 {
			t.Errorf("apply: %d", code)
		}
		if fp.Capture.Last() == nil || fp.Capture.Last().Closed() {
			t.Error("route mode isn't capturing")
		}

		// The user starts Lowpath again.
		if err := run(context.Background(), o, fp.Platform(), second, inst); err != nil {
			t.Errorf("second start: %v", err)
		}
		desk.mu.Lock()
		shown := append([]string(nil), desk.shown...)
		desk.mu.Unlock()
		if len(shown) != 1 || !strings.Contains(shown[0], "/api/session?t=") {
			t.Errorf("second start showed %q, want the window with a fresh launch link", shown)
		} else if res, err := http.Get(shown[0]); err != nil || res.StatusCode != 200 {
			t.Errorf("fresh launch link doesn't sign in: %v", err)
		}
		if len(second.browsed()) != 0 {
			t.Errorf("second start opened a browser: %q", second.browsed())
		}
		return nil // the user closes the window
	}

	done := make(chan error, 1)
	go func() { done <- run(context.Background(), o, fp.Platform(), desk, inst) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("closing the window didn't quit")
	}
	if s := fp.Capture.Last(); s == nil || !s.Closed() {
		t.Error("capture still on after the window closed")
	}
	if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		c.Close()
		t.Error("UI port still served after quitting")
	}
	if len(desk.menu) != 1 {
		t.Errorf("app menu entry asked for %d times, want once", len(desk.menu))
	}
}

// No window program: the UI opens in the browser, signed in.
func TestNoWindowOpensTheBrowser(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	desk := &fakeDesktop{}
	addr := freeAddr(t)
	// The artwork fetch may still be failing in the background when run
	// returns, so the data folder is not t.TempDir (which must be empty).
	data, _ := os.MkdirTemp("", "lowpath-shell-")
	t.Cleanup(func() { time.Sleep(200 * time.Millisecond); os.RemoveAll(data) })
	o := options{addr: addr, data: data, logs: t.TempDir(), noElevate: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, o, fp.Platform(), desk, &fakeInstance{}) }()

	deadline := time.Now().Add(10 * time.Second)
	for len(desk.browsed()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	b := desk.browsed()
	if len(b) != 1 || !strings.Contains(b[0], "/api/session?t=") {
		t.Fatalf("browser opened at %q, want the launch link", b)
	}
	windowClient(t, b[0])
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A second start while the first runs in the browser opens another
// browser tab with a fresh launch link (a used one would be refused).
func TestShowAgainWithoutWindow(t *testing.T) {
	fp := fakeplatform.New(t.TempDir())
	desk := &fakeDesktop{showErr: platform.ErrNoWindow}
	sh := &shell{o: options{}, desk: desk, url: "http://127.0.0.1:1", log: discardLog()}
	srv, err := buildServer(t.Context(), options{data: t.TempDir()}, fp.Platform(), discardLog(), func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	sh.srv = srv
	first := srv.LaunchPath()
	sh.showAgain()
	if b := desk.browsed(); len(b) != 1 || !strings.HasSuffix(b[0], first) {
		t.Errorf("browser at %q, want the unused launch link %q", b, first)
	}
}

func TestShowErrorWhenNothingOpens(t *testing.T) {
	desk := &brokenBrowser{}
	sh := &shell{o: options{open: true}, desk: desk, url: "http://127.0.0.1:7878", log: discardLog()}
	sh.browser("http://127.0.0.1:7878/api/session?t=secret")
	if len(desk.errors) != 1 || strings.Contains(desk.errors[0], "secret") || !strings.Contains(desk.errors[0], "http://127.0.0.1:7878") {
		t.Errorf("errors shown: %q, want one naming the UI address without the launch token", desk.errors)
	}
}

type brokenBrowser struct{ fakeDesktop }

func (*brokenBrowser) OpenBrowser(string) error { return errors.New("no xdg-open") }

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
