package platform

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// windowKind is one way to show the UI in a window of its own: a WebKit
// view driven by a helper the OS ships, or a Chromium-family browser in
// app mode. Each is a separate program, so the cgo-free app needs no GUI
// toolkit linked in, and a toolkit that is missing or broken costs only a
// fallback to the next kind.
type windowKind struct {
	name string
	// command builds the command that shows url, or says why this kind
	// isn't available here.
	command func(url string) (command, error)
	// cleanup runs after its last window has closed (it removes a browser
	// profile made for this run); may be nil.
	cleanup func()
}

// Window timings: a window program that exits with an error within
// startGrace didn't manage to open (no display, no toolkit) and the next
// kind is tried; stopWait is how long a window may take to close when the
// app quits before it is killed.
const (
	startGrace = 2500 * time.Millisecond
	stopWait   = 3 * time.Second
)

// AppWindows is the Linux and macOS desktop: the UI in app windows run as
// child processes, the first kind that opens wins. Closing the last window
// ends RunWindow, which makes the app quit and stop any capture.
type AppWindows struct {
	kinds   []windowKind
	x       execer
	grace   time.Duration
	stop    time.Duration
	log     *slog.Logger
	browser func(url string) command
	dialog  func(title, msg string) []command
	menu    func(exe string) error // nil: no per-user app menu
	stderr  io.Writer              // nil: os.Stderr

	mu     sync.Mutex
	active *windowKind
	procs  map[process]struct{}
	empty  chan struct{} // closed when the last of procs has exited
}

// RunWindow starts the first kind of window that opens and waits until all
// of that kind's windows are closed, or ctx ends.
func (w *AppWindows) RunWindow(ctx context.Context, url string) error {
	var tried []string
	for i := range w.kinds {
		k := &w.kinds[i]
		c, err := k.command(url)
		if err != nil {
			tried = append(tried, k.name+": "+err.Error())
			continue
		}
		p, err := w.x.Start(c)
		if err != nil {
			tried = append(tried, k.name+": "+err.Error())
			w.cleanup(k)
			continue
		}
		exited := make(chan error, 1)
		go func() { exited <- p.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				w.log.Info("window didn't open", "kind", k.name, "err", err)
				tried = append(tried, k.name+": "+err.Error())
				w.cleanup(k)
				continue
			}
			// Closed by the user before the grace period was over.
			w.cleanup(k)
			return nil
		case <-ctx.Done():
			w.terminate([]process{p})
			<-exited
			w.cleanup(k)
			return nil
		case <-time.After(w.grace):
		}
		w.log.Info("window open", "kind", k.name)
		return w.watch(ctx, k, p, exited)
	}
	if len(tried) == 0 {
		tried = append(tried, "no window program found")
	}
	return fmt.Errorf("%w: %s", ErrNoWindow, strings.Join(tried, "; "))
}

// watch tracks p (and the windows ShowWindow adds) until none is left.
func (w *AppWindows) watch(ctx context.Context, k *windowKind, p process, exited chan error) error {
	w.mu.Lock()
	w.active, w.procs, w.empty = k, map[process]struct{}{}, make(chan struct{})
	w.addLocked(p, exited)
	empty := w.empty
	w.mu.Unlock()

	select {
	case <-empty:
	case <-ctx.Done():
		w.mu.Lock()
		procs := make([]process, 0, len(w.procs))
		for p := range w.procs {
			procs = append(procs, p)
		}
		w.mu.Unlock()
		w.terminate(procs)
		select {
		case <-empty:
		case <-time.After(w.stop):
			w.log.Warn("a window didn't close")
		}
	}
	w.mu.Lock()
	w.active, w.procs = nil, nil
	w.mu.Unlock()
	w.cleanup(k)
	return nil
}

