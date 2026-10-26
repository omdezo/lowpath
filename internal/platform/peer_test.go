package platform

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sessionsFake is a session source the test fills in.
type sessionsFake struct {
	name string
	in   map[uint32]bool
	err  error
}

func (s sessionsFake) String() string { return s.name }
func (s sessionsFake) SignedIn(uid uint32) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.in[uid], nil
}

func TestSessionPolicy(t *testing.T) {
	logind := sessionsFake{name: "logind", in: map[uint32]bool{1000: true}}
	noLogind := sessionsFake{name: "logind", err: fmt.Errorf("no logind: %w", errors.ErrUnsupported)}
	broken := sessionsFake{name: "logind", err: errors.New("permission denied")}
	people := RegularUsers{Min: 1000, Max: 60000}
	for _, tc := range []struct {
		name    string
		sources []Sessions
		uid     uint32
		ok      bool
		reason  string // part of the refusal
	}{
		{"root", nil, 0, true, ""},
		{"the service's own account", nil, 500, true, ""},
		{"signed in", []Sessions{logind, people}, 1000, true, ""},
		{"a person who isn't signed in", []Sessions{logind, people}, 1001, false, "has no login session (logind)"},
		{"daemon", []Sessions{logind, people}, 33, false, "uid 33 (process 42) has no login session"},
		// logind decides when it runs: the range isn't asked.
		{"nobody with logind", []Sessions{logind, people}, 65534, false, "logind"},
		{"no logind: the range", []Sessions{noLogind, people}, 1001, true, ""},
		{"no logind: nobody", []Sessions{noLogind, people}, 65534, false, "uid 1000 to 60000"},
		{"no logind: daemon", []Sessions{noLogind, people}, 999, false, "uid 999"},
		{"logind unreadable: the range", []Sessions{broken, people}, 1001, true, ""},
		{"nothing to ask", nil, 1000, false, "only root may connect"},
		{"only sources that fail", []Sessions{broken}, 1000, false, "permission denied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &SessionPolicy{Self: 500, Sources: tc.sources}
			err := p.Admit(PeerCred{UID: tc.uid, PID: 42})
			if tc.ok {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrPeerRefused) || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Admit = %v, want ErrPeerRefused with %q", err, tc.reason)
			}
		})
	}
}

func TestPeerCredString(t *testing.T) {
	for p, want := range map[PeerCred]string{
		{UID: 1000, PID: 4242}: "uid 1000 (process 4242)",
		{UID: 65534}:           "uid 65534",
		{UID: 0, PID: 1}:       "uid 0 (process 1)",
		{UID: unknownUID}:      "an account the system didn't name",
	} {
		if got := p.String(); got != want {
			t.Errorf("%#v: %q, want %q", p, got, want)
		}
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLogind(t *testing.T) {
	dir := t.TempDir()
	// What logind writes (abridged), one file per session.
	writeFiles(t, dir, map[string]string{
		"2":    "# This is private data. Do not parse.\nUID=1000\nUSER=example\nACTIVE=1\nIS_DISPLAY=1\nSTATE=active\nREMOTE=0\nTYPE=wayland\nCLASS=user\nSEAT=seat0\n",
		"c1":   "UID=120\nUSER=gdm\nSTATE=online\nCLASS=greeter\n",
		"5":    "UID=1001\nUSER=builder\nSTATE=online\nCLASS=background\n", // a cron job
		"7":    "UID=1002\nSTATE=closing\nCLASS=user\n",                    // signed out, a process lingers
		"8":    "UID=1003\nSTATE=online\nCLASS=user-early\n",
		"9":    "UID=1006\nSTATE=online\nREMOTE=1\nTYPE=tty\nCLASS=user\n", // ssh
		"10":   "UID=1005\n",                                               // old logind: no class or state
		"11":   "UID=1007\nCLASS=manager\nSTATE=online\n",                  // the user's systemd
		".#2x": "UID=1004\nCLASS=user\n",                                   // being written
	})
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Logind{Dir: dir}
	for uid, want := range map[uint32]bool{
		1000: true, 120: false, 1001: false, 1002: false, 1003: true, 1006: true,
		1005: true, 1007: false, 1004: false, 65534: false,
	} {
		got, err := l.SignedIn(uid)
		if err != nil || got != want {
			t.Errorf("SignedIn(%d) = %v, %v; want %v", uid, got, err, want)
		}
	}

	// Not running: the next source decides.
	if _, err := (Logind{Dir: filepath.Join(dir, "none")}).SignedIn(1000); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("no folder: %v, want ErrUnsupported", err)
	}
	// Running with nobody signed in.
	empty := t.TempDir()
	if ok, err := (Logind{Dir: empty}).SignedIn(1000); ok || err != nil {
		t.Errorf("no sessions: %v, %v", ok, err)
	}
	// A format this code doesn't know is an error, not "nobody is signed in".
	odd := t.TempDir()
	writeFiles(t, odd, map[string]string{"2": "[Session]\nOwner=1000\n", "3": "UID=x\n"})
	if _, err := (Logind{Dir: odd}).SignedIn(1000); err == nil || errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("unknown format: %v, want an error", err)
	}
}

