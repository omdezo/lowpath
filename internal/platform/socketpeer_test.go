package platform

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
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
