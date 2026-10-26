package platform

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PeerCred is the local account at the other end of a Unix socket, as the
// kernel recorded it when the connection was made (SO_PEERCRED on Linux,
// LOCAL_PEERCRED on macOS): the client can't choose or change it.
type PeerCred struct {
	UID uint32
	// PID is the peer's process id, 0 when the system doesn't say.
	PID int
}

func (p PeerCred) String() string {
	if p.PID == 0 {
		return "uid " + strconv.FormatUint(uint64(p.UID), 10)
	}
	return fmt.Sprintf("uid %d (process %d)", p.UID, p.PID)
}

// PeerPolicy decides which local accounts may use the service's socket.
type PeerPolicy interface {
	// Admit returns nil when peer may use the socket, or why not, matching
	// ErrPeerRefused.
	Admit(peer PeerCred) error
}

// ErrPeerRefused is why the service's socket turned an account away: the
// Unix counterpart of the pipe's access list, which only admits users
// signed in at the PC.
var ErrPeerRefused = errors.New("the Lowpath service only serves accounts signed in at this computer")

// Sessions tells which accounts are signed in at this computer.
type Sessions interface {
	// SignedIn reports whether uid has a login session now. An error
	// matching errors.ErrUnsupported means the system doesn't keep this
	// list (no logind, no utmpx file), so the next source is asked.
	SignedIn(uid uint32) (bool, error)
	// String names the source, for the refusal ("logind").
	String() string
}

// SessionPolicy is the service socket's PeerPolicy on Linux and macOS, as
// Interactive Users is the pipe's on Windows: root, the service's own
// account and accounts with a login session get in; daemon accounts
// (nobody, www-data, _www) and users who aren't signed in don't. The
// sources are asked in order, and the first one the system has decides; a
// source that fails to read is skipped, and with none left the peer is
// refused.
type SessionPolicy struct {
	// Self is the service's own uid (root for the installed service; the
	// developer for a service run by hand on a socket of its own).
	Self    uint32
	Sources []Sessions
}

func (p *SessionPolicy) Admit(peer PeerCred) error {
	if peer.UID == 0 || peer.UID == p.Self {
		return nil
	}
	var errs []error
	for _, s := range p.Sources {
		ok, err := s.SignedIn(peer.UID)
		switch {
		case err == nil && ok:
			return nil
		case err == nil:
			return fmt.Errorf("%w: %s has no login session (%s)", ErrPeerRefused, peer, s)
		case !errors.Is(err, errors.ErrUnsupported):
			errs = append(errs, fmt.Errorf("%s: %w", s, err))
		}
	}
	if len(errs) == 0 {
		return fmt.Errorf("%w: this system keeps no list of who is signed in, so only root may connect, not %s", ErrPeerRefused, peer)
	}
	return fmt.Errorf("%w: can't tell whether %s is signed in: %w", ErrPeerRefused, peer, errors.Join(errs...))
}

// maxRecordFile bounds one session file read: logind's are a few hundred
// bytes, a utmpx file a few hundred records.
const maxRecordFile = 1 << 20

// Logind reads the session records of systemd-logind (and elogind) in Dir,
// /run/systemd/sessions: one file per session with KEY=value lines, the
// files sd-login(3) reads. logind makes the folder when it starts, so a
// missing one means it isn't running. A session counts while its class is a
// person's (user, user-early, user-light...; not greeter, lock-screen,
// background or manager, which cron jobs and the login screen get) and it
// isn't closing (the user signed out and a process lingers).
type Logind struct{ Dir string }

func (Logind) String() string { return "logind" }

func (l Logind) SignedIn(uid uint32) (bool, error) {
	entries, err := os.ReadDir(l.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("logind isn't running (no %s): %w", l.Dir, errors.ErrUnsupported)
	}
	if err != nil {
		return false, err
	}
	read := 0
	for _, e := range entries {
		// logind writes each file to a hidden temporary one first and
		// renames it into place.
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		b, err := readSmall(filepath.Join(l.Dir, e.Name()))
		if err != nil {
			continue // the session ended while the folder was read
		}
		s, ok := parseLogindSession(b)
		if !ok {
			continue
		}
		read++
		if s.uid == uid && s.counts() {
			return true, nil
		}
	}
	if read == 0 && len(entries) > 0 {
		// Files, but none with a UID: a format this code doesn't know.
		return false, fmt.Errorf("none of the %d files in %s is a session record", len(entries), l.Dir)
	}
	return false, nil
}

type logindSession struct {
	uid          uint32
	class, state string
}

// counts reports a person's open session. Older logind versions wrote no
// CLASS or STATE; their sessions are users' and open.
func (s logindSession) counts() bool {
	return (s.class == "" || s.class == "user" || strings.HasPrefix(s.class, "user-")) && s.state != "closing"
}

