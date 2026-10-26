// Lowpath is the desktop app: it serves the UI and API on 127.0.0.1:7878,
// runs the optimization engine, and (in route mode) captures the selected
// game's UDP traffic through the platform's capture backend (WinDivert on
// Windows, a TUN device on Linux). It shows the UI in an app window
// (WebView2 on Windows, WebKit or a Chromium app window on Linux, WKWebView
// on macOS), hands a second start over to the copy that runs, and asks for
// the rights route mode needs (UAC, pkexec, the macOS administrator
// prompt). Everything OS-specific comes from internal/platform.
//
// When the background service (cmd/lowpathsvc, LowpathSvc) is installed,
// the app runs no engine of its own and asks for no rights: it starts the
// service if it is stopped, serves the UI, and passes the API on to the
// service over its named pipe, or its Unix socket on Linux and macOS
// (app.Frontend).
//
//	Lowpath.exe                       window; uses the service, or asks for admin rights (UAC)
//	Lowpath.exe -no-elevate           window, without admin (route mode then says so)
//	Lowpath.exe -no-service           run the engine here even when the service is installed
//	Lowpath.exe -demo                 simulated relays and game server (never the service)
//	Lowpath.exe -headless             no window; Ctrl+C stops
//	Lowpath.exe -open                 open the UI in the default browser instead
//	Lowpath.exe -relays relays.json   also use a read-only list of relays
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/compat"
	"lowpath/internal/diag"
	"lowpath/internal/platform"
	"lowpath/internal/shell"
	"lowpath/ui"
)

type options struct {
	addr, data, logs, relays string
	bloatServer              string
	demo, headless, open     bool
	noElevate, dataFromArg   bool
	logsFromArg              bool
	waitPID                  int
	traceCount               int
	traceInterval            time.Duration
	compatInventory          string
	noService                bool
	servicePipe              string
	// serviceNote is set by run, not a flag: why the installed service
	// isn't used (connectService).
	serviceNote string
	wifiReading string
}

// appIcon is the app's icon for the windows and the app menu on Linux and
// macOS (Windows takes it from the exe's resources).
//
//go:embed winres/icon.png
var appIcon []byte

func main() {
	plat := platform.Native()
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:7878", "UI/API listen address (loopback only)")
	flag.StringVar(&o.data, "data", defaultDataDir(plat.Paths), "directory for settings, relays and learned servers")
	flag.StringVar(&o.logs, "logs", "", "directory for the log and crash reports (default: the OS's per-user log folder, or -data when that is given)")
	flag.StringVar(&o.relays, "relays", "", "optional read-only relay list (JSON) to use alongside the relays added in the app")
	flag.BoolVar(&o.demo, "demo", false, "run against a simulated network instead of real relays")
	flag.BoolVar(&o.headless, "headless", false, "no window; stop with Ctrl+C")
	flag.BoolVar(&o.open, "open", false, "open the UI in the default browser instead of a window")
	flag.BoolVar(&o.noElevate, "no-elevate", false, "don't ask for the rights route mode needs at start")
	flag.StringVar(&o.bloatServer, "bloat-server", "", "load server for the bufferbloat test (default "+diag.DefaultLoadURL+"); it must answer GET /__down?bytes=N and POST /__up")
	flag.IntVar(&o.traceCount, "trace-count", 0, "probes per hop in a route trace (default 200; for tests)")
	flag.DurationVar(&o.traceInterval, "trace-interval", 0, "time between probes to one hop in a route trace (default 1s; for tests)")
	flag.StringVar(&o.compatInventory, "compat-inventory", "", "read the other network software (boosters, drivers, VPN adapters) from this JSON file instead of the system (for tests)")
	flag.BoolVar(&o.noService, "no-service", false, "run the engine in this process even when the background service (LowpathSvc) is installed")
	flag.StringVar(&o.servicePipe, "service-pipe", "", "talk to a service run by hand on this pipe (Windows) or socket (elsewhere) instead of the installed one (for development and tests)")
	flag.StringVar(&o.wifiReading, "wifi-reading", "", "read the Wi-Fi link, survey and scan pause from this JSON file instead of the adapter (for tests)")
	flag.IntVar(&o.waitPID, "wait-pid", 0, "wait for this process to exit before starting (used when restarting with more rights)")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) {
		o.dataFromArg = o.dataFromArg || f.Name == "data"
		o.logsFromArg = o.logsFromArg || f.Name == "logs"
	})
	o.logs = logDir(o, plat.Paths)
	if o.compatInventory != "" {
		// The end-to-end test plays "ExitLag is running" on any PC, and the
		// real system's software doesn't change its results.
		plat.Inventory = compat.NewFileSystem(o.compatInventory)
	}
	if o.wifiReading != "" {
		// The end-to-end test shows a Wi-Fi card on any PC; the real
		// adapter is never read or tuned.
		plat.WiFi = diag.NewFileWLAN(o.wifiReading)
	}
	desk := platform.NewDesktop(platform.DesktopConfig{ProfileDir: windowProfile(plat), Icon: appIcon})

	if err := run(context.Background(), o, plat, desk, platform.NewInstance(o.addr)); err != nil {
		slog.Error("fatal", "err", err)
		if !o.headless {
			desk.ShowError("Lowpath", err.Error())
		}
		os.Exit(1)
	}
}

