package capture

import (
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"testing"
)

type fixedElevation bool

func (e fixedElevation) Elevated() bool { return bool(e) }

type fakeDirs struct {
	dirs  []string
	errs  []error
	asked []bool // the elevated flag of each call
}

func (f *fakeDirs) DriverDirs(elevated bool) ([]string, []error) {
	f.asked = append(f.asked, elevated)
	return slices.Clone(f.dirs), slices.Clone(f.errs)
}

func newTestWinDivert(elevated bool, dirs *fakeDirs, failDirs ...string) (*WinDivert, *[]string) {
	w := NewWinDivert(newFakeOS(), fixedElevation(elevated), dirs)
	var installed []string
	w.install = func(dir string) error {
		installed = append(installed, dir)
		if slices.Contains(failDirs, dir) {
			return errors.New("access denied: " + dir)
		}
		return nil
	}
	return w, &installed
}

func TestWinDivertKeepsFirstDriverFolderThatWorks(t *testing.T) {
	dirs := &fakeDirs{dirs: []string{"pf", "beside-exe"}}
	w, installed := newTestWinDivert(false, dirs, "pf")
	dir, err := w.installDriver()
	if err != nil || dir != "beside-exe" {
		t.Fatalf("installDriver = %q, %v; want the fallback folder", dir, err)
	}
	if dir, err := w.installDriver(); err != nil || dir != "beside-exe" || len(*installed) != 2 {
		t.Fatalf("second installDriver = %q, %v after %v; want the same folder, no new writes", dir, err, *installed)
	}
	if !slices.Equal(dirs.asked, []bool{false}) {
		t.Errorf("DriverDirs asked %v, want once for an unelevated process", dirs.asked)
	}
}

func TestWinDivertNoDriverFolder(t *testing.T) {
	why := errors.New("find Program Files: not found")
	w, _ := newTestWinDivert(true, &fakeDirs{dirs: []string{"pf"}, errs: []error{why}}, "pf")
	_, err := w.installDriver()
	if !errors.Is(err, ErrNoDriver) || !errors.Is(err, why) || !strings.Contains(err.Error(), "access denied: pf") {
		t.Fatalf("installDriver = %v; want ErrNoDriver with every reason", err)
	}
	w, _ = newTestWinDivert(true, &fakeDirs{})
	if _, err := w.installDriver(); !errors.Is(err, ErrNoDriver) {
		t.Fatalf("no folders: %v, want ErrNoDriver", err)
	}
}

// Without the rights capture needs, Start fails before writing anything.
func TestWinDivertStartNeedsRights(t *testing.T) {
	w, installed := newTestWinDivert(false, &fakeDirs{dirs: []string{"pf"}})
	w.start = func(Config) (*Capture, error) { t.Fatal("started without rights"); return nil, nil }
	_, err := w.Start(Config{})
	if err == nil || err != w.Available() || len(*installed) != 0 {
		t.Fatalf("Start = %v (Available %v), installed %v", err, w.Available(), *installed)
	}
	want := ErrNeedAdmin
	switch {
	case runtime.GOOS != "windows":
		want = ErrUnsupported
	case runtime.GOARCH != "amd64":
		want = ErrNoDriver
	}
	if !errors.Is(err, want) {
		t.Fatalf("Start = %v, want %v on %s/%s", err, want, runtime.GOOS, runtime.GOARCH)
	}
	if runtime.GOOS != "windows" && !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("%v does not match errors.ErrUnsupported", err)
	}
}

func TestWinDivertStartFillsDriverAndOwners(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("WinDivert runs on 64-bit Windows only")
	}
	w, _ := newTestWinDivert(true, &fakeDirs{dirs: []string{"pf"}})
	var got Config
	w.start = func(cfg Config) (*Capture, error) { got = cfg; return nil, errors.New("stop here") }
	if _, err := w.Start(Config{Processes: []string{"game.exe"}}); err == nil || err.Error() != "stop here" {
		t.Fatalf("Start = %v", err)
	}
	if got.DriverDir != "pf" || got.Owners != w.owners {
		t.Fatalf("start got driver %q owners %v; want the installed folder and the platform resolver", got.DriverDir, got.Owners)
	}
}

func TestUnavailableBackend(t *testing.T) {
	why := errors.New("no capture here")
	var b Backend = NewUnavailable(why)
	if b.Name() != "none" || b.Available() != why {
		t.Fatalf("Unavailable: %s, %v", b.Name(), b.Available())
	}
	s, err := b.Start(Config{OnPacket: func(_, _ netip.AddrPort, _ []byte) bool { return false }})
	if s != nil || err != why {
		t.Fatalf("Start = %v, %v", s, err)
	}
}
