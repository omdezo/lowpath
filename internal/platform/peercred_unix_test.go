//go:build linux || darwin

package platform

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The kernel names this process at both ends of a socket it connects to
// itself.
func TestConnPeerCred(t *testing.T) {
	path := filepath.Join(socketDir(t), "peer.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server := <-accepted
	defer server.Close()
	want := PeerCred{UID: uint32(os.Geteuid()), PID: os.Getpid()}
	for name, c := range map[string]net.Conn{"server side": server, "client side": client} {
		got, err := connPeerCred(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != want {
			t.Errorf("%s: %v, want %v", name, got, want)
		}
	}
	if _, err := connPeerCred(fakeConn{}); err == nil {
		t.Error("a connection with no descriptor has a peer")
	}
}

type fakeConn struct{ net.Conn }

// peerChildEnv names the socket a child started by
// TestSocketAPIPeerCheckAsRoot asks.
const peerChildEnv = "LOWPATH_PEER_CHILD"

// TestPeerChild is the other account's side of
// TestSocketAPIPeerCheckAsRoot: it asks the socket for /version and prints
// the status.
func TestPeerChild(t *testing.T) {
	path := os.Getenv(peerChildEnv)
	if path == "" {
		t.Skip("run by TestSocketAPIPeerCheckAsRoot")
	}
	status, _, err := get(t, NewSocketAPI(path, 0, nil))
	if err != nil {
		fmt.Printf("child: error %v\n", err)
		return
	}
	fmt.Printf("child: status %d\n", status)
}

// As root on Linux, the real check against other real accounts: children
// of this test run as nobody and as two people's uids and connect to a
// service socket that uses the kernel's peer credentials (no fakes but the
// session list in the second half). In WSL2: wsl -u root.
func TestSocketAPIPeerCheckAsRoot(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("needs root on Linux")
	}
	dir := socketDir(t)
	os.Chmod(dir, 0o755) // other accounts reach the socket in it
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ask := func(t *testing.T, path string, uid uint32) string {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestPeerChild$", "-test.v")
		cmd.Env = append(os.Environ(), peerChildEnv+"="+path, "HOME=/tmp")
		cmd.Dir = "/"
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid}}
		out, err := cmd.CombinedOutput()
		for _, line := range strings.Split(string(out), "\n") {
			if s, ok := strings.CutPrefix(line, "child: "); ok {
				return s
			}
		}
		t.Fatalf("child as uid %d: %v\n%s", uid, err, out)
		return ""
	}
	serve := func(t *testing.T, name string, policy PeerPolicy) string {
		path := filepath.Join(dir, name)
		ln, err := NewSocketAPI(path, 0o666, policy).Listen()
		if err != nil {
			t.Fatal(err)
		}
		hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"version":"test"}`)
		})}
		go hs.Serve(ln)
		t.Cleanup(func() { hs.Close() })
		return path
	}

	t.Run("this system's sessions", func(t *testing.T) {
		p := NewSessionPolicy()
		path := serve(t, "native.sock", p)
		_, logind := os.Stat("/run/systemd/sessions")
		r := LoginDefsUsers("/etc/login.defs")
		t.Logf("logind: %v; people's uids %d to %d", logind == nil, r.Min, r.Max)
		if got := ask(t, path, 65534); got != "status 403" {
			t.Errorf("nobody: %s, want status 403", got)
		}
		for _, uid := range []uint32{1000, 1001} {
			want := "status 403"
			if p.Admit(PeerCred{UID: uid}) == nil {
				want = "status 200"
			}
			if got := ask(t, path, uid); got != want {
				t.Errorf("uid %d: %s, want %s", uid, got, want)
			}
		}
	})

	t.Run("logind sessions", func(t *testing.T) {
		sessions := t.TempDir()
		writeFiles(t, sessions, map[string]string{
			"2":  "UID=1000\nUSER=example\nSTATE=active\nCLASS=user\n",
			"c1": "UID=1001\nSTATE=online\nCLASS=greeter\n",
		})
		path := serve(t, "logind.sock", &SessionPolicy{Self: 0, Sources: []Sessions{Logind{Dir: sessions}, LoginDefsUsers("/etc/login.defs")}})
		for uid, want := range map[uint32]string{1000: "status 200", 1001: "status 403", 1002: "status 403", 65534: "status 403"} {
			if got := ask(t, path, uid); got != want {
				t.Errorf("uid %d: %s, want %s", uid, got, want)
			}
		}
	})

	// The window's check on the installed service's socket: root serves it.
	t.Run("server is root", func(t *testing.T) {
		path := serve(t, "server.sock", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := NewSocketAPI(path, 0, nil).WithServer(0).Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	})
}
