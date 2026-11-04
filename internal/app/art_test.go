package app

import (
	"context"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeCDN plays Steam's CDN and store API in memory, so art tests never
// reach the network. serve answers every request; with serve nil, each
// request hangs until it is cancelled.
type fakeCDN struct {
	serve    http.HandlerFunc
	started  chan string // each request's path as it arrives (when there is room)
	requests atomic.Int32
	inFlight atomic.Int32
}

func (f *fakeCDN) RoundTrip(r *http.Request) (*http.Response, error) {
	f.requests.Add(1)
	f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	select {
	case f.started <- r.URL.Path:
	default:
	}
	if f.serve == nil {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	rec := httptest.NewRecorder()
	f.serve(rec, r)
	return rec.Result(), nil
}

func (f *fakeCDN) client() *http.Client { return &http.Client{Transport: f} }

// fakeJPEG is enough of a JPEG for the content sniffing the store does.
var fakeJPEG = append([]byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), make([]byte, 64)...)

// tmpFiles lists the temporary files left in dir.
func tmpFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var tmp []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			tmp = append(tmp, e.Name())
		}
	}
	return tmp
}

// A cover Steam's client doesn't have comes from the CDN once, is kept in
// the art folder, and later lookups read it from there.
func TestArtDownloadsFromCDN(t *testing.T) {
	cdn := &fakeCDN{serve: func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/730/library_600x900.jpg") || r.URL.Host != "shared.akamai.steamstatic.com" {
			http.NotFound(w, r)
			return
		}
		w.Write(fakeJPEG)
	}}
	dir := filepath.Join(t.TempDir(), "art")
	fp := newFake(t)
	a := newArtStore(dir, discard, cdn.client(), fp.Games, fp.Icons)
	g := Game{ID: "cs2", SteamApp: 730}
	p, err := a.path(t.Context(), g, "cover")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != string(fakeJPEG) || filepath.Dir(p) != dir {
		t.Fatalf("cover at %s holds %q", p, b)
	}
	if tmp := tmpFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temporary files left: %v", tmp)
	}
	// A fresh store (the next start) finds it on disk.
	b := newArtStore(dir, discard, cdn.client(), fp.Games, fp.Icons)
	if p2, err := b.path(t.Context(), g, "cover"); err != nil || p2 != p {
		t.Fatalf("second start: %s, %v", p2, err)
	}
	if n := cdn.requests.Load(); n != 1 {
		t.Errorf("%d downloads, want 1", n)
	}
	// The CDN has no hero: a miss, remembered.
	if _, err := a.path(t.Context(), g, "hero"); err == nil {
		t.Error("a missing hero was found")
	}
	if _, ok := a.miss["cs2_hero"]; !ok {
		t.Error("miss not remembered")
	}
}

// Close ends the game-art warm-up Run started and waits for it: a download
// still running after Close would write into the art folder later, while
// the service's data folder (or a test's temporary one) is being removed,
// and a shutdown would leave a half-written file (#206).
func TestCloseStopsArtWarmUp(t *testing.T) {
	cdn := &fakeCDN{started: make(chan string, 1)} // every download hangs
	dir := filepath.Join(t.TempDir(), "art")
	_, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.ArtDir, c.ArtClient = dir, cdn.client()
	})
	srv.Run(context.Background()) // only Close can end it
	select {
	case <-cdn.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the warm-up never asked the CDN for a cover")
	}
	srv.Close()
	if n := cdn.inFlight.Load(); n != 0 {
		t.Fatalf("%d art downloads still running after Close", n)
	}
	if tmp := tmpFiles(t, dir); len(tmp) != 0 {
		t.Errorf("temporary files left: %v", tmp)
	}
	// Downloads cut short by the shutdown aren't misses: they said nothing
	// about the art.
	srv.art.mu.Lock()
	defer srv.art.mu.Unlock()
	for _, g := range srv.games {
		if _, ok := srv.art.miss[g.ID+"_cover"]; ok && g.SteamApp != 0 {
			t.Errorf("%s: a download cut short remembered as a miss", g.ID)
		}
	}
}

func TestArtFromSteamCache(t *testing.T) {
	root := t.TempDir()
	lib := filepath.Join(root, "appcache", "librarycache", "730")
	if err := os.MkdirAll(filepath.Join(lib, "abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Steam keeps some files in the app folder and some one level below it.
	os.WriteFile(filepath.Join(lib, "library_hero.jpg"), []byte("hero"), 0o644)
	os.WriteFile(filepath.Join(lib, "abc123", "header.jpg"), []byte("header"), 0o644)

	fp := newFake(t)
	a := newArtStore("", nil, nil, fp.Games, fp.Icons)
	a.steam = func() []string { return []string{root} }
	a.libs = func() []string { return nil }
	g := Game{ID: "cs2", SteamApp: 730}

	for kind, want := range map[string]string{"hero": "hero", "header": "header"} {
		p, err := a.path(t.Context(), g, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Fatalf("%s: got %q", kind, b)
		}
	}
	// Not cached locally and no cache dir to download into: a miss, remembered.
	if _, err := a.path(t.Context(), g, "logo"); err != errNoArt {
		t.Fatalf("logo: want errNoArt, got %v", err)
	}
	if _, ok := a.miss["cs2_logo"]; !ok {
		t.Fatal("miss not remembered")
	}
	if _, err := a.path(t.Context(), g, "bogus"); err != errNoArt {
		t.Fatalf("unknown kind: %v", err)
	}
	// A game without a Steam id and no program on disk has no art at all.
	if _, err := a.path(t.Context(), Game{ID: "custom-x", Processes: []string{"no-such-program.exe"}}, "icon"); err != errNoArt {
		t.Fatalf("icon: %v", err)
	}
}

// A game's icon comes from its running program through the platform's
// detector and icon reader, and is cached as a PNG.
func TestArtIconFromRunningProgram(t *testing.T) {
	fp := newFake(t)
	exe := filepath.Join(t.TempDir(), "mygame.exe")
	os.WriteFile(exe, nil, 0o644)
	fp.Games.SetRunning("mygame.exe")
	fp.Games.SetExe("mygame.exe", exe)
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(1, 1, color.NRGBA{255, 0, 0, 255})
	fp.Icons.Set(exe, img)

	a := newArtStore(t.TempDir(), discard, nil, fp.Games, fp.Icons)
	g := Game{ID: "custom-mygame", Processes: []string{"mygame.exe"}}
	p, err := a.path(t.Context(), g, "icon")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _, _ := got.At(1, 1).RGBA(); r != 0xffff {
		t.Errorf("cached icon pixel %v, want the program's red", got.At(1, 1))
	}

	// Without an icon reader that knows the program: no art, not a crash.
	if _, err := newArtStore(t.TempDir(), discard, nil, fp.Games, newFake(t).Icons).path(t.Context(), g, "icon"); err != errNoArt {
		t.Errorf("unknown icon: %v, want errNoArt", err)
	}
}

func TestSteamHostAllowList(t *testing.T) {
	for h, ok := range map[string]bool{
		"shared.akamai.steamstatic.com":  true,
		"cdn.cloudflare.steamstatic.com": true,
		"store.steampowered.com":         true,
		"steamstatic.com.evil.example":   false,
		"evilsteamstatic.com":            false,
	} {
		if steamHost(h) != ok {
			t.Errorf("%s: want %v", h, ok)
		}
	}
}
