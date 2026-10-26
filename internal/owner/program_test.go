package owner

import (
	"reflect"
	"testing"
)

func TestProgramNames(t *testing.T) {
	env := func(pairs ...string) func(string) string { return envLookup(pairs) }
	for _, c := range []struct {
		name string
		in   procFacts
		want Program
	}{
		{"native", procFacts{pid: 1, comm: "dota2", exe: "/games/dota 2 beta/game/bin/linuxsteamrt64/dota2", argv0: "./dota2"},
			Program{PID: 1, Name: "dota2", Names: []string{"dota2"}, Exe: "/games/dota 2 beta/game/bin/linuxsteamrt64/dota2"}},
		{"wine without a prefix variable uses ~/.wine", procFacts{pid: 2, comm: "TslGame.exe", exe: "/usr/bin/wine64-preloader",
			argv0: `C:\Program Files\PUBG\TslGame.exe`, env: env("HOME=/home/player")},
			Program{PID: 2, Name: "TslGame.exe", Names: []string{"TslGame.exe", "wine64-preloader"},
				Exe: "/home/player/.wine/dosdevices/c:/Program Files/PUBG/TslGame.exe"}},
		{"wine on the Z: drive, environment unreadable", procFacts{pid: 3, comm: "deadlock.exe", argv0: `Z:\home\player\deadlock.exe`},
			Program{PID: 3, Name: "deadlock.exe", Names: []string{"deadlock.exe"}}},
		{"wine with forward slashes", procFacts{pid: 4, comm: "Overwatch.exe", exe: "/opt/wine/bin/wine", argv0: "c:/Overwatch/_retail_/Overwatch.exe",
			env: env("WINEPREFIX=/pfx", "WINEPREFIX=/ignored")},
			Program{PID: 4, Name: "Overwatch.exe", Names: []string{"Overwatch.exe", "wine"}, Exe: "/pfx/dosdevices/c:/Overwatch/_retail_/Overwatch.exe"}},
		{"wine, environment without HOME", procFacts{pid: 5, comm: "x.exe", argv0: `D:\x.exe`, env: env("PATH=/bin")},
			Program{PID: 5, Name: "x.exe", Names: []string{"x.exe"}}},
		{"kernel thread", procFacts{pid: 6, comm: "kworker/0:1"},
			Program{PID: 6, Name: "kworker/0:1", Names: []string{"kworker/0:1"}}},
		{"argv0 unrelated to the short name", procFacts{pid: 7, comm: "bash", argv0: "-bash"},
			Program{PID: 7, Name: "bash", Names: []string{"bash", "-bash"}}},
		{"case differences listed once", procFacts{pid: 8, comm: "Game", exe: "/g/game", argv0: "/g/GAME"},
			Program{PID: 8, Name: "game", Names: []string{"game"}, Exe: "/g/game"}},
		{"a name with spaces under Wine", procFacts{pid: 9, comm: "League of Legen", argv0: `C:\Riot Games\League of Legends\League of Legends.exe`},
			Program{PID: 9, Name: "League of Legends.exe", Names: []string{"League of Legends.exe", "League of Legen"}}},
	} {
		if got := c.in.program(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestWinePath(t *testing.T) {
	for in, want := range map[string]bool{
		`C:\x.exe`: true, `z:/home/x.exe`: true, `C:`: false, `C:x`: false, `/usr/bin/x`: false,
		`1:\x`: false, `\\server\share\x.exe`: false, "": false,
	} {
		if got := winePath(in); got != want {
			t.Errorf("winePath(%q) = %v", in, got)
		}
	}
}
