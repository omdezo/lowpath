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
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"lowpath/internal/catalog"
	"lowpath/internal/compat"
	"lowpath/internal/demo"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/engine"
	"lowpath/internal/icmp"
	"lowpath/internal/netsim"
	"lowpath/internal/platform"
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

	// Locator finds the public IP and ISP for /api/location. Nil: STUN and
	// RIPEstat over the internet, only when the UI asks.
	Locator Locator
	// ServerLookup finds the network (AS, ISP, city) of a game server for
	// the current server card. Nil: RIPEstat, cached, only when the card
	// shows a server.
	ServerLookup diag.NetworkLookup
	// Platform is the OS layer: capture backend, socket owners, privileges,
	// game detection, icons and folders. Nil: platform.Native(), the one
	// built for this OS. Tests pass fakeplatform.
	Platform *platform.Platform
	UI       fs.FS // nil: API only
	Log      *slog.Logger
	ArtDir   string // where downloaded and extracted game art is kept; "" = Steam's own cache only

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

	// NetStats reads the default route and adapter counters for the
	// bufferbloat test and the passive detector. Nil: this OS's reader.
	NetStats diag.NetStats
	// BloatLoader fills the line during the bufferbloat test. Nil: an
	// HTTP loader against diag.DefaultLoadURL.
	BloatLoader diag.Loader
	// Ping sends one ICMP echo. Nil: icmp.Ping.
	Ping diag.PingFunc
	// Marker records a running route-mode session on disk, so the next
	// start can clean up after a run that was killed or crashed. Nil: no
	// record (tests); cmd/lowpath keeps capture-session.json in the data
	// folder.
	Marker SessionMarker
	// PingTTL sends one TTL-limited echo for the route trace. Nil:
	// icmp.PingTTL. Names looks up hop names; nil: reverse DNS.
	PingTTL diag.TTLPingFunc
	Names   diag.NameFunc
	// TraceCount and TraceInterval shorten the route trace (tests, the
	// end-to-end run); zero: 200 probes per hop, one per second.
	TraceCount    int
	TraceInterval time.Duration
	// InService: this server runs inside the background service
	// (cmd/lowpathsvc) and the window reaches it through a Frontend.
	InService bool
	// ServiceNote says why this window runs its own engine although the
	// background service is installed (it wouldn't start, didn't answer,
	// or -no-service); the Settings row shows it instead of guessing.
	ServiceNote string
	// GatewayInterval is the router pinger's interval; zero: 50 ms (20 Hz).
	GatewayInterval time.Duration
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
	plat    *platform.Platform
	locator Locator
	log     *slog.Logger
	ui      fs.FS
	elevate func() error
	quit    func()
	auth    *auth
	art     *artStore
	bloat   *bloatRunner
	trace   *traceRunner
	line    *lineWatch
	current *currentServers
	// conflicts names other boosters, packet filter drivers and
	// full-tunnel VPNs (internal/compat).
	conflicts *conflictWatch
	verdict   *verdicts
	wifi      *wifiWatch
	gateway   *diag.GatewayPinger // router pings for Diagnostics and the verdict
	marker    SessionMarker       // nil: none
	inService bool
	// serviceNote: see Config.ServiceNote.
	serviceNote string

	// lifetime ends when the server closes; background work (the
	// bufferbloat test) runs under it.
	lifetime context.Context
	endLife  context.CancelFunc

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
	failsafe  *failsafeView // the last boost that ended without the user asking
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
	if cfg.Platform == nil {
		cfg.Platform = platform.Native()
	}
	if err := cfg.Platform.Check(); err != nil {
		return nil, err
	}
	if cfg.Quit == nil {
		cfg.Quit = func() {}
	}
	if cfg.NetStats == nil {
		cfg.NetStats = diag.NewNetStats()
	}
	if cfg.Ping == nil {
		cfg.Ping = icmp.Ping
	}
	if cfg.PingTTL == nil {
		cfg.PingTTL = icmp.PingTTL
	}
	if cfg.Names == nil {
		cfg.Names = reverseName
	}
	if cfg.BloatLoader == nil {
		l, err := diag.NewHTTPLoader(diag.HTTPLoaderConfig{UserAgent: "Lowpath/" + Version + " (bufferbloat test)"})
		if err != nil {
			return nil, err
		}
		cfg.BloatLoader = l
	}
	// One RIPEstat client for the Network card and the Current server card,
	// so its cap on requests in flight holds for the whole app (RIPEstat
	// allows 8 at once per address).
	var ripe *diag.RIPEstat
	if cfg.Locator == nil || cfg.ServerLookup == nil {
		ripe = diag.NewRIPEstat(diag.RIPEstatOptions{})
	}
	if cfg.Locator == nil {
		cfg.Locator = diag.NewInternetLocator(ripe)
	}
	if cfg.ServerLookup == nil {
		cfg.ServerLookup = diag.NewCachedLookup(ripe, diag.CacheOptions{Max: 256})
	}
	life, endLife := context.WithCancel(context.Background())
	s := &Server{
		store: cfg.Store, games: cfg.Games, net: cfg.Network, relays: cfg.Relays, servers: cfg.Servers, sim: cfg.Sim,
		plat: cfg.Platform, locator: cfg.Locator, log: cfg.Log, ui: cfg.UI, elevate: cfg.Elevate, quit: cfg.Quit,
		auth:   newAuth(cfg.Session),
		art:    newArtStore(cfg.ArtDir, cfg.Log, cfg.Platform.Games, cfg.Platform.Icons),
		status: "idle", analysis: map[string]analysisResult{},
		installed: map[string]bool{}, running: map[string]bool{},
		done:     make(chan struct{}),
		lifetime: life, endLife: endLife,
		line:        newLineWatch(cfg.NetStats, lineRate(cfg.Store.Bloat())),
		verdict:     newVerdicts(cfg.NetStats),
		wifi:        newWiFiWatch(cfg.Platform.WiFi, cfg.NetStats, cfg.Log.With("component", "wifi")),
		marker:      cfg.Marker,
		inService:   cfg.InService,
		serviceNote: cfg.ServiceNote,
	}
	s.bloat = &bloatRunner{
		net: cfg.NetStats, loader: cfg.BloatLoader, ping: cfg.Ping, store: cfg.Store,
		log:    cfg.Log.With("component", "bufferbloat"),
		relays: func() []engine.PathSpec { return cfg.Relays.Paths("") },
		done:   func(r diag.BloatResult) { s.line.setLineRate(lineRate(&r)) },
	}
	s.trace = &traceRunner{
		ping: cfg.PingTTL, names: cfg.Names, net: cfg.NetStats, store: cfg.Store, log: cfg.Log.With("component", "trace"),
		count: cfg.TraceCount, interval: cfg.TraceInterval,
	}
	s.gateway = newGatewayPinger(cfg, s)
	// Idle until the Diagnostics tab asks or a boost starts; ends with
	// the server's lifetime.
	go s.gateway.Run(life)
	s.conflicts = newConflictWatch(cfg.Platform, life, cfg.Log.With("component", "conflicts"))
	s.recoverLeftover()
	s.current = newCurrentServers(cfg.Platform.Flows, cfg.Platform.Owners, cfg.ServerLookup, cfg.Locator, cfg.Ping, cfg.Store, life, cfg.Log.With("component", "current-server"))
	return s, nil
}

