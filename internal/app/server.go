// Package app is the local service behind the UI: it owns the game catalog,
// the local settings store, the relay list, route analysis, and the
// running optimization session (engine + packet capture), and exposes them
// over a localhost-only HTTP API.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"lowpath/internal/catalog"
	"lowpath/internal/demo"
	"lowpath/internal/detect"
	"lowpath/internal/engine"
	"lowpath/internal/netsim"
)

const Version = "0.2.0"

// Config wires a Server. Store and Games are required; everything else has a
// working default.
type Config struct {
	Store *Store
	Games []Game // built-in catalog; custom games live in the store

	// Relays is the user's relay list (plus imported and loopback relays).
	// Nil: one is made from Store with no import.
	Relays *RelayNetwork
	// Network decides the paths and server for a session. Nil: Relays (real
	// mode). Demo mode passes DemoNetwork and Sim.
	Network Network
	Sim     *demo.Network

	// Servers answers "which servers does this game have, and how far are
	// they" before the game connects (Valve's SDR POPs). Nil: no server
	// lists; the game page leaves the card out.
	Servers ServerLists

	Capture Capturer // nil: WinDivert
	UI      fs.FS    // nil: API only
	Log     *slog.Logger
	ArtDir  string // where downloaded and extracted game art is kept; "" = Steam's own cache only

	// Elevate relaunches the app as administrator (it returns once the new
	// process is starting, or ErrElevateCancelled); Quit then asks this
	// process to exit so the elevated copy can take over the port. Nil
	// Elevate: not supported on this platform.
	Elevate func() error
	Quit    func()

	// Session is the secret every /api request must carry in the session
	// cookie (see LaunchPath). Empty: a random one, which is what the app
	// wants; tests set their own.
	Session string
}

// ServerLists is the catalog of known game servers (catalog.ValveServers).
// List and Ping answer at once from memory and refresh in the background.
type ServerLists interface {
	List(appID int) catalog.ServerList
	Ping(appID int) catalog.ServerList
	Close()
}

type Server struct {
	store   *Store
	games   []Game
	net     Network
	relays  *RelayNetwork
	servers ServerLists   // nil: no server lists
	sim     *demo.Network // nil unless demo mode
	capt    Capturer
	log     *slog.Logger
	ui      fs.FS
	elevate func() error
	quit    func()
	auth    *auth
	art     *artStore

	// life serializes starting and stopping sessions, which take long enough
	// (driver load, engine start) that s.mu must not be held across them.
	life sync.Mutex

	mu        sync.Mutex
	status    string // idle | analyzing | applying | optimizing
	analysis  map[string]analysisResult
	sess      *session
	installed map[string]bool
	running   map[string]bool
	mapping   *mappingResult
	closed    bool
	done      chan struct{} // closed by Close; ends the live streams
}

type analysisResult struct {
	At          time.Time               `json:"at"`
	Direct      engine.Stats            `json:"direct"`
	DirectKnown bool                    `json:"direct_known"`
	Server      string                  `json:"server,omitempty"` // what the baseline pinged (route mode's last server)
	Paths       map[string]engine.Stats `json:"paths"`
}

type mappingResult struct {
	At     time.Time `json:"at"`
	Relays int       `json:"relays"`
	Best   string    `json:"best"`
	BestMS float64   `json:"best_ms"`
}

func NewServer(cfg Config) (*Server, error) {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Relays == nil {
		r, err := NewRelayNetwork(cfg.Store, nil, cfg.Log)
		if err != nil {
			return nil, err
		}
		cfg.Relays = r
	}
	if cfg.Network == nil {
		cfg.Network = cfg.Relays
	}
	if cfg.Capture == nil {
		cfg.Capture = &WinDivert{}
	}
	if cfg.Quit == nil {
		cfg.Quit = func() {}
	}
	return &Server{
		store: cfg.Store, games: cfg.Games, net: cfg.Network, relays: cfg.Relays, servers: cfg.Servers, sim: cfg.Sim,
		capt: cfg.Capture, log: cfg.Log, ui: cfg.UI, elevate: cfg.Elevate, quit: cfg.Quit,
		auth:   newAuth(cfg.Session),
		art:    newArtStore(cfg.ArtDir, cfg.Log),
		status: "idle", analysis: map[string]analysisResult{},
		installed: map[string]bool{}, running: map[string]bool{},
		done: make(chan struct{}),
	}, nil
}

