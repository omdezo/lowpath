// Lowpath is the desktop app: it serves the UI and API on 127.0.0.1:7878,
// runs the optimization engine, and (in route mode) captures the selected
// game's UDP traffic with WinDivert. On Windows it shows the UI in its own
// WebView2 window and restarts itself as administrator when needed.
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lowpath/internal/app"
	"lowpath/internal/capture"
	"lowpath/internal/demo"
	"lowpath/ui"
)

type options struct {
	addr, data, relays     string
	demo, headless, open   bool
	noElevate, dataFromArg bool
	waitPID                int
}

func main() {
	var o options
	flag.StringVar(&o.addr, "addr", "127.0.0.1:7878", "UI/API listen address (loopback only)")
	flag.StringVar(&o.data, "data", defaultDataDir(), "directory for settings, relays and the log")
	flag.StringVar(&o.relays, "relays", "", "optional read-only relay list (JSON) to use alongside the relays added in the app")
	flag.BoolVar(&o.demo, "demo", false, "run against a simulated network instead of real relays")
	flag.BoolVar(&o.headless, "headless", false, "no window; stop with Ctrl+C")
	flag.BoolVar(&o.open, "open", false, "open the UI in the default browser instead of a window")
	flag.BoolVar(&o.noElevate, "no-elevate", false, "don't ask for administrator rights at start (route mode needs them)")
	flag.IntVar(&o.waitPID, "wait-pid", 0, "wait for this process to exit before starting (used when restarting elevated)")
	flag.Parse()
	flag.Visit(func(f *flag.Flag) { o.dataFromArg = o.dataFromArg || f.Name == "data" })

	if err := run(o); err != nil {
		slog.Error("fatal", "err", err)
		if !o.headless {
			showError("Lowpath", err.Error())
		}
		os.Exit(1)
	}
}

func run(o options) error {
	if err := checkLoopback(o.addr); err != nil {
		return err
	}
	logger, closeLog, err := openLog(o.data)
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
	if o.waitPID == 0 && !o.noElevate && !o.demo && elevationSupported && !capture.IsAdmin() {
		switch err := relaunchElevated(childArgs(o)); {
		case err == nil:
			logger.Info("restarting as administrator")
			ln.Close() // the elevated copy waits for this process to exit, then binds
			return nil
		case errors.Is(err, app.ErrElevateCancelled):
			logger.Warn("administrator prompt cancelled; running without admin rights (route mode needs them)")
		default:
			logger.Warn("could not restart as administrator", "err", err)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	srv, err := newServer(ctx, o, logger, cancel)
	if err != nil {
		ln.Close()
		return err
	}

	hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- hs.Serve(ln) }()
	url := "http://" + ln.Addr().String()
	logger.Info("Lowpath is running", "ui", url, "version", app.Version, "demo", o.demo, "admin", capture.IsAdmin(), "data", o.data)
	// The launch link (see app.Server.LaunchPath) goes straight to the
	// window or browser, never to the log file.
	launch := url + srv.LaunchPath()

	switch {
	case o.headless:
		fmt.Fprintf(os.Stderr, "\nOpen the UI (this link works once): %s\n\n", launch)
	case o.open:
		openBrowser(launch)
	default:
		// The window owns the main thread until it closes; closing it quits.
		if err := runWindow(ctx, launch, logger); err != nil {
			logger.Warn("no app window; opening the browser instead", "err", err)
			openBrowser(launch)
		} else {
			cancel()
		}
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

// newServer builds the API server: real relays by default, the simulated
// network with -demo.
func newServer(ctx context.Context, o options, logger *slog.Logger, quit func()) (*app.Server, error) {
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
	cfg := app.Config{Store: store, Games: games, Relays: relays, UI: dist, Log: logger, Quit: quit, ArtDir: filepath.Join(o.data, "art")}
	if elevationSupported {
		cfg.Elevate = func() error { return relaunchElevated(childArgs(o)) }
	}
	if o.demo {
		sim, err := demo.Start(ctx, demo.DefaultPaths(), demo.DirectProfile(), logger.With("component", "relay"))
		if err != nil {
			return nil, err
		}
		cfg.Network, cfg.Sim = app.DemoNetwork{N: sim}, sim
		logger.Info("demo network up", "game_server", sim.GameAddr(), "egress", sim.Egress.Addr(), "paths", len(sim.Paths))
	}
	srv, err := app.NewServer(cfg)
	if err != nil {
		relays.Close()
		return nil, err
	}
	srv.Run(ctx)
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
	return append(args, "-wait-pid", strconv.Itoa(os.Getpid()))
}

func defaultDataDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return filepath.Join(d, "Lowpath")
	}
	return ".lowpath"
}

// maxLogSize is when the log starts over (checked at start only).
const maxLogSize = 5 << 20

// openLog logs to <data>\lowpath.log and stderr, and points the standard
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
