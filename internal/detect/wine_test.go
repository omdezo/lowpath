package detect

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lowpath/internal/owner"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// slash puts a path in a JSON string or YAML line as the launchers write it.
func slash(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }

// A home folder with every kind of prefix and launcher note, as on a Linux
// gaming PC: a plain ~/.wine, a Lutris game in ~/Games, a Lutris prefix
// elsewhere, a Heroic prefix, a Proton prefix in a second Steam library,
// and Legendary's list of Epic installs.
func TestWinePrefixesAndInstalls(t *testing.T) {
	home := t.TempDir()
	other := t.TempDir()
	j := filepath.Join
	plain := mkdir(t, home, ".wine", "drive_c")
	lutrisGame := mkdir(t, home, "Games", "overwatch", "drive_c")
	mkdir(t, home, "Games", "not-a-prefix", "readme")
	lutrisElsewhere := mkdir(t, other, "lutris", "league", "drive_c")
	heroicPfx := mkdir(t, other, "heroic", "Rocket League", "drive_c")
	steam := j(home, "Steam")
	mkdir(t, steam, "steamapps", "common")
	lib2 := j(other, "SteamLibrary")
	mkdir(t, lib2, "steamapps", "common")
	proton := mkdir(t, lib2, "steamapps", "compatdata", "1172470", "pfx", "drive_c")
	mkdir(t, lib2, "steamapps", "compatdata", "228980") // no pfx yet
	write(t, j(steam, "steamapps", "libraryfolders.vdf"), `"libraryfolders" { "1" { "path" "`+slash(lib2)+`" } }`)

	lutrisDir := j(home, "lutris", "games")
	write(t, j(lutrisDir, "league-of-legends-1700000000.yml"), "game:\n  exe: "+j(other, "lutris", "league", "drive_c", "x.exe")+
		"\n  prefix: '"+j(other, "lutris", "league")+"'\nwine:\n  version: lutris-ge\n")
	write(t, j(lutrisDir, "overwatch-1.yml"), "game:\n  prefix: \""+j(home, "Games", "overwatch")+"\"\n") // also matched by the glob
	write(t, j(lutrisDir, "relative.yml"), "game:\n  prefix: Games/x\n")
	write(t, j(lutrisDir, "notes.txt"), "prefix: "+other)

	heroicDir := j(home, "heroic", "GamesConfig")
	write(t, j(heroicDir, "Sugar.json"), `{"Sugar": {"winePrefix": "`+slash(j(other, "heroic", "Rocket League"))+`", "wineVersion": {}}}`)
	write(t, j(heroicDir, "broken.json"), `{"x": `)
	write(t, j(heroicDir, "noprefix.json"), `{"x": {"autoInstallDxvk": true}}`)

	epic := mkdir(t, home, "Games", "Heroic", "rocketleague")
	legendary := j(home, "legendary", "installed.json")
	write(t, legendary, `{"Sugar": {"app_name": "Sugar", "install_path": "`+slash(epic)+`", "title": "Rocket League"},
		"Fortnite": {"install_path": "`+slash(j(home, "Games", "Heroic", "Fortnite"))+`"}, "bad": {"install_path": 5}}`)

	s := NewSystem(nil, Locations{
		Steam:     []string{steam},
		Prefixes:  []string{j(home, ".wine"), j(home, "Games", "*"), j(home, "nothing", "*")},
		Lutris:    []string{lutrisDir, j(home, "missing")},
		Heroic:    []string{heroicDir},
		Legendary: []string{legendary, j(home, "missing.json")},
	})
	got := s.WinePrefixes()
	want := []string{plain, lutrisGame, proton, lutrisElsewhere, heroicPfx}
	if !slices.Equal(got, want) {
		t.Errorf("WinePrefixes =\n%q\nwant\n%q", got, want)
	}
	if got := s.LauncherInstalls(); !slices.Equal(got, []string{epic}) {
		t.Errorf("LauncherInstalls = %q, want only the existing %q", got, epic)
	}

	lib := Library{Steam: s.SteamLibraries(), DriveC: got, Installs: s.LauncherInstalls()}
	for _, c := range []struct {
		path string
		want bool
	}{
		{`C:\Riot Games\League of Legends`, false},
		{`C:\Program Files\Epic Games\rocketleague`, true}, // Legendary's folder
		{`C:\Program Files\Epic Games\Fortnite`, false},    // listed, but deleted
	} {
		drive, parts, _ := splitWindowsPath(c.path)
		if got := lib.hasWindowsPath(drive, parts); got != c.want {
			t.Errorf("hasWindowsPath(%s) = %v", c.path, got)
		}
	}
	mkdir(t, other, "lutris", "league", "drive_c", "Riot Games", "League of Legends")
	drive, parts, _ := splitWindowsPath(`C:\riot games\LEAGUE OF LEGENDS`)
	if !lib.hasWindowsPath(drive, parts) {
		t.Error("a game inside a Lutris prefix, named in another case, not found")
	}
	drive, parts, _ = splitWindowsPath(`D:\Riot Games\League of Legends`)
	if lib.hasWindowsPath(drive, parts) {
		t.Error("a D: path matched inside drive_c")
	}
}

func TestSplitWindowsPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\Riot Games\VALORANT`: "c|Riot Games|VALORANT",
		`d:/Games//x/`:           "d|Games|x",
		`C:\`:                    "c",
		`C:\Games\..\Windows`:    "",
		`/usr/games`:             "",
		`C:Games`:                "",
		`1:\x`:                   "",
	} {
		drive, parts, ok := splitWindowsPath(in)
		got := ""
		if ok {
			got = strings.Join(append([]string{string(drive)}, parts...), "|")
		}
		if got != want {
			t.Errorf("splitWindowsPath(%q) = %q, want %q", in, got, want)
		}
	}
	if (Library{DriveC: []string{t.TempDir()}}).hasWindowsPath('c', nil) {
		t.Error("a bare C: counted as an installed game")
	}
}

func TestLauncherFileParsers(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "pfx")
	if got := lutrisPrefixes([]byte("game:\n  prefix: " + abs + "\n  prefix:\n  prefix: \"rel\"\n")); !slices.Equal(got, []string{abs}) {
		t.Errorf("lutrisPrefixes = %q", got)
	}
	if got := heroicPrefixes([]byte(`[1, 2]`)); got != nil {
		t.Errorf("heroicPrefixes of a list = %q", got)
	}
	if got := legendaryInstalls([]byte(`not json`)); got != nil {
		t.Errorf("legendaryInstalls of junk = %q", got)
	}
	big := filepath.Join(t.TempDir(), "huge.json")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(maxManifest + 1)
	f.Close()
	if _, err := readSmall(big); err == nil {
		t.Error("an oversized launcher file was read")
	}
}

func TestUnixLocations(t *testing.T) {
	j := filepath.Join
	home := j(string(filepath.Separator)+"home", "player")
	env := func(vars map[string]string) func(string) string { return func(k string) string { return vars[k] } }

	lin := UnixLocations("linux", home, env(nil))
	if !slices.Contains(lin.Steam, j(home, ".local", "share", "Steam")) || !slices.Contains(lin.Steam, j(home, ".steam", "steam")) ||
		!slices.Contains(lin.Steam, j(home, ".var", "app", "com.valvesoftware.Steam", ".local", "share", "Steam")) {
		t.Errorf("Linux Steam folders: %q", lin.Steam)
	}
	if !slices.Contains(lin.Legendary, j(home, ".config", "heroic", "legendaryConfig", "legendary", "installed.json")) ||
		!slices.Contains(lin.Heroic, j(home, ".var", "app", "com.heroicgameslauncher.hgl", "config", "heroic", "GamesConfig")) ||
		!slices.Contains(lin.Lutris, j(home, ".config", "lutris", "games")) || !slices.Contains(lin.Prefixes, j(home, ".wine")) {
		t.Errorf("Linux launchers: %+v", lin)
	}
	xdg := t.TempDir()
	custom := UnixLocations("linux", home, env(map[string]string{"XDG_CONFIG_HOME": xdg, "XDG_DATA_HOME": "relative/is/ignored"}))
	if !slices.Contains(custom.Lutris, j(xdg, "lutris", "games")) || !slices.Contains(custom.Steam, j(home, ".local", "share", "Steam")) {
		t.Errorf("XDG folders: %+v", custom)
	}

	mac := UnixLocations("darwin", home, nil)
	if !slices.Equal(mac.Steam, []string{j(home, "Library", "Application Support", "Steam")}) ||
		!slices.Contains(mac.Prefixes, j(home, "Library", "Application Support", "CrossOver", "Bottles", "*")) || mac.Lutris != nil {
		t.Errorf("macOS: %+v", mac)
	}
	if l := UnixLocations("linux", "", nil); l.Steam != nil || l.Prefixes != nil {
		t.Errorf("no home: %+v", l)
	}
	if l := UnixLocations("plan9", home, nil); l.Steam != nil {
		t.Errorf("unknown OS: %+v", l)
	}
}

type fakeTable struct {
	progs []owner.Program
	err   error
}

func (f fakeTable) Programs() ([]owner.Program, error) { return f.progs, f.err }

func TestSystemWithProcessTable(t *testing.T) {
	exe := filepath.Join(mkdir(t, t.TempDir(), "bin"), "RocketLeague.exe")
	write(t, exe, "MZ")
	s := NewSystem(fakeTable{progs: []owner.Program{
		{PID: 10, Name: "RocketLeague.exe", Names: []string{"RocketLeague.exe", "RocketLeague.ex", "wine64-preloader"}, Exe: exe},
		{PID: 11, Name: "cs2", Names: []string{"cs2"}, Exe: filepath.Join(t.TempDir(), "gone")},
		{PID: 12, Name: "dota2", Names: []string{"dota2"}},
	}}, Locations{})
	run := s.Running()
	for _, n := range []string{"rocketleague.exe", "rocketleague.ex", "wine64-preloader", "cs2", "dota2"} {
		if !run[n] {
			t.Errorf("Running lacks %q: %v", n, run)
		}
	}
	if len(run) != 5 {
		t.Errorf("Running = %v", run)
	}
	if got := s.RunningExe([]string{"rocketleague.EXE"}); got != exe {
		t.Errorf("RunningExe = %q, want %q", got, exe)
	}
	if got := s.RunningExe([]string{"cs2", "dota2", "nope"}); got != "" {
		t.Errorf("RunningExe of a removed or unknown file = %q", got)
	}

	broken := NewSystem(fakeTable{err: errors.New("no /proc")}, Locations{})
	if run := broken.Running(); run == nil || len(run) != 0 || broken.RunningExe([]string{"cs2"}) != "" {
		t.Errorf("unreadable process table: %v", run)
	}
}

// ~/.steam/steam is a link to ~/.local/share/Steam: one library, not two.
func TestSteamLinkedTwiceListedOnce(t *testing.T) {
	home := t.TempDir()
	real := filepath.Join(home, ".local", "share", "Steam")
	common := mkdir(t, real, "steamapps", "common")
	mkdir(t, real, "appcache")
	mkdir(t, home, ".steam")
	link := filepath.Join(home, ".steam", "steam")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks need rights here: %v", err)
	}
	s := NewWithSteam([]string{link, real})
	if got := s.SteamLibraries(); len(got) != 1 || dirKey(got[0]) != dirKey(common) {
		t.Errorf("SteamLibraries = %q", got)
	}
	if got := s.SteamRoots(); len(got) != 1 {
		t.Errorf("SteamRoots = %q", got)
	}
}
