package platform

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testExe = "/home/p/Apps/Lowpath/lowpath"

func testPolkit(x *fakeExec, m mountFlags, caps *bool) *Polkit {
	return &Polkit{
		Root:   Root{euid: func() int { return 1000 }},
		x:      x,
		exe:    func() (string, error) { return testExe, nil },
		mount:  func(string) (mountFlags, error) { return m, nil },
		getcap: func(string) bool { return *caps },
	}
}

// Route mode's rights on Linux: one polkit prompt gives the program file
// CAP_NET_ADMIN and CAP_NET_RAW (setcap, run as root by pkexec, both by
// absolute path), then the program starts again as the same user with the
// arguments it was given.
func TestPolkitGrantsCapabilitiesAndRestarts(t *testing.T) {
	x := newFakeExec("/usr/bin/pkexec", "/usr/sbin/setcap")
	caps := false
	x.run = func(c command) (int, string) { caps = true; return 0, "" }
	p := testPolkit(x, mountFlags{}, &caps)
	if !p.CanElevate() {
		t.Fatalf("CanElevate false: %v", p.why())
	}
	args := []string{"-data", "/home/p/.config/Lowpath", "-wait-pid", "42"}
	if err := p.Relaunch(args); err != nil {
		t.Fatal(err)
	}
	if len(x.runs) != 1 || x.runs[0].path != "/usr/bin/pkexec" || !slices.Equal(x.runs[0].args, []string{"/usr/sbin/setcap", "cap_net_admin,cap_net_raw+ep", testExe}) {
		t.Errorf("prompt ran %v", x.runs)
	}
	if len(x.spawned) != 1 || x.spawned[0].path != testExe || !slices.Equal(x.spawned[0].args, args) {
		t.Errorf("restarted as %v", x.spawned)
	}

	// Already granted (the prompt was answered, the copy failed to bind):
	// just start again.
	x.runs, x.spawned = nil, nil
	if err := p.Relaunch(args); err != nil || len(x.runs) != 0 || len(x.spawned) != 1 {
		t.Errorf("with the capabilities on the file: %v, runs %v, spawned %v", err, x.runs, x.spawned)
	}
}

func TestPolkitAnswers(t *testing.T) {
	for _, c := range []struct {
		name      string
		code      int
		stderr    string
		cancelled bool
		want      string
	}{
		{"dialog dismissed", 126, "Error executing command as another user: Request dismissed", true, ""},
		{"not authorized", 127, "Error executing command as another user: Not authorized", false, "polkit said no"},
		{"setcap fails", 1, "Failed to set capabilities on file `x' (Operation not supported)", false, "Operation not supported"},
	} {
		x := newFakeExec("/usr/bin/pkexec", "/sbin/setcap")
		x.run = func(command) (int, string) { return c.code, c.stderr }
		caps := false
		err := testPolkit(x, mountFlags{}, &caps).Relaunch(nil)
		if errors.Is(err, ErrElevateCancelled) != c.cancelled || (c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want))) {
			t.Errorf("%s: %v", c.name, err)
		}
		if len(x.spawned) != 0 {
			t.Errorf("%s: restarted anyway", c.name)
		}
	}

	// setcap says yes but the file has no capabilities (a filesystem
	// without extended attributes): don't restart into the same problem.
	x := newFakeExec("/usr/bin/pkexec", "/usr/sbin/setcap")
	caps := false
	if err := testPolkit(x, mountFlags{}, &caps).Relaunch(nil); err == nil || len(x.spawned) != 0 {
		t.Errorf("capabilities missing after setcap: %v, spawned %v", err, x.spawned)
	}
}

// No prompt where its answer couldn't take effect or there's nothing to
// show it with: the UI offers proxy mode instead.
func TestPolkitCantElevate(t *testing.T) {
	caps := false
	for name, p := range map[string]*Polkit{
		"no pkexec":  testPolkit(newFakeExec("/usr/sbin/setcap"), mountFlags{}, &caps),
		"no setcap":  testPolkit(newFakeExec("/usr/bin/pkexec"), mountFlags{}, &caps),
		"nosuid":     testPolkit(newFakeExec("/usr/bin/pkexec", "/usr/sbin/setcap"), mountFlags{noSUID: true}, &caps),
		"read-only":  testPolkit(newFakeExec("/usr/bin/pkexec", "/usr/sbin/setcap"), mountFlags{readOnly: true}, &caps),
		"setcap≠abs": testPolkit(newFakeExec("/usr/bin/pkexec", "setcap"), mountFlags{}, &caps),
	} {
		if p.CanElevate() {
			t.Errorf("%s: CanElevate", name)
		}
		if err := p.Relaunch(nil); err == nil {
			t.Errorf("%s: Relaunch worked", name)
		}
		if x := p.x.(*fakeExec); len(x.runs) != 0 || len(x.spawned) != 0 {
			t.Errorf("%s: ran %v %v", name, x.runs, x.spawned)
		}
	}
}