// Run starts background detection; call once. The session is stopped when
// ctx ends, but callers should still call Close for an orderly shutdown.
func (s *Server) Run(ctx context.Context) {
	// Fetch the library's artwork in the background so covers are ready
	// the first time the home screen opens.
	go s.art.warm(ctx, s.allGames())
	go func() {
		libs := detect.SteamLibraries()
		inst := map[string]bool{}
		for _, g := range s.games {
			if detect.Installed(g.Paths, g.Steam, libs) {
				inst[g.ID] = true
			}
		}
		s.mu.Lock()
		s.installed = inst
		s.mu.Unlock()
	}()
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			s.scanRunning()
			select {
			case <-ctx.Done():
				s.stopSession()
				return
			case <-t.C:
			}
		}
	}()
}

// Close stops the session (capture first, so the game's traffic goes direct
// again), shuts the loopback relay and ends live streams so an HTTP shutdown
// doesn't wait on them. No new session can start afterwards.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	close(s.done)
	s.mu.Unlock()
	s.stopSession()
	s.relays.Close()
	if s.servers != nil {
		s.servers.Close()
	}
}

// allGames is the catalog followed by the user's custom games.
func (s *Server) allGames() []Game {
	return append(slices.Clone(s.games), s.store.CustomGames()...)
}

func (s *Server) game(id string) (Game, bool) {
	for _, g := range s.allGames() {
		if g.ID == id {
			return g, true
		}
	}
	return Game{}, false
}

func (s *Server) scanRunning() {
	procs := detect.Running()
	run := map[string]bool{}
	for _, g := range s.allGames() {
		for _, p := range g.Processes {
			if procs[strings.ToLower(p)] {
				run[g.ID] = true
			}
		}
	}
	s.mu.Lock()
	s.running = run
	sess := s.sess
	stop := false
	if sess != nil && !s.net.Demo() {
		if run[sess.GameID] {
			sess.sawGame = true
		} else if sess.sawGame && s.store.Settings().StopOnGameExit {
			stop = true
		}
	}
	s.mu.Unlock()
	if stop {
		s.log.Info("game exited, stopping optimization", "game", sess.GameID)
		s.stopSession()
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/mapping", s.handleMapping)
	mux.HandleFunc("GET /api/games/{id}/routes", s.handleRoutes)
	mux.HandleFunc("POST /api/games/{id}/analyze", s.handleAnalyze)
	mux.HandleFunc("POST /api/games/{id}/favorite", s.handleFavorite)
	mux.HandleFunc("GET /api/games/{id}/servers", s.handleServers)
	mux.HandleFunc("POST /api/games/{id}/servers/ping", s.handleServers)
	mux.HandleFunc("POST /api/games/custom", s.handleAddCustomGame)
	mux.HandleFunc("DELETE /api/games/custom/{id}", s.handleDeleteCustomGame)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("PUT /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/relays", s.handleRelays)
	mux.HandleFunc("POST /api/relays", s.handleAddRelay)
	mux.HandleFunc("POST /api/relays/test", s.handleTestRelay)
	mux.HandleFunc("DELETE /api/relays/{id}", s.handleDeleteRelay)
	mux.HandleFunc("GET /api/processes", s.handleProcesses)
	mux.HandleFunc("GET /api/art/{id}/{kind}", s.handleArt)
	mux.HandleFunc("POST /api/elevate", s.handleElevate)
	mux.HandleFunc("GET /api/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("POST /api/demo/impair", s.handleImpair)
	mux.HandleFunc("GET "+pathSession, s.handleSession)
	mux.HandleFunc("GET "+pathVersion, s.handleVersion)
	if s.ui != nil {
		mux.Handle("/", spa(s.ui))
	}
	return s.guard(mux)
}

// guard keeps the API to the app's own window: requests must target a
// loopback Host (blocks DNS rebinding), mutating calls must carry a custom
// header, which a cross-origin page cannot send without a CORS preflight we
// never answer, and /api needs the session cookie (see auth.go). The UI's
// static files are served to anyone; they hold nothing secret.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if r.Method != http.MethodGet && r.Header.Get("X-Lowpath") != "1" {
				http.Error(w, "missing X-Lowpath header", http.StatusForbidden)
				return
			}
			if r.URL.Path != pathSession && r.URL.Path != pathVersion && !s.auth.ok(r) {
				writeErr(w, http.StatusUnauthorized, "unauthorized",
					errors.New("this page is not signed in to Lowpath: use the Lowpath window, or close Lowpath and start it again"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// spa serves the built UI and falls back to index.html for client routes.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if _, err := fs.Stat(ui, p); err == nil {
				files.ServeHTTP(w, r)
				return
			}
		}
		http.ServeFileFS(w, r, ui, "index.html")
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, kind string, err error) {
	writeJSON(w, code, map[string]string{"error": kind, "message": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<16)
	return json.NewDecoder(r.Body).Decode(v)
}

type gameView struct {
	Game
	Favorite  bool `json:"favorite"`
	Installed bool `json:"installed"`
	Running   bool `json:"running"`
	Routable  bool `json:"routable"`
}

func (s *Server) gameViews(games ...Game) []gameView {
	if len(games) == 0 {
		games = s.allGames()
	}
	fav := s.store.Favorites()
	// Every relay can carry every game, so one relay makes them all routable.
	routable := len(s.net.Paths("")) > 0
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]gameView, 0, len(games))
	for _, g := range games {
		out = append(out, gameView{Game: g, Favorite: slices.Contains(fav, g.ID), Installed: s.installed[g.ID], Running: s.running[g.ID], Routable: routable})
	}
	return out
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	mapping := s.mapping
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"version":  Version,
		"demo":     s.net.Demo(),
		"platform": s.platform(),
		"relays":   s.relays.Views(),
		"settings": s.store.Settings(),
		"games":    s.gameViews(),
		"recent":   s.store.Recent(),
		"mapping":  mapping,
		"live":     s.live(),
	})
}

