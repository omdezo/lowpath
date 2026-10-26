package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/engine"
)

// The route trace (MTR) in Settings → Diagnostics. It runs in the
// background, at most one at a time, while the UI polls; the last finished
// trace is kept in the store.

var (
	errTraceRunning = errors.New("a route trace is already running")
	errTraceBloat   = errors.New("wait for the bufferbloat test to finish: it fills your connection, which would show up as delay at every hop")
	errBloatTrace   = errors.New("wait for the route trace to finish, or stop it: a full connection would make its numbers wrong")
)

// publicTraceTarget is offered when the user has no relay yet: an anycast
// address that answers ping and sits close to almost everyone.
var publicTraceTarget = traceTarget{ID: "cloudflare", Label: "Cloudflare DNS", Addr: "1.1.1.1"}

// traceTarget is a suggestion the UI lists.
type traceTarget struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Addr  string `json:"addr"`
	// UDP: a relay, which also answers UDP probes on its port.
	UDP  bool   `json:"udp"`
	Kind string `json:"kind"` // public | relay | server
}

// traceView is GET /api/diag/trace.
type traceView struct {
	Running  bool              `json:"running"`
	Progress *diag.TraceResult `json:"progress"` // the trace so far
	Result   *diag.TraceResult `json:"result"`   // the last finished trace, also after a restart
	Error    string            `json:"error,omitempty"`
	Targets  []traceTarget     `json:"targets"`
	Defaults traceDefaultsView `json:"defaults"`
}

type traceDefaultsView struct {
	Count     int     `json:"count"`
	IntervalS float64 `json:"interval_s"`
}

// traceRequest is POST /api/diag/trace: a suggested target or a host
// typed by the user, and the mode.
type traceRequest struct {
	TargetID string `json:"target_id"`
	Host     string `json:"host"`
	Mode     string `json:"mode"`
}

// traceRunner owns the background trace.
type traceRunner struct {
	ping     diag.TTLPingFunc
	names    diag.NameFunc
	net      diag.NetStats // the connection the trace runs on; nil: unknown
	store    *Store
	log      *slog.Logger
	count    int           // probes per hop (0: the default 200)
	interval time.Duration // between probes to one hop (0: 1 s)

	mu       sync.Mutex
	running  bool
	progress *diag.TraceResult
	err      string
	cancel   context.CancelFunc
	finished chan struct{}
}

func (t *traceRunner) view(targets []traceTarget) traceView {
	d := diag.NewTrace(t.tune(diag.TraceConfig{}))
	cfg := d.Config()
	t.mu.Lock()
	v := traceView{Running: t.running, Progress: t.progress, Error: t.err, Targets: targets,
		Defaults: traceDefaultsView{Count: cfg.Count, IntervalS: cfg.Interval.Seconds()}}
	t.mu.Unlock()
	v.Result = t.store.Trace()
	return v
}

func (t *traceRunner) tune(cfg diag.TraceConfig) diag.TraceConfig {
	if t.count > 0 {
		cfg.Count = t.count
	}
	if t.interval > 0 {
		cfg.Interval = t.interval
	}
	return cfg
}

func (t *traceRunner) isRunning() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// start begins a trace of cfg in the background.
func (t *traceRunner) start(parent context.Context, cfg diag.TraceConfig) error {
	cfg.Ping, cfg.Names = t.ping, t.names
	if t.net != nil {
		// The verdict ignores a trace taken on another connection.
		if r, err := t.net.DefaultRoute(); err == nil {
			cfg.Route = r
		}
	}
	tr := diag.NewTrace(t.tune(cfg))
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.running {
		return errTraceRunning
	}
	ctx, cancel := context.WithCancel(parent)
	t.running, t.err, t.cancel = true, "", cancel
	t.progress = &diag.TraceResult{Running: true, Target: cfg.Target.String(), Host: cfg.Host, Mode: cfg.Mode, StartedAt: time.Now(), Hops: []diag.TraceHop{}}
	t.finished = make(chan struct{})
	go t.run(ctx, cancel, tr, t.finished)
	return nil
}

// stop cancels a running trace and waits for it to end; what it measured
// so far is kept.
func (t *traceRunner) stop() {
	t.mu.Lock()
	cancel, fin := t.cancel, t.finished
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		<-fin
	}
}

func (t *traceRunner) run(ctx context.Context, cancel context.CancelFunc, tr *diag.Trace, finished chan struct{}) {
	defer close(finished)
	// A trace that ends on its own releases its context too, else each one
	// stays registered on the server's lifetime until Lowpath exits.
	defer cancel()
	defer func() {
		t.mu.Lock()
		t.running, t.progress, t.cancel = false, nil, nil
		t.mu.Unlock()
	}()
	res, err := tr.Run(ctx, func(p diag.TraceResult) {
		t.mu.Lock()
		t.progress = &p
		t.mu.Unlock()
	})
	if err != nil {
		t.mu.Lock()
		t.err = err.Error()
		t.mu.Unlock()
		t.log.Warn("route trace", "err", err)
		return
	}
	lossFrom := "none"
	if res.LossFrom != nil {
		lossFrom = fmt.Sprint(*res.LossFrom)
	}
	t.log.Info("route trace", "mode", res.Mode, "hops", len(res.Hops), "rounds", res.Rounds, "reached", res.Reached,
		"dest_loss", fmtPct(res.DestLossPct), "loss_from", lossFrom, "stopped", res.Stopped)
	if err := t.store.SetTrace(res); err != nil {
		t.log.Warn("save route trace", "err", err)
	}
}

func fmtPct(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f%%", *v)
}