func parseLogindSession(b []byte) (logindSession, bool) {
	var s logindSession
	hasUID := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "UID":
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return s, false
			}
			s.uid, hasUID = uint32(n), true
		case "CLASS":
			s.class = v
		case "STATE":
			s.state = v
		}
	}
	return s, hasUID
}

// Utmpx reads the macOS login records in Path, /var/run/utmpx. loginwindow,
// login (Terminal), sshd and screen sharing add a USER_PROCESS record when
// a session opens and mark it DEAD_PROCESS when it ends; /var/run is
// emptied at boot. Libc keeps the file in the 32-bit struct utmpx layout on
// every Mac (utmpx32: 628 bytes, little-endian on Intel and Apple silicon),
// after a signature record whose ut_user is "utmpx-1.00".
type Utmpx struct {
	Path string
	// Name is the account name of uid, which the records carry instead of
	// uids (os/user.LookupId).
	Name func(uid uint32) (string, error)
}

func (Utmpx) String() string { return "utmpx" }

const (
	utmpxRecord      = 628 // sizeof(struct utmpx32)
	utmpxUserSize    = 256 // ut_user, at 0
	utmpxTypeOffset  = 296 // ut_type (short), after ut_id[4], ut_line[32], ut_pid
	utmpxUserProcess = 7   // USER_PROCESS
	utmpxSignature   = 10  // SIGNATURE
	utmpxVersion     = "utmpx-1.00"
)

func (u Utmpx) SignedIn(uid uint32) (bool, error) {
	b, err := readSmall(u.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("no %s: %w", u.Path, errors.ErrUnsupported)
	}
	if err != nil {
		return false, err
	}
	users, err := parseUtmpx(b)
	if err != nil {
		return false, fmt.Errorf("%s: %w", u.Path, err)
	}
	name, err := u.Name(uid)
	if err != nil {
		return false, nil // no account by that uid: nobody signed in as it
	}
	return users[name], nil
}

// parseUtmpx returns the names with an open session (USER_PROCESS).
func parseUtmpx(b []byte) (map[string]bool, error) {
	if len(b) < utmpxRecord || len(b)%utmpxRecord != 0 {
		return nil, fmt.Errorf("%d bytes is not a whole number of %d-byte login records", len(b), utmpxRecord)
	}
	if typ, user := utmpxEntry(b[:utmpxRecord]); typ != utmpxSignature || user != utmpxVersion {
		return nil, fmt.Errorf("no %q signature: not a utmpx file this code knows", utmpxVersion)
	}
	users := map[string]bool{}
	for r := b[utmpxRecord:]; len(r) > 0; r = r[utmpxRecord:] {
		if typ, user := utmpxEntry(r[:utmpxRecord]); typ == utmpxUserProcess && user != "" {
			users[user] = true
		}
	}
	return users, nil
}

func utmpxEntry(r []byte) (typ int16, user string) {
	name := r[:utmpxUserSize]
	if i := bytes.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return int16(binary.LittleEndian.Uint16(r[utmpxTypeOffset:])), string(name)
}

// RegularUsers is the last resort where the system keeps no session list
// (Linux without logind, such as WSL without systemd or Alpine; a Mac whose
// utmpx can't be read): it admits the accounts made for people, Min to Max,
// and none of the daemon accounts below or above them, such as nobody
// (65534 on Linux, -2 on macOS).
type RegularUsers struct{ Min, Max uint32 }

func (r RegularUsers) String() string {
	return fmt.Sprintf("no session list on this system, so the accounts for people, uid %d to %d, are let in", r.Min, r.Max)
}

func (r RegularUsers) SignedIn(uid uint32) (bool, error) { return uid >= r.Min && uid <= r.Max, nil }

// LoginDefsUsers is the people's uid range from login.defs(5) at path
// (UID_MIN and UID_MAX, which useradd also follows), 1000 to 60000 where it
// doesn't say.
func LoginDefsUsers(path string) RegularUsers {
	r := RegularUsers{Min: 1000, Max: 60000}
	b, err := readSmall(path)
	if err != nil {
		return r
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 || strings.HasPrefix(f[0], "#") {
			continue
		}
		// shadow reads these with strtoul(..., 0): decimal, 0x hex or 0 octal.
		n, err := strconv.ParseUint(f[1], 0, 32)
		if err != nil {
			continue
		}
		switch f[0] {
		case "UID_MIN":
			r.Min = uint32(n)
		case "UID_MAX":
			r.Max = uint32(n)
		}
	}
	return r
}

func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRecordFile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxRecordFile {
		return nil, fmt.Errorf("%s is larger than %d bytes", path, maxRecordFile)
	}
	return b, nil
}
