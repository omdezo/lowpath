package platform

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"strconv"

	"golang.org/x/sys/unix"
)

// xucredVersion is XUCRED_VERSION in sys/ucred.h, which x/sys/unix has no
// name for.
const xucredVersion = 0

// connPeerCred reads LOCAL_PEERCRED, what getpeereid(3) returns: the peer's
// credentials from when it called connect(2) (or listen(2), seen from the
// client), and LOCAL_PEERPID for its process.
func connPeerCred(c net.Conn) (PeerCred, error) {
	var p PeerCred
	err := controlConn(c, func(fd int) error {
		cred, err := unix.GetsockoptXucred(fd, unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if err != nil {
			return err
		}
		if cred.Version != xucredVersion {
			return fmt.Errorf("peer credentials version %d, want %d", cred.Version, xucredVersion)
		}
		p.UID = cred.Uid
		if pid, err := unix.GetsockoptInt(fd, unix.SOL_LOCAL, unix.LOCAL_PEERPID); err == nil {
			p.PID = pid
		}
		return nil
	})
	return p, err
}

// NewSessionPolicy admits root, this process's account and people with a
// session in utmpx; when utmpx can't be read, the accounts macOS makes for
// people: uid 501 and up, short of nobody (-2) and -1.
func NewSessionPolicy() *SessionPolicy {
	return &SessionPolicy{
		Self: uint32(os.Geteuid()),
		Sources: []Sessions{
			Utmpx{Path: "/var/run/utmpx", Name: lookupName},
			RegularUsers{Min: 501, Max: 1<<32 - 3},
		},
	}
}

// lookupName asks Directory Services (through libSystem, cgo or not) for
// the account name of uid.
func lookupName(uid uint32) (string, error) {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return "", err
	}
	return u.Username, nil
}