// reverseName is the host name of a hop, "" when it has none.
func reverseName(ctx context.Context, a netip.Addr) string {
	names, err := net.DefaultResolver.LookupAddr(ctx, a.String())
	if err != nil || len(names) == 0 {
		return ""
	}
	return strings.TrimSuffix(names[0], ".")
}

// traceTargets suggests what to trace: the user's relays (the only targets
// with a UDP mode), the servers the games last connected to, and a public
// address.
func (s *Server) traceTargets() []traceTarget {
	out := []traceTarget{}
	for _, p := range s.net.Paths("") {
		out = append(out, traceTarget{ID: "relay:" + p.ID, Label: p.Label, Addr: p.Addr.Addr().Unmap().String(), UDP: true, Kind: "relay"})
	}
	for _, g := range s.allGames() {
		if sv := s.store.Servers(g.ID); len(sv) > 0 {
			out = append(out, traceTarget{ID: "server:" + g.ID, Label: g.Name + " server", Addr: sv[0].Addr().Unmap().String(), Kind: "server"})
		}
	}
	return append(out, publicTraceTarget)
}

// traceConfig turns a request into what to trace.
func (s *Server) traceConfig(ctx context.Context, req traceRequest) (diag.TraceConfig, error) {
	mode := req.Mode
	if mode == "" {
		mode = diag.TraceICMP
	}
	if mode != diag.TraceICMP && mode != diag.TraceUDP {
		return diag.TraceConfig{}, fmt.Errorf("unknown trace mode %q", req.Mode)
	}
	cfg := diag.TraceConfig{Mode: mode}
	switch {
	case req.TargetID != "":
		var spec *engine.PathSpec
		for _, p := range s.net.Paths("") {
			if "relay:"+p.ID == req.TargetID {
				spec = &p
				break
			}
		}
		for _, t := range s.traceTargets() {
			if t.ID != req.TargetID {
				continue
			}
			cfg.Target, cfg.Host = netip.MustParseAddr(t.Addr), t.Label
		}
		if !cfg.Target.IsValid() {
			return cfg, errors.New("that target is gone; pick another one")
		}
		if spec != nil && mode == diag.TraceUDP {
			cfg.End = newRelayProber(*spec)
		}
	case strings.TrimSpace(req.Host) != "":
		host := strings.TrimSpace(req.Host)
		a, err := resolveTraceHost(ctx, host)
		if err != nil {
			return cfg, err
		}
		cfg.Target, cfg.Host = a, host
	default:
		return cfg, errors.New("pick a target or type a host name or IP address")
	}
	if mode == diag.TraceUDP && cfg.End == nil {
		return cfg, errors.New("UDP mode needs one of your relays as the target: only a relay answers UDP probes on its port")
	}
	return cfg, nil
}

// resolveTraceHost parses or looks up host (2 s at most) and checks that
// it can be traced. A name with both kinds of address is traced over IPv4,
// the family most game servers use.
func resolveTraceHost(ctx context.Context, host string) (netip.Addr, error) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap(), checkTraceAddr(a.Unmap())
	}
	if len(host) > 253 || strings.ContainsAny(host, " /\\:@") {
		return netip.Addr{}, fmt.Errorf("%q is not a host name or IP address", host)
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(addrs) == 0 {
		return netip.Addr{}, fmt.Errorf("couldn't find %s: check the name, or that you are online", host)
	}
	pick := firstPerFamily(addrs, 0)[0].Addr()
	return pick, checkTraceAddr(pick)
}

func checkTraceAddr(a netip.Addr) error {
	switch {
	case a.IsUnspecified(), a.IsMulticast(), a == netip.AddrFrom4([4]byte{255, 255, 255, 255}):
		return fmt.Errorf("%s can't be traced: it isn't a single host", a)
	}
	return nil
}

// lastTrace is the last trace for the diagnostics report, with the host
// part of every address removed; nil when none ran.
func (s *Server) lastTrace() *diag.TraceResult {
	res := s.store.Trace()
	if res == nil {
		return nil
	}
	red := res.Redacted()
	return &red
}

// --- HTTP ---------------------------------------------------------------

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.trace.view(s.traceTargets()))
}

func (s *Server) handleStartTrace(w http.ResponseWriter, r *http.Request) {
	var req traceRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	cfg, err := s.traceConfig(r.Context(), req)
	if err != nil {
		writeErr(w, 400, "bad_target", err)
		return
	}
	// s.life orders this against the bufferbloat test starting.
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	switch {
	case closed:
		writeErr(w, 503, "shutting_down", errors.New("Lowpath is shutting down"))
		return
	case s.bloat.isRunning():
		writeErr(w, 409, "bloat_running", errTraceBloat)
		return
	}
	if err := s.trace.start(s.lifetime, cfg); err != nil {
		writeErr(w, 409, "trace_running", err)
		return
	}
	writeJSON(w, 202, s.trace.view(s.traceTargets()))
}

func (s *Server) handleStopTrace(w http.ResponseWriter, r *http.Request) {
	s.trace.stop()
	writeJSON(w, 200, s.trace.view(s.traceTargets()))
}

// handleTraceReport is the last finished trace as a text table, for a
// support ticket. It has the full addresses: the user exports it on
// purpose, and an ISP needs them.
func (s *Server) handleTraceReport(w http.ResponseWriter, r *http.Request) {
	res := s.store.Trace()
	if res == nil {
		writeErr(w, 404, "no_trace", errors.New("no route trace yet"))
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="lowpath-route-trace.txt"`)
	w.Write([]byte(res.Report()))
}
