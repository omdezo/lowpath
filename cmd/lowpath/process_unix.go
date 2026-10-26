//go:build !windows

package main

import (
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
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
	h := handback{self: 0, chown: unix.Fchown}
	for _, dir := range dirs {
		h.dir(dir)
	}
}

// maxHandBackDepth bounds the walk: Lowpath's own folders are two deep.
const maxHandBackDepth = 16

// handback walks a folder the user can write to while running as root, so
// it never goes by path: each entry is opened relative to its folder's
// descriptor without following symbolic links, checked through that
// descriptor, and changed through it. A folder swapped for a link halfway
// through therefore can't send a chown anywhere else.
type handback struct {
	self  uint32 // the user this copy runs as (root); only its files change hands
	chown func(fd, uid, gid int) error
}

// dir hands back what self made under dir to the folder's owner.
func (h handback) dir(dir string) {
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Uid == h.self {
		unix.Close(fd)
		return
	}
	h.tree(fd, int(st.Uid), int(st.Gid), 0)
}

// tree hands back the entries of the folder open as fd, and closes fd.
func (h handback) tree(fd, uid, gid, depth int) {
	f := os.NewFile(uintptr(fd), "")
	defer f.Close()
	names, _ := f.Readdirnames(-1)
	for _, name := range names {
		// O_NOFOLLOW: a symbolic link isn't opened at all; O_NONBLOCK: a
		// FIFO doesn't hang the walk.
		cfd, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		}
		var st unix.Stat_t
		if unix.Fstat(cfd, &st) != nil {
			unix.Close(cfd)
			continue
		}
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		// Only what this copy made: files of other users stay theirs, and
		// a file with more than one name is a hard link the user may have
		// made to a system file (which also shows root as its owner).
		if st.Uid == h.self && (isDir || st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1) {
			h.chown(cfd, uid, gid)
		}
		if isDir && depth < maxHandBackDepth {
			h.tree(cfd, uid, gid, depth+1)
			continue
		}
		unix.Close(cfd)
	}
}
