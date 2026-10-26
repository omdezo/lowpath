package platform

import (
	"context"
	"errors"
	"net"
)

// ServiceName is the background service's name with the OS's service
// manager (the Windows service control manager).
const ServiceName = "LowpathSvc"

// ServiceManager installs, queries and starts the background service
// (LowpathSvc): the process that runs the engine and packet capture with
// the rights capture needs, so the window can stay unelevated and nobody
// has to answer an administrator prompt after the one-time install. A
// system without a service implementation yet reports errors matching
// errors.ErrUnsupported.
type ServiceManager interface {
	// State reports whether the service is installed and running. It needs
	// no administrator rights.
	State() (ServiceState, error)
	// Start asks the service manager to start the installed service. The
	// install lets signed-in users do this without administrator rights.
	// Starting a running service is not an error.
	Start() error
	// Install registers the service to run cfg.Exe with cfg.Args at boot
	// as the system account, or updates an existing one, and starts it.
	// It needs administrator rights (an error matching ErrNeedAdmin
	// otherwise).
	Install(cfg ServiceConfig) error
	// Uninstall stops and removes the service. It needs administrator
	// rights. Removing a service that isn't installed is not an error.
	Uninstall() error
}

// ServiceState is what the service manager knows about the service.
type ServiceState struct {
	Installed bool
	Running   bool
	// PID is the running service's process id (0 when not running).
	PID uint32
	// Exe is the program the service runs ("" when not installed or the
	// system won't say).
	Exe string
}

// ServiceConfig is what Install sets up.
type ServiceConfig struct {
	// Exe is the service program to install. Install copies it into a
	// folder only administrators can write to first: a service that runs
	// as the system account from a folder the user can write to (such as
	// Downloads) would let any program replace it and gain full control
	// of the PC.
	Exe string
	// Args are the service's start-up arguments.
	Args []string
	// DataDir is the service's state and log folder. Install creates it
	// and limits it to the system account and administrators, for the same
	// reason.
	DataDir string
	// Seed are files (by name) written into DataDir after it is secured
	// and before the service first starts, when DataDir has none of that
	// name yet: the installing user's settings and relays.
	Seed map[string][]byte
}

// ServiceAPI is how the window reaches the service's HTTP API: the service
// listens and the window dials, on a channel only local signed-in users can
// open (a named pipe on Windows, a Unix socket elsewhere). It replaces the
// loopback TCP port, which every program on the PC can reach.
type ServiceAPI interface {
	// Listen opens the service's end. It fails when another process
	// already holds the name, so nothing can pose as the service by
	// taking it first.
	Listen() (net.Listener, error)
	// Dial connects to the service. Errors match ErrServiceNotRunning when
	// nobody listens, and ErrNotService when the other end is not the
	// installed service.
	Dial(ctx context.Context) (net.Conn, error)
	// String names the channel, for logs ("\\.\pipe\lowpath").
	String() string
}

// ServiceRunner runs a program as the OS service when the service manager
// started it.
type ServiceRunner interface {
	// Run calls run with a context that ends when the service manager asks
	// the service to stop (or the system shuts down), and reports the
	// service stopped once run returns. It returns ok=false at once when
	// this process was not started by the service manager, so the caller
	// can run in the foreground instead.
	Run(name string, run func(ctx context.Context) error) (ok bool, err error)
}

var (
	// ErrServiceNotRunning is what ServiceAPI.Dial returns when the service
	// isn't listening.
	ErrServiceNotRunning = errors.New("the Lowpath service is not running")
	// ErrNotService is what ServiceAPI.Dial returns when the process at the
	// other end is not the installed service.
	ErrNotService = errors.New("the program answering is not the Lowpath service")
	// ErrNeedAdmin is what ServiceManager.Install and Uninstall return
	// without administrator rights.
	ErrNeedAdmin = errors.New("administrator rights are needed")
)
