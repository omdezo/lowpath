package platform

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gi is a stand-in for PyGObject's gi module with the same require_version
// rules: a version is recorded as soon as it is available, and asking for
// another version of a recorded namespace fails ("already requires").
const fakeGI = `import json, sys, types
available = json.loads(sys.argv[2])
gi = types.ModuleType("gi")
versions = {}
def require_version(ns, v):
    if ns in versions and versions[ns] != v:
        raise ValueError("Namespace %s already requires version %s" % (ns, versions[ns]))
    if v not in available.get(ns, []):
        raise ValueError("Namespace %s not available for version %s" % (ns, v))
    versions[ns] = v
gi.require_version = require_version
repo = types.ModuleType("gi.repository")
for ns in ("GLib", "Gtk", "Gdk", "WebKit", "WebKit2"):
    setattr(repo, ns, types.SimpleNamespace(name=ns))
gi.repository = repo
sys.modules["gi"], sys.modules["gi.repository"] = gi, repo
script = {"__name__": "webkitgtk"}
exec(compile(open(sys.argv[1]).read(), "webkitgtk.py", "exec"), script)
gtk, webkit = script["load"]()
print(gtk, webkit.name if webkit else None)
`

// python finds a Python 3 to run the window script's logic with, or skips:
// the script itself is only ever run by the system's Python on Linux.
func python(t *testing.T) string {
	t.Helper()
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			if out, err := exec.Command(p, "-c", "import sys; print(sys.version_info[0])").Output(); err == nil && strings.TrimSpace(string(out)) == "3" {
				return p
			}
		}
	}
	t.Skip("no Python 3 to run the WebKitGTK script with")
	return ""
}

// The window script picks the newest WebKitGTK that is installed. GTK 4's
// typelib being there without WebKitGTK 6.0 (only WebKit2GTK 4.x for
// GTK 3) must still give the GTK 3 window, not "no WebKitGTK".
func TestWebKitGTKScriptPicksInstalledVersion(t *testing.T) {
	py := python(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "webkitgtk.py")
	harness := filepath.Join(dir, "harness.py")
	if err := os.WriteFile(script, []byte(webkitGTKScript), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness, []byte(fakeGI), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		installed map[string][]string
		want      string
	}{
		{"WebKitGTK 6.0", map[string][]string{"Gtk": {"3.0", "4.0"}, "WebKit": {"6.0"}, "WebKit2": {"4.1"}}, "4.0 WebKit"},
		{"GTK 4 without WebKit 6.0", map[string][]string{"Gtk": {"3.0", "4.0"}, "WebKit2": {"4.1"}}, "3.0 WebKit2"},
		{"only WebKit2GTK 4.0", map[string][]string{"Gtk": {"3.0", "4.0"}, "WebKit2": {"4.0"}}, "3.0 WebKit2"},
		{"no WebKitGTK", map[string][]string{"Gtk": {"3.0", "4.0"}}, "None None"},
	} {
		avail, _ := json.Marshal(c.installed)
		cmd := exec.Command(py, "-I", harness, script, string(avail))
		cmd.Env = append(os.Environ(), "LOWPATH_URL=")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != c.want {
			t.Errorf("%s: load() = %q, want %q", c.name, got, c.want)
		}
	}
}
