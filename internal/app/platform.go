package app

import (
	"cmp"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"lowpath/internal/capture"
	"lowpath/internal/owner"
	"lowpath/internal/platform"
)

type platformView struct {
	OS    string `json:"os"`
	Admin bool   `json:"admin"`
	// CanElevate: POST /api/elevate can restart the app with the rights
	// route mode needs, and those rights are all capture lacks (the UI shows
	// the button only then; a restart can't fix a missing driver).
	CanElevate bool        `json:"can_elevate"`
	Capture    captureInfo `json:"capture"`
	// Service is the background service (LowpathSvc) as this server sees
	// it.
	Service serviceView `json:"service"`
}

// serviceView says whether the window goes through the background service.
type serviceView struct {
	// State: "in_use" (this server is the service), "running" (installed
	// and running, but this window runs its own engine, as with -demo),
	// "stopped", "not_installed", "unsupported" (none on this OS yet) or
	// "unknown" (the service manager didn't answer; Message says why).
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}

type captureInfo struct {
	Backend   string `json:"backend"` // "divert", "none", ...
	Available bool   `json:"available"`
	Reason    string `json:"reason"` // why not, as a phrase the UI puts in a sentence
}

func (s *Server) platform() platformView {
	admin := s.plat.Privileges.Elevated()
	avail := s.plat.Capture.Available()
	v := platformView{
		OS: s.plat.OS, Admin: admin,
		CanElevate: s.elevate != nil && !admin && platform.NeedsElevation(avail),
		Capture:    captureInfo{Backend: s.plat.Capture.Name()},
		Service:    s.serviceView(),
	}
	if err := avail; err != nil {
		v.Capture.Reason = captureReason(err)
	} else {
		v.Capture.Available = true
	}
	return v
}

func (s *Server) serviceView() serviceView {
	if s.inService {
		return serviceView{State: "in_use"}
	}
	st, err := s.plat.Service.State()
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		return serviceView{State: "unsupported"}
	case err != nil:
		return serviceView{State: "unknown", Message: err.Error()}
	case !st.Installed:
		return serviceView{State: "not_installed"}
	case st.Running:
		return serviceView{State: "running"}
	default:
		return serviceView{State: "stopped"}
	}
}

func captureReason(err error) string {
	var rights *capture.RightsError
	switch {
	case errors.As(err, &rights):
		return rights.Reason // the backend says which rights, for its OS
	case errors.Is(err, capture.ErrNeedAdmin):
		return "administrator rights are needed"
	case errors.Is(err, capture.ErrUnsupported):
		return "packet capture needs 64-bit Windows"
	case errors.Is(err, errors.ErrUnsupported):
		return "packet capture isn't available on this system yet"
	case errors.Is(err, capture.ErrNoDriver):
		return "the WinDivert driver can't run on this PC"
	case errors.Is(err, capture.ErrNoTUN):
		return "the kernel has no TUN device (/dev/net/tun)"
	default:
		return err.Error()
	}
}

// handleProcesses lists programs with open UDP sockets for "Add game". The
// ones with the most sockets come first: a game in a match usually has
// several, background apps one.
func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	self := uint32(os.Getpid())
	procs, err := owner.ListProcesses(s.plat.Owners, self)
	if errors.Is(err, errors.ErrUnsupported) {
		writeErr(w, 501, "unsupported", errors.New("listing programs isn't available on this system yet"))
		return
	}
	if err != nil {
		writeErr(w, 500, "failed", err)
		return
	}
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
// to the administrator prompt.
var ErrElevateCancelled = platform.ErrElevateCancelled

// handleElevate restarts the app as administrator: the elevated copy is
// launched, this one answers and then exits so the copy can take the port.
func (s *Server) handleElevate(w http.ResponseWriter, r *http.Request) {
	if s.elevate == nil {
		writeErr(w, 501, "unsupported", errors.New("restarting with administrator rights isn't available on this system yet"))
		return
	}
	if s.plat.Privileges.Elevated() {
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
