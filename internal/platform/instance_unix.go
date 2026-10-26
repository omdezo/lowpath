//go:build !windows

package platform

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SocketInstance is the single-instance channel on Linux and macOS: the copy
// that runs listens on a Unix socket in a folder only this user can open; a
// later start connects, says "show", and the running copy opens its window
// with a fresh launch link (the link is never sent over the socket). The
// socket is per UI address, so test runs on other ports don't meet.
type SocketInstance struct {
	path    string
	timeout time.Duration
}

// maxSocketPath is the room for a socket path (sun_path is 104 bytes on
// macOS, 108 on Linux, with the terminating NUL).
const maxSocketPath = 103

// NewInstance returns the single-instance channel for the UI at addr: under
// $XDG_RUNTIME_DIR when the session has one, else in a private folder in
// the temporary folder (per user on macOS).
func NewInstance(addr string) Instance {
	return newSocketInstance(addr, os.Getenv("XDG_RUNTIME_DIR"), os.TempDir(), os.Getuid())
}

func newSocketInstance(addr, runtimeDir, tmp string, uid int) *SocketInstance {
	name := "ui-" + strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '-'
	}, addr) + ".sock"
	dir := filepath.Join(tmp, "lowpath-"+strconv.Itoa(uid))
	if filepath.IsAbs(runtimeDir) {
		if d := filepath.Join(runtimeDir, "lowpath"); len(filepath.Join(d, name)) <= maxSocketPath {
			dir = d
		}
	}
	if len(filepath.Join(dir, name)) > maxSocketPath {
		dir = filepath.Join("/tmp", "lowpath-"+strconv.Itoa(uid))
	}
	return &SocketInstance{path: filepath.Join(dir, name), timeout: 2 * time.Second}
}

// Path is the socket's path.
func (s *SocketInstance) Path() string { return s.path }

// Serve listens until the returned Closer is closed. It fails when another
// copy already listens, or when the folder isn't private to this user (in
// a shared temporary folder another user could have made it first).
func (s *SocketInstance) Serve(onShow func()) (io.Closer, error) {
	if err := privateDir(filepath.Dir(s.path)); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", s.path)
	if err != nil && errors.Is(err, syscall.EADDRINUSE) {
		if c, derr := net.DialTimeout("unix", s.path, s.timeout); derr == nil {
			c.Close()
			return nil, fmt.Errorf("platform: another Lowpath answers on %s", s.path)
		}
		// Left behind by a copy that was killed.
		os.Remove(s.path)
		ln, err = net.Listen("unix", s.path)
	}
	if err != nil {
		return nil, fmt.Errorf("platform: single-instance socket: %w", err)
	}
	l := &instanceListener{ln: ln, path: s.path}
	go l.accept(onShow, s.timeout)
	return l, nil
}

// Signal asks the copy that listens to show its window.
func (s *SocketInstance) Signal() bool {
	c, err := net.DialTimeout("unix", s.path, s.timeout)
	if err != nil {
		return false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(s.timeout))
	if _, err := io.WriteString(c, "show\n"); err != nil {
		return false
	}
	line, err := bufio.NewReader(io.LimitReader(c, 64)).ReadString('\n')
	return err == nil && line == "ok\n"
}

type instanceListener struct {
	ln   net.Listener
	path string
	once sync.Once
}

func (l *instanceListener) accept(onShow func(), timeout time.Duration) {
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return // closed
		}
		go func() {
			defer c.Close()
			c.SetDeadline(time.Now().Add(timeout))
			line, err := bufio.NewReader(io.LimitReader(c, 64)).ReadString('\n')
			if err != nil || line != "show\n" {
				return
			}
			io.WriteString(c, "ok\n")
			go onShow()
		}()
	}
}

func (l *instanceListener) Close() error {
	var err error
	l.once.Do(func() {
		err = l.ln.Close() // removes the socket file too
		os.Remove(l.path)
	})
	return err
}

// privateDir makes dir (mode 0700) or checks that the one there is a real
// folder of this user that nobody else can open.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	switch {
	case !fi.IsDir():
		return fmt.Errorf("platform: %s isn't a folder", dir)
	case ok && int(st.Uid) != os.Getuid():
		return fmt.Errorf("platform: %s belongs to another user", dir)
	case fi.Mode().Perm()&0o077 != 0:
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("platform: %s is open to other users: %w", dir, err)
		}
	}
	return nil
}
