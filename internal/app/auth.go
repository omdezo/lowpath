package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"sync"
)

// The API listens on loopback, which every program on this PC can reach, and
// every user signed in to it. The app behind it usually runs as administrator
// and can divert any program's traffic through any relay, so a fixed header
// is not enough: /api also needs a secret made fresh at each launch that only
// the app's own window (or the browser it opened) holds.
//
// The secret travels as an HttpOnly, SameSite=Strict session cookie: pages
// can't read it, other sites' requests don't carry it, and the browser keeps
// it in memory only. The window gets it by first loading LaunchPath, whose
// token works once; the app hands that path to its window directly.

const sessionCookie = "lowpath_session"

// Paths reachable without the session: the launch-link exchange itself, and the
// version check a second copy of the app uses to find this one.
const (
	pathSession = "/api/session"
	pathVersion = "/api/version"
)

type auth struct {
	session string // the cookie value, fixed for the life of the process

	mu     sync.Mutex
	launch string // one-time token for LaunchPath; "" once used
}

func newAuth(session string) *auth {
	if session == "" {
		session = randToken()
	}
	return &auth{session: session, launch: randToken()}
}

func randToken() string {
	var b [32]byte
	rand.Read(b[:]) // never fails (crypto/rand panics instead)
	return hex.EncodeToString(b[:])
}

// redeem reports whether token is the launch token, and burns it.
func (a *auth) redeem(token string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.launch == "" || subtle.ConstantTimeCompare([]byte(token), []byte(a.launch)) != 1 {
		return false
	}
	a.launch = ""
	return true
}

// ok reports whether r carries the session cookie.
func (a *auth) ok(r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.session)) == 1
}

// LaunchPath is the first URL path the app's window (or browser) must open:
// it signs that browser in and redirects to the UI. It works once per launch,
// so it must not be logged or shown anywhere another program could read it
// before the window uses it.
func (s *Server) LaunchPath() string {
	s.auth.mu.Lock()
	defer s.auth.mu.Unlock()
	if s.auth.launch == "" {
		return "/"
	}
	return pathSession + "?t=" + s.auth.launch
}

// handleSession trades the launch token for the session cookie.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.auth.redeem(r.URL.Query().Get("t")) {
		if s.auth.ok(r) { // already signed in, e.g. this URL was reloaded
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		writeErr(w, http.StatusForbidden, "bad_launch_token",
			errors.New("this launch link was already used or is from another run of Lowpath; close Lowpath and start it again"))
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.auth.session, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"version": Version})
}
