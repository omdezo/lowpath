//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func waitForExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p, err := os.FindProcess(pid)
		if err != nil || p.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// stopSignals end the app cleanly (capture off, networking back to normal):
// Ctrl+C, a kill or shutdown, and the terminal or session going away.
func stopSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP} }

// handBack gives the files under each of dirs back to the user that owns
// the folder, when this process runs as root on that user's behalf (the
// copy restarted with administrator rights on macOS, or sudo with -data).
// Otherwise the user's next normal start couldn't append to its own log or
// replace its settings.
func handBack(dirs ...string) {
	if os.Geteuid() != 0 {
		return
	}
	for _, dir := range dirs {
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || st.Uid == 0 {
			continue
		}
		uid, gid := int(st.Uid), int(st.Gid)
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if info, err := d.Info(); err == nil {
				if s, ok := info.Sys().(*syscall.Stat_t); ok && int(s.Uid) != uid {
					os.Lchown(path, uid, gid)
				}
			}
			return nil
		})
	}
}
