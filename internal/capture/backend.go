package capture

import (
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"lowpath/internal/divert"
	"lowpath/internal/owner"
)

// Backend is one way of taking the selected game's packets off the network
// and putting the relays' replies back. The app and the engine only see this
// interface, so they stay the same on every OS: WinDivert on Windows today,
// TUN-based backends on Linux and macOS later.
type Backend interface {
	// Name identifies the backend in logs and the API ("divert", "none").
	Name() string
	// Available is nil, or why Start can't work here: an error matching
	// ErrNeedAdmin, ErrNoDriver or errors.ErrUnsupported, or another reason.
	// It must be cheap: the UI asks on every bootstrap.
	Available() error
	// Start begins capturing. cfg.OnPacket is required; the backend fills in
	// what it owns (driver folder, socket owners).
	Start(cfg Config) (Session, error)
	// Recover undoes whatever a session of this backend that never reached
	// Close left behind (the process was killed, crashed or lost power).
	// The app calls it at start for a Marker whose process is gone. It must
	// be safe to call when there is nothing to undo, and must not need
	// rights that Start would not need.
	Recover(m Marker) error
}

// Session is a running capture.
type Session interface {
	// Inject delivers a reply to the game's socket. Safe for concurrent use;
	// payload is not retained.
	Inject(local, remote netip.AddrPort, payload []byte) error
	Stats() Stats
	// Close stops capturing and must leave networking as it was. Safe to call
	// more than once.
	Close() error
	// Done is closed once the capture has stopped, by Close or on its own
	// (the fail-open watchdog, a broken handle), and networking is back to
	// normal.
	Done() <-chan struct{}
	// Err says why the capture stopped on its own; nil while it runs and
	// after a plain Close.
	Err() error
}

// Elevation reports whether this process has the rights capture needs
// (administrator on Windows).
type Elevation interface {
	Elevated() bool
}

// DriverLocations lists the folders the driver files may be written to,
// best first, and why a usual folder is missing from the list. An elevated
// process must only get folders ordinary users can't write to.
type DriverLocations interface {
	DriverDirs(elevated bool) ([]string, []error)
}

// WinDivert is the Backend built on the WinDivert driver embedded in
// internal/divert. It works on 64-bit Windows; elsewhere Available reports
// ErrUnsupported.
type WinDivert struct {
	owners owner.Resolver
	priv   Elevation
	dirs   DriverLocations

	// install and start are divert.Install and Start; tests replace them.
	install func(dir string) error
	start   func(Config) (*Capture, error)

	mu  sync.Mutex
	dir string // where the driver files went; "" until the first Start
}

// NewWinDivert wires the backend to the socket-owner resolver, the
// privilege check and the driver folders of this platform.
func NewWinDivert(owners owner.Resolver, priv Elevation, dirs DriverLocations) *WinDivert {
	return &WinDivert{owners: owners, priv: priv, dirs: dirs, install: divert.Install, start: Start}
}

func (w *WinDivert) Name() string     { return "divert" }
func (w *WinDivert) Available() error { return available(w.priv.Elevated()) }

func (w *WinDivert) Start(cfg Config) (Session, error) {
	if err := w.Available(); err != nil {
		return nil, err
	}
	dir, err := w.installDriver()
	if err != nil {
		return nil, err
	}
	cfg.DriverDir = dir
	if cfg.Owners == nil {
		cfg.Owners = w.owners
	}
	c, err := w.start(cfg)
	if err != nil {
		return nil, err // not c: a nil *Capture in an interface isn't nil
	}
	return c, nil
}

// Recover has nothing to undo for WinDivert. A WinDivert handle is a kernel
// handle: when the process ends in any way, Windows closes it, the driver
// reinjects the packets still queued for it and deletes its filters, so the
// traffic it diverted goes out directly again. The driver service stays
// registered until WinDivert removes it at the next reboot, which is
// harmless: it diverts nothing without a handle.
func (w *WinDivert) Recover(Marker) error { return nil }

// installDriver writes the driver files to the first folder that takes
// them, and keeps using it: a process loads WinDivert.dll once.
func (w *WinDivert) installDriver() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dir != "" {
		return w.dir, nil
	}
	dirs, errs := w.dirs.DriverDirs(w.priv.Elevated())
	for _, dir := range dirs {
		if err := w.install(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		w.dir = dir
		return dir, nil
	}
	if len(errs) == 0 {
		errs = append(errs, errors.New("no folder to put the driver in"))
	}
	return "", fmt.Errorf("%w: %w", ErrNoDriver, errors.Join(errs...))
}

// Unavailable is the Backend for systems with no capture method yet. Route
// mode then explains why instead of failing in an odd way; proxy mode keeps
// working.
type Unavailable struct{ err error }

// NewUnavailable makes a backend that always reports err (which should
// match errors.ErrUnsupported when the OS simply lacks a backend).
func NewUnavailable(err error) *Unavailable { return &Unavailable{err: err} }

func (u *Unavailable) Name() string                  { return "none" }
func (u *Unavailable) Available() error              { return u.err }
func (u *Unavailable) Start(Config) (Session, error) { return nil, u.err }
func (u *Unavailable) Recover(Marker) error          { return nil }
