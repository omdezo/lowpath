// Lowpath is the desktop app: it serves the UI and API on 127.0.0.1:7878,
// runs the optimization engine, and (in route mode) captures the selected
// game's UDP traffic through the platform's capture backend (WinDivert on
// Windows). On Windows it shows the UI in its own WebView2 window and
// restarts itself as administrator when needed. Everything OS-specific
// comes from internal/platform.
//
// When the background service (cmd/lowpathsvc, LowpathSvc) is installed,
// the app runs no engine of its own and asks for no rights: it starts the
// service if it is stopped, serves the UI, and passes the API on to the
// service over its named pipe (app.Frontend).
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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/compat"
	"lowpath/internal/diag"
	"lowpath/internal/platform"
	"lowpath/internal/shell"
	"lowpath/ui"
)

type options struct {
	addr, data, relays     string
	bloatServer            string
	demo, headless, open   bool
	noElevate, dataFromArg bool
	waitPID                int
	traceCount             int
	traceInterval          time.Duration
	compatInventory        string
	noService              bool
	servicePipe            string
	// serviceNote is set by run, not a flag: why the installed service
	// isn't used (connectService).
	serviceNote string
}

func main() {
	plat := platform.Native()
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:7878", "UI/API listen address (loopback only)")
	flag.StringVar(&o.data, "data", defaultDataDir(plat.Paths), "directory for settings, relays and the log")
	flag.StringVar(&o.relays, "relays", "", "optional read-only relay list (JSON) to use alongside the relays added in the app")
	flag.BoolVar(&o.demo, "demo", false, "run against a simulated network instead of real relays")
	flag.BoolVar(&o.headless, "headless", false, "no window; stop with Ctrl+C")
	flag.BoolVar(&o.open, "open", false, "open the UI in the default browser instead of a window")
	flag.BoolVar(&o.noElevate, "no-elevate", false, "don't ask for administrator rights at start (route mode needs them)")
	flag.StringVar(&o.bloatServer, "bloat-server", "", "load server for the bufferbloat test (default "+diag.DefaultLoadURL+"); it must answer GET /__down?bytes=N and POST /__up")
	flag.IntVar(&o.traceCount, "trace-count", 0, "probes per hop in a route trace (default 200; for tests)")
	flag.DurationVar(&o.traceInterval, "trace-interval", 0, "time between probes to one hop in a route trace (default 1s; for tests)")
	flag.StringVar(&o.compatInventory, "compat-inventory", "", "read the other network software (boosters, drivers, VPN adapters) from this JSON file instead of the system (for tests)")
	flag.BoolVar(&o.noService, "no-service", false, "run the engine in this process even when the background service (LowpathSvc) is installed")
	flag.StringVar(&o.servicePipe, "service-pipe", "", "talk to a service run by hand on this pipe (Windows) or socket (elsewhere) instead of the installed one (for development and tests)")
	flag.IntVar(&o.waitPID, "wait-pid", 0, "wait for this process to exit before starting (used when restarting elevated)")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) { o.dataFromArg = o.dataFromArg || f.Name == "data" })
	if o.compatInventory != "" {
		// The end-to-end test plays "ExitLag is running" on any PC, and the
		// real system's software doesn't change its results.
		plat.Inventory = compat.NewFileSystem(o.compatInventory)
	}

	if err := run(o, plat); err != nil {
		slog.Error("fatal", "err", err)
		if !o.headless {
			showError("Lowpath", err.Error())
		}
		os.Exit(1)
	}
}

