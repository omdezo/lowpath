package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/demo"
	"lowpath/internal/engine"
)

// recordEvery is how often route mode saves the game's primary server, which
// the next Analyze pings for its "before" number.
const recordEvery = 5 * time.Second

type session struct {
	GameID      string    `json:"game_id"`
	Mode        string    `json:"mode"` // smart | region | specific
	Region      string    `json:"region,omitempty"`
	RouteID     string    `json:"route_id,omitempty"`
	Started     time.Time `json:"started_at"`
	Listen      string    `json:"listen"`       // proxy mode: where the game sends; "" in route mode
	CaptureMode string    `json:"capture_mode"` // route | proxy

	sawGame bool
	eng     *engine.Engine
	capt    capture.Session    // route mode only
	cancel  context.CancelFunc // demo traffic, server recorder
	wifi    *wifiTuner         // the Wi-Fi scan pause; nil: not on Wi-Fi or turned off
}

type captureView struct {
	Mode string `json:"mode"` // always "route"
	capture.Stats
}

type applyReq struct {
	GameID  string `json:"game_id"`
	Mode    string `json:"mode"`
	Region  string `json:"region"`
	RouteID string `json:"route_id"`
	// IgnoreConflicts starts route mode even next to another booster or
	// packet tool (the UI's "Boost anyway", for a program Lowpath names
	// wrongly or one the player knows is idle).
	IgnoreConflicts bool `json:"ignore_conflicts"`
}

// apiError carries the HTTP status and error kind a failed apply reports.
type apiError struct {
	code      int
	kind      string
	err       error
	conflicts []compat.Conflict // what is in the way (kind "conflict", or a capture failure they may explain)
}

func (e *apiError) Error() string { return e.err.Error() }
func (e *apiError) Unwrap() error { return e.err }

func failf(code int, kind, format string, a ...any) *apiError {
	return &apiError{code: code, kind: kind, err: fmt.Errorf(format, a...)}
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req applyReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	if err := s.apply(req); err != nil {
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.conflicts != nil:
			writeJSON(w, ae.code, map[string]any{"error": ae.kind, "message": ae.err.Error(), "conflicts": ae.conflicts})
		case errors.As(err, &ae):
			writeErr(w, ae.code, ae.kind, ae.err)
		default:
			writeErr(w, 500, "apply_failed", err)
		}
		return
	}
	writeJSON(w, 200, s.live())
}

