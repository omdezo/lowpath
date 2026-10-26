package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a window program the test opens, fails or closes.
type fakeProc struct {
	cmd     command
	done    chan struct{}
	once    sync.Once
	err     error
	onTerm  bool // closes on SIGTERM, as the real windows do
	mu      sync.Mutex
	signals []os.Signal
	killed  bool
}

func newFakeProc(c command) *fakeProc {
	return &fakeProc{cmd: c, done: make(chan struct{}), onTerm: true}
}

func (p *fakeProc) exit(err error) { p.once.Do(func() { p.err = err; close(p.done) }) }
func (p *fakeProc) Wait() error    { <-p.done; return p.err }
func (p *fakeProc) Signal(sig os.Signal) error {
	p.mu.Lock()
	p.signals = append(p.signals, sig)
	p.mu.Unlock()
	if p.onTerm && sig == syscall.SIGTERM {
		p.exit(nil)
	}
	return nil
}
func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.exit(errors.New("signal: killed"))
	return nil
}

// fakeExec is the system the shell and the privilege helpers run programs
// on: which programs exist, what each started one does, and what a run
// prints and returns.
type fakeExec struct {
	mu       sync.Mutex
	programs map[string]bool
	onStart  func(p *fakeProc) // what a started program does (nil: stays open)
	started  []*fakeProc
	runs     []command
	run      func(c command) (int, string)
	spawned  []command
	spawnErr error
}

func newFakeExec(programs ...string) *fakeExec {
	x := &fakeExec{programs: map[string]bool{}}
	for _, p := range programs {
		x.programs[p] = true
	}
	return x
}

func (x *fakeExec) LookPath(name string) (string, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.programs[name] {
		if strings.HasPrefix(name, "/") {
			return name, nil
		}
		return "/usr/bin/" + name, nil
	}
	return "", errors.New("not found: " + name)
}

func (x *fakeExec) Start(c command) (process, error) {
	p := newFakeProc(c)
	x.mu.Lock()
	x.started = append(x.started, p)
	on := x.onStart
	x.mu.Unlock()
	if on != nil {
		go on(p)
	}
	return p, nil
}

func (x *fakeExec) Run(ctx context.Context, c command) (int, string, error) {
	x.mu.Lock()
	x.runs = append(x.runs, c)
	run := x.run
	x.mu.Unlock()
	if run == nil {
		return 0, "", nil
	}
	code, stderr := run(c)
	return code, stderr, nil
}

func (x *fakeExec) Spawn(c command) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.spawned = append(x.spawned, c)
	return x.spawnErr
}

func (x *fakeExec) procs() []*fakeProc {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]*fakeProc(nil), x.started...)
}

func testLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// kind is a window kind that runs the program path.
func kind(name, path string, cleaned *int) windowKind {
	return windowKind{
		name:    name,
		command: func(url string) (command, error) { return command{path: path, args: []string{url}}, nil },
		cleanup: func() {
			if cleaned != nil {
				*cleaned++
			}
		},
	}
}

func testWindows(x *fakeExec, kinds ...windowKind) *AppWindows {
	return &AppWindows{kinds: kinds, x: x, grace: 50 * time.Millisecond, stop: 200 * time.Millisecond, log: testLog(), stderr: io.Discard}
}

func runAsync(w *AppWindows, ctx context.Context, url string) chan error {
	done := make(chan error, 1)
	go func() { done <- w.RunWindow(ctx, url) }()
	return done
}

func waitStarted(t *testing.T, x *fakeExec, n int) []*fakeProc {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ps := x.procs(); len(ps) >= n {
			return ps
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d windows started, want %d", len(x.procs()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("RunWindow didn't return")
		return nil
	}
}

// A window program that fails at start (no display, no toolkit) costs a
// fallback to the next kind; the one that stays open is the window, and
// closing it ends RunWindow.
func TestRunWindowFallsBackAndEndsWhenClosed(t *testing.T) {
	x := newFakeExec()
	x.onStart = func(p *fakeProc) {
		if p.cmd.path == "broken" {
			p.exit(errors.New("exit status 3"))
		}
	}
	var cleanedBroken, cleanedGood int
	w := testWindows(x, kind("broken", "broken", &cleanedBroken), kind("good", "good", &cleanedGood))
	done := runAsync(w, context.Background(), "http://127.0.0.1:7878/api/session?t=x")
	ps := waitStarted(t, x, 2)
	time.Sleep(100 * time.Millisecond) // past the grace period
	select {
	case <-done:
		t.Fatal("RunWindow returned with the window open")
	default:
	}
	if cleanedBroken != 1 {
		t.Errorf("failed kind cleaned up %d times, want 1", cleanedBroken)
	}
	ps[1].exit(nil) // the user closes the window
	if err := waitDone(t, done); err != nil {
		t.Fatalf("RunWindow = %v", err)
	}
	if cleanedGood != 1 {
		t.Errorf("window kind cleaned up %d times, want 1", cleanedGood)
	}
}

// With no kind that opens, RunWindow says so, with each kind's reason.
func TestRunWindowNone(t *testing.T) {
	x := newFakeExec()
	x.onStart = func(p *fakeProc) { p.exit(errors.New("cannot open display")) }
	missing := windowKind{name: "webkit", command: func(string) (command, error) { return command{}, errors.New("no python3") }}
	w := testWindows(x, missing, kind("chromium", "chromium", nil))
	err := w.RunWindow(context.Background(), "u")
	if !errors.Is(err, ErrNoWindow) || !strings.Contains(err.Error(), "no python3") || !strings.Contains(err.Error(), "cannot open display") {
		t.Errorf("RunWindow = %v, want ErrNoWindow with both reasons", err)
	}
	if err := testWindows(x).RunWindow(context.Background(), "u"); !errors.Is(err, ErrNoWindow) {
		t.Errorf("no kinds: %v", err)
	}
}

