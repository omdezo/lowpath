package platform

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// socketDir is a short temporary folder: a Unix socket path must fit in
// 108 bytes, which t.TempDir on macOS can exceed.
func socketDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "lp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func testMode() os.FileMode {
	if runtime.GOOS == "windows" {
		return 0 // AF_UNIX files on Windows have no Unix modes
	}
	return 0o600
}

func TestSocketAPI(t *testing.T) {
	path := filepath.Join(socketDir(t), "run", "api.sock")
	api := NewSocketAPI(path, testMode(), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Nothing listens yet: the window learns the service isn't running.
	if _, err := api.Dial(ctx); !errors.Is(err, ErrServiceNotRunning) {
		t.Fatalf("dial with no service: %v, want ErrServiceNotRunning", err)
	}

	ln, err := api.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("socket mode %v, %v; want 0600", fi.Mode(), err)
		}
	}
	for range 3 { // several connections, one after another
		c, err := api.Dial(ctx)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("ping"))
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("echo: %q, %v", buf, err)
		}
		c.Close()
	}

	// A second copy can't take the socket from a running one.
	if ln2, err := NewSocketAPI(path, testMode(), nil).Listen(); err == nil {
		ln2.Close()
		t.Fatal("a second listener took the socket of a running service")
	}

	// After a crash the file stays behind; the next start takes over.
	ln.Close()
	os.WriteFile(path, nil, 0o600) // what a killed process leaves (Close removed ours)
	ln3, err := api.Listen()
	if err != nil {
		t.Fatalf("listen over a stale socket file: %v", err)
	}
	ln3.Close()
}

func TestSocketAPIKeepsOtherFiles(t *testing.T) {
	path := filepath.Join(socketDir(t), "api.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ln, err := NewSocketAPI(path, testMode(), nil).Listen(); err == nil {
		ln.Close()
		t.Fatal("listen replaced a regular file")
	}
	if b, _ := os.ReadFile(path); string(b) != "not a socket" {
		t.Errorf("file changed to %q", b)
	}
}
