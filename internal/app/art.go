package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"lowpath/internal/detect"
	"lowpath/internal/platform"
)

// Game art: real artwork for the library and the game screen. For Steam
// games it comes from the user's own Steam client cache, else from Steam's
// public image CDN (downloaded once, then kept in <data>\art); for any other
// installed or running game it is the icon inside the game's own .exe.
// Nothing is bundled with Lowpath, and custom games never touch the network.

type artKind struct {
	local []string // file names in Steam's appcache\librarycache\<appid>
	cdn   string   // file name on Steam's CDN
	ext   string
}

var artKinds = map[string]artKind{
	"cover":  {[]string{"library_600x900.jpg", "library_capsule.jpg"}, "library_600x900.jpg", ".jpg"}, // portrait, 600×900
	"hero":   {[]string{"library_hero.jpg"}, "library_hero.jpg", ".jpg"},                              // wide banner
	"logo":   {[]string{"logo.png"}, "logo.png", ".png"},                                              // transparent title logo
	"header": {[]string{"header.jpg", "library_header.jpg"}, "header.jpg", ".jpg"},                    // 460×215 store header
	"icon":   {ext: ".png"},                                                                           // from the game's .exe
}

const (
	steamCDN       = "https://shared.akamai.steamstatic.com/store_item_assets/steam/apps/"
	steamAppAPI    = "https://store.steampowered.com/api/appdetails?filters=basic&appids="
	maxArtBytes    = 4 << 20
	artMissTTL     = time.Hour // retry a miss (offline, game not installed yet) after this
	iconSize       = 128
	exeSearchDepth = 7
	exeSearchLimit = 30000
)

type artStore struct {
	dir    string // cache directory; "" = memory only (tests)
	log    *slog.Logger
	client *http.Client
	steam  func() []string // Steam client roots
	libs   func() []string // Steam library common\ folders
	games  platform.GameDetector
	icons  platform.IconProvider

	mu    sync.Mutex
	files map[string]string    // key -> cached file path
	miss  map[string]time.Time // key -> when we last failed to find it
	keyMu map[string]*sync.Mutex
	fetch chan struct{} // bounds concurrent downloads
}

func newArtStore(dir string, log *slog.Logger, games platform.GameDetector, icons platform.IconProvider) *artStore {
	return &artStore{
		dir:    dir,
		log:    log,
		client: &http.Client{Timeout: 10 * time.Second},
		steam:  games.SteamRoots,
		libs:   games.SteamLibraries,
		games:  games,
		icons:  icons,
		files:  map[string]string{},
		miss:   map[string]time.Time{},
		keyMu:  map[string]*sync.Mutex{},
		fetch:  make(chan struct{}, 4),
	}
}

var errNoArt = errors.New("no artwork for this game")

// path returns a file holding the art, finding (and caching) it on first use.
// Lookups for different games run in parallel; the same one runs once.
func (a *artStore) path(g Game, kind string) (string, error) {
	k, ok := artKinds[kind]
	if !ok {
		return "", errNoArt
	}
	key := g.ID + "_" + kind
	a.mu.Lock()
	if p, ok := a.files[key]; ok {
		a.mu.Unlock()
		return p, nil
	}
	if t, ok := a.miss[key]; ok && time.Since(t) < artMissTTL {
		a.mu.Unlock()
		return "", errNoArt
	}
	km := a.keyMu[key]
	if km == nil {
		km = &sync.Mutex{}
		a.keyMu[key] = km
	}
	a.mu.Unlock()

	km.Lock()
	defer km.Unlock()
	a.mu.Lock()
	p, done := a.files[key]
	a.mu.Unlock()
	if done {
		return p, nil
	}

	p, err := a.find(g, kind, k, key)
	a.mu.Lock()
	if err != nil {
		a.miss[key] = time.Now()
	} else {
		a.files[key] = p
		delete(a.miss, key)
	}
	a.mu.Unlock()
	return p, err
}

func (a *artStore) find(g Game, kind string, k artKind, key string) (string, error) {
	cached := filepath.Join(a.dir, safeName(key)+k.ext)
	if a.dir != "" {
		if fi, err := os.Stat(cached); err == nil && fi.Size() > 0 {
			return cached, nil
		}
	}
	if kind == "icon" {
		return a.exeIcon(g, cached)
	}
	if g.SteamApp == 0 {
		return "", errNoArt
	}
	// The Steam client already has the art for games the user owns: no
	// download, and it works offline.
	if p := a.steamLocal(g.SteamApp, k.local); p != "" {
		return p, nil
	}
	if a.dir == "" {
		return "", errNoArt
	}
	b, err := a.download(steamCDN + strconv.Itoa(g.SteamApp) + "/" + k.cdn)
	if err != nil && kind == "header" {
		// Newer store pages keep the header under a hashed path; the store
		// API knows it.
		if u := a.headerURL(g.SteamApp); u != "" {
			b, err = a.download(u)
		}
	}
	if err != nil {
		return "", err
	}
	return cached, writeFileAtomic(cached, b)
}

