package platform

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// connPeerCred reads SO_PEERCRED: the peer's credentials from when it
// called connect(2) (or listen(2), seen from the client), kept by the
// kernel (unix(7)).
func connPeerCred(c net.Conn) (PeerCred, error) {
	var cred *unix.Ucred
	err := controlConn(c, func(fd int) error {
		var err error
		cred, err = unix.GetsockoptUcred(fd, unix.SOL_SOCKET, unix.SO_PEERCRED)
		return err
	})
	if err != nil {
		return PeerCred{}, err
	}
	return PeerCred{UID: cred.Uid, PID: int(cred.Pid)}, nil
}

// NewSessionPolicy admits root, this process's account and people with a
// logind session; without logind, the people's uid range of login.defs.
// utmp isn't asked: glibc keeps it only where a login program writes it
// (musl and WSL don't), so an empty one can't be told from nobody signed
// in, and every account it could name is in that range anyway.
func NewSessionPolicy() *SessionPolicy {
	return &SessionPolicy{
		Self:    uint32(os.Geteuid()),
		Sources: []Sessions{Logind{Dir: "/run/systemd/sessions"}, LoginDefsUsers("/etc/login.defs")},
	}
}