// --- routes & analysis ---------------------------------------------------

type routeView struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Region    string `json:"region"`
	Continent string `json:"continent"`
}

var errNoRelays = errors.New("no relays yet: add one in Settings → Relays, or turn on the loopback test relay")

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.game(id); !ok {
		writeErr(w, 404, "not_found", errors.New("unknown game"))
		return
	}
	routes := []routeView{}
	for _, p := range s.net.Paths(id) {
		routes = append(routes, routeView{p.ID, p.Label, p.Region, p.Continent})
	}
	s.mu.Lock()
	a, ok := s.analysis[id]
	s.mu.Unlock()
	resp := map[string]any{"routes": routes}
	if ok {
		resp["analysis"] = a
	}
	writeJSON(w, 200, resp)
}

// baseline says what Analyze measures the direct route against: the
// simulator's UDP echo in demo mode, otherwise an ICMP ping to the game's
// last known server (none yet: the analysis shows relay RTTs only).
func (s *Server) baseline(gameID string) engine.AnalyzeOptions {
	target, direct, ok := s.net.Endpoint(gameID)
	switch {
	case !ok:
		return engine.AnalyzeOptions{}
	case s.net.Demo():
		return engine.AnalyzeOptions{Direct: direct}
	default:
		return engine.AnalyzeOptions{Server: target.Addr()}
	}
}

func (s *Server) handleAnalyze(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.game(id); !ok {
		writeErr(w, 404, "not_found", errors.New("unknown game"))
		return
	}
	paths := s.net.Paths(id)
	if len(paths) == 0 {
		writeErr(w, 409, "no_relays", errNoRelays)
		return
	}
	opts := s.baseline(id)
	s.mu.Lock()
	if s.status == "idle" {
		s.status = "analyzing"
	}
	s.mu.Unlock()
	res, err := engine.Analyze(r.Context(), paths, opts, 2500*time.Millisecond)
	s.mu.Lock()
	if s.status == "analyzing" {
		s.status = "idle"
	}
	if err == nil {
		a := analysisResult{At: time.Now(), Direct: res.Direct, DirectKnown: res.DirectKnown, Paths: res.Paths}
		if opts.Server.IsValid() {
			a.Server = opts.Server.String()
		}
		s.analysis[id] = a
	}
	out := s.analysis[id]
	s.mu.Unlock()
	if err != nil {
		writeErr(w, 500, "analyze_failed", err)
		return
	}
	s.store.TouchRecent(id)
	writeJSON(w, 200, out)
}

// handleMapping is the startup "mapping the best routes for you" sweep:
// one latency measurement to every relay.
func (s *Server) handleMapping(w http.ResponseWriter, r *http.Request) {
	seen := map[string]bool{}
	var specs []engine.PathSpec
	for _, g := range s.allGames() {
		for _, p := range s.net.Paths(g.ID) {
			if !seen[p.ID] {
				seen[p.ID] = true
				specs = append(specs, p)
			}
		}
	}
	if len(specs) == 0 {
		writeErr(w, 409, "no_relays", errNoRelays)
		return
	}
	var opts engine.AnalyzeOptions
	if s.net.Demo() {
		opts = s.baseline(s.games[0].ID)
	}
	res, err := engine.Analyze(r.Context(), specs, opts, 1200*time.Millisecond)
	if err != nil {
		writeErr(w, 500, "mapping_failed", err)
		return
	}
	m := &mappingResult{At: time.Now(), Relays: len(specs)}
	for _, p := range specs {
		st := res.Paths[p.ID]
		if st.Up && (m.Best == "" || st.RTT < m.BestMS) {
			m.Best, m.BestMS = p.Label, st.RTT
		}
	}
	s.mu.Lock()
	s.mapping = m
	s.mu.Unlock()
	writeJSON(w, 200, m)
}