// run serves the app until the window closes, a stop signal comes, or parent
// ends.
//
// The order matters. The log comes first, so every later step is in it.
// The UI port is bound before anything that could show a prompt: when a
// copy already runs, this start only brings that copy's window forward.
// Then the background service: when it answers, it holds the rights route
// mode needs, so this process asks for none and runs no engine. Only
// without it does the app ask for those rights (UAC on Windows, polkit on
// Linux, the administrator dialog on macOS) and run the engine itself.
func run(parent context.Context, o options, plat *platform.Platform, desk platform.Desktop, inst platform.Instance) error {
	if err := checkLoopback(o.addr); err != nil {
		return err
	}
	logger, closeLog, err := shell.OpenLog(o.logs)
	if err != nil {
		return err
	}
	defer closeLog()
	// A copy running as root for the user (macOS's elevated restart) leaves
	// the user's files the user's.
	handBack(o.logs, o.data)
	defer handBack(o.logs, o.data)

	if o.waitPID != 0 {
		// The copy we replace still holds the port until it has exited.
		waitForExit(o.waitPID, 30*time.Second)
	}
	// Bind before asking for more rights: when a copy is already running,
	// bring it forward instead of showing a pointless prompt first.
	ln, err := listen(o)
	if err != nil {
		if url, ok := existingInstance(o.addr); ok {
			logger.Info("Lowpath is already running", "ui", url)
			// The running copy shows its window, with a launch link of its
			// own; a browser opened here is signed in only if it already
			// was, since only that copy can hand out a launch link.
			if !o.headless && !inst.Signal() {
				if err := desk.OpenBrowser(url); err != nil {
					logger.Warn("open browser", "err", err)
				}
			}
			return nil
		}
		return err
	}

	ctx, cancel := signal.NotifyContext(parent, stopSignals()...)
	defer cancel()
	if !ui.Built() {
		// Built before npm run build (a fresh clone): the window shows a
		// page that says how to build the UI instead of the UI.
		logger.Warn("this build has no UI, only a placeholder page: run npm run build in ui, then build Lowpath again")
	}

	// The background service holds the rights route mode needs, so with it
	// this process needs none and shows no prompt.
	fe, note, err := connectService(ctx, o, plat, logger)
	o.serviceNote = note
	if err != nil {
		ln.Close()
		return err
	}
	if fe != nil {
		defer shell.KeepCrashes(o.logs, logger)()
		defer fe.Close()
		// Closing the window stops the boost, as it does without the
		// service; the service itself keeps running for the next launch.
		stop := func() {
			c, done := context.WithTimeout(context.Background(), 3*time.Second)
			defer done()
			if err := fe.StopSession(c); err != nil {
				logger.Warn("can't ask the service to stop the boost", "err", err)
			}
		}
		return serve(ctx, cancel, o, desk, inst, logger, ln, fe, stop)
	}

	priv := plat.Privileges
	// Ask only when the rights are what route mode lacks: a prompt can't
	// fix a missing driver (WinDivert has no ARM64 build) or an OS with no
	// capture backend yet.
	if o.waitPID == 0 && !o.noElevate && !o.demo && plat.ElevationHelps() {
		switch err := priv.Relaunch(childArgs(o)); {
		case err == nil:
			logger.Info("restarting with the rights route mode needs")
			ln.Close() // the new copy waits for this process to exit, then binds
			return nil
		case errors.Is(err, platform.ErrElevateCancelled):
			logger.Warn("prompt for more rights cancelled; running without them (route mode needs them)")
		default:
			logger.Warn("could not restart with more rights", "err", err)
		}
	}

	// Only the copy that serves keeps crash reports: the one that just
	// relaunched elevated, or found Lowpath already running, adds nothing
	// to match a report (or a session marker's PID) against.
	defer shell.KeepCrashes(o.logs, logger)()

	srv, err := newServer(ctx, o, plat, logger, cancel)
	if err != nil {
		ln.Close()
		return err
	}
	logger.Info("running the engine in this process", "demo", o.demo, "os", plat.OS,
		"capture", plat.Capture.Name(), "admin", priv.Elevated(), "data", o.data)
	// Stop the session first (capture off, networking back to normal), then
	// let in-flight requests finish.
	return serve(ctx, cancel, o, desk, inst, logger, ln, srv, srv.Close)
}

