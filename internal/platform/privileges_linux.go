package platform

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// NewPolkit returns the privilege helper for this process.
func NewPolkit() *Polkit {
	return &Polkit{Root: *NewRoot(), x: osExec{}, exe: executable, mount: statMount, getcap: hasFileCaps}
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
