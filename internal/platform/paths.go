package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// appDir is the folder name under the per-user config and cache folders.
const appDir = "Lowpath"

// userDirs answers DataDir and CacheDir from the OS's per-user folders:
// %AppData% and %LocalAppData% on Windows, ~/Library/Application Support and
// ~/Library/Caches on macOS, $XDG_CONFIG_HOME and $XDG_CACHE_HOME (or
// ~/.config, ~/.cache) elsewhere.
type userDirs struct {
	config, cache func() (string, error)
}

func newUserDirs() userDirs { return userDirs{config: os.UserConfigDir, cache: os.UserCacheDir} }

func (u userDirs) DataDir() (string, error)  { return under(u.config, "config") }
func (u userDirs) CacheDir() (string, error) { return under(u.cache, "cache") }

func under(base func() (string, error), what string) (string, error) {
	d, err := base()
	if err != nil {
		return "", fmt.Errorf("platform: no per-user %s folder: %w", what, err)
	}
	return filepath.Join(d, appDir), nil
}

// driverDirs lists where the driver may go, best first: the protected
// system folder from protected (Program Files on Windows), then, only when
// not elevated, a "driver" folder next to the program. That folder (often
// Downloads) is writable by any program the user runs, so an elevated
// process must never load a driver from it; unelevated, the driver can't be
// opened anyway and the folder only lets the files be checked.
func driverDirs(elevated bool, protected, executable func() (string, error)) (dirs []string, errs []error) {
	if pf, err := protected(); err == nil {
		dirs = append(dirs, filepath.Join(pf, appDir, "driver"))
	} else {
		errs = append(errs, fmt.Errorf("find Program Files: %w", err))
	}
	if !elevated {
		if exe, err := executable(); err == nil {
			dirs = append(dirs, filepath.Join(filepath.Dir(exe), "driver"))
		} else {
			errs = append(errs, fmt.Errorf("find the program's folder: %w", err))
		}
	}
	return dirs, errs
}
