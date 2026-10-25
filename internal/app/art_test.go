package app

import (
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

	a := newArtStore("", nil)
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