// front is what the UI port serves: the engine's own server (app.Server),
// or the window's side of the background service (app.Frontend). Either
// one hands out the launch links that sign the window in.
type front interface {
	Handler() http.Handler
	LaunchPath() string
	NewLaunchPath() string
}

// serve runs the UI server on ln and the window (or browser) until either
// ends, then calls stop before letting in-flight requests finish. A second
// start of the app meanwhile reaches this copy through inst.
func serve(ctx context.Context, cancel func(), o options, desk platform.Desktop, inst platform.Instance, logger *slog.Logger, ln net.Listener, f front, stop func()) error {
	hs := &http.Server{Handler: f.Handler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	url := "http://" + ln.Addr().String()
	logger.Info("Lowpath is running", "ui", url, "version", app.Version, "logs", o.logs)

	sh := &uiShell{o: o, desk: desk, front: f, url: url, log: logger}
	if l, err := inst.Serve(sh.showAgain); err != nil {
		logger.Warn("a second start won't find this copy", "err", err)
	} else {
		defer l.Close()
	}
	if sh.show(ctx) {
		cancel() // the window was closed: quit
	}
	var err error
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}

	logger.Info("shutting down")
	stop()
	shut, done := context.WithTimeout(context.Background(), 3*time.Second)
	defer done()
	if serr := hs.Shutdown(shut); serr != nil {
		hs.Close()
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

// uiShell puts the UI in front of the user: the app window, the browser,
// or (headless) a link on the terminal, and again on a second start.
type uiShell struct {
	o     options
	desk  platform.Desktop
	front front  // hands out the launch links
	url   string // the UI's base URL
	log   *slog.Logger
}

// show shows the UI the way the options say, and reports whether it was a
// window that the user has now closed (closing it quits). The launch link
// (see app.Server.LaunchPath) goes straight to the window or browser,
// never to the log file.
func (s *uiShell) show(ctx context.Context) bool {
	launch := s.url + s.front.LaunchPath()
	switch {
	case s.o.headless:
		fmt.Fprintf(os.Stderr, "\nOpen the UI (this link works once): %s\n\n", launch)
		return false
	case s.o.open:
		s.browser(launch)
		return false
	}
	if exe, err := os.Executable(); err == nil {
		go func() {
			if err := s.desk.AddToAppMenu(exe); err != nil {
				s.log.Warn("app menu entry", "err", err)
			}
		}()
	}
	// The window owns the main thread until it closes.
	if err := s.desk.RunWindow(ctx, launch); err != nil {
		s.log.Warn("no app window; opening the browser instead", "err", err)
		s.browser(launch)
		return false
	}
	return true
}

// showAgain answers a second start of the app: the window comes forward
// (or another one opens), else the browser opens, signed in with a fresh
// launch link.
func (s *uiShell) showAgain() {
	launch := s.url + s.front.NewLaunchPath()
	if !s.o.headless && !s.o.open {
		err := s.desk.ShowWindow(launch)
		if err == nil {
			return
		}
		if !errors.Is(err, platform.ErrNoWindow) {
			s.log.Warn("show window", "err", err)
		}
	}
	s.browser(launch)
}

func (s *uiShell) browser(url string) {
	if err := s.desk.OpenBrowser(url); err != nil {
		s.log.Warn("open browser", "err", err)
		if s.o.headless {
			return
		}
		// Started from the desktop, nobody reads the terminal.
		s.desk.ShowError("Lowpath", "Lowpath is running, but no window or browser could be opened. Open "+s.url+" in a browser.")
	}
}

// newServer builds the API server: real relays by default, the simulated
// network with -demo.
func newServer(ctx context.Context, o options, plat *platform.Platform, logger *slog.Logger, quit func()) (*app.Server, error) {
	srv, err := buildServer(ctx, o, plat, logger, quit)
	if err != nil {
		return nil, err
	}
	srv.Run(ctx)
	return srv, nil
}

// buildServer wires the API server (shell.Build) with this program's UI and
// restart, without starting background work, so tests can run it on a fake
// platform.
func buildServer(ctx context.Context, o options, plat *platform.Platform, logger *slog.Logger, quit func()) (*app.Server, error) {
	dist, err := ui.FS()
	if err != nil {
		return nil, err
	}
	so := shell.Options{
		Data: o.data, Relays: o.relays, BloatServer: o.bloatServer, Demo: o.demo,
		TraceCount: o.traceCount, TraceInterval: o.traceInterval, UI: dist,
		ServiceNote: o.serviceNote,
	}
	if plat.Privileges.CanElevate() {
		so.Elevate = func() error { return plat.Privileges.Relaunch(childArgs(o)) }
	}
	return shell.Build(ctx, so, plat, logger, quit)
}

func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("-addr must be a loopback address")
	}
	return nil
}

