package detect

import (
	"os"
	"path/filepath"

	"lowpath/internal/owner"
)

// New returns the detector for this operating system.
func New() *System { return NewWithSteam(steamBases()) }

// defaultProcs is nil: Windows reads its Toolhelp32 snapshot directly.
func defaultProcs() owner.ProcessTable { return nil }

// steamBases are where the Steam client installs on Windows.
func steamBases() []string {
	var out []string
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
		if v := os.Getenv(env); v != "" {
			out = append(out, filepath.Join(v, "Steam"))
		}
	}
	return out
}