// steamLocal looks for one of names in Steam's per-game artwork cache (the
// files sit either in the app's folder or one folder below it).
func (a *artStore) steamLocal(app int, names []string) string {
	for _, root := range a.steam() {
		dir := filepath.Join(root, "appcache", "librarycache", strconv.Itoa(app))
		for _, n := range names {
			if p := filepath.Join(dir, n); fileExists(p) {
				return p
			}
		}
		subs, _ := os.ReadDir(dir)
		for _, s := range subs {
			if !s.IsDir() {
				continue
			}
			for _, n := range names {
				if p := filepath.Join(dir, s.Name(), n); fileExists(p) {
					return p
				}
			}
		}
	}
	return ""
}

// exeIcon renders the icon of the game's program: the running process, the
// path remembered for a custom game, or the binary found in its install
// folder.
func (a *artStore) exeIcon(g Game, out string) (string, error) {
	exe := g.Exe
	if exe == "" || !fileExists(exe) {
		exe = a.games.RunningExe(g.Processes)
	}
	if exe == "" {
		roots := slicesClone(g.Paths)
		if g.Steam != "" {
			for _, l := range a.libs() {
				roots = append(roots, filepath.Join(l, g.Steam))
			}
		}
		names := append(slicesClone(g.IconExe), g.Processes...)
		exe = detect.FindExe(existing(roots), names, exeSearchDepth, exeSearchLimit)
	}
	if exe == "" {
		return "", errNoArt
	}
	img, err := a.icons.ExeIcon(exe, iconSize)
	if err != nil {
		return "", errNoArt
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	if a.dir == "" {
		return "", errNoArt
	}
	return out, writeFileAtomic(out, buf.Bytes())
}

func (a *artStore) download(u string) ([]byte, error) {
	if pu, err := url.Parse(u); err != nil || pu.Scheme != "https" || !steamHost(pu.Hostname()) {
		return nil, fmt.Errorf("art: refusing %q", u)
	}
	a.fetch <- struct{}{}
	defer func() { <-a.fetch }()
	res, err := a.client.Get(u)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("art: %s: %s", u, res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxArtBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxArtBytes {
		return nil, errors.New("art: image too large")
	}
	if ct := http.DetectContentType(b); ct != "image/jpeg" && ct != "image/png" {
		return nil, fmt.Errorf("art: unexpected %s", ct)
	}
	return b, nil
}

// headerURL asks the Steam store for an app's header image address.
func (a *artStore) headerURL(app int) string {
	id := strconv.Itoa(app)
	a.fetch <- struct{}{}
	res, err := a.client.Get(steamAppAPI + id)
	<-a.fetch
	if err != nil {
		return ""
	}
	defer res.Body.Close()
	var body map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			HeaderImage string `json:"header_image"`
		} `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body) != nil || !body[id].Success {
		return ""
	}
	return body[id].Data.HeaderImage
}

func steamHost(h string) bool {
	return h == "steamstatic.com" || strings.HasSuffix(h, ".steamstatic.com") || h == "store.steampowered.com"
}

// handleArt serves GET /api/art/{id}/{kind}: 404 when the game has none, so
// the UI falls back to its letter tile.
func (s *Server) handleArt(w http.ResponseWriter, r *http.Request) {
	g, ok := s.game(r.PathValue("id"))
	if !ok {
		writeErr(w, 404, "not_found", errors.New("unknown game"))
		return
	}
	p, err := s.art.path(g, r.PathValue("kind"))
	if err != nil {
		writeErr(w, 404, "no_art", errNoArt)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		writeErr(w, 404, "no_art", errNoArt)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeErr(w, 404, "no_art", errNoArt)
		return
	}
	// Art rarely changes; the browser may keep it for a week.
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeContent(w, r, filepath.Base(p), fi.ModTime(), f)
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

func existing(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		if errors.Is(err, fs.ErrExist) {
			return nil
		}
		return err
	}
	return nil
}

// warm finds every game's cover and icon ahead of time, a few at a time.
func (a *artStore) warm(ctx context.Context, games []Game) {
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for _, g := range games {
		for _, kind := range []string{"cover", "icon"} {
			select {
			case <-ctx.Done():
				wg.Wait()
				return
			case sem <- struct{}{}:
			}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				a.path(g, kind)
			}()
		}
	}
	wg.Wait()
}
