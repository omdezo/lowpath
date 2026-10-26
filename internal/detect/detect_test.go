package detect

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The Steam client folder plus every library in libraryfolders.vdf, each
// once, and only the ones that exist.
func TestSteamLibraries(t *testing.T) {
	root := t.TempDir()
	steam := filepath.Join(root, "Steam")
	own := mkdir(t, steam, "steamapps", "common")
	lib := filepath.Join(root, "Games", "SteamLibrary")
	other := mkdir(t, lib, "steamapps", "common")
	// Steam writes Windows paths with doubled backslashes; elsewhere they
	// are plain.
	libPath := strings.ReplaceAll(lib, `\`, `\\`)
	vdf := `"libraryfolders"
{
	"0"	{ "path"	"` + strings.ReplaceAll(steam, `\`, `\\`) + `" }
	"1"	{ "path"	"` + libPath + `" }
	"2"	{ "path"	"` + strings.ReplaceAll(filepath.Join(root, "gone"), `\`, `\\`) + `" }
}`
	if err := os.WriteFile(filepath.Join(steam, "steamapps", "libraryfolders.vdf"), []byte(vdf), 0o644); err != nil {
		t.Fatal(err)
	}
	mkdir(t, steam, "appcache")

	s := NewWithSteam([]string{steam, filepath.Join(root, "NoSteamHere")})
	if got, want := s.SteamLibraries(), []string{own, other}; !slices.Equal(got, want) {
		t.Errorf("SteamLibraries = %q, want %q", got, want)
	}
	if got := s.SteamRoots(); !slices.Equal(got, []string{steam}) {
		t.Errorf("SteamRoots = %q, want only the folder with an appcache", got)
	}
	if got := NewWithSteam(nil).SteamLibraries(); len(got) != 0 {
		t.Errorf("no Steam: %q", got)
	}
}

func TestInstalledAndFindExe(t *testing.T) {
	root := t.TempDir()
	bin := mkdir(t, root, "Game", "Binaries", "Win64")
	exe := filepath.Join(bin, "Game-Win64-Shipping.exe")
	os.WriteFile(exe, nil, 0o644)

	if !(Library{}).Has([]string{filepath.Join(root, "nope"), bin}, "") {
		t.Error("existing install path not found")
	}
	steam := Library{Steam: []string{root}}
	if !steam.Has(nil, "Game") || steam.Has(nil, "Other") || steam.Has(nil, "") {
		t.Error("Steam folder lookup wrong")
	}
	if got := FindExe([]string{root}, []string{"game-win64-shipping.EXE"}, 5, 100); got != exe {
		t.Errorf("FindExe = %q, want %q", got, exe)
	}
	if got := FindExe([]string{root}, []string{"Game-Win64-Shipping.exe"}, 2, 100); got != "" {
		t.Errorf("FindExe beyond depth = %q", got)
	}
}

// Every OS answers: real data, or an honest "unsupported", never a panic.
func TestSystemOnThisOS(t *testing.T) {
	s := New()
	if s.Running() == nil {
		t.Error("Running returned nil")
	}
	if p := s.RunningExe([]string{"no-such-program-lowpath.exe"}); p != "" {
		t.Errorf("RunningExe of nothing = %q", p)
	}
	_, err := s.ExeIcon(filepath.Join(t.TempDir(), "missing.exe"), 32)
	if err == nil {
		t.Error("icon of a missing file")
	}
	if runtime.GOOS != "windows" && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("ExeIcon on %s: %v, want errors.ErrUnsupported", runtime.GOOS, err)
	}
}

// On Linux and macOS the real process table lists this test binary under
// its own name, and its path.
func TestSystemSeesSelf(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the Windows snapshot is covered by TestSystemOnThisOS")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	s := New()
	if name := filepath.Base(exe); !s.Running()[strings.ToLower(name)] {
		t.Errorf("%q not among the running programs", name)
	}
	got := s.RunningExe([]string{filepath.Base(exe)})
	if real, _ := filepath.EvalSymlinks(exe); got != exe && got != real {
		t.Errorf("RunningExe = %q, want %q", got, exe)
	}
}
