package platform

import (
	"slices"
	"strings"
	"testing"
)

// The shell's answer, whatever %ProgramFiles% says: the environment of an
// elevated process comes partly from the user's own settings.
func TestWindowsDriverDirsIgnoreEnvironment(t *testing.T) {
	t.Setenv("ProgramFiles", t.TempDir())
	dirs, errs := NewWindowsPaths().DriverDirs(true)
	if len(dirs) != 1 || len(errs) != 0 || !strings.HasSuffix(strings.ToLower(dirs[0]), `\program files\lowpath\driver`) {
		t.Errorf("elevated driver folders %q (%v), want only <Program Files>\\Lowpath\\driver", dirs, errs)
	}
	user, _ := NewWindowsPaths().DriverDirs(false)
	if len(user) != 2 || !slices.Contains(user, dirs[0]) {
		t.Errorf("unelevated driver folders %q, want Program Files then the exe's folder", user)
	}
}

func TestUACReportsTokenElevation(t *testing.T) {
	u := NewUAC()
	if !u.CanElevate() {
		t.Error("UAC can't elevate")
	}
	t.Logf("elevated: %v", u.Elevated()) // the maintainer's CI runs unelevated
}
