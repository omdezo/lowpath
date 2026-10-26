package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"time"

	"lowpath/internal/diag"
)

type Settings struct {
	DupLevel       int    `json:"dup_level"` // 0 auto, 1 off, 2 balanced, 4 max
	AutoSwitch     bool   `json:"auto_switch"`
	KillSwitch     bool   `json:"kill_switch"`
	AutoAnalyze    bool   `json:"auto_analyze"`
	StopOnGameExit bool   `json:"stop_on_game_exit"`
	Theme          string `json:"theme"` // dark | light
	LaunchOnBoot   bool   `json:"launch_on_boot"`
	MinimizeToTray bool   `json:"minimize_to_tray"`
	Notifications  bool   `json:"notifications"`
	CaptureMode    string `json:"capture_mode"`   // route | proxy
	LoopbackRelay  bool   `json:"loopback_relay"` // run the built-in test relay on 127.0.0.1
}

const (
	CaptureRoute = "route" // WinDivert takes the game's packets; nothing to configure in the game
	CaptureProxy = "proxy" // the game sends to a local port
)

// defaultCaptureMode is route mode where the capture driver exists (Windows)
// and proxy mode everywhere else.
func defaultCaptureMode() string {
	if runtime.GOOS == "windows" {
		return CaptureRoute
	}
	return CaptureProxy
}

func DefaultSettings() Settings {
	return Settings{AutoSwitch: true, AutoAnalyze: true, StopOnGameExit: true, Theme: "dark", MinimizeToTray: true, Notifications: true, CaptureMode: defaultCaptureMode()}
}

// StoredRelay is a relay the user added in Settings → Relays.
type StoredRelay struct {
	ID        string         `json:"id"`
	Label     string         `json:"label"`
	Region    string         `json:"region"`
	Continent string         `json:"continent"`
	Host      string         `json:"host"` // as typed (may be a name); Addr is what it resolved to
	Addr      netip.AddrPort `json:"addr"`
	Key       string         `json:"key"`
	Egress    string         `json:"egress"` // id of the egress relay this one forwards to; "" = it is an egress
	CreatedAt time.Time      `json:"created_at"`
}

// maxServers is how many recent game servers are kept per game.
const maxServers = 5

type persisted struct {
	Favorites   []string      `json:"favorites"`
	Recent      []string      `json:"recent"`
	Settings    Settings      `json:"settings"`
	Relays      []StoredRelay `json:"relays"`
	CustomGames []Game        `json:"custom_games"`
	// Servers holds the game servers route mode saw each game use, newest
	// first, so the next Analyze has something to ping for the baseline.
	Servers map[string][]netip.AddrPort `json:"servers"`
	// Bloat is the last finished bufferbloat test; its throughput is the
	// line rate the passive detector compares against.
	Bloat *diag.BloatResult `json:"bloat,omitempty"`
	// Trace is the last finished route trace.
	Trace *diag.TraceResult `json:"trace,omitempty"`
}

type Store struct {
	path string
	mu   sync.Mutex
	d    persisted
}

func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, d: persisted{Settings: DefaultSettings()}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		return nil, err
	}
	// State written before capture modes existed has none.
	if s.d.Settings.CaptureMode != CaptureRoute && s.d.Settings.CaptureMode != CaptureProxy {
		s.d.Settings.CaptureMode = defaultCaptureMode()
	}
	return s, nil
}

func (s *Store) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s.d, "", "  ")
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

var (
	ErrNotFound    = errors.New("not found")
	ErrRelayExists = errors.New("a relay with this address already exists")
	ErrRelayInUse  = errors.New("other relays forward to this egress relay; delete them first")
	ErrGameExists  = errors.New("a custom game with this name already exists")
)

func (s *Store) Settings() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Settings
}

func (s *Store) SetSettings(v Settings) error {
	switch v.DupLevel {
	case 0, 1, 2, 4:
	default:
		return errors.New("dup_level must be 0, 1, 2 or 4")
	}
	if v.Theme != "light" {
		v.Theme = "dark"
	}
	switch v.CaptureMode {
	case "":
		v.CaptureMode = defaultCaptureMode()
	case CaptureRoute, CaptureProxy:
	default:
		return errors.New(`capture_mode must be "route" or "proxy"`)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Settings = v
	return s.saveLocked()
}

func (s *Store) Favorites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.Favorites)
}

