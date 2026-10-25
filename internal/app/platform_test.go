package app

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// Elevated, the driver may only come from Program Files: a folder next to the
// exe (often Downloads) is writable by any program the user runs.
func TestDriverDirsElevatedUseProgramFilesOnly(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	beside := filepath.Join(filepath.Dir(exe), "driver")

	admin, _ := driverDirs(true)
	if slices.Contains(admin, beside) {
		t.Errorf("elevated driver folders %q include the exe's folder", admin)
	}
	user, _ := driverDirs(false)
	if !slices.Contains(user, beside) {
		t.Errorf("unelevated driver folders %q lack the fallback next to the exe", user)
	}
	if runtime.GOOS != "windows" {
		return
	}
	// The shell's answer, whatever %ProgramFiles% says.
	t.Setenv("ProgramFiles", t.TempDir())
	admin, errs := driverDirs(true)
	if len(admin) != 1 || len(errs) != 0 || !strings.HasSuffix(strings.ToLower(admin[0]), `\program files\lowpath\driver`) {
		t.Errorf("elevated driver folders %q (%v), want only <Program Files>\\Lowpath\\driver", admin, errs)
	}
}
