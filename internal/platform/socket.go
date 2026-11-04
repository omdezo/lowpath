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
	return newPeerListener(ln, s.admit, s.peer), nil
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

// maxPeerChecks bounds the peer checks in progress at once. Past it the
// accept loop waits for a check to finish, so an account that floods the
// socket with connections can't make the service start a goroutine for
// each.
const maxPeerChecks = 32

// peerListener hands Accept only the connections admit lets in. The checks
// run off the accept loop, one goroutine per connection: on macOS Admit can
// wait seconds on a directory service that doesn't answer, and that must
// not hold up root or the signed-in user connecting meanwhile.
type peerListener struct {
	net.Listener
	admit PeerPolicy
	peer  func(net.Conn) (PeerCred, error)

	// out carries what Accept returns: admitted connections, and the
	// accept loop's errors, which it hands on so the server can back off
	// a temporary one or stop at the last one.
	out    chan accepted
	slots  chan struct{} // one per check in progress (maxPeerChecks)
	done   chan struct{} // closed by Close
	closer sync.Once
	wg     sync.WaitGroup // the accept loop and every check

	mu       sync.Mutex
	report   func(PeerCred, error)
	reported map[uint32]time.Time
	checking map[net.Conn]struct{} // connections Close must end
}

type accepted struct {
	c   net.Conn
	err error
}

func newPeerListener(ln net.Listener, admit PeerPolicy, peer func(net.Conn) (PeerCred, error)) *peerListener {
	l := &peerListener{
		Listener: ln, admit: admit, peer: peer,
		out:      make(chan accepted),
		slots:    make(chan struct{}, maxPeerChecks),
		done:     make(chan struct{}),
		reported: map[uint32]time.Time{},
		checking: map[net.Conn]struct{}{},
	}
	l.wg.Add(1)
	go l.acceptLoop()
	return l
}

func (l *peerListener) OnRefused(fn func(PeerCred, error)) {
	l.mu.Lock()
	l.report = fn
	l.mu.Unlock()
}

// Accept returns the next connection a check admitted.
func (l *peerListener) Accept() (net.Conn, error) {
	select {
	case a := <-l.out:
		return a.c, a.err
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// Close stops accepting, ends the connections still being checked or
// refused, and returns once every goroutine the listener started has: a
// check in progress is waited for until its Admit returns.
func (l *peerListener) Close() error {
	err := net.ErrClosed
	l.closer.Do(func() {
		close(l.done)
		err = l.Listener.Close()
		l.mu.Lock()
		for c := range l.checking {
			c.Close()
		}
		l.mu.Unlock()
		l.wg.Wait()
	})
	return err
}

// acceptLoop accepts connections and starts a check for each. The peer's
// credentials are read here: the kernel recorded them at connect, so the
// read is quick, and the checks see the connections in the order they came.
func (l *peerListener) acceptLoop() {
	defer l.wg.Done()
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			select {
			case l.out <- accepted{err: err}:
			case <-l.done:
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue // temporary (out of file descriptors): Accept's caller backs off
		}
		p, perr := l.peer(c)
		select {
		case l.slots <- struct{}{}:
		case <-l.done:
			c.Close()
			return
		}
		if !l.track(c) {
			<-l.slots
			c.Close()
			return
		}
		l.wg.Add(1)
		go l.check(c, p, perr)
	}
}

// track records a connection Close must end; false once closed.
func (l *peerListener) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.done:
		return false
	default:
	}
	l.checking[c] = struct{}{}
	return true
}

func (l *peerListener) untrack(c net.Conn) {
	l.mu.Lock()
	delete(l.checking, c)
	l.mu.Unlock()
}

// check admits c and hands it to Accept, or answers it with 403.
func (l *peerListener) check(c net.Conn, p PeerCred, err error) {
	defer l.wg.Done()
	if err != nil {
		// Fail closed: an account that can't be named isn't served.
		p = PeerCred{UID: unknownUID}
		err = fmt.Errorf("%w: can't tell which account connected: %w", ErrPeerRefused, err)
	} else {
		err = l.admit.Admit(p)
	}
	<-l.slots // the slow part is over: let the next check start
	if err != nil {
		l.refused(p, err)
		RefusePeer(c) // Close ends it early by closing c
		l.untrack(c)
		return
	}
	// From here c is the server's once Accept returns it, and a Close
	// during a graceful shutdown must leave it alone; until then it is
	// closed here if the listener closes first.
	l.untrack(c)
	select {
	case l.out <- accepted{c: c}:
	case <-l.done:
		c.Close()
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
