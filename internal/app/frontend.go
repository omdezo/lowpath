package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"

	"lowpath/internal/platform"
)

// Frontend is the window's side of the split with the background service
// (LowpathSvc): it serves the UI on loopback HTTP behind the same
// per-launch session as Server, and passes every other /api request on to
// the service over the service's pipe. The window process needs no
// administrator rights; the service holds them.
//
// The session secret stays here: the pipe doesn't need it (its access
// list only admits users signed in at this PC), so the cookie is removed
// before a request goes on.
type Frontend struct {
	auth      *auth
	ui        fs.FS
	log       *slog.Logger
	transport *http.Transport
	proxy     *httputil.ReverseProxy
}

// FrontendConfig wires a Frontend.
type FrontendConfig struct {
	// Dial connects to the service (platform.ServiceAPI.Dial). Required.
	Dial func(ctx context.Context) (net.Conn, error)
	UI   fs.FS // nil: API only
	Log  *slog.Logger
	// Session is the cookie secret; empty: a random one (tests set their
	// own).
	Session string
}

// serviceHost is the Host the service sees on requests from the window. It
// is never resolved: every connection goes through Dial.
const serviceHost = "lowpath-service"

// dialTimeout bounds connecting to the service, so a hung service turns
// into "not answering" instead of a request that never returns.
const dialTimeout = 3 * time.Second

func NewFrontend(cfg FrontendConfig) (*Frontend, error) {
	if cfg.Dial == nil {
		return nil, errors.New("frontend: no way to reach the service")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	f := &Frontend{auth: newAuth(cfg.Session), ui: cfg.UI, log: cfg.Log}
	f.transport = &http.Transport{
		Proxy: nil, // never an HTTP proxy from the environment
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			return cfg.Dial(ctx)
		},
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     30 * time.Second,
		DisableCompression:  true,
	}
	f.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: serviceHost})
			pr.Out.Header.Del("Cookie")
		},
		Transport: f.transport,
		// Live is a Server-Sent Events stream: pass each event on at once.
		FlushInterval: -1,
		ErrorHandler:  f.serviceError,
		ErrorLog:      slog.NewLogLogger(cfg.Log.Handler(), slog.LevelDebug),
	}
	return f, nil
}

// Handler serves the UI and the API to the window.
func (f *Frontend) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+pathSession, f.auth.handleSession)
	// Answered here, so a second launch finds this window even while the
	// service restarts.
	mux.HandleFunc("GET "+pathVersion, handleVersion)
	mux.Handle("/api/", f.proxy)
	if f.ui != nil {
		mux.Handle("/", spa(f.ui))
	}
	return f.auth.guard(mux)
}

// LaunchPath is the window's first URL (see Server.LaunchPath).
func (f *Frontend) LaunchPath() string { return f.auth.launchPath() }

// NewLaunchPath is a launch path for one more window when the app is
// started again while it runs (see Server.NewLaunchPath). The session lives
// here, not in the service, so a second start signs in even while the
// service restarts.
func (f *Frontend) NewLaunchPath() string { return f.auth.newLaunchPath() }

// Check asks the service for its version: whether it answers at all, and
// whether it is the same build as this window.
func (f *Frontend) Check(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+serviceHost+pathVersion, nil)
	if err != nil {
		return "", err
	}
	res, err := f.transport.RoundTrip(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var v struct {
		Version string `json:"version"`
	}
	if res.StatusCode == http.StatusForbidden {
		// Only the socket's peer check answers 403: the API itself has no
		// guard behind the pipe or socket.
		return "", fmt.Errorf("%w (the service answered %s)", platform.ErrPeerRefused, res.Status)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the service answered %s", res.Status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<16)).Decode(&v); err != nil || v.Version == "" {
		return "", fmt.Errorf("the service's answer is not Lowpath's: %v", err)
	}
	return v.Version, nil
}

// StopSession asks the service to stop the running boost, as closing the
// window does when the engine runs in it: a boost nobody can see or stop
// any more shouldn't carry on in the background.
func (f *Frontend) StopSession(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+serviceHost+"/api/stop", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Lowpath", "1")
	res, err := f.transport.RoundTrip(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("the service answered %s", res.Status)
	}
	return nil
}

// Close drops the idle connections to the service.
func (f *Frontend) Close() { f.transport.CloseIdleConnections() }

// serviceError answers a request the service couldn't: 503
// service_unavailable, which the UI shows as "Lost connection to the Lowpath
// service. Reconnecting…" until the service manager has restarted it.
func (f *Frontend) serviceError(w http.ResponseWriter, r *http.Request, err error) {
	if r.Context().Err() != nil {
		return // the window went away first
	}
	f.log.Warn("service not answering", "path", r.URL.Path, "err", err)
	msg := "the Lowpath service isn't answering. If it stopped, your game went back to its normal connection, and the installed service restarts by itself within a few seconds"
	if errors.Is(err, platform.ErrNotService) {
		msg = "the program answering on Lowpath's service pipe is not the Lowpath service, so Lowpath won't talk to it"
	}
	writeErr(w, http.StatusServiceUnavailable, "service_unavailable", errors.New(msg))
}
