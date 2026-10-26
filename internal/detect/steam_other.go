//go:build !windows

package detect

import (
	"os"
	"runtime"

	"lowpath/internal/owner"
)

// New returns the detector for this operating system: the process table
// from internal/owner (/proc on Linux, libproc on macOS) and the usual
// Steam, Wine and launcher folders under the home folder.
func New() *System {
	home, _ := os.UserHomeDir() // none: no folders to look in
	return NewSystem(defaultProcs(), UnixLocations(runtime.GOOS, home, os.Getenv))
}

func defaultProcs() owner.ProcessTable { return owner.NewTable() }