// listen binds the UI port. An elevated restart retries for a while: the
// copy it replaces may take a moment to let go of the port.
func listen(o options) (net.Listener, error) {
	deadline := time.Now()
	if o.waitPID != 0 {
		deadline = deadline.Add(10 * time.Second)
	}
	for {
		ln, err := net.Listen("tcp", o.addr)
		if err == nil || time.Now().After(deadline) {
			if err != nil {
				err = fmt.Errorf("can't listen on %s (is another program using it? start with -addr 127.0.0.1:<port>): %w", o.addr, err)
			}
			return ln, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// existingInstance reports whether a Lowpath already serves addr.
func existingInstance(addr string) (string, bool) {
	url := "http://" + addr
	c := http.Client{Timeout: 2 * time.Second}
	res, err := c.Get(url + "/api/version") // the one API call that needs no session
	if err != nil {
		return "", false
	}
	defer res.Body.Close()
	var b struct {
		Version string `json:"version"`
	}
	if res.StatusCode != 200 || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&b) != nil || b.Version == "" {
		return "", false
	}
	return url, true
}

// childArgs are the arguments for the elevated copy: ours, plus the PID it
// must wait for, plus the data directory so it opens the same state even if
// the administrator account differs from the signed-in one.
func childArgs(o options) []string {
	var args []string
	skip := false
	for _, a := range os.Args[1:] {
		name := strings.TrimLeft(a, "-")
		switch {
		case skip:
			skip = false
		case name == "wait-pid":
			skip = true // and its value
		case strings.HasPrefix(name, "wait-pid="):
		default:
			args = append(args, a)
		}
	}
	if !o.dataFromArg {
		args = append(args, "-data", o.data)
	}
	if !o.logsFromArg {
		args = append(args, "-logs", o.logs)
	}
	return append(args, "-wait-pid", strconv.Itoa(os.Getpid()))
}

// defaultDataDir is the platform's per-user folder (%APPDATA%\Lowpath on
// Windows), or .lowpath in the working directory when the OS has none.
func defaultDataDir(paths platform.Paths) string {
	if d, err := paths.DataDir(); err == nil {
		return d
	}
	return ".lowpath"
}

// logDir is where the log goes: -logs, else the -data folder when one was
// given (tests and the elevated restart keep everything together), else
// the OS's per-user log folder.
func logDir(o options, paths platform.Paths) string {
	switch {
	case o.logsFromArg:
		return o.logs
	case o.dataFromArg:
		return o.data
	}
	if d, err := paths.LogDir(); err == nil {
		return d
	}
	return o.data
}

// windowProfile is the app window's browser profile. An elevated run gets
// its own: WebView2 fails to start (and go-webview2 then exits the process)
// on a folder that browser processes of the other elevation level still
// use, as the old window's do for a moment after "Restart as
// administrator". The UI keeps no state in the browser, so nothing is lost
// by not sharing it.
func windowProfile(plat *platform.Platform) string {
	base, err := plat.Paths.CacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "Lowpath")
	}
	name := "window"
	if plat.OS == "windows" {
		name = "WebView2"
	}
	if plat.Privileges.Elevated() {
		name += "-admin"
	}
	return filepath.Join(base, name)
}
