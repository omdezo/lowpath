//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"golang.org/x/sys/unix"
)

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Ino)
}

// handBackTree is a data folder as a root copy leaves it, plus what a user
// could plant there: a hard link to a file outside, and a symbolic link to
// one. It returns the folder and the file outside.
func handBackTree(t *testing.T) (dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "data")
	outside = filepath.Join(base, "system-file")
	for _, f := range []string{outside, filepath.Join(dir, "lowpath.log"), filepath.Join(dir, "catalog", "valve.json")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(outside, filepath.Join(dir, "hard")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "catalog", "soft")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}
	return dir, outside
}

// The walk changes only what this copy made, through descriptors: never a
// symbolic link or what it points at, and never a file with a second name
// (a hard link to a system file shows root as its owner too).
func TestHandBackWalk(t *testing.T) {
	dir, outside := handBackTree(t)
	var changed []uint64
	h := handback{self: uint32(os.Geteuid()), chown: func(fd, uid, gid int) error {
		var st unix.Stat_t
		if err := unix.Fstat(fd, &st); err != nil {
			t.Error(err)
		}
		if uid != 4242 || gid != 4343 {
			t.Errorf("chown to %d:%d", uid, gid)
		}
		changed = append(changed, uint64(st.Ino))
		return nil
	}}
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		t.Fatal(err)
	}
	h.tree(fd, 4242, 4343, 0)

	want := []uint64{inode(t, filepath.Join(dir, "lowpath.log")), inode(t, filepath.Join(dir, "catalog")), inode(t, filepath.Join(dir, "catalog", "valve.json"))}
	slices.Sort(want)
	slices.Sort(changed)
	if !slices.Equal(changed, want) {
		t.Errorf("changed inodes %v, want %v", changed, want)
	}
	if slices.Contains(changed, inode(t, outside)) {
		t.Error("the hard-linked file outside was handed over")
	}

	// A folder this copy owns itself is nobody else's: nothing to do.
	changed = nil
	h.dir(dir)
	if len(changed) != 0 {
		t.Errorf("own folder: changed %v", changed)
	}
}

// As root for real (WSL or a Linux CI run as root): the user gets the log
// and settings back, a planted hard link and symbolic link don't hand a
// system file over, and another user's file stays theirs.
func TestHandBackAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	dir, outside := handBackTree(t)
	const user, other = 64500, 64501
	if err := os.Chown(dir, user, user); err != nil {
		t.Fatal(err)
	}
	theirs := filepath.Join(dir, "theirs")
	if err := os.WriteFile(theirs, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chown(theirs, other, other)

	handBack(dir)

	owner := func(p string) uint32 {
		var st unix.Stat_t
		if err := unix.Lstat(p, &st); err != nil {
			t.Fatal(err)
		}
		return st.Uid
	}
	for _, p := range []string{"lowpath.log", "catalog", "catalog/valve.json"} {
		if got := owner(filepath.Join(dir, p)); got != user {
			t.Errorf("%s owned by %d, want %d", p, got, user)
		}
	}
	if got := owner(outside); got != 0 {
		t.Errorf("file outside owned by %d after a hard and a symbolic link to it", got)
	}
	if got := owner(filepath.Dir(outside)); got != 0 {
		t.Errorf("folder outside owned by %d", got)
	}
	if got := owner(theirs); got != other {
		t.Errorf("another user's file owned by %d", got)
	}
}
