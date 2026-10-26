//go:build !windows

package platform

import (
	"os"
	"runtime"

	"lowpath/internal/compat"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
)

// Native is the Linux and macOS platform. Proxy mode, relays, measurements
// and the UI work as on Windows; the socket owners and running programs
// come from /proc (Linux) or libproc (macOS), and games are found in Steam
// libraries, Wine and Proton prefixes and Heroic's installs. The conflict
// detector sees processes, network adapters and the internet route. Route
// mode captures through a TUN device on Linux (nativeCapture); on macOS it
// reports that capture isn't available yet (issue #42). The Wi-Fi signal
// comes from /proc/net/wireless on Linux; macOS has no Wi-Fi reader yet.
func Native() *Platform {
	owners := owner.NewTable()
	home, _ := os.UserHomeDir() // none: no game folders to look in
	games := detect.NewSystem(owners, detect.UnixLocations(runtime.GOOS, home, os.Getenv))
	return &Platform{
		OS:         runtime.GOOS,
		Capture:    nativeCapture(owners),
		Owners:     owners,
		Privileges: NewRoot(),
		Games:      games,
		Icons:      games,
		Paths:      NewUnixPaths(),
		Flows:      detect.NewKernelNetwork(),
		Inventory:  compat.NewSystem(games, diag.NewNetStats()),
		Service:    NewServiceManager(),
		// The socket's folder is root's, so only a root service can take the name.
		ServiceAPI: NewSocketAPI(DefaultSocketPath, 0o666),
		WiFi:       diag.NewWLAN(),
	}
}
