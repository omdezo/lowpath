package platform

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func homeAt(dir string) func() (string, error) { return func() (string, error) { return dir, nil } }

func noHome() (string, error) { return "", errors.New("$HOME is not defined") }

func hasEnv(c command, kv string) bool { return slices.Contains(c.env, kv) }

// The Linux kinds, in order: WebKitGTK through the system's Python, then a
// Chromium-family browser in app mode with a profile of this run's own.
// The launch link reaches the WebKit helper in its environment, never on
// the command line other users can read.
func TestXDGKinds(t *testing.T) {
	dir := t.TempDir()
	x := newFakeExec("/usr/bin/python3", "chromium")
	w := newXDGDesktop(DesktopConfig{ProfileDir: dir, Icon: []byte("png"), Log: testLog()}, x, env(nil), homeAt(dir))
	if len(w.kinds) != 2 || w.kinds[0].name != "webkitgtk" || w.kinds[1].name != "chromium" {
		t.Fatalf("kinds %v", w.kinds)
	}
	url := "http://127.0.0.1:7878/api/session?t=secret"
	c, err := w.kinds[0].command(url)
	if err != nil {
		t.Fatal(err)
	}
	if c.path != "/usr/bin/python3" || !slices.Equal(c.args[:2], []string{"-I", "-c"}) || !strings.Contains(c.args[2], "WebKit") {
		t.Errorf("webkit command %s", c)
	}
	if strings.Contains(strings.Join(c.args, " "), "secret") || !hasEnv(c, "LOWPATH_URL="+url) {
		t.Errorf("launch link not passed in the environment only: args %q env %q", c.args, c.env)
	}
	if !hasEnv(c, "LOWPATH_ICON="+filepath.Join(dir, "lowpath.png")) {
		t.Errorf("no icon in %q", c.env)
	}

	cc, err := w.kinds[1].command("http://127.0.0.1:7878/")
	if err != nil {
		t.Fatal(err)
	}
	if cc.path != "/usr/bin/chromium" || !slices.Contains(cc.args, "--app=http://127.0.0.1:7878/") || !slices.Contains(cc.args, "--class=lowpath") {
		t.Errorf("chromium command %s", cc)
	}
	profile := strings.TrimPrefix(cc.args[slices.IndexFunc(cc.args, func(a string) bool { return strings.HasPrefix(a, "--user-data-dir=") })], "--user-data-dir=")
	if filepath.Dir(profile) != filepath.Join(dir, "chromium") {
		t.Errorf("profile %q, want one under the window profile folder", profile)
	}
	// ShowWindow reuses the same profile, so the running browser takes it.
	again, _ := w.kinds[1].command("http://127.0.0.1:7878/x")
	if !slices.Contains(again.args, "--user-data-dir="+profile) {
		t.Errorf("second window with another profile: %s", again)
	}
	os.MkdirAll(profile, 0o700)
	w.kinds[1].cleanup()
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Error("profile left after the windows closed")
	}

	// Nothing installed: each kind says what is missing.
	none := newXDGDesktop(DesktopConfig{ProfileDir: dir}, newFakeExec(), env(nil), homeAt(dir))
	for _, k := range none.kinds {
		if _, err := k.command("u"); err == nil {
			t.Errorf("%s available with nothing installed", k.name)
		}
	}
}

// NVIDIA's driver needs WebKitGTK's DMA-BUF renderer off, unless the user
// set the variable themselves.
func TestWebKitGTKNvidia(t *testing.T) {
	x := newFakeExec("python3")
	k := webkitGTKKind(DesktopConfig{}, x, env(nil))
	c, _ := k.command("u")
	if c.path != "/usr/bin/python3" {
		t.Errorf("python from PATH: %s", c.path)
	}
	if fileExists("/proc/driver/nvidia/version") != hasEnv(c, "WEBKIT_DISABLE_DMABUF_RENDERER=1") {
		t.Errorf("DMA-BUF switch %q doesn't follow the NVIDIA driver", c.env)
	}
	k = webkitGTKKind(DesktopConfig{}, x, env(map[string]string{"WEBKIT_DISABLE_DMABUF_RENDERER": "0"}))
	if c, _ := k.command("u"); slices.ContainsFunc(c.env, func(e string) bool { return strings.HasPrefix(e, "WEBKIT_DISABLE_DMABUF_RENDERER") }) {
		t.Errorf("user's own setting overridden: %q", c.env)
	}
}

