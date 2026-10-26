package platform

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// SocketAPI is the service's API on a Unix domain socket: the Linux and
// macOS ServiceAPI, and on every OS (Windows 10 has AF_UNIX too) a stand-in
// for the named pipe in tests.
type SocketAPI struct {
	path string
	// mode is the socket file's permission bits once it listens. 0o666 for
	// the service: file modes can't say "signed in", so every account may
	// connect and admit decides.
	mode fs.FileMode
	// admit decides which accounts the listener serves; nil serves whoever
	// the file mode lets connect.
	admit PeerPolicy
	// serverUID, when not nil, is the only account Dial accepts at the
	// other end (root for the installed service's socket).
	serverUID *uint32
	// peer reads the account at the other end of a connection
	// (connPeerCred; tests play it).
	peer func(net.Conn) (PeerCred, error)
}

// NewSocketAPI makes the socket at path. admit, when not nil, decides which
// local accounts the service serves: the others get a 403 answer and are
// disconnected before the API sees their request.
func NewSocketAPI(path string, mode fs.FileMode, admit PeerPolicy) *SocketAPI {
	return &SocketAPI{path: path, mode: mode, admit: admit, peer: connPeerCred}
}

// WithServer makes Dial refuse a socket served by any account but uid,
// with ErrNotService, as the window refuses a pipe served by any process
// but the installed service on Windows. The socket's folder is root's
// already; this still holds if someone loosened it.
func (s *SocketAPI) WithServer(uid uint32) *SocketAPI {
	s.serverUID = &uid
	return s
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
		c, derr := s.dial(ctx)
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
	if s.admit == nil {
		return ln, nil
	}
	return &peerListener{Listener: ln, admit: s.admit, peer: s.peer, reported: map[uint32]time.Time{}}, nil
}

// Dial connects to the socket; ErrServiceNotRunning when nothing listens,
// ErrNotService when another account than WithServer's serves it.
func (s *SocketAPI) Dial(ctx context.Context) (net.Conn, error) {
	c, err := s.dial(ctx)
	if err != nil || s.serverUID == nil {
		return c, err
	}
	p, err := s.peer(c)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("service socket %s: can't tell who serves it: %w", s.path, err)
	}
	if p.UID != *s.serverUID {
		c.Close()
		return nil, fmt.Errorf("%w: %s is served by %s, the service runs as uid %d", ErrNotService, s.path, p, *s.serverUID)
	}
	return c, nil
}

func (s *SocketAPI) dial(ctx context.Context) (net.Conn, error) {
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

// RefusingListener is a service listener that turns some local accounts
// away (the Unix socket's peer check). OnRefused reports them, for the
// service's log: the first refusal of each account, then at most one a
// minute, so a daemon that keeps trying can't fill the log.
type RefusingListener interface {
	net.Listener
	OnRefused(fn func(peer PeerCred, err error))
}

// refusalReportEvery is how often one account's refusals are reported.
const refusalReportEvery = time.Minute

// peerListener hands Accept only the connections admit lets in.
type peerListener struct {
	net.Listener
	admit PeerPolicy
	peer  func(net.Conn) (PeerCred, error)

	mu       sync.Mutex
	report   func(PeerCred, error)
	reported map[uint32]time.Time
}

func (l *peerListener) OnRefused(fn func(PeerCred, error)) {
	l.mu.Lock()
	l.report = fn
	l.mu.Unlock()
}

func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		p, err := l.peer(c)
		if err != nil {
			// Fail closed: an account that can't be named isn't served.
			p = PeerCred{UID: unknownUID}
			err = fmt.Errorf("%w: can't tell which account connected: %w", ErrPeerRefused, err)
		} else {
			err = l.admit.Admit(p)
		}
		if err == nil {
			return c, nil
		}
		l.refused(p, err)
		go RefusePeer(c)
	}
}

func (l *peerListener) refused(p PeerCred, err error) {
	l.mu.Lock()
	fn := l.report
	now := time.Now()
	last, seen := l.reported[p.UID]
	due := fn != nil && (!seen || now.Sub(last) >= refusalReportEvery)
	if due {
		if len(l.reported) >= 1024 {
			clear(l.reported) // accounts are few; this only bounds a flood
		}
		l.reported[p.UID] = now
	}
	l.mu.Unlock()
	if due {
		fn(p, err)
	}
}

// refusal is what a turned-away client reads: an HTTP answer, so the
// window can say why instead of reporting a dropped connection.
var refusal = []byte("HTTP/1.1 403 Forbidden\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"Connection: close\r\n" +
	fmt.Sprintf("Content-Length: %d\r\n\r\n", len(ErrPeerRefused.Error())+1) +
	ErrPeerRefused.Error() + "\n")

// refuseWait bounds how long a refused client gets to send its request
// and read the answer.
const refuseWait = 2 * time.Second

// maxRefusedRead bounds each of RefusePeer's two reads of a refused
// client's request, the head and then the rest: the window's are a few
// hundred bytes.
const maxRefusedRead = 64 << 10

// RefusePeer answers a connection the service won't serve with 403
// Forbidden and closes it. The request head is read first: net/http's
// client fails a round trip whose answer arrives before it has sent the
// request ("unsolicited response received on idle HTTP channel") instead
// of returning the 403. What the client sends after the head is read and
// dropped once the answer is out: closing a Unix socket with unread data
// resets the connection, and the client could lose the answer. The fake
// service socket uses it too.
func RefusePeer(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(refuseWait))
	// Answer whatever ended the head: the blank line, the size limit or
	// the client's EOF. Past the deadline the write fails at once.
	readRequestHead(bufio.NewReader(io.LimitReader(c, maxRefusedRead)))
	if _, err := c.Write(refusal); err != nil {
		return
	}
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	io.Copy(io.Discard, io.LimitReader(c, maxRefusedRead))
}

// readRequestHead reads r up to the blank line that ends an HTTP request
// head. As RFC 9112 section 2.2 lets a server, it skips empty lines before
// the request line, and a bare LF ends a line too.
func readRequestHead(r *bufio.Reader) error {
	inLine, started := false, false
	for {
		line, err := r.ReadSlice('\n')
		switch {
		case err == bufio.ErrBufferFull: // a line longer than the buffer
			inLine, started = true, true
		case err != nil:
			return err
		case !inLine && (len(line) == 1 || len(line) == 2 && line[0] == '\r'):
			if started {
				return nil
			}
		default:
			inLine, started = false, true
		}
	}
}

// controlConn runs fn on the connection's file descriptor.
func controlConn(c net.Conn, fn func(fd int) error) error {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return fmt.Errorf("%T has no file descriptor", c)
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return err
	}
	var ferr error
	if err := raw.Control(func(fd uintptr) { ferr = fn(int(fd)) }); err != nil {
		return err
	}
	return ferr
}