// macOS: the administrator dialog runs the restart as root. The command
// is an argument of the script, quoted for sh word by word.
func TestAdminPrompt(t *testing.T) {
	x := newFakeExec("/usr/bin/osascript")
	a := &AdminPrompt{Root: Root{euid: func() int { return 501 }}, x: x, exe: func() (string, error) {
		return "/Applications/Lowpath.app/Contents/MacOS/lowpath", nil
	}}
	if !a.CanElevate() {
		t.Fatal("CanElevate false with osascript")
	}
	if err := a.Relaunch([]string{"-data", "/Users/p/Library/Application Support/Lowpath", "-x", "it's"}); err != nil {
		t.Fatal(err)
	}
	if len(x.runs) != 1 {
		t.Fatalf("runs %v", x.runs)
	}
	args := x.runs[0].args
	script := strings.Join(args[:len(args)-2], " ")
	if !strings.Contains(script, "with administrator privileges") || strings.Contains(script, "Lowpath.app") {
		t.Errorf("script %q: the command must not be in its source", script)
	}
	want := `'/Applications/Lowpath.app/Contents/MacOS/lowpath' '-data' '/Users/p/Library/Application Support/Lowpath' '-x' 'it'\''s' >/dev/null 2>&1 &`
	if got := args[len(args)-2]; got != want {
		t.Errorf("command\n got %s\nwant %s", got, want)
	}

	x.run = func(command) (int, string) { return 1, "execution error: User canceled. (-128)" }
	if err := a.Relaunch(nil); !errors.Is(err, ErrElevateCancelled) {
		t.Errorf("cancel: %v", err)
	}
	x.run = func(command) (int, string) {
		return 1, "execution error: The administrator user name or password was incorrect. (-60007)"
	}
	if err := a.Relaunch(nil); err == nil || errors.Is(err, ErrElevateCancelled) || !strings.Contains(err.Error(), "incorrect") {
		t.Errorf("failure: %v", err)
	}
	if (&AdminPrompt{x: newFakeExec()}).CanElevate() {
		t.Error("CanElevate without osascript")
	}
}

func TestLogBase(t *testing.T) {
	home := homeAt("/home/p")
	for _, c := range []struct {
		goos string
		env  map[string]string
		want string
	}{
		{"linux", nil, "/home/p/.local/state"},
		{"linux", map[string]string{"XDG_STATE_HOME": "/var/state/p"}, "/var/state/p"},
		{"linux", map[string]string{"XDG_STATE_HOME": "relative"}, "/home/p/.local/state"},
		{"darwin", map[string]string{"XDG_STATE_HOME": "/x"}, "/home/p/Library/Logs"},
	} {
		got, err := logBase(c.goos, env(c.env), home)
		if err != nil || filepath.ToSlash(got) != c.want {
			t.Errorf("%s %v: %q, %v; want %q", c.goos, c.env, got, err, c.want)
		}
	}
	if _, err := logBase("linux", env(nil), noHome); err == nil {
		t.Error("no home and no XDG_STATE_HOME gave a folder")
	}
}

// The app menu entry starts this program with one click and survives
// unusual paths; writing it again changes nothing.
func TestMenuEntry(t *testing.T) {
	data := t.TempDir()
	m, err := NewMenuEntry([]byte("png"), env(map[string]string{"XDG_DATA_HOME": data}), noHome)
	if err != nil {
		t.Fatal(err)
	}
	exe := "/home/p/My Games/100% Lowpath/lowpath"
	if err := m.Install(exe); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(m.Path())
	if err != nil {
		t.Fatal(err)
	}
	entry := string(b)
	for _, want := range []string{
		"[Desktop Entry]\n", "Type=Application\n", "Name=Lowpath\n", "Icon=lowpath\n", "Terminal=false\n",
		"StartupWMClass=lowpath\n", "TryExec=/home/p/My Games/100% Lowpath/lowpath\n",
		`Exec="/home/p/My Games/100%% Lowpath/lowpath"` + "\n",
	} {
		if !strings.Contains(entry, want) {
			t.Errorf("entry lacks %q:\n%s", want, entry)
		}
	}
	icon := filepath.Join(data, "icons", "hicolor", "256x256", "apps", "lowpath.png")
	if b, _ := os.ReadFile(icon); string(b) != "png" {
		t.Errorf("icon %q", b)
	}
	fi, _ := os.Stat(m.Path())
	if err := m.Install(exe); err != nil {
		t.Fatal(err)
	}
	if fi2, _ := os.Stat(m.Path()); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("unchanged entry rewritten")
	}
	// A moved program updates its entry.
	m.Install("/opt/lowpath")
	if b, _ := os.ReadFile(m.Path()); !strings.Contains(string(b), "Exec=/opt/lowpath\n") {
		t.Errorf("entry not updated:\n%s", b)
	}
	// "go run" builds are gone after they exit.
	m.Install("/tmp/go-build123/b001/exe/lowpath")
	if b, _ := os.ReadFile(m.Path()); strings.Contains(string(b), "go-build") {
		t.Error("entry points at a temporary build")
	}
	if err := m.Install("lowpath"); err == nil {
		t.Error("relative program path accepted")
	}

	// A relative $XDG_DATA_HOME is ignored, as the spec says.
	m, _ = NewMenuEntry(nil, env(map[string]string{"XDG_DATA_HOME": "rel"}), homeAt("/home/p"))
	if filepath.ToSlash(m.Path()) != "/home/p/.local/share/applications/lowpath.desktop" {
		t.Errorf("entry path %q", m.Path())
	}
	if _, err := NewMenuEntry(nil, env(nil), noHome); err == nil {
		t.Error("menu entry without a home folder")
	}
}

func TestQuoteExecArg(t *testing.T) {
	for in, want := range map[string]string{
		"/opt/lowpath":            "/opt/lowpath",
		"/home/p/My Apps/lowpath": `"/home/p/My Apps/lowpath"`,
		`/x/a"b$c` + "`d\\e":      `"/x/a\"b\$c` + "\\`d\\\\e\"",
		"/x/50%":                  "/x/50%%",
		"/x/it's":                 `"/x/it's"`,
	} {
		if got := quoteExecArg(in); got != want {
			t.Errorf("quoteExecArg(%q) = %s, want %s", in, got, want)
		}
	}
	// The string-level escapes come on top: a backslash is doubled again.
	if got := escapeString(quoteExecArg(`/x/a\b`)); got != `"/x/a\\\\b"` {
		t.Errorf("Exec value %s", got)
	}
}