// A snap browser (Ubuntu's Chromium) may not write to ~/.cache, so its
// profile goes in its snap folder.
func TestSnapChromiumProfile(t *testing.T) {
	home := t.TempDir()
	x := newFakeExec("/snap/bin/chromium")
	k := xdgChromiumKind(DesktopConfig{ProfileDir: filepath.Join(home, ".cache", "Lowpath", "window")}, &snapExec{x}, homeAt(home))
	c, err := k.command("u")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "snap", "chromium", "common", "lowpath-window")
	if !slices.ContainsFunc(c.args, func(a string) bool { return strings.HasPrefix(a, "--user-data-dir="+want) }) {
		t.Errorf("snap profile not under %s: %s", want, c)
	}
	k = xdgChromiumKind(DesktopConfig{}, &snapExec{x}, noHome)
	if _, err := k.command("u"); err == nil {
		t.Error("snap browser without a home folder")
	}
}

type snapExec struct{ *fakeExec }

func (s *snapExec) LookPath(name string) (string, error) {
	if name == "chromium" {
		return "/snap/bin/chromium", nil
	}
	return s.fakeExec.LookPath(name)
}

func TestSnapName(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "chromium-browser")
	os.WriteFile(wrapper, []byte("#!/bin/sh\n# transitional package\nexec /snap/bin/chromium \"$@\"\n"), 0o755)
	plain := filepath.Join(dir, "google-chrome")
	os.WriteFile(plain, []byte("#!/bin/bash\nexec -a \"$0\" /opt/google/chrome/chrome \"$@\"\n"), 0o755)
	binary := filepath.Join(dir, "brave")
	os.WriteFile(binary, []byte("\x7fELF..."), 0o755)
	for path, want := range map[string]string{
		"/snap/bin/chromium":                   "chromium",
		"/snap/bin/brave.browser":              "brave",
		"/snap/chromium/2900/usr/lib/chromium": "chromium",
		wrapper:                                "chromium",
		plain:                                  "",
		binary:                                 "",
		filepath.Join(dir, "missing"):          "",
	} {
		if got := snapName(path); got != want {
			t.Errorf("snapName(%s) = %q, want %q", path, got, want)
		}
	}
}

// Profiles of runs that are gone are removed; this run's and a running
// one's stay.
func TestRunProfileCleansLeftovers(t *testing.T) {
	base := t.TempDir()
	for _, name := range []string{"999999999", "not-a-pid"} {
		os.MkdirAll(filepath.Join(base, name), 0o700)
	}
	mine := runProfile(base)
	if filepath.Base(mine) != strconv.Itoa(os.Getpid()) {
		t.Errorf("profile %q not named after this process", mine)
	}
	if _, err := os.Stat(filepath.Join(base, "999999999")); !os.IsNotExist(err) {
		t.Error("a dead run's profile was kept")
	}
	if _, err := os.Stat(filepath.Join(base, "not-a-pid")); err != nil {
		t.Error("a folder that isn't a run's profile was removed")
	}
}

func itoa(n int) string {
	return strings.TrimLeft(strings.Repeat(" ", 0)+string(rune('0'+0)), "0") + fmtInt(n)
}

func fmtInt(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return fmtInt(n/10) + string(rune('0'+n%10))
}

// macOS: the WKWebView helper (osascript) first, loading localhost (App
// Transport Security exempts it, not 127.0.0.1), then Chromium-family
// browsers in ~/Applications or /Applications.
func TestMacKinds(t *testing.T) {
	home := t.TempDir()
	x := newFakeExec("/usr/bin/osascript", "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
	w := newMacDesktop(DesktopConfig{ProfileDir: home, Log: testLog()}, x, homeAt("/Users/p"))
	c, err := w.kinds[0].command("http://127.0.0.1:7878/api/session?t=secret")
	if err != nil {
		t.Fatal(err)
	}
	if c.path != "/usr/bin/osascript" || !slices.Equal(c.args[:2], []string{"-l", "JavaScript"}) || !strings.Contains(c.args[3], "WKWebView") {
		t.Errorf("wkwebview command %s", c)
	}
	if !hasEnv(c, "LOWPATH_URL=http://localhost:7878/api/session?t=secret") || strings.Contains(strings.Join(c.args, " "), "secret") {
		t.Errorf("launch link: args %q env %q", c.args, c.env)
	}
	cc, err := w.kinds[1].command("u")
	if err != nil {
		t.Fatal(err)
	}
	if cc.path != "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" {
		t.Errorf("mac chromium at %q", cc.path)
	}
	if w.browser("u").path != "/usr/bin/open" {
		t.Error("browser isn't open(1)")
	}
	if d := w.dialog("Lowpath", `it said "no"`); len(d) != 1 || d[0].args[len(d[0].args)-1] != `it said "no"` {
		t.Errorf("dialog command %v: the message must be an argument, not script source", d)
	}
}