func run(o options, plat *platform.Platform) error {
	if err := checkLoopback(o.addr); err != nil {
		return err
	}
	logger, closeLog, err := shell.OpenLog(o.data)
	if err != nil {
		return err
	}
	defer closeLog()

	if o.waitPID != 0 {
		// The copy we replace still holds the port until it has exited.
		waitForExit(o.waitPID, 30*time.Second)
	}
	// Bind before asking for admin rights: when a copy is already running,
	// bring it forward instead of showing a pointless UAC prompt first.
	ln, err := listen(o)
	if err != nil {
		if url, ok := existingInstance(o.addr); ok {
			logger.Info("Lowpath is already running", "ui", url)
			// A browser opened here is signed in only if it already was:
			// just the running copy can hand out a launch link.
			if !o.headless && !focusExisting() {
				openBrowser(url)
			}
			return nil
		}
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The background service holds the rights route mode needs, so with it
	// this process needs none and shows no prompt.
	fe, note, err := connectService(ctx, o, plat, logger)
	o.serviceNote = note
	if err != nil {
		ln.Close()
		return err
	}
	if fe != nil {
		defer shell.KeepCrashes(o.data, logger)()
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
		return serve(ctx, cancel, o, plat, logger, ln, fe.Handler(), fe.LaunchPath(), stop)
	}

	priv := plat.Privileges
	// Ask only when the rights are what route mode lacks: a prompt can't
	// fix a missing driver (WinDivert has no ARM64 build) or an OS with no
	// capture backend yet.
	if o.waitPID == 0 && !o.noElevate && !o.demo && plat.ElevationHelps() {
		switch err := priv.Relaunch(childArgs(o)); {
		case err == nil:
			logger.Info("restarting as administrator")
			ln.Close() // the elevated copy waits for this process to exit, then binds
			return nil
		case errors.Is(err, platform.ErrElevateCancelled):
			logger.Warn("administrator prompt cancelled; running without admin rights (route mode needs them)")
		default:
			logger.Warn("could not restart as administrator", "err", err)
		}
	}

	// Only the copy that serves keeps crash reports: the one that just
	// relaunched elevated, or found Lowpath already running, adds nothing
	// to match a report (or a session marker's PID) against.
	defer shell.KeepCrashes(o.data, logger)()

	srv, err := newServer(ctx, o, plat, logger, cancel)
	if err != nil {
		ln.Close()
		return err
	}
	logger.Info("running the engine in this process", "demo", o.demo, "os", plat.OS,
		"capture", plat.Capture.Name(), "admin", priv.Elevated(), "data", o.data)
	// Stop the session first (capture off, networking back to normal), then
	// let in-flight requests finish.
	return serve(ctx, cancel, o, plat, logger, ln, srv.Handler(), srv.LaunchPath(), srv.Close)
}

// serve runs the UI server on ln and the window (or browser) until either
// ends, then calls stop before letting in-flight requests finish.
func serve(ctx context.Context, cancel func(), o options, plat *platform.Platform, logger *slog.Logger, ln net.Listener, h http.Handler, launchPath string, stop func()) error {
	hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	url := "http://" + ln.Addr().String()
	logger.Info("Lowpath is running", "ui", url, "version", app.Version)
	// The launch link (see app.Server.LaunchPath) goes straight to the
	// window or browser, never to the log file.
	launch := url + launchPath

	switch {
	case o.headless:
		fmt.Fprintf(os.Stderr, "\nOpen the UI (this link works once): %s\n\n", launch)
	case o.open:
		openBrowser(launch)
	default:
		// The window owns the main thread until it closes; closing it quits.
		if err := runWindow(ctx, launch, webViewDir(plat), logger); err != nil {
			logger.Warn("no app window; opening the browser instead", "err", err)
			openBrowser(launch)
		} else {
			cancel()
		}
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
	dist, err := fs.Sub(ui.Dist, "dist")
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

// webViewDir is the app window's browser profile. An elevated run gets its
// own: WebView2 fails to start (and go-webview2 then exits the process) on a
// folder that browser processes of the other elevation level still use, as
// the old window's do for a moment after "Restart as administrator". The UI
// keeps no state in the browser, so nothing is lost by not sharing it.
func webViewDir(plat *platform.Platform) string {
	base, err := plat.Paths.CacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "Lowpath")
	}
	name := "WebView2"
	if plat.Privileges.Elevated() {
		name = "WebView2-admin"
	}
	return filepath.Join(base, name)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		slog.Warn("open browser", "err", err)
	}
}
