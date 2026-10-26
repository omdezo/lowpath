package platform

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// NewPolkit returns the privilege helper for this process.
func NewPolkit() *Polkit {
	return &Polkit{Root: *NewRoot(), x: osExec{}, exe: executable, mount: statMount, getcap: hasFileCaps,
		noNewPrivs: noNewPrivs, ownerOnly: ownerOnly}
}

// noNewPrivs reports this process's no_new_privs flag.
func noNewPrivs() bool {
	v, err := unix.PrctlRetInt(unix.PR_GET_NO_NEW_PRIVS, 0, 0, 0, 0)
	return err == nil && v == 1
}

// ownerOnly makes path rwx for its owner only (0700 at most) when this user
// owns it. A file someone else owns (root's /usr/local/bin) is a shared
// install whose rights its owner chose; it is left as it is.
func ownerOnly(path string) error {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return err
	}
	if int(st.Uid) != os.Getuid() || st.Mode&0o077 == 0 {
		return nil
	}
	return os.Chmod(path, os.FileMode(st.Mode&0o700))
}

func nativePrivileges() Privileges { return NewPolkit() }

// executable is this program's file, with symbolic links resolved: the
// capabilities go on the file itself.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func statMount(path string) (mountFlags, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return mountFlags{}, err
	}
	return mountFlags{readOnly: st.Flags&unix.ST_RDONLY != 0, noSUID: st.Flags&unix.ST_NOSUID != 0}, nil
}

// hasFileCaps reports whether path carries file capabilities.
func hasFileCaps(path string) bool {
	n, err := unix.Getxattr(path, "security.capability", nil)
	return err == nil && n > 0
}
