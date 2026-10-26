// Package platform is the operating-system layer under the app: packet
// capture, socket-owner lookup, privileges, game detection, program icons
// and folders, each behind a small interface.
//
// Native returns the implementation for the OS the binary is built for,
// chosen at build time (native_windows.go, native_other.go). Everything
// above this package (app, engine, UI) is the same on every OS; a part that
// is not implemented on an OS yet reports an error matching
// errors.ErrUnsupported, so the UI can say "not available here" instead of
// showing a broken feature. Tests use fakeplatform to run the whole app
// without admin rights, drivers or real games, on any OS.
package platform

import (
	"errors"
	"fmt"
	"image"

	"lowpath/internal/capture"
	"lowpath/internal/compat"
	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/owner"
)

// CaptureBackend takes the selected game's packets off the network and puts
// replies back (route mode).
type CaptureBackend = capture.Backend

// CaptureSession is a running capture.
type CaptureSession = capture.Session

// OwnerResolver maps local UDP sockets to processes.
type OwnerResolver = owner.Resolver

// Privileges is the privilege helper: whether this process has the rights
// route mode needs, and how to get them.
type Privileges interface {
	// Elevated reports whether the process runs as administrator (Windows)
	// or root.
	Elevated() bool
	// CanElevate reports whether Relaunch can ask the user for the rights
	// (a UAC prompt on Windows).
	CanElevate() bool
	// Relaunch starts this program again with args, elevated. It returns once
	// the new process is starting, an error matching ErrElevateCancelled when
	// the user declines, or one matching errors.ErrUnsupported when the
	// system has no way to ask.
	Relaunch(args []string) error
}

// GameDetector finds installed and running games.
type GameDetector interface {
	// Running returns the lower-cased image names of all running processes.
	Running() map[string]bool
	// RunningExe returns the full path of a running process with one of the
	// given image names, or "" when none runs or the OS won't tell.
	RunningExe(names []string) string
	// SteamLibraries returns every steamapps/common folder found.
	SteamLibraries() []string
	// SteamRoots returns the Steam client folders that have an appcache.
	SteamRoots() []string
	// WinePrefixes returns the drive_c folders of the Wine and Proton
	// prefixes found, where Windows games live on Linux and macOS (none on
	// Windows).
	WinePrefixes() []string
	// LauncherInstalls returns the game folders launchers such as Heroic
	// report as installed.
	LauncherInstalls() []string
}

// FlowWatcher reports which remote endpoints programs exchange packets
// with, without capturing or touching the packets, so the game page can
// name the server a game is on when Lowpath isn't routing it (and next to
// anti-cheats that refuse a packet driver).
type FlowWatcher interface {
	// Watch calls onEvent for every UDP and TCP send and receive of every
	// process until the watch is closed. onEvent runs on the OS's event
	// thread and must return quickly. Errors match detect.ErrNeedAdmin when
	// the OS wants administrator rights, or errors.ErrUnsupported on a
	// system without a watcher yet.
	Watch(onEvent func(detect.FlowEvent)) (detect.FlowWatch, error)
}

// SystemInventory reads what else on the PC handles network traffic: running
// programs, services and drivers, installed programs, network adapters and
// the internet route. The conflict detector (internal/compat) names other
// boosters, packet filter drivers and full-tunnel VPNs from it. A part the
// OS doesn't have reports errors.ErrUnsupported.
type SystemInventory = compat.System

// WiFi reads the Wi-Fi link (signal, band, rates, the access point's load)
// and pauses the adapter's background scans during a boost (diag.WLAN).
type WiFi = diag.WLAN

// IconProvider reads a program's own icon.
type IconProvider interface {
	// ExeIcon renders the program's main icon at size×size.
	ExeIcon(path string, size int) (image.Image, error)
}

// Paths are the folders Lowpath keeps its files in.
type Paths interface {
	// DataDir holds settings, relays and the log (per user).
	DataDir() (string, error)
	// CacheDir holds what can be rebuilt, such as the window's browser
	// profile (per user).
	CacheDir() (string, error)
	// LogDir holds the log and crash reports (per user): the data folder on
	// Windows, $XDG_STATE_HOME/Lowpath (~/.local/state/Lowpath) on Linux,
	// ~/Library/Logs/Lowpath on macOS.
	LogDir() (string, error)
	// DriverDirs lists where capture driver files may go, best first, and
	// why a usual folder is missing. An elevated process only gets folders
	// that ordinary users can't write to.
	DriverDirs(elevated bool) ([]string, []error)
	// ServiceDataDir holds the background service's settings, relays and
	// log (machine-wide, administrators only).
	ServiceDataDir() (string, error)
}

// Platform bundles one implementation of each interface.
type Platform struct {
	OS         string // runtime.GOOS of the implementation ("windows", "linux", "darwin")
	Capture    CaptureBackend
	Owners     OwnerResolver
	Privileges Privileges
	Games      GameDetector
	Icons      IconProvider
	Paths      Paths
	Flows      FlowWatcher
	Inventory  SystemInventory
	// Service installs and starts the background service, and ServiceAPI
	// is the channel the window and the service talk over.
	Service    ServiceManager
	ServiceAPI ServiceAPI
	WiFi       WiFi
}

// Check reports a Platform with a missing part, which would otherwise fail
// later with a nil pointer.
func (p *Platform) Check() error {
	if p == nil {
		return errors.New("platform: nil")
	}
	var missing []string
	for _, part := range []struct {
		name  string
		unset bool
	}{
		{"OS", p.OS == ""}, {"Capture", p.Capture == nil}, {"Owners", p.Owners == nil},
		{"Privileges", p.Privileges == nil}, {"Games", p.Games == nil}, {"Icons", p.Icons == nil},
		{"Paths", p.Paths == nil}, {"Flows", p.Flows == nil}, {"Inventory", p.Inventory == nil},
		{"Service", p.Service == nil}, {"ServiceAPI", p.ServiceAPI == nil},
		{"WiFi", p.WiFi == nil},
	} {
		if part.unset {
			missing = append(missing, part.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("platform: missing %v", missing)
	}
	return nil
}

// ElevationHelps reports whether restarting with more rights would make
// route mode work: the system can ask for them, this process doesn't have
// them, and the capture backend's only complaint is the missing rights. A
// missing driver or an OS without a backend isn't fixed by a prompt, so
// neither the start-up restart nor the UI's restart button should offer one.
func (p *Platform) ElevationHelps() bool {
	return p.Privileges.CanElevate() && !p.Privileges.Elevated() && NeedsElevation(p.Capture.Available())
}

// NeedsElevation reports whether err (from CaptureBackend.Available) is
// only about missing rights.
func NeedsElevation(err error) bool { return errors.Is(err, capture.ErrNeedAdmin) }

// ErrElevateCancelled is what Privileges.Relaunch returns when the user says
// no to the administrator prompt.
var ErrElevateCancelled = errors.New("the administrator prompt was cancelled")
