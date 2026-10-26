// Package detect finds which supported games are installed or running, and
// reads program icons.
package detect

import (
	"errors"
	"image"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// System finds games on this computer: running processes, Steam libraries
// and program icons. New picks the implementation for the operating system
// the binary is built for; parts not implemented on an OS report nothing (or
// an error matching errors.ErrUnsupported), never made-up results.
type System struct {
	steam []string // Steam client install folders to look in
}

// New returns the detector for this operating system.
func New() *System { return NewWithSteam(steamBases()) }

// NewWithSteam is New with the Steam client folders given (tests, or a
// Steam installed somewhere unusual).
func NewWithSteam(steamDirs []string) *System { return &System{steam: steamDirs} }

// Running returns the lower-cased image names of all running processes.
func (s *System) Running() map[string]bool { return running() }

// RunningExe returns the full path of a running process with one of the
// given image names, or "" when none runs or the OS won't tell.
func (s *System) RunningExe(names []string) string { return runningExe(names) }

// ExeIcon returns the main icon of a program, rendered at size×size.
func (s *System) ExeIcon(path string, size int) (image.Image, error) { return exeIcon(path, size) }

// SteamLibraries returns every steamapps/common directory we can find: the
// Steam client's own and every library listed in its libraryfolders.vdf.
func (s *System) SteamLibraries() []string {
	var roots []string
	for _, base := range s.steam {
		roots = append(roots, base)
		vdf, err := os.ReadFile(filepath.Join(base, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range vdfPath.FindAllStringSubmatch(string(vdf), -1) {
			roots = append(roots, filepath.FromSlash(strings.ReplaceAll(m[1], `\\`, `\`)))
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range roots {
		dir := filepath.Join(r, "steamapps", "common")
		key := strings.ToLower(filepath.Clean(dir))
		if seen[key] {
			continue
		}
		seen[key] = true
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

// SteamRoots returns the Steam client install folders that exist (where the
// appcache with each game's library artwork lives).
func (s *System) SteamRoots() []string {
	var out []string
	for _, dir := range s.steam {
		if fi, err := os.Stat(filepath.Join(dir, "appcache")); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

var vdfPath = regexp.MustCompile(`"path"\s+"([^"]+)"`)

// Installed reports whether any of the given absolute paths, or a Steam
// library folder with the given name, exists.
func Installed(paths []string, steamDir string, libs []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	if steamDir != "" {
		for _, l := range libs {
			if _, err := os.Stat(filepath.Join(l, steamDir)); err == nil {
				return true
			}
		}
	}
	return false
}

// errFound stops the directory walk once the program turns up.
var errFound = errors.New("found")

// FindExe looks under roots (at most depth levels down, and at most limit
// entries in all) for a file with one of the given names, case-insensitive.
// Game folders are deep (UE games keep the binary in Game\Binaries\Win64),
// but the bounds keep a stray root like C:\ from turning into a disk scan.
func FindExe(roots, names []string, depth, limit int) string {
	want := map[string]bool{}
	for _, n := range names {
		want[strings.ToLower(n)] = true
	}
	seen := 0
	for _, root := range roots {
		root = filepath.Clean(root)
		base := strings.Count(root, string(filepath.Separator))
		var found string
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if seen++; seen > limit {
				return fs.SkipAll
			}
			if d.IsDir() {
				if strings.Count(p, string(filepath.Separator))-base >= depth {
					return fs.SkipDir
				}
				return nil
			}
			if want[strings.ToLower(d.Name())] {
				found = p
				return errFound
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return ""
}
