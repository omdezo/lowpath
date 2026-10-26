//go:build !linux && !darwin

package platform

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
)

// connPeerCred has no implementation here: Windows serves the API on a
// named pipe (its AF_UNIX sockets only stand in for it in tests), and the
// BSDs have no peer check yet, so their service socket admits no one.
func connPeerCred(net.Conn) (PeerCred, error) {
	return PeerCred{}, fmt.Errorf("platform: reading a socket peer's account is not available on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

// NewSessionPolicy admits root and this process's account only: the system
// keeps no session list this code reads.
func NewSessionPolicy() *SessionPolicy {
	return &SessionPolicy{Self: uint32(os.Geteuid())}
}
