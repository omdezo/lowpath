//go:build !windows

package platform

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A later start reaches the copy that runs, which shows its window; the
// socket is gone after Close, and a later Serve takes over.
func TestSocketInstance(t *testing.T) {
	dir := t.TempDir()
	a := newSocketInstance("127.0.0.1:7878", dir, os.TempDir(), os.Getuid())
	if filepath.Dir(a.Path()) != filepath.Join(dir, "lowpath") {
		t.Fatalf("socket at %s, want under the runtime folder", a.Path())
	}
	if a.Signal() {
		t.Fatal("Signal answered with nobody listening")
	}
	shown := make(chan struct{}, 4)
	l, err := a.Serve(func() { shown <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Dir(a.Path())); fi.Mode().Perm() != 0o700 {
		t.Errorf("socket folder mode %v, want 0700", fi.Mode().Perm())
	}
	b := newSocketInstance("127.0.0.1:7878", dir, os.TempDir(), os.Getuid())
	if !b.Signal() {
		t.Fatal("second start not answered")
	}
	select {
	case <-shown:
	case <-time.After(5 * time.Second):
		t.Fatal("onShow not called")
	}
	if _, err := b.Serve(func() {}); err == nil || !strings.Contains(err.Error(), "another Lowpath") {
		t.Errorf("second Serve: %v, want refused", err)
	}
	// Another UI address is another instance.
	other := newSocketInstance("127.0.0.1:7879", dir, os.TempDir(), os.Getuid())
	if other.Path() == a.Path() || other.Signal() {
		t.Error("instances on other ports meet")
	}
	// Garbage on the socket doesn't show anything.
	c, err := net.Dial("unix", a.Path())
	if err == nil {
		c.Write([]byte("open the pod bay doors\n"))
		c.Close()
	}
	select {
	case <-shown:
		t.Error("onShow for a bad request")
	case <-time.After(100 * time.Millisecond):
	}
	l.Close()
	if _, err := os.Stat(a.Path()); !os.IsNotExist(err) {
		t.Error("socket left after Close")
	}
	if b.Signal() {
		t.Error("Signal answered after Close")
	}
}

// A socket file left by a killed copy is replaced.
// The single-instance socket and the background service's API socket
// (DefaultSocketPath) never share a folder, even for root with
// XDG_RUNTIME_DIR=/run.
func TestSocketInstanceNotInServiceFolder(t *testing.T) {
	svcDir := filepath.Dir(DefaultSocketPath)
	s := newSocketInstance("127.0.0.1:7878", filepath.Dir(svcDir), os.TempDir(), 0)
	if filepath.Dir(s.Path()) == svcDir {
		t.Errorf("single-instance socket %s is in the service's folder", s.Path())
	}
	if s.Path() == DefaultSocketPath {
		t.Error("single-instance socket is the service's socket")
	}
}

func TestSocketInstanceStale(t *testing.T) {
	dir := t.TempDir()
	s := newSocketInstance("127.0.0.1:7878", dir, os.TempDir(), os.Getuid())
	os.MkdirAll(filepath.Dir(s.Path()), 0o700)
	ln, err := net.Listen("unix", s.Path())
	if err != nil {
		t.Fatal(err)
	}
	// Close the listener without removing the file, as a kill -9 leaves it.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Stat(s.Path()); err != nil {
		t.Fatalf("no stale socket to test with: %v", err)
	}
	l, err := s.Serve(func() {})
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	l.Close()
}

// Without a runtime folder, or with one too long for a socket path, the
// socket goes in a private folder of this user in the temporary folder;
// one there that is open to others is closed, a non-folder is refused.
func TestSocketInstanceFallbacks(t *testing.T) {
	tmp := t.TempDir()
	s := newSocketInstance("127.0.0.1:7878", "", tmp, 1000)
	if s.Path() != filepath.Join(tmp, "lowpath-1000", "ui-127-0-0-1-7878.sock") {
		t.Errorf("socket at %s", s.Path())
	}
	long := "/" + strings.Repeat("x", 120)
	if s := newSocketInstance("127.0.0.1:7878", long, tmp, 1000); strings.HasPrefix(s.Path(), long) {
		t.Errorf("socket path over the limit: %s", s.Path())
	}
	if s := newSocketInstance("127.0.0.1:7878", "", long, 1000); !strings.HasPrefix(s.Path(), "/tmp/lowpath-1000/") {
		t.Errorf("long temp folder: %s", s.Path())
	}

	open := filepath.Join(tmp, "open")
	os.Mkdir(open, 0o777)
	os.Chmod(open, 0o777)
	if err := privateDir(open); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Errorf("mode %v after privateDir", fi.Mode().Perm())
	}
	file := filepath.Join(tmp, "file")
	os.WriteFile(file, nil, 0o600)
	if err := privateDir(file); err == nil {
		t.Error("a file taken for the socket folder")
	}
	link := filepath.Join(tmp, "link")
	os.Symlink(open, link)
	if err := privateDir(link); err == nil {
		t.Error("a symbolic link taken for the socket folder")
	}
}
