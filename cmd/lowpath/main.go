// Lowpath is the desktop app: it serves the UI and API on 127.0.0.1:7878,
// runs the optimization engine, and (in route mode) captures the selected
// game's UDP traffic through the platform's capture backend (WinDivert on
// Windows, a TUN device on Linux). It shows the UI in an app window
// (WebView2 on Windows, WebKit or a Chromium app window on Linux, WKWebView
// on macOS), hands a second start over to the copy that runs, and asks for
// the rights route mode needs (UAC, pkexec, the macOS administrator
// prompt). Everything OS-specific comes from internal/platform.
//
//	Lowpath.exe                       window; asks for admin rights (UAC)
//	Lowpath.exe -no-elevate           window, without admin (route mode then says so)
//	Lowpath.exe -demo                 simulated relays and game server
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
	"io/fs"
	"log"
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
	"lowpath/internal/capture"
	"lowpath/internal/catalog"
	"lowpath/internal/compat"
	"lowpath/internal/demo"
	"lowpath/internal/diag"
	"lowpath/internal/platform"
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
func run(parent context.Context, o options, plat *platform.Platform, desk platform.Desktop, inst platform.Instance) error {
	if err := checkLoopback(o.addr); err != nil {
		return err
	}
	logger, closeLog, err := openLog(o.logs)
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
	defer keepCrashes(o.logs, logger)()

	ctx, cancel := signal.NotifyContext(parent, stopSignals()...)
	defer cancel()
	srv, err := newServer(ctx, o, plat, logger, cancel)
	if err != nil {
		ln.Close()
		return err
	}

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	url := "http://" + ln.Addr().String()
	logger.Info("Lowpath is running", "ui", url, "version", app.Version, "demo", o.demo, "os", plat.OS,
		"capture", plat.Capture.Name(), "admin", priv.Elevated(), "data", o.data, "logs", o.logs)

	sh := &shell{o: o, desk: desk, srv: srv, url: url, log: logger}
	if l, err := inst.Serve(sh.showAgain); err != nil {
		logger.Warn("a second start won't find this copy", "err", err)
	} else {
		defer l.Close()
	}
	if sh.show(ctx) {
		cancel() // the window was closed: quit
	}
	select {
	case <-ctx.Done():
	case err = <-serveErr:
	}

	// Stop the session first (capture off, networking back to normal), then
	// let in-flight requests finish.
	logger.Info("shutting down")
	srv.Close()
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

// shell puts the UI in front of the user: the app window, the browser, or
// (headless) a link on the terminal, and again on a second start.
type shell struct {
	o    options
	desk platform.Desktop
	srv  *app.Server
	url  string // the UI's base URL
	log  *slog.Logger
}

// show shows the UI the way the options say, and reports whether it was a
// window that the user has now closed (closing it quits). The launch link
// (see app.Server.LaunchPath) goes straight to the window or browser,
// never to the log file.
func (s *shell) show(ctx context.Context) bool {
	launch := s.url + s.srv.LaunchPath()
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
func (s *shell) showAgain() {
	launch := s.url + s.srv.NewLaunchPath()
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

func (s *shell) browser(url string) {
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

// buildServer wires the API server to the store, catalog, relays and the
// platform layer without starting background work, so tests can run it on
// a fake platform.
func buildServer(ctx context.Context, o options, plat *platform.Platform, logger *slog.Logger, quit func()) (*app.Server, error) {
	store, err := app.OpenStore(filepath.Join(o.data, "state.json"))
	if err != nil {
		return nil, err
	}
	games, err := app.LoadCatalog()
	if err != nil {
		return nil, err
	}
	var imp *app.RelayImport
	if o.relays != "" {
		if imp, err = app.LoadRelayImport(o.relays); err != nil {
			return nil, err
		}
		logger.Info("imported", "file", o.relays, "relays", len(imp.Relays), "games", len(imp.Games))
	}
	relays, err := app.NewRelayNetwork(store, imp, logger)
	if err != nil {
		return nil, err
	}
	dist, err := fs.Sub(ui.Dist, "dist")
	if err != nil {
		return nil, err
	}
	// Valve's SDR POP lists for CS2, Dota 2 and Deadlock: downloaded from
	// Steam when a game page opens, kept on disk for offline starts, pinged
	// from this PC.
	servers := catalog.NewValveServers(catalog.ValveOptions{
		Source: catalog.NewSDRSource(nil, catalog.SteamAPI),
		Prober: catalog.NewPOPProber(catalog.ICMPPinger{}, nil),
		Cache:  catalog.NewDirCache(filepath.Join(o.data, "catalog")),
		Log:    logger.With("component", "catalog"),
	})
	cfg := app.Config{Store: store, Games: games, Relays: relays, Servers: servers, Platform: plat, UI: dist, Log: logger, Quit: quit, ArtDir: filepath.Join(o.data, "art"),
		TraceCount: o.traceCount, TraceInterval: o.traceInterval,
		// While route mode captures, this file says so; finding it at the
		// next start means that run was killed or crashed.
		Marker: capture.NewMarkerFile(filepath.Join(o.data, "capture-session.json")),
	}
	if o.bloatServer != "" {
		l, err := diag.NewHTTPLoader(diag.HTTPLoaderConfig{BaseURL: o.bloatServer, UserAgent: "Lowpath/" + app.Version + " (bufferbloat test)"})
		if err != nil {
			servers.Close()
			relays.Close()
			return nil, err
		}
		cfg.BloatLoader = l
	}
	if plat.Privileges.CanElevate() {
		cfg.Elevate = func() error { return plat.Privileges.Relaunch(childArgs(o)) }
	}
	if o.demo {
		sim, err := demo.Start(ctx, demo.DefaultPaths(), demo.DirectProfile(), logger.With("component", "relay"))
		if err != nil {
			servers.Close()
			relays.Close()
			return nil, err
		}
		cfg.Network, cfg.Sim = app.DemoNetwork{N: sim}, sim
		logger.Info("demo network up", "game_server", sim.GameAddr(), "egress", sim.Egress.Addr(), "paths", len(sim.Paths))
	}
	srv, err := app.NewServer(cfg)
	if err != nil {
		servers.Close()
		relays.Close()
		return nil, err
	}
	return srv, nil
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

// maxLogSize is when the log starts over (checked at start only).
const maxLogSize = 5 << 20

// openLog logs to <dir>/lowpath.log and stderr, and points the standard
// library's log (used by the WebView2 binding) at the same place.
func openLog(dir string) (*slog.Logger, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(dir, "lowpath.log")
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, nil, err
	}
	w := tee{f, os.Stderr}
	logger := slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	log.SetOutput(w)
	return logger, func() { f.Close() }, nil
}

// tee writes to every writer and never fails: a GUI build has no stderr, and
// logging must not break the app.
type tee []io.Writer

func (t tee) Write(p []byte) (int, error) {
	for _, w := range t {
		w.Write(p)
	}
	return len(p), nil
}