// utmpxFile builds a macOS utmpx file: the signature, then one record per
// entry.
func utmpxFile(entries ...struct {
	typ  int16
	user string
}) []byte {
	rec := func(typ int16, user string) []byte {
		r := make([]byte, utmpxRecord)
		copy(r, user)
		copy(r[256:], "s000")                        // ut_id
		copy(r[260:], "ttys000")                     // ut_line
		binary.LittleEndian.PutUint32(r[292:], 4242) // ut_pid
		binary.LittleEndian.PutUint16(r[utmpxTypeOffset:], uint16(typ))
		copy(r[308:], "192.0.2.10") // ut_host
		return r
	}
	b := rec(utmpxSignature, utmpxVersion)
	for _, e := range entries {
		b = append(b, rec(e.typ, e.user)...)
	}
	return b
}

func TestUtmpx(t *testing.T) {
	type entry = struct {
		typ  int16
		user string
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "utmpx")
	names := map[uint32]string{501: "alice", 502: "bob", 503: "carol", 504: "utmpx-1.00"}
	u := Utmpx{Path: path, Name: func(uid uint32) (string, error) {
		if n, ok := names[uid]; ok {
			return n, nil
		}
		return "", errors.New("unknown user")
	}}
	os.WriteFile(path, utmpxFile(
		entry{utmpxUserProcess, "alice"}, // console
		entry{8, "bob"},                  // DEAD_PROCESS: signed out
		entry{2, "carol"},                // BOOT_TIME, not a session
	), 0o644)
	for uid, want := range map[uint32]bool{501: true, 502: false, 503: false, 504: false, 9999: false} {
		got, err := u.SignedIn(uid)
		if err != nil || got != want {
			t.Errorf("SignedIn(%d) = %v, %v; want %v", uid, got, err, want)
		}
	}

	for name, b := range map[string][]byte{
		"truncated":    utmpxFile(entry{utmpxUserProcess, "alice"})[:700],
		"no signature": utmpxFile(entry{utmpxUserProcess, "alice"})[utmpxRecord:],
		"empty":        {},
	} {
		os.WriteFile(path, b, 0o644)
		if _, err := u.SignedIn(501); err == nil || errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("%s: %v, want an error", name, err)
		}
	}
	os.Remove(path)
	if _, err := u.SignedIn(501); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("no file: %v, want ErrUnsupported", err)
	}
}

func TestLoginDefsUsers(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body string
		want       RegularUsers
	}{
		{"defaults", "", RegularUsers{1000, 60000}},
		{"set", "# comment\nUID_MIN\t\t 500\nUID_MAX 0x7530\nSYS_UID_MAX 499\n", RegularUsers{500, 30000}},
		{"commented out", "#UID_MIN 2000\n# UID_MAX 3000\n", RegularUsers{1000, 60000}},
		{"garbage", "UID_MIN lots\nUID_MAX\n", RegularUsers{1000, 60000}},
	} {
		path := filepath.Join(dir, tc.name)
		os.WriteFile(path, []byte(tc.body), 0o644)
		if got := LoginDefsUsers(path); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if got := LoginDefsUsers(filepath.Join(dir, "missing")); got != (RegularUsers{1000, 60000}) {
		t.Errorf("no file: %+v", got)
	}
}

func TestNewSessionPolicy(t *testing.T) {
	p := NewSessionPolicy()
	if uint32(os.Geteuid()) != p.Self {
		t.Errorf("Self = %d, want %d", p.Self, os.Geteuid())
	}
	if err := p.Admit(PeerCred{UID: 0}); err != nil {
		t.Errorf("root refused: %v", err)
	}
	if err := p.Admit(PeerCred{UID: p.Self}); err != nil {
		t.Errorf("own account refused: %v", err)
	}
}
