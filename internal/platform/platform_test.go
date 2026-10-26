package platform

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"lowpath/internal/capture"
	"lowpath/internal/diag"
)

func TestNativeIsComplete(t *testing.T) {
	p := Native()
	if err := p.Check(); err != nil {
		t.Fatal(err)
	}
	if p.OS != runtime.GOOS {
		t.Errorf("OS = %q, want %q", p.OS, runtime.GOOS)
	}
	if p.Capture.Name() == "" {
		t.Error("capture backend has no name")
	}
	if d, err := p.Paths.DataDir(); err == nil && filepath.Base(d) != appDir {
		t.Errorf("DataDir = %q, want a %s folder", d, appDir)
	}
	// Wi-Fi: this machine may have no adapter, a disconnected one or a
	// connected one; what matters is that a missing reading says why.
	if _, err := p.WiFi.Link(0); err != nil && !errors.Is(err, diag.ErrNoWiFi) && !errors.Is(err, diag.ErrWiFiDisconnected) && !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Wi-Fi link: %v, want a reading or a known reason", err)
	}
	if runtime.GOOS == "darwin" {
		if _, err := p.WiFi.Tune(0); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("Wi-Fi tuning on darwin: %v, want errors.ErrUnsupported", err)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	// Elsewhere, what isn't built yet says so instead of pretending. Linux
	// captures through a TUN device, which needs root or CAP_NET_ADMIN.
	switch err := p.Capture.Available(); runtime.GOOS {
	case "linux":
		if p.Capture.Name() != "tun" || (err != nil && !errors.Is(err, capture.ErrNeedAdmin) && !errors.Is(err, capture.ErrNoTUN)) {
			t.Errorf("capture on linux: %s, %v; want tun, available or needing rights", p.Capture.Name(), err)
		}
		if p.ElevationHelps() {
			t.Error("ElevationHelps without a prompt to show")
		}
	default:
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("capture on %s: %v, want errors.ErrUnsupported", runtime.GOOS, err)
		}
	}
	switch _, err := p.Owners.UDPSockets(); runtime.GOOS {
	case "linux", "darwin":
		if err != nil {
			t.Errorf("owner lookup on %s: %v", runtime.GOOS, err)
		}
	default:
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("owner lookup on %s: %v, want errors.ErrUnsupported", runtime.GOOS, err)
		}
	}
	if p.Privileges.CanElevate() {
		t.Error("CanElevate without a prompt to show")
	}
	if err := p.Privileges.Relaunch(nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("Relaunch: %v, want errors.ErrUnsupported", err)
	}
	if _, err := p.Flows.Watch(nil); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("flow watch on %s: %v, want errors.ErrUnsupported", runtime.GOOS, err)
	}
	// No Windows services or program list to look through for boosters;
	// adapters (for full-tunnel VPNs) are read.
	if _, err := p.Inventory.Services(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("services on %s: %v, want errors.ErrUnsupported", runtime.GOOS, err)
	}
	if _, err := p.Inventory.Adapters(); err != nil {
		t.Errorf("adapters on %s: %v", runtime.GOOS, err)
	}
}

func TestCheckNamesMissingParts(t *testing.T) {
	var nilP *Platform
	if nilP.Check() == nil {
		t.Error("nil platform passed")
	}
	err := (&Platform{OS: "x", Paths: newUnixLikePaths()}).Check()
	if err == nil || !strings.Contains(err.Error(), "Capture") || !strings.Contains(err.Error(), "Icons") || strings.Contains(err.Error(), "Paths") {
		t.Errorf("Check = %v", err)
	}
}

// newUnixLikePaths is any Paths value, for Check.
func newUnixLikePaths() Paths { return fakePaths{} }

type fakePaths struct{ userDirs }

func (fakePaths) DriverDirs(bool) ([]string, []error) { return nil, nil }
func (fakePaths) ServiceDataDir() (string, error)     { return "", nil }

func TestUserDirs(t *testing.T) {
	u := userDirs{
		config: func() (string, error) { return filepath.Join("home", "cfg"), nil },
		cache:  func() (string, error) { return "", errors.New("$HOME is not defined") },
	}
	if d, err := u.DataDir(); err != nil || d != filepath.Join("home", "cfg", "Lowpath") {
		t.Errorf("DataDir = %q, %v", d, err)
	}
	if _, err := u.CacheDir(); err == nil || !strings.Contains(err.Error(), "$HOME") {
		t.Errorf("CacheDir error lost: %v", err)
	}
}

