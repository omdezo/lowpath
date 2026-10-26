package platform

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
)

// peerQueue plays the kernel's peer credentials: each connection gets the
// next one, in order.
type peerQueue struct {
	mu    sync.Mutex
	peers []PeerCred
	errs  []error
}

func (q *peerQueue) push(p PeerCred, err error) {
	q.mu.Lock()
	q.peers, q.errs = append(q.peers, p), append(q.errs, err)
	q.mu.Unlock()
}

func (q *peerQueue) next(net.Conn) (PeerCred, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.peers) == 0 {
		return PeerCred{}, errors.New("test: no peer queued")
	}
	p, err := q.peers[0], q.errs[0]
	q.peers, q.errs = q.peers[1:], q.errs[1:]
	return p, err
}

// get asks the API at api for /version on a fresh connection.
func get(t *testing.T, api *SocketAPI) (int, string, error) {
	t.Helper()
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return api.Dial(ctx) },
		DisableKeepAlives: true,
	}
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://lowpath/version", nil)
	res, err := tr.RoundTrip(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return res.StatusCode, string(b), err
}

func TestSocketAPIPeerCheck(t *testing.T) {
	q := &peerQueue{}
	policy := &SessionPolicy{Self: 0, Sources: []Sessions{
		sessionsFake{name: "logind", in: map[uint32]bool{1000: true}},
	}}
	api := NewSocketAPI(filepath.Join(socketDir(t), "api.sock"), testMode(), policy)
	api.peer = q.next
	ln, err := api.Listen()
	if err != nil {
		t.Fatal(err)
	}
	type refusal struct {
		peer PeerCred
		err  error
	}
	reports := make(chan refusal, 16)
	rl, ok := ln.(RefusingListener)
	if !ok {
		t.Fatalf("%T is not a RefusingListener", ln)
	}
	rl.OnRefused(func(p PeerCred, err error) { reports <- refusal{p, err} })
	served := make(chan struct{}, 16)
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- struct{}{}
		io.WriteString(w, `{"version":"test"}`)
	})}
	go hs.Serve(ln)
	defer hs.Close()

	for _, tc := range []struct {
		name   string
		peer   PeerCred
		err    error
		status int
		report bool // the service log hears of it
	}{
		{"signed in", PeerCred{UID: 1000, PID: 4242}, nil, http.StatusOK, false},
		{"root", PeerCred{UID: 0, PID: 1}, nil, http.StatusOK, false},
		{"nobody", PeerCred{UID: 65534, PID: 99}, nil, http.StatusForbidden, true},
		{"nobody again: not logged twice", PeerCred{UID: 65534, PID: 100}, nil, http.StatusForbidden, false},
		{"another daemon", PeerCred{UID: 33, PID: 101}, nil, http.StatusForbidden, true},
		{"unknown account", PeerCred{}, errors.New("getsockopt: bad file descriptor"), http.StatusForbidden, true},
		{"signed in after refusals", PeerCred{UID: 1000, PID: 4243}, nil, http.StatusOK, false},
	} {
		q.push(tc.peer, tc.err)
		status, body, err := get(t, api)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if status != tc.status {
			t.Errorf("%s: status %d, want %d", tc.name, status, tc.status)
		}
		if status == http.StatusForbidden {
			if strings.TrimSpace(body) != ErrPeerRefused.Error() {
				t.Errorf("%s: body %q", tc.name, body)
			}
			select {
			case <-served:
				t.Errorf("%s: the API saw a refused request", tc.name)
			default:
			}
		} else {
			<-served
		}
		want := tc.peer
		if tc.err != nil {
			want = PeerCred{UID: unknownUID} // not uid 0: root is never refused
		}
		select {
		case r := <-reports:
			if !tc.report {
				t.Errorf("%s: reported %v", tc.name, r.err)
			} else if !errors.Is(r.err, ErrPeerRefused) || r.peer != want {
				t.Errorf("%s: report %v %v", tc.name, r.peer, r.err)
			}
		default:
			if tc.report {
				t.Errorf("%s: not reported", tc.name)
			}
		}
	}
}