func (s *Server) apply(req applyReq) error {
	g, ok := s.game(req.GameID)
	if !ok {
		return failf(404, "not_found", "unknown game")
	}
	paths, err := pickPaths(s.net.Paths(req.GameID), &req)
	if err != nil {
		return err
	}

	mode := s.store.Settings().CaptureMode
	if s.net.Demo() {
		mode = CaptureProxy // the simulated game client sends to a local port
	}
	var target, direct netip.AddrPort
	switch mode {
	case CaptureRoute:
		// Before the rights check: restarting as administrator wouldn't
		// help while another program captures the same packets.
		// Only a scan that finished now refuses: a stale report (the check
		// hangs) may name a program that has closed since, so it fails open.
		if !req.IgnoreConflicts {
			rep, fresh := s.conflicts.check()
			if block := rep.Blocking(); len(block) > 0 {
				if !fresh {
					s.log.Warn("conflict check didn't finish; boosting without it", "last_found", compat.Names(block))
				} else {
					s.log.Warn("boost refused: another program captures game traffic", "conflicts", compat.Names(block))
					return conflictErr(block)
				}
			}
		}
		if err := s.plat.Capture.Available(); err != nil {
			return captureErr(err)
		}
		if len(g.Processes) == 0 {
			return failf(400, "no_processes", "%s has no process names to capture", g.Name)
		}
	default:
		if target, direct, ok = s.net.Endpoint(req.GameID); !ok {
			return failf(409, "no_endpoint", "proxy mode needs the game's server address: run the game once in route mode, or list it in a -relays file")
		}
	}

	s.life.Lock()
	defer s.life.Unlock()
	// The bufferbloat test fills the line on purpose; a game must not
	// start under it. Stopped under s.life, which handleStartBloat also
	// holds, so no test can start between this and the session going live.
	s.bloat.stop("A boost started, so the test was stopped.")
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return failf(503, "shutting_down", "Lowpath is shutting down")
	}
	s.mu.Unlock()
	s.stopSessionLocked() // applying a new game replaces the current one
	s.mu.Lock()
	s.status = "applying"
	var seed map[string]engine.Stats
	if a, ok := s.analysis[req.GameID]; ok && time.Since(a.At) < time.Minute {
		seed = a.Paths
	}
	s.mu.Unlock()

	st := s.store.Settings()
	cfg := engine.Config{
		Paths: paths, DupLevel: st.DupLevel, AutoSwitch: st.AutoSwitch, KillSwitch: st.KillSwitch, Seed: seed,
	}
	if req.Mode == "specific" {
		cfg.Locked = req.RouteID
	}
	sess := &session{GameID: req.GameID, Mode: req.Mode, Region: req.Region, RouteID: req.RouteID, Started: time.Now(), CaptureMode: mode}
	if mode == CaptureRoute {
		err = s.startRoute(sess, g, cfg)
	} else {
		err = s.startProxy(sess, cfg, target, direct)
	}
	if err != nil {
		s.mu.Lock()
		s.status = "idle"
		s.mu.Unlock()
		return err
	}
	// The demo's network is simulated: its boosts leave the PC's Wi-Fi alone.
	sess.wifi = s.wifi.startTuning(st.WiFiTuning && !s.net.Demo())
	s.mu.Lock()
	s.sess = sess
	s.status = "optimizing"
	s.failsafe = nil // a new boost: the old notice is history
	s.mu.Unlock()
	s.store.TouchRecent(req.GameID)
	s.log.Info("optimizing", "game", req.GameID, "mode", req.Mode, "capture", mode, "paths", len(paths), "listen", sess.Listen)
	return nil
}

// pickPaths narrows the relays to what the route mode asks for: all of them
// (smart), one continent (region) or a single relay (specific).
func pickPaths(paths []engine.PathSpec, req *applyReq) ([]engine.PathSpec, error) {
	if len(paths) == 0 {
		return nil, &apiError{code: 409, kind: "no_relays", err: errNoRelays}
	}
	switch req.Mode {
	case "smart", "":
		req.Mode = "smart"
	case "region":
		paths = slices.DeleteFunc(slices.Clone(paths), func(p engine.PathSpec) bool { return p.Continent != req.Region })
	case "specific":
		if !slices.ContainsFunc(paths, func(p engine.PathSpec) bool { return p.ID == req.RouteID }) {
			return nil, failf(400, "bad_route", "unknown route")
		}
	default:
		return nil, failf(400, "bad_mode", "unknown mode %q", req.Mode)
	}
	if len(paths) == 0 {
		return nil, failf(400, "no_routes", "no routes in that region")
	}
	return paths, nil
}

// captureErr turns "route mode can't run here" into the error kinds the UI
// knows: needs_admin (it offers a restart as administrator) or capture_failed.
func captureErr(err error) *apiError {
	var rights *capture.RightsError
	if errors.As(err, &rights) {
		return &apiError{code: 409, kind: "needs_admin", err: fmt.Errorf("route mode can't capture game traffic: %s. To fix it, %s; or switch to proxy mode", rights.Reason, rights.Fix)}
	}
	if errors.Is(err, capture.ErrNeedAdmin) {
		return &apiError{code: 409, kind: "needs_admin", err: errors.New("route mode needs administrator rights: restart Lowpath as administrator, or switch to proxy mode")}
	}
	if errors.Is(err, errors.ErrUnsupported) {
		// Not a failure: this system has no capture backend (yet).
		return &apiError{code: 501, kind: "unsupported", err: fmt.Errorf("route mode can't capture game traffic here (%s): switch to proxy mode", captureReason(err))}
	}
	return &apiError{code: 500, kind: "capture_failed", err: err}
}

// capHolder lets the engine's Deliver callback see the capture through an
// atomic pointer: the engine starts first, and an interface can't be stored
// atomically by itself.
type capHolder struct{ c capture.Session }

