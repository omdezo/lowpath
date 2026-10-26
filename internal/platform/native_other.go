//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
)

// Native is the Linux and macOS platform. Proxy mode, relays, measurements
// and the UI work as on Windows; the socket owners and running programs
// come from /proc (Linux) or libproc (macOS), and games are found in Steam
// libraries, Wine and Proton prefixes and Heroic's installs. The conflict
// detector sees processes, network adapters and the internet route. Route mode
// reports that capture isn't available here until the TUN backends land
// (issues #41 and #42).
func Native() *Platform {
	owners := owner.NewTable()
	home, _ := os.UserHomeDir() // none: no game folders to look in
	games := detect.NewSystem(owners, detect.UnixLocations(runtime.GOOS, home, os.Getenv))
	return &Platform{
		OS:         runtime.GOOS,
		Capture:    capture.NewUnavailable(fmt.Errorf("capture: packet capture is not available on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)),
		Owners:     owners,
		Privileges: NewRoot(),
		Games:      games,
		Icons:      games,
		Paths:      NewUnixPaths(),
		Flows:      detect.NewKernelNetwork(),
		Inventory:  compat.NewSystem(games, diag.NewNetStats()),
	}
}