func TestSocketAPIServerCheck(t *testing.T) {
	path := filepath.Join(socketDir(t), "api.sock")
	ln, err := NewSocketAPI(path, testMode(), nil).Listen()
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tc := range []struct {
		name    string
		peer    PeerCred
		peerErr error
		want    error // nil: connected
	}{
		{"root serves it", PeerCred{UID: 0, PID: 1}, nil, nil},
		{"a user serves it", PeerCred{UID: 1000, PID: 4242}, nil, ErrNotService},
		{"can't tell", PeerCred{}, errors.New("getsockopt: not supported"), errors.New("can't tell")},
	} {
		api := NewSocketAPI(path, testMode(), nil).WithServer(0)
		api.peer = func(net.Conn) (PeerCred, error) { return tc.peer, tc.peerErr }
		c, err := api.Dial(ctx)
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != nil && err == nil:
			c.Close()
			t.Errorf("%s: connected", tc.name)
		case tc.want == ErrNotService && !errors.Is(err, ErrNotService):
			t.Errorf("%s: %v, want ErrNotService", tc.name, err)
		case tc.want != nil && tc.want != ErrNotService && !strings.Contains(err.Error(), tc.want.Error()):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
		if c != nil {
			c.Close()
		}
	}

	// Nothing listening is still ErrServiceNotRunning, not a server check.
	ln.Close()
	api := NewSocketAPI(path, testMode(), nil).WithServer(0)
	api.peer = func(net.Conn) (PeerCred, error) { t.Error("peer read without a connection"); return PeerCred{}, nil }
	if _, err := api.Dial(ctx); !errors.Is(err, ErrServiceNotRunning) {
		t.Errorf("no service: %v", err)
	}
}

// A refused client that never reads can't hold up the others.
func TestSocketAPIRefusedClientDoesNotBlock(t *testing.T) {
	q := &peerQueue{}
	api := NewSocketAPI(filepath.Join(socketDir(t), "api.sock"), testMode(), &SessionPolicy{})
	api.peer = q.next
	ln, err := api.Listen()
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
	})}
	go hs.Serve(ln)
	defer hs.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for range 5 {
		q.push(PeerCred{UID: 65534}, nil)
		c, err := api.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close() // never reads or writes
	}
	q.push(PeerCred{UID: 0}, nil)
	start := time.Now()
	if status, _, err := get(t, api); err != nil || status != http.StatusOK {
		t.Fatalf("root after silent refused clients: %d, %v", status, err)
	}
	if d := time.Since(start); d > refuseWait {
		t.Errorf("took %v", d)
	}
}

func TestReadRequestHead(t *testing.T) {
	long := strings.Repeat("a", 5000) // past bufio's 4096-byte buffer
	for _, tc := range []struct {
		name string
		in   string
		rest string // left unread; "" with err: none of it is a head
		err  error
	}{
		{"request", "GET /version HTTP/1.1\r\nHost: lowpath\r\n\r\n", "", nil},
		{"body left unread", "POST /api/stop HTTP/1.1\r\nContent-Length: 2\r\n\r\n{}", "{}", nil},
		{"bare LF", "GET / HTTP/1.1\nHost: lowpath\n\nnext", "next", nil},
		{"empty lines before the request line", "\r\n\nGET / HTTP/1.1\r\n\r\n", "", nil},
		{"long header line", "GET / HTTP/1.1\r\nX-Long: " + long + "\r\n\r\nbody", "body", nil},
		// The buffer fills just before "\r\n": the line's end is not a
		// blank line.
		{"CRLF across the buffer's end", "GET / HTTP/1.1\r\nX: " + strings.Repeat("b", 4096-4) + "\r\nY: 1\r\n\r\n", "", nil},
		{"cut short", "GET / HTTP/1.1\r\nHost: lowpath\r\n", "", io.EOF},
		{"nothing sent", "", "", io.EOF},
		{"only empty lines", "\r\n\r\n", "", io.EOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// One byte a read: the head arrives in pieces.
			r := bufio.NewReader(iotest.OneByteReader(strings.NewReader(tc.in)))
			if err := readRequestHead(r); err != tc.err {
				t.Fatalf("err %v, want %v", err, tc.err)
			}
			if rest, _ := io.ReadAll(r); string(rest) != tc.rest {
				t.Errorf("left %q, want %q", rest, tc.rest)
			}
		})
	}
}