func (s *Server) handleFavorite(w http.ResponseWriter, r *http.Request) {
	fav, err := s.store.ToggleFavorite(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, "failed", err)
		return
	}
	writeJSON(w, 200, map[string]any{"favorites": fav})
}

// --- live stats ----------------------------------------------------------

type liveView struct {
	Status   string           `json:"status"`
	Session  *session         `json:"session"`
	Snapshot *engine.Snapshot `json:"snapshot"`
	Capture  *captureView     `json:"capture"` // route mode only
	Running  []string         `json:"running"`
}

func (s *Server) live() liveView {
	s.mu.Lock()
	v := liveView{Status: s.status, Session: s.sess}
	for id := range s.running {
		v.Running = append(v.Running, id)
	}
	sess := s.sess
	s.mu.Unlock()
	if sess != nil {
		snap := sess.eng.Snapshot()
		v.Snapshot = &snap
		if sess.capt != nil {
			v.Capture = &captureView{Mode: CaptureRoute, Stats: sess.capt.Stats()}
		}
	}
	return v
}

// handleLive streams status + engine metrics as Server-Sent Events at 2 Hz.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		b, _ := json.Marshal(s.live())
		fmt.Fprintf(w, "data: %s\n\n", b)
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-t.C:
		}
	}
}

// --- settings, diagnostics, demo controls --------------------------------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	old := s.store.Settings()
	v := old
	if err := readJSON(r, &v); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	if v.LoopbackRelay != old.LoopbackRelay {
		if err := s.relays.SetLoopback(v.LoopbackRelay); err != nil {
			writeErr(w, 500, "loopback_failed", err)
			return
		}
	}
	if err := s.store.SetSettings(v); err != nil {
		s.relays.SetLoopback(old.LoopbackRelay)
		writeErr(w, 400, "bad_settings", err)
		return
	}
	writeJSON(w, 200, s.store.Settings())
}

func (s *Server) handleDiagnostics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	analysis := make(map[string]analysisResult, len(s.analysis))
	for k, v := range s.analysis {
		analysis[k] = v
	}
	s.mu.Unlock()
	w.Header().Set("Content-Disposition", `attachment; filename="lowpath-report.json"`)
	writeJSON(w, 200, map[string]any{
		"generated_at": time.Now(),
		"version":      Version,
		"demo":         s.net.Demo(),
		"platform":     s.platform(),
		"relays":       s.relays.Views(), // never includes keys
		"settings":     s.store.Settings(),
		"analysis":     analysis,
		"live":         s.live(),
	})
}

// handleImpair lets the demo UI break or heal a simulated path so route
// degradation, auto-switch and fallback can be seen live.
func (s *Server) handleImpair(w http.ResponseWriter, r *http.Request) {
	if s.sim == nil {
		writeErr(w, 404, "not_demo", errors.New("only available in demo mode"))
		return
	}
	var req struct {
		PathID string `json:"path_id"`
		Action string `json:"action"` // degrade | outage | restore
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "bad_request", err)
		return
	}
	links := map[string]*netsim.Link{}
	if req.PathID == "direct" {
		links["direct"] = s.sim.Direct
	} else if req.PathID == "all" {
		links = s.sim.Links
	} else if l, ok := s.sim.Links[req.PathID]; ok {
		links[req.PathID] = l
	} else {
		writeErr(w, 404, "not_found", errors.New("unknown path"))
		return
	}
	defaults := map[string]netsim.Profile{"direct": demo.DirectProfile()}
	for _, d := range demo.DefaultPaths() {
		defaults[d.Spec.ID] = d.Profile
	}
	for id, l := range links {
		base := defaults[id]
		switch req.Action {
		case "degrade":
			p := base
			p.Delay += 45 * time.Millisecond
			p.Jitter += 12 * time.Millisecond
			p.Loss += 0.08
			l.SetProfile(p)
		case "outage":
			l.SetProfile(netsim.Profile{Loss: 1})
		case "restore":
			l.SetProfile(base)
		default:
			writeErr(w, 400, "bad_action", fmt.Errorf("unknown action %q", req.Action))
			return
		}
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
