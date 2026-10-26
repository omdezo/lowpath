package app

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

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
	a := newArtStore("", nil, fp.Games, fp.Icons)
	a.steam = func() []string { return []string{root} }
	a.libs = func() []string { return nil }
	g := Game{ID: "cs2", SteamApp: 730}

	for kind, want := range map[string]string{"hero": "hero", "header": "header"} {
		p, err := a.path(g, kind)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if b, _ := os.ReadFile(p); string(b) != want {
			t.Fatalf("%s: got %q", kind, b)
		}
	}
	// Not cached locally and no cache dir to download into: a miss, remembered.
	if _, err := a.path(g, "logo"); err != errNoArt {
		t.Fatalf("logo: want errNoArt, got %v", err)
	}
	if _, ok := a.miss["cs2_logo"]; !ok {
		t.Fatal("miss not remembered")
	}
	if _, err := a.path(g, "bogus"); err != errNoArt {
		t.Fatalf("unknown kind: %v", err)
	}
	// A game without a Steam id and no program on disk has no art at all.
	if _, err := a.path(Game{ID: "custom-x", Processes: []string{"no-such-program.exe"}}, "icon"); err != errNoArt {
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

	a := newArtStore(t.TempDir(), discard, fp.Games, fp.Icons)
	g := Game{ID: "custom-mygame", Processes: []string{"mygame.exe"}}
	p, err := a.path(g, "icon")
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
	if _, err := newArtStore(t.TempDir(), discard, fp.Games, newFake(t).Icons).path(g, "icon"); err != errNoArt {
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