// RefusePeer answers only once the request head is in: net/http's client
// fails on an answer that comes before its request (#197). Whatever the
// client sends after the head, it reads the whole 403.
func TestRefusePeer(t *testing.T) {
	type conns func(t *testing.T) (client, server net.Conn)
	pipe := func(*testing.T) (net.Conn, net.Conn) { return net.Pipe() } // the fake service's
	unix := func(t *testing.T) (net.Conn, net.Conn) {
		ln, err := net.Listen("unix", filepath.Join(socketDir(t), "api.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		client, err := net.Dial("unix", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		server, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		return client, server
	}
	for _, tc := range []struct {
		name  string
		parts []string // written in turn
		// head is how many parts make the request head: before each of
		// them nothing may be answered. The rest are written while the
		// answer is read, as net/http's client does.
		head       int
		closeWrite bool // the client ends its side after the parts
		unixOnly   bool // net.Pipe can't half-close
	}{
		{"request", []string{"GET /version HTTP/1.1\r\nHost: lowpath\r\n\r\n"}, 1, false, false},
		{"head in pieces", []string{"GET /version HTTP/1.1\r\n", "Host: lowpath\r", "\n", "\r\n"}, 4, false, false},
		{"empty line first", []string{"\r\n", "GET / HTTP/1.1\r\n\r\n"}, 2, false, false},
		{"body after the head", []string{"POST /api/stop HTTP/1.1\r\nHost: lowpath\r\nContent-Length: 4096\r\n\r\n", strings.Repeat("x", 4096)}, 1, false, false},
		{"head over the limit", []string{"GET / HTTP/1.1\r\nX: " + strings.Repeat("a", maxRefusedRead)}, 0, false, false},
		{"client stops mid-head", []string{"GET / HTTP/1.1\r\n"}, 1, true, true},
	} {
		for _, kind := range []struct {
			name  string
			conns conns
		}{{"pipe", pipe}, {"unix", unix}} {
			if tc.unixOnly && kind.name != "unix" {
				continue
			}
			t.Run(kind.name+"/"+tc.name, func(t *testing.T) {
				client, server := kind.conns(t)
				defer client.Close()
				done := make(chan struct{})
				go func() { RefusePeer(server); close(done) }()

				one := make([]byte, 1)
				for _, p := range tc.parts[:tc.head] {
					// Nothing to read yet: the answer waits for the head.
					client.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
					if n, err := client.Read(one); n > 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Fatalf("answered before the head was sent: %q, %v", one[:n], err)
					}
					if _, err := io.WriteString(client, p); err != nil {
						t.Fatal(err)
					}
				}
				wrote := make(chan error, 1)
				go func() {
					for _, p := range tc.parts[tc.head:] {
						if _, err := io.WriteString(client, p); err != nil {
							wrote <- err
							return
						}
					}
					if tc.closeWrite {
						wrote <- client.(*net.UnixConn).CloseWrite()
						return
					}
					wrote <- nil
				}()

				client.SetReadDeadline(time.Now().Add(5 * time.Second))
				res, err := http.ReadResponse(bufio.NewReader(client), nil)
				if err != nil {
					t.Fatalf("no answer: %v", err)
				}
				body, err := io.ReadAll(res.Body)
				if err != nil || res.StatusCode != http.StatusForbidden || strings.TrimSpace(string(body)) != ErrPeerRefused.Error() {
					t.Errorf("answer %s %q, %v", res.Status, body, err)
				}
				if err := <-wrote; err != nil {
					t.Errorf("write: %v", err)
				}
				client.Close()
				select {
				case <-done:
				case <-time.After(refuseWait / 2): // not left to the deadline
					t.Error("RefusePeer still runs after the client left")
				}
			})
		}
	}
}