// The app quitting (Quit in the UI, a signal) closes the windows: SIGTERM,
// then a kill for one that hangs.
func TestRunWindowClosesOnQuit(t *testing.T) {
	x := newFakeExec()
	w := testWindows(x, kind("good", "good", nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(w, ctx, "u")
	ps := waitStarted(t, x, 1)
	time.Sleep(100 * time.Millisecond)
	// A second window, as a second start of the app opens, that ignores
	// SIGTERM.
	if err := w.ShowWindow("u2"); err != nil {
		t.Fatal(err)
	}
	ps = waitStarted(t, x, 2)
	ps[1].onTerm = false
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(ps[0].signals) == 0 || ps[0].signals[0] != syscall.SIGTERM {
		t.Errorf("window got %v, want SIGTERM", ps[0].signals)
	}
	time.Sleep(300 * time.Millisecond)
	ps[1].mu.Lock()
	killed := ps[1].killed
	ps[1].mu.Unlock()
	if !killed {
		t.Error("a window that ignored SIGTERM wasn't killed")
	}
	if err := w.ShowWindow("u3"); !errors.Is(err, ErrNoWindow) {
		t.Errorf("ShowWindow after quitting: %v, want ErrNoWindow", err)
	}
}

// Quitting during the grace period also closes the window.
func TestRunWindowQuitWhileOpening(t *testing.T) {
	x := newFakeExec()
	w := testWindows(x, kind("good", "good", nil))
	w.grace = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(w, ctx, "u")
	ps := waitStarted(t, x, 1)
	cancel()
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
	if len(ps[0].signals) == 0 {
		t.Error("window not asked to close")
	}
}

// A second start opens another window of the same kind with the fresh
// link; the app quits only when the last window is closed. A Chromium
// browser hands the second one to its running process and exits at once,
// which doesn't end anything.
func TestShowWindowAddsWindows(t *testing.T) {
	x := newFakeExec()
	w := testWindows(x, kind("good", "good", nil))
	if err := w.ShowWindow("u"); !errors.Is(err, ErrNoWindow) {
		t.Errorf("ShowWindow before a window: %v", err)
	}
	done := runAsync(w, context.Background(), "first")
	ps := waitStarted(t, x, 1)
	time.Sleep(100 * time.Millisecond)

	if err := w.ShowWindow("second"); err != nil {
		t.Fatal(err)
	}
	ps = waitStarted(t, x, 2)
	if ps[1].cmd.args[0] != "second" {
		t.Errorf("second window at %q", ps[1].cmd.args)
	}
	ps[1].exit(nil) // the forwarding browser exits, or the user closes it
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("RunWindow returned with the first window open")
	default:
	}
	if err := w.ShowWindow("third"); err != nil {
		t.Fatal(err)
	}
	ps = waitStarted(t, x, 3)
	ps[0].exit(nil)
	time.Sleep(20 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("RunWindow returned with the third window open")
	default:
	}
	ps[2].exit(nil)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

// A window closed within the grace period was closed by the user: no
// fallback to another kind.
func TestRunWindowClosedQuickly(t *testing.T) {
	x := newFakeExec()
	x.onStart = func(p *fakeProc) { p.exit(nil) }
	w := testWindows(x, kind("a", "a", nil), kind("b", "b", nil))
	if err := w.RunWindow(context.Background(), "u"); err != nil {
		t.Fatal(err)
	}
	if n := len(x.procs()); n != 1 {
		t.Errorf("%d windows started, want 1", n)
	}
}

func TestOpenBrowserAndShowError(t *testing.T) {
	x := newFakeExec("xdg-open", "kdialog")
	w := testWindows(x)
	w.browser = func(url string) command { return command{path: "xdg-open", args: []string{url}} }
	w.dialog = func(title, msg string) []command {
		return []command{{path: "zenity", args: []string{msg}}, {path: "kdialog", args: []string{msg}}}
	}
	if err := w.OpenBrowser("http://127.0.0.1:7878/"); err != nil {
		t.Fatal(err)
	}
	if ps := x.procs(); len(ps) != 1 || ps[0].cmd.path != "/usr/bin/xdg-open" {
		t.Errorf("browser started as %v", ps)
	}
	w.ShowError("Lowpath", "port in use")
	if len(x.runs) != 1 || x.runs[0].path != "/usr/bin/kdialog" {
		t.Errorf("dialogs run: %v, want kdialog (zenity is missing)", x.runs)
	}
	if err := testWindowsNoBrowser().OpenBrowser("u"); err == nil {
		t.Error("OpenBrowser without xdg-open succeeded")
	}
}

func testWindowsNoBrowser() *AppWindows {
	w := testWindows(newFakeExec())
	w.browser = func(url string) command { return command{path: "xdg-open", args: []string{url}} }
	return w
}

func TestTailBuffer(t *testing.T) {
	b := &tailBuffer{max: 10}
	b.Write([]byte("0123456789abc\nlast line\n"))
	if got := b.String(); len(got) != 10 || !strings.HasSuffix(got, "line\n") {
		t.Errorf("tail = %q", got)
	}
	if got := lastLine("one\ntwo\n\n"); got != "two" {
		t.Errorf("lastLine = %q", got)
	}
}
