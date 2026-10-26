package platform

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"
)

// DefaultSocketPath is the service's API socket on Linux and macOS. /run is
// writable only by root, so no other user can take the name first.
const DefaultSocketPath = "/run/lowpath/api.sock"

// SocketAPI is the service's API on a Unix domain socket: the Linux and
// macOS ServiceAPI, and on every OS (Windows 10 has AF_UNIX too) a stand-in
// for the named pipe in tests.
type SocketAPI struct {
	path string
	// mode is the socket file's permission bits once it listens. 0o666:
	// every local user's window may drive the service, as Interactive
	// Users may open the pipe on Windows.
	mode fs.FileMode
}

func NewSocketAPI(path string, mode fs.FileMode) *SocketAPI {
	return &SocketAPI{path: path, mode: mode}
}

func (s *SocketAPI) String() string { return s.path }

// Listen makes the socket's folder (0755) and the socket. A socket file
// left by a service that was killed is removed first, but only once nothing
// answers on it: a running service keeps its socket.
func (s *SocketAPI) Listen() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return nil, fmt.Errorf("service socket: %w", err)
	}
	if fi, err := os.Lstat(s.path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 && fi.Mode().IsRegular() && fi.Size() > 0 {
			// Not ours to delete (on Windows a socket shows as a plain,
			// empty file).
			return nil, fmt.Errorf("service socket: %s exists and is not a socket", s.path)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		c, derr := s.Dial(ctx)
		cancel()
		if derr == nil {
			c.Close()
			return nil, fmt.Errorf("service socket: %s: another copy of the service is running", s.path)
		}
		os.Remove(s.path)
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		return nil, fmt.Errorf("service socket: %w", err)
	}
	if s.mode != 0 {
		if err := os.Chmod(s.path, s.mode); err != nil {
			ln.Close()
			return nil, fmt.Errorf("service socket: %w", err)
		}
	}
	return ln, nil
}

// Dial connects to the socket; ErrServiceNotRunning when nothing listens.
func (s *SocketAPI) Dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", s.path)
	if err != nil {
		if ctx.Err() == nil && isNoListener(err) {
			return nil, fmt.Errorf("%w (%s)", ErrServiceNotRunning, s.path)
		}
		return nil, fmt.Errorf("service socket: %w", err)
	}
	return c, nil
}

// isNoListener reports a connect that failed because nothing listens: no
// socket file, or a file nobody holds open (refused). Permission denied is
// a different answer: the service runs, this user may not reach it.
func isNoListener(err error) bool {
	if errors.Is(err, fs.ErrPermission) {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) {
		var se *os.SyscallError
		if errors.As(op.Err, &se) {
			return se.Syscall == "connect"
		}
	}
	return false
}
