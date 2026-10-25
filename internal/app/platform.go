package app

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/divert"
)

// Capturer is the packet-capture backend behind route mode. WinDivert is the
// real one; tests substitute a fake so route mode runs without admin rights.
type Capturer interface {
	// Available is nil, or why route mode can't start here
	// (capture.ErrNeedAdmin, capture.ErrNoDriver, capture.ErrUnsupported).
	Available() error
	Admin() bool
	Start(cfg capture.Config) (CaptureSession, error)
	Processes() ([]capture.Process, error)
}

// CaptureSession is a running capture (*capture.Capture).
type CaptureSession interface {
	Inject(local, remote netip.AddrPort, payload []byte) error
	Stats() capture.Stats
	Close() error
}

// WinDivert captures with the driver embedded in internal/divert.
type WinDivert struct {
	mu  sync.Mutex
	dir string // where the driver files went; "" until the first Start
}

func (w *WinDivert) Available() error                      { return capture.Available() }
func (w *WinDivert) Admin() bool                           { return capture.IsAdmin() }
func (w *WinDivert) Processes() ([]capture.Process, error) { return capture.Processes() }

func (w *WinDivert) Start(cfg capture.Config) (CaptureSession, error) {
	dir, err := w.install()
	if err != nil {
		return nil, err
	}
	cfg.DriverDir = dir
	c, err := capture.Start(cfg)
	if err != nil {
		return nil, err // not c: a nil *Capture in an interface isn't nil
	}
	return c, nil
}

// install writes the driver files to <Program Files>\Lowpath\driver. Only
// administrators can write there, and an elevated process must not load a
// DLL from a folder any user could swap it in. So elevated, that is the only
// choice; the "driver" folder next to the exe (often Downloads) is a fallback
// for unelevated runs only, where the driver can't be opened anyway. The
// first folder that works is kept, since a process loads WinDivert.dll once.
func (w *WinDivert) install() (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dir != "" {
		return w.dir, nil
	}
	dirs, errs := driverDirs(capture.IsAdmin())
	for _, dir := range dirs {
		if err := divert.Install(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		w.dir = dir
		return dir, nil
	}
	return "", fmt.Errorf("%w: %w", capture.ErrNoDriver, errors.Join(errs...))
}

// driverDirs lists where the driver may go, best first, and why a folder
// is missing from the list.
func driverDirs(admin bool) (dirs []string, errs []error) {
	// From the shell, not %ProgramFiles%: the environment of an elevated
	// process comes partly from the user's own (writable) settings.
	if pf, err := programFiles(); err == nil {
		dirs = append(dirs, filepath.Join(pf, "Lowpath", "driver"))
	} else {
		errs = append(errs, fmt.Errorf("find Program Files: %w", err))
	}
	if !admin {
		if exe, err := os.Executable(); err == nil {
			dirs = append(dirs, filepath.Join(filepath.Dir(exe), "driver"))
		}
	}
	return dirs, errs
}

type platformView struct {
	OS      string      `json:"os"`
	Admin   bool        `json:"admin"`
	Capture captureInfo `json:"capture"`
}

type captureInfo struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"` // why not, as a phrase the UI puts in a sentence
}

func (s *Server) platform() platformView {
	v := platformView{OS: runtime.GOOS, Admin: s.capt.Admin()}
	if err := s.capt.Available(); err != nil {
		v.Capture.Reason = captureReason(err)
	} else {
		v.Capture.Available = true
	}
	return v
}

func captureReason(err error) string {
	switch {
	case errors.Is(err, capture.ErrNeedAdmin):
		return "administrator rights are needed"
	case errors.Is(err, capture.ErrUnsupported):
		return "packet capture needs 64-bit Windows"
	case errors.Is(err, capture.ErrNoDriver):
		return "the WinDivert driver can't run on this PC"
	default:
		return err.Error()
	}
}

// handleProcesses lists programs with open UDP sockets for "Add game". The
// ones with the most sockets come first: a game in a match usually has
// several, background apps one.
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	procs, err := s.capt.Processes()
	if errors.Is(err, capture.ErrUnsupported) {
		writeErr(w, 501, "unsupported", errors.New("listing processes is only supported on Windows"))
		return
	}
	if err != nil {
		writeErr(w, 500, "failed", err)
		return
	}
	self := uint32(os.Getpid())
	procs = slices.DeleteFunc(procs, func(p capture.Process) bool { return p.PID == self })
	slices.SortFunc(procs, func(a, b capture.Process) int {
		if c := cmp.Compare(len(b.UDPPorts), len(a.UDPPorts)); c != 0 {
			return c
		}
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
	if procs == nil {
		procs = []capture.Process{}
	}
	writeJSON(w, 200, map[string]any{"processes": procs})
}

// ErrElevateCancelled is what Config.Elevate returns when the user says no
// to the UAC prompt.
var ErrElevateCancelled = errors.New("the administrator prompt was cancelled")

// handleElevate restarts the app as administrator: the elevated copy is
// launched, this one answers and then exits so the copy can take the port.
func (s *Server) handleElevate(w http.ResponseWriter, r *http.Request) {
	if s.elevate == nil {
		writeErr(w, 501, "unsupported", errors.New("restarting as administrator is only possible on Windows"))
		return
	}
	if s.capt.Admin() {
		writeErr(w, 409, "already_admin", errors.New("Lowpath already runs as administrator"))
		return
	}
	if err := s.elevate(); err != nil {
		if errors.Is(err, ErrElevateCancelled) {
			writeErr(w, 409, "elevate_cancelled", err)
		} else {
			writeErr(w, 500, "elevate_failed", err)
		}
		return
	}
	s.log.Info("elevated copy started; exiting")
	writeJSON(w, 200, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// Quit after this handler returns, so the reply isn't cut off.
	time.AfterFunc(100*time.Millisecond, s.quit)
}