// addLocked tracks p; its exit is read from exited, or from p.Wait when
// exited is nil.
func (w *AppWindows) addLocked(p process, exited chan error) {
	w.procs[p] = struct{}{}
	empty := w.empty
	go func() {
		var err error
		if exited != nil {
			err = <-exited
		} else {
			err = p.Wait()
		}
		if err != nil {
			w.log.Info("window exited", "err", err)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if _, ok := w.procs[p]; !ok {
			return
		}
		delete(w.procs, p)
		if len(w.procs) == 0 {
			close(empty)
		}
	}()
}

// ShowWindow opens another window of the kind that is open. A Chromium
// browser hands it to its running process (same profile), which raises it;
// a WebKit helper opens one more window, and the app quits when the last
// one is closed.
func (w *AppWindows) ShowWindow(url string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.active == nil || len(w.procs) == 0 {
		return ErrNoWindow
	}
	c, err := w.active.command(url)
	if err != nil {
		return err
	}
	p, err := w.x.Start(c)
	if err != nil {
		return err
	}
	w.addLocked(p, nil)
	return nil
}

// terminate asks the windows to close (SIGTERM, which both kinds handle by
// closing), and kills those still running after the stop timeout.
func (w *AppWindows) terminate(procs []process) {
	for _, p := range procs {
		p.Signal(syscall.SIGTERM)
	}
	time.AfterFunc(w.stop, func() {
		for _, p := range procs {
			p.Kill() // fails harmlessly when it has exited
		}
	})
}

func (w *AppWindows) cleanup(k *windowKind) {
	if k.cleanup != nil {
		k.cleanup()
	}
}

// AddToAppMenu adds exe to the app menu where the desktop has one per user.
func (w *AppWindows) AddToAppMenu(exe string) error {
	if w.menu == nil {
		return nil
	}
	return w.menu(exe)
}

func (w *AppWindows) OpenBrowser(url string) error {
	c := w.browser(url)
	path, err := w.x.LookPath(c.path)
	if err != nil {
		return fmt.Errorf("no program to open the browser with: %w", err)
	}
	c.path = path
	p, err := w.x.Start(c)
	if err != nil {
		return err
	}
	go p.Wait()
	return nil
}

// ShowError prints the error and shows it in the first dialog program
// found; a desktop start has no terminal to read it in.
func (w *AppWindows) ShowError(title, msg string) {
	out := w.stderr
	if out == nil {
		out = os.Stderr
	}
	fmt.Fprintf(out, "%s: %s\n", title, msg)
	for _, c := range w.dialog(title, msg) {
		path, err := w.x.LookPath(c.path)
		if err != nil {
			continue
		}
		c.path = path
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		_, _, err = w.x.Run(ctx, c)
		cancel()
		if err == nil {
			return
		}
	}
}

// chromiumApp is a Chromium-family browser in app mode: a window with no
// tabs or address bar. Its profile is one of its own, made fresh for each
// run, so the browser is a process of this run (and RunWindow can tell when
// its window closes) instead of a tab handed to a browser the user already
// has open.
type chromiumApp struct {
	path    string // the browser program
	profile string // this run's profile folder
}

func (c chromiumApp) command(url string) command {
	return command{path: c.path, args: []string{
		"--app=" + url,
		"--user-data-dir=" + c.profile,
		"--no-first-run", "--no-default-browser-check",
		// The profile is thrown away: no keyring prompt for a password
		// store nothing is saved in.
		"--password-store=basic",
		"--window-size=1100,720",
		// X11 window class, so the desktop matches the window to the menu
		// entry (StartupWMClass) and its icon.
		"--class=lowpath",
	}}
}

// runProfile is a folder for this run's window profile under base, named
// after this process. Folders left behind by runs that are gone are removed.
func runProfile(base string) string {
	if entries, err := os.ReadDir(base); err == nil {
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid == os.Getpid() || processAlive(pid) {
				continue
			}
			os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return filepath.Join(base, strconv.Itoa(os.Getpid()))
}

// writeIcon puts the app icon in dir for a window to show, and returns its
// path ("" when there is no icon or it can't be written).
func writeIcon(dir string, png []byte) string {
	if len(png) == 0 || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	path := filepath.Join(dir, "lowpath.png")
	if os.WriteFile(path, png, 0o600) != nil {
		return ""
	}
	return path
}

// chromiumKind is the app-mode window of the browser locate finds; locate
// also says under which folder the run's profile goes.
func chromiumKind(locate func() (path, profileBase string, err error)) windowKind {
	var app *chromiumApp
	return windowKind{
		name: "chromium",
		command: func(url string) (command, error) {
			if app == nil {
				path, base, err := locate()
				if err != nil {
					return command{}, err
				}
				app = &chromiumApp{path: path, profile: runProfile(base)}
			}
			return app.command(url), nil
		},
		cleanup: func() {
			if app != nil {
				os.RemoveAll(app.profile)
				app = nil
			}
		},
	}
}