// Run starts background detection; call once. The session is stopped when
// ctx ends, but callers should still call Close for an orderly shutdown.
func (s *Server) Run(ctx context.Context) {
	// Fetch the library's artwork in the background so covers are ready
	// the first time the home screen opens.
	go s.art.warm(ctx, s.allGames())
	go s.scanInstalled()
	go s.line.run(s.lifetime, s.lineInput)
	go s.conflicts.run(s.lifetime)
	go func() {
		// The current server card's flow learning: once a second, so the
		// primary-server window counts seconds like a session's does.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-s.lifetime.Done():
				return
			case <-t.C:
				s.current.tick(s.runningGames())
			}
		}
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
	s.bloat.stop("Lowpath is shutting down.")
	s.trace.stop()
	s.endLife()
	s.current.close()
	s.stopSession()
	s.relays.Close()
	if s.servers != nil {
		s.servers.Close()
	}
}

// lineInput feeds the passive detector: the session's engine snapshot.
func (s *Server) lineInput() lineInput {
	s.mu.Lock()
	sess := s.sess
	s.mu.Unlock()
	in := lineInput{bloatTest: s.bloat.isRunning()}
	if sess != nil {
		snap := sess.eng.Snapshot()
		in.snap, in.started = &snap, sess.Started
	}
	return in
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

// scanInstalled finds which catalog games are installed: in a Steam
// library, at their usual folder or, on Linux and macOS, inside a Wine or
// Proton prefix or a launcher's install folder.
func (s *Server) scanInstalled() {
	games := s.plat.Games
	lib := detect.Library{Steam: games.SteamLibraries(), DriveC: games.WinePrefixes(), Installs: games.LauncherInstalls()}
	inst := map[string]bool{}
	for _, g := range s.games {
		if lib.Has(g.Paths, g.Steam) {
			inst[g.ID] = true
		}
	}
	s.mu.Lock()
	s.installed = inst
	s.mu.Unlock()
}

func (s *Server) scanRunning() {
	procs := s.plat.Games.Running()
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

// Handler serves the UI and the API to the app's window over loopback HTTP,
// behind the per-launch session (see auth.go).
func (s *Server) Handler() http.Handler {
	mux := s.routes()
	mux.HandleFunc("GET "+pathSession, s.auth.handleSession)
	if s.ui != nil {
		mux.Handle("/", spa(s.ui))
	}
	return s.auth.guard(mux)
}

// ServiceHandler serves the API alone, without the session check, for the
// background service's pipe: the pipe's access list already limits it to
// users signed in at this PC, and the window in front of it (Frontend)
// keeps its own session for the browser side.
func (s *Server) ServiceHandler() http.Handler { return s.routes() }

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("POST /api/mapping", s.handleMapping)
	mux.HandleFunc("GET /api/games/{id}/routes", s.handleRoutes)
	mux.HandleFunc("POST /api/games/{id}/analyze", s.handleAnalyze)
	mux.HandleFunc("POST /api/games/{id}/favorite", s.handleFavorite)
	mux.HandleFunc("GET /api/games/{id}/servers", s.handleServers)
	mux.HandleFunc("POST /api/games/{id}/servers/ping", s.handleServers)
	mux.HandleFunc("GET /api/games/{id}/current-server", s.handleCurrentServer)
	mux.HandleFunc("POST /api/games/custom", s.handleAddCustomGame)
	mux.HandleFunc("DELETE /api/games/custom/{id}", s.handleDeleteCustomGame)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("POST /api/stop", s.handleStop)
	mux.HandleFunc("GET /api/live", s.handleLive)
	mux.HandleFunc("DELETE /api/failsafe", s.handleDismissFailsafe)
	mux.HandleFunc("PUT /api/settings", s.handleSettings)
	mux.HandleFunc("GET /api/relays", s.handleRelays)
	mux.HandleFunc("POST /api/relays", s.handleAddRelay)
	mux.HandleFunc("POST /api/relays/test", s.handleTestRelay)
	mux.HandleFunc("DELETE /api/relays/{id}", s.handleDeleteRelay)
	mux.HandleFunc("GET /api/processes", s.handleProcesses)
	mux.HandleFunc("GET /api/art/{id}/{kind}", s.handleArt)
	mux.HandleFunc("POST /api/elevate", s.handleElevate)
	mux.HandleFunc("GET /api/diagnostics", s.handleDiagnostics)
	mux.HandleFunc("GET /api/location", s.handleLocation)
	mux.HandleFunc("GET /api/conflicts", s.handleConflicts)
	mux.HandleFunc("GET /api/diag/bloat", s.handleBloat)
	mux.HandleFunc("POST /api/diag/bloat", s.handleStartBloat)
	mux.HandleFunc("DELETE /api/diag/bloat", s.handleStopBloat)
	mux.HandleFunc("GET /api/diag/trace", s.handleTrace)
	mux.HandleFunc("POST /api/diag/trace", s.handleStartTrace)
	mux.HandleFunc("DELETE /api/diag/trace", s.handleStopTrace)
	mux.HandleFunc("GET /api/diag/trace/report", s.handleTraceReport)
	mux.HandleFunc("GET /api/diag/gateway", s.handleGateway)
	mux.HandleFunc("GET /api/diag/verdict", s.handleVerdict)
	mux.HandleFunc("GET /api/diag/verdict/report", s.handleVerdictReport)
	mux.HandleFunc("GET /api/diag/wifi", s.handleWiFi)
	mux.HandleFunc("POST /api/diag/wifi/survey", s.handleWiFiSurvey)
	mux.HandleFunc("POST /api/demo/impair", s.handleImpair)
	mux.HandleFunc("GET "+pathVersion, handleVersion)
	return mux
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
	// Line is the passive bufferbloat detector during a session (nil
	// without one).
	Line *diag.PassiveState `json:"line"`
	// Failsafe is the last boost that ended without the user asking (nil:
	// none since the last Boost).
	Failsafe *failsafeView `json:"failsafe"`
	// Conflicts is the last scan for other boosters, packet filter drivers
	// and full-tunnel VPNs (nil until the first scan ends).
	Conflicts *compat.Report `json:"conflicts"`
	// WiFi is the boost's Wi-Fi scan pause (nil: no boost, not on Wi-Fi,
	// or turned off in Settings).
	WiFi *wifiTuneView `json:"wifi"`
}

func (s *Server) live() liveView {
	s.mu.Lock()
	v := liveView{Status: s.status, Session: s.sess, Failsafe: s.failsafe, Conflicts: s.conflicts.view()}
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
		v.Line = s.line.view()
		v.WiFi = sess.wifi.current()
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
	live := s.live()
	if live.Conflicts != nil {
		r := live.Conflicts.Redacted()
		live.Conflicts = &r
	}
	w.Header().Set("Content-Disposition", `attachment; filename="lowpath-report.json"`)
	writeJSON(w, 200, map[string]any{
		"generated_at": time.Now(),
		"version":      Version,
		"demo":         s.net.Demo(),
		"platform":     s.platform(),
		"relays":       s.relays.Views(), // never includes keys
		"settings":     s.store.Settings(),
		"analysis":     analysis,
		"location":     s.lastLocation(), // addresses cut to /24 and /48
		"bufferbloat":  s.store.Bloat(),
		"route_trace":  s.lastTrace(),                    // addresses cut to /24 and /48
		"conflicts":    s.conflicts.current().Redacted(), // user folder names cut out
		"verdict":      s.diagnosis(),
		"gateway":      s.gateway.View().Redacted(),
		"wifi":         s.wifiView(), // no network name or access point address
		"live":         live,
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