// startRoute runs the engine in capture mode and points WinDivert at the
// game's processes: outbound game packets go to engine.Send, the relays'
// replies come back through Deliver and are injected into the game's socket.
func (s *Server) startRoute(sess *session, g Game, cfg engine.Config) error {
	var holder atomic.Pointer[capHolder]
	cfg.Deliver = func(local, remote netip.AddrPort, payload []byte) {
		if h := holder.Load(); h != nil {
			h.c.Inject(local, remote, payload) // failures are counted in the capture stats
		}
	}
	eng, err := engine.Start(cfg)
	if err != nil {
		return fmt.Errorf("start engine: %w", err)
	}
	// Marked before capture starts: a crash right after Start must still be
	// found at the next launch.
	s.markSession(sess.GameID)
	c, err := s.plat.Capture.Start(capture.Config{
		Processes: g.Processes,
		// Relay endpoints must never be captured: a tunnel packet that got
		// tunneled again would loop.
		Exclude:  s.relays.Endpoints(),
		OnPacket: eng.Send,
		Log:      s.log.With("component", "capture"),
	})
	if err != nil {
		s.unmarkSession()
		eng.Stop()
		return s.withConflictHint(captureErr(err))
	}
	holder.Store(&capHolder{c})
	sess.eng, sess.capt = eng, c

	ctx, cancel := context.WithCancel(context.Background())
	sess.cancel = cancel
	go s.watchCapture(ctx, sess, c)
	go func() {
		t := time.NewTicker(recordEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.recordPrimary(sess)
			}
		}
	}()
	return nil
}

// startProxy runs the engine with a local UDP port the game sends to. Real
// games get the server's own port when it is free (so only the IP has to be
// changed in the game); the demo uses any free port.
func (s *Server) startProxy(sess *session, cfg engine.Config, target, direct netip.AddrPort) error {
	cfg.Target, cfg.Direct = target, direct
	// A real server doesn't echo the demo's UDP baseline probes; ping it.
	cfg.PingBefore = !s.net.Demo()
	cfg.Listen = "127.0.0.1:0"
	if !s.net.Demo() && target.Port() != 0 {
		cfg.Listen = fmt.Sprintf("127.0.0.1:%d", target.Port())
	}
	eng, err := engine.Start(cfg)
	if err != nil && cfg.Listen != "127.0.0.1:0" {
		s.log.Warn("proxy: server port unavailable, using a free port", "port", target.Port(), "err", err)
		cfg.Listen = "127.0.0.1:0"
		eng, err = engine.Start(cfg)
	}
	if err != nil {
		return err
	}
	sess.eng = eng
	sess.Listen = eng.ListenAddr().String()
	if s.sim != nil {
		ctx, cancel := context.WithCancel(context.Background())
		sess.cancel = cancel
		go demo.Traffic(ctx, eng.ListenAddr(), 64)
	}
	return nil
}

// recordPrimary saves the server the game talks to most, for next time.
func (s *Server) recordPrimary(sess *session) {
	for _, sv := range sess.eng.Snapshot().Servers {
		if sv.Primary {
			if err := s.store.RecordServer(sess.GameID, sv.Addr); err != nil {
				s.log.Warn("record game server", "err", err)
			}
			return
		}
	}
}

func (s *Server) stopSession() {
	s.life.Lock()
	defer s.life.Unlock()
	s.stopSessionLocked()
}

// stopSessionLocked tears the session down with s.life held: capture first,
// so the game's packets go out directly again before the engine (and with it
// the tunnel) disappears.
func (s *Server) stopSessionLocked() {
	s.mu.Lock()
	sess := s.sess
	s.sess = nil
	if s.status == "optimizing" || s.status == "applying" {
		s.status = "idle"
	}
	s.mu.Unlock()
	if sess == nil {
		return
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	if sess.capt != nil {
		if err := sess.capt.Close(); err != nil {
			s.log.Warn("close capture", "err", err)
		}
		s.unmarkSession() // the game's traffic is direct again
	}
	s.stopEngine(sess)
	sess.wifi.stop()
	s.log.Info("stopped", "game", sess.GameID)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	s.stopSession()
	writeJSON(w, 200, s.live())
}
