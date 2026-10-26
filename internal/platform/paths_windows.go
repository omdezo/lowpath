package platform

import (
	"os"

	"golang.org/x/sys/windows"
)

// WindowsPaths keeps the driver under Program Files, resolved through the
// shell's known folder (FOLDERID_ProgramFiles) rather than %ProgramFiles%:
// the environment of an elevated process comes partly from the user's own,
// writable settings.
type WindowsPaths struct {
	userDirs
	programFiles func() (string, error)
	executable   func() (string, error)
}

func NewWindowsPaths() *WindowsPaths {
	return &WindowsPaths{
		userDirs:     newUserDirs(),
		programFiles: func() (string, error) { return windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0) },
		executable:   os.Executable,
	}
}

func (p *WindowsPaths) DriverDirs(elevated bool) ([]string, []error) {
	return driverDirs(elevated, p.programFiles, p.executable)
}