// Elevated, the driver may only come from the protected folder: one next to
// the exe (often Downloads) is writable by any program the user runs.
func TestDriverDirsElevatedUseProtectedFolderOnly(t *testing.T) {
	pf := func() (string, error) { return filepath.Join("C:", "Program Files"), nil }
	exe := func() (string, error) { return filepath.Join("Downloads", "Lowpath.exe"), nil }
	inPF := filepath.Join("C:", "Program Files", "Lowpath", "driver")
	beside := filepath.Join("Downloads", "driver")

	if dirs, errs := driverDirs(true, pf, exe); !slices.Equal(dirs, []string{inPF}) || len(errs) != 0 {
		t.Errorf("elevated: %q %v, want only %q", dirs, errs, inPF)
	}
	if dirs, _ := driverDirs(false, pf, exe); !slices.Equal(dirs, []string{inPF, beside}) {
		t.Errorf("unelevated: %q, want %q then %q", dirs, inPF, beside)
	}
	broken := func() (string, error) { return "", errors.New("no shell") }
	dirs, errs := driverDirs(true, broken, exe)
	if len(dirs) != 0 || len(errs) != 1 || !strings.Contains(errs[0].Error(), "no shell") {
		t.Errorf("elevated without Program Files: %q %v; want no folder and the reason", dirs, errs)
	}
	if dirs, errs := driverDirs(false, broken, broken); len(dirs) != 0 || len(errs) != 2 {
		t.Errorf("nothing found: %q %v", dirs, errs)
	}
}

func TestDriverDirsOnThisOS(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(exe), "driver")
	dirs, errs := Native().Paths.DriverDirs(true)
	if slices.Contains(dirs, beside) {
		t.Errorf("elevated driver folders %q include the exe's folder", dirs)
	}
	if runtime.GOOS != "windows" && (len(dirs) != 0 || len(errs) == 0 || !errors.Is(errs[0], errors.ErrUnsupported)) {
		t.Errorf("driver folders on %s: %q %v, want none (unsupported)", runtime.GOOS, dirs, errs)
	}
}

type privs struct{ elevated, canElevate bool }

func (p privs) Elevated() bool        { return p.elevated }
func (p privs) CanElevate() bool      { return p.canElevate }
func (privs) Relaunch([]string) error { return nil }

// A restart prompt only helps when missing rights are capture's sole
// problem: it can't install a driver WinDivert doesn't ship (ARM64) or give
// an OS a backend it doesn't have.
func TestElevationHelps(t *testing.T) {
	for _, c := range []struct {
		name  string
		priv  privs
		avail error
		want  bool
	}{
		{"needs admin, can ask", privs{canElevate: true}, fmt.Errorf("open: %w", capture.ErrNeedAdmin), true},
		{"needs admin, no prompt", privs{}, capture.ErrNeedAdmin, false},
		{"already elevated", privs{elevated: true, canElevate: true}, capture.ErrNeedAdmin, false},
		{"capture works", privs{canElevate: true}, nil, false},
		{"no driver for this CPU", privs{canElevate: true}, capture.ErrNoDriver, false},
		{"no backend on this OS", privs{canElevate: true}, fmt.Errorf("x: %w", errors.ErrUnsupported), false},
	} {
		p := &Platform{Privileges: c.priv, Capture: capture.NewUnavailable(c.avail)}
		if got := p.ElevationHelps(); got != c.want {
			t.Errorf("%s: ElevationHelps = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNativeServiceParts(t *testing.T) {
	p := Native()
	want := DefaultSocketPath
	switch runtime.GOOS {
	case "windows":
		want = `\\.\pipe\lowpath`
	case "darwin":
		// macOS has no /run.
		if want != "/var/run/lowpath/api.sock" {
			t.Errorf("DefaultSocketPath = %q on macOS", want)
		}
	case "linux":
		if want != "/run/lowpath/api.sock" {
			t.Errorf("DefaultSocketPath = %q on Linux", want)
		}
	}
	if p.ServiceAPI.String() != want {
		t.Errorf("service API %q, want %q", p.ServiceAPI.String(), want)
	}
	if d, err := p.Paths.ServiceDataDir(); err != nil || !strings.EqualFold(filepath.Base(d), appDir) && d != "/var/lib/lowpath" {
		t.Errorf("ServiceDataDir = %q, %v", d, err)
	}
}
