package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lowpath/internal/capture"
)

// engineStopWait bounds how long ending a session waits for the engine. A
// session that failed open may have a stuck engine (that is what tripped the
// watchdog); the game's traffic is already direct by then, so the app moves
// on rather than hang the API behind it.
const engineStopWait = 2 * time.Second

// SessionMarker is where route mode records a running capture, so the next
// start can clean up after a run that never reached Stop (capture.MarkerFile).
type SessionMarker interface {
	Save(capture.Marker) error
	Load() (capture.Marker, bool, error)
	Clear() error
}

// failsafeView tells the UI that a boost ended without the user asking: the
// capture failed open during this run ("stopped"), or the last run ended
// without cleaning up ("recovered"). Either way the game is on the direct
// connection.
type failsafeView struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // stopped | recovered
	GameID  string    `json:"game_id"`
	Game    string    `json:"game"` // display name, "" when the game is unknown now
	Message string    `json:"message"`
	Detail  string    `json:"detail,omitempty"`
}

// selfExe is this program's image name, which the marker records so a later
// start can tell "that Lowpath is still running" from "that PID was reused".
func selfExe() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Base(p)
}

// markSession records the route-mode session about to start. It is best
// effort: a marker that can't be written only means a crash isn't reported
// at the next start (WinDivert needs no cleanup), so boosting goes on.
func (s *Server) markSession(gameID string) {
	if s.marker == nil {
		return
	}
	m := capture.Marker{
		Backend: s.plat.Capture.Name(), PID: uint32(os.Getpid()), Exe: selfExe(),
		Game: gameID, Started: time.Now().UTC(),
	}
	if err := s.marker.Save(m); err != nil {
		s.log.Warn("session marker", "err", err)
	}
}

func (s *Server) unmarkSession() {
	if s.marker == nil {
		return
	}
	if err := s.marker.Clear(); err != nil {
		s.log.Warn("session marker", "err", err)
	}
}

// recoverLeftover runs once at start: a marker on disk means the last
// route-mode session never reached Stop (killed, crashed, power cut). Its
// backend undoes what is left, and the UI is told.
func (s *Server) recoverLeftover() {
	if s.marker == nil {
		return
	}
	m, ok, err := s.marker.Load()
	if err != nil {
		// Not a marker we can act on; nothing to recover from it.
		s.log.Warn("ignoring a broken session marker", "err", err)
		s.unmarkSession()
		return
	}
	if !ok {
		return
	}
	if s.stillRunning(m) {
		// That Lowpath still owns its capture (two data folders shared,
		// or a copy that is shutting down); undoing it would be wrong.
		s.log.Warn("session marker belongs to a running process; leaving it", "pid", m.PID, "exe", m.Exe)
		return
	}
	v := &failsafeView{At: time.Now(), Kind: "recovered", GameID: m.Game}
	if g, ok := s.game(m.Game); ok {
		v.Game = g.Name
	}
	game := nameOr(v.Game, "a game")
	switch {
	case m.Backend != s.plat.Capture.Name():
		// A marker from another capture method (an older or newer build);
		// this one can't undo it, so say so instead of claiming it's clean.
		v.Detail = fmt.Sprintf("It used the %q capture method, which this version can't clean up.", m.Backend)
		s.log.Warn("session marker from another capture backend", "backend", m.Backend, "have", s.plat.Capture.Name())
	default:
		if err := s.plat.Capture.Recover(m); err != nil {
			v.Detail = "Cleaning up after it failed: " + err.Error()
			s.log.Error("recover capture leftovers", "backend", m.Backend, "err", err)
		}
	}
	if v.Detail == "" {
		v.Message = fmt.Sprintf("Lowpath closed unexpectedly while boosting %s. Your game went back to your normal connection when it closed.", game)
	} else {
		// Only a clean Recover says the connection is back to normal.
		v.Message = fmt.Sprintf("Lowpath closed unexpectedly while boosting %s and could not finish cleaning up after it, so your connection may not be back to normal yet.", game)
	}
	s.log.Warn("the last route-mode session ended without stopping", "game", m.Game, "pid", m.PID, "started", m.Started)
	s.unmarkSession()
	s.mu.Lock()
	s.failsafe = v
	s.mu.Unlock()
}

// stillRunning reports whether the process that wrote m is alive: same PID
// and same image name (Windows reuses PIDs). When the OS can't list
// processes the answer is no, since Recover must be safe either way.
func (s *Server) stillRunning(m capture.Marker) bool {
	if m.PID == uint32(os.Getpid()) {
		return false // our PID from a run before a reboot
	}
	names, err := s.plat.Owners.ProcessNames()
	if err != nil {
		return false
	}
	name, ok := names[m.PID]
	return ok && m.Exe != "" && strings.EqualFold(name, m.Exe)
}

// handleDismissFailsafe forgets the fail-open notice, so it stays closed
// after a reload and on every other window showing this Lowpath.
func (s *Server) handleDismissFailsafe(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.failsafe = nil
	s.mu.Unlock()
	writeJSON(w, 200, s.live())
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// watchCapture ends the session when its capture fails open on its own
// (watchdog, panic in the packet path, driver gone). The capture has already
// put the traffic back on the direct path; this tidies up the engine and
// tells the UI.
func (s *Server) watchCapture(ctx context.Context, sess *session, c capture.Session) {
	select {
	case <-ctx.Done():
		return
	case <-c.Done():
	}
	err := c.Err()
	if err == nil {
		return // closed by Stop
	}
	s.log.Error("capture failed open; ending the session", "game", sess.GameID, "err", err)
	v := &failsafeView{At: time.Now(), Kind: "stopped", GameID: sess.GameID, Detail: err.Error()}
	if g, ok := s.game(sess.GameID); ok {
		v.Game = g.Name
	}
	v.Message = failOpenMessage(err, nameOr(v.Game, "the game"))
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	current := s.sess == sess
	if current {
		s.failsafe = v
	}
	s.mu.Unlock()
	if current {
		s.stopSessionLocked()
	}
}

// failOpenMessage says why a capture stopped on its own, for game.
func failOpenMessage(err error, game string) string {
	if errors.Is(err, capture.ErrRouteRemoved) {
		// Not Lowpath's packet handling, and something the user can change:
		// name the usual culprits.
		return fmt.Sprintf("Boost stopped: another program (a network manager, VPN or firewall) removed the routing that sends %s through Lowpath, so the game went back to your normal connection.", game)
	}
	return fmt.Sprintf("Boost stopped: Lowpath's packet handling for %s stopped working, so the game went back to your normal connection.", game)
}

// stopEngine stops the engine (recording the game's server first in route
// mode) but waits at most engineStopWait: a stuck engine is abandoned
// rather than blocking every later API call behind s.life.
func (s *Server) stopEngine(sess *session) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if sess.capt != nil {
			s.recordPrimary(sess)
		}
		sess.eng.Stop()
	}()
	select {
	case <-done:
	case <-time.After(engineStopWait):
		s.log.Error("engine did not stop in time; abandoning it", "game", sess.GameID, "wait", engineStopWait)
	}
}