func (s *Store) ToggleFavorite(id string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.Index(s.d.Favorites, id); i >= 0 {
		s.d.Favorites = slices.Delete(s.d.Favorites, i, i+1)
	} else {
		s.d.Favorites = append(s.d.Favorites, id)
	}
	return slices.Clone(s.d.Favorites), s.saveLocked()
}

func (s *Store) Recent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.Recent)
}

func (s *Store) TouchRecent(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Recent = slices.DeleteFunc(s.d.Recent, func(x string) bool { return x == id })
	s.d.Recent = append([]string{id}, s.d.Recent...)
	if len(s.d.Recent) > 8 {
		s.d.Recent = s.d.Recent[:8]
	}
	return s.saveLocked()
}

// Relays returns the user's relays in the order they were added.
func (s *Store) Relays() []StoredRelay {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.Relays)
}

// AddRelay saves r under a unique ID derived from r.ID ("fra", "fra-2", ...);
// taken reports IDs used outside the store (imported and built-in relays).
func (s *Store) AddRelay(r StoredRelay, taken func(id string) bool) (StoredRelay, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.d.Relays, func(x StoredRelay) bool { return x.Addr == r.Addr }) {
		return StoredRelay{}, ErrRelayExists
	}
	base := r.ID
	for n := 2; taken(r.ID) || slices.ContainsFunc(s.d.Relays, func(x StoredRelay) bool { return x.ID == r.ID }); n++ {
		r.ID = fmt.Sprintf("%s-%d", base, n)
	}
	s.d.Relays = append(s.d.Relays, r)
	return r, s.saveLocked()
}

func (s *Store) DeleteRelay(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.d.Relays, func(r StoredRelay) bool { return r.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	if slices.ContainsFunc(s.d.Relays, func(r StoredRelay) bool { return r.Egress == id }) {
		return ErrRelayInUse
	}
	s.d.Relays = slices.Delete(s.d.Relays, i, i+1)
	return s.saveLocked()
}

func (s *Store) CustomGames() []Game {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.CustomGames)
}

func (s *Store) AddCustomGame(g Game) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if slices.ContainsFunc(s.d.CustomGames, func(x Game) bool { return x.ID == g.ID }) {
		return ErrGameExists
	}
	s.d.CustomGames = append(s.d.CustomGames, g)
	return s.saveLocked()
}

// DeleteCustomGame removes the game and everything remembered about it.
func (s *Store) DeleteCustomGame(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.d.CustomGames, func(x Game) bool { return x.ID == id })
	if i < 0 {
		return ErrNotFound
	}
	s.d.CustomGames = slices.Delete(s.d.CustomGames, i, i+1)
	isID := func(x string) bool { return x == id }
	s.d.Favorites = slices.DeleteFunc(s.d.Favorites, isID)
	s.d.Recent = slices.DeleteFunc(s.d.Recent, isID)
	delete(s.d.Servers, id)
	return s.saveLocked()
}

// Servers returns the game servers recently seen for a game, newest first.
func (s *Store) Servers(gameID string) []netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.d.Servers[gameID])
}

// RecordServer remembers a as the game's latest server. It only writes the
// file when the list actually changes, so calling it every few seconds is cheap.
func (s *Store) RecordServer(gameID string, a netip.AddrPort) error {
	if !a.IsValid() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.d.Servers[gameID]
	if len(cur) > 0 && cur[0] == a {
		return nil
	}
	next := append([]netip.AddrPort{a}, slices.DeleteFunc(slices.Clone(cur), func(x netip.AddrPort) bool { return x == a })...)
	if len(next) > maxServers {
		next = next[:maxServers]
	}
	if s.d.Servers == nil {
		s.d.Servers = map[string][]netip.AddrPort{}
	}
	s.d.Servers[gameID] = next
	return s.saveLocked()
}

// Bloat is the last finished bufferbloat test, or nil.
func (s *Store) Bloat() *diag.BloatResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Bloat
}

// Trace is the last finished route trace, or nil.
func (s *Store) Trace() *diag.TraceResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Trace
}

// SetTrace saves a finished route trace.
func (s *Store) SetTrace(r diag.TraceResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Trace = &r
	return s.saveLocked()
}

// SetBloat saves a finished bufferbloat test.
func (s *Store) SetBloat(r diag.BloatResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Bloat = &r
	return s.saveLocked()
}
