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
	"slices"
	"strings"

	"lowpath/internal/owner"
)

// System finds games on this computer: running processes, Steam libraries,
// Wine prefixes, launcher installs and program icons. New picks the
// implementation for the operating system the binary is built for; parts
// not implemented on an OS report nothing (or an error matching
// errors.ErrUnsupported), never made-up results.
type System struct {
	loc Locations
	// procs lists running programs (Linux, macOS); nil means the OS's own
	// process snapshot (Windows).
	procs owner.ProcessTable
}

// NewWithSteam is New with the Steam client folders given (tests, or a
// Steam installed somewhere unusual).
func NewWithSteam(steamDirs []string) *System {
	return &System{loc: Locations{Steam: steamDirs}, procs: defaultProcs()}
}

// NewSystem looks in loc and lists processes with procs (nil: the OS's own
// process snapshot, which only Windows has).
func NewSystem(procs owner.ProcessTable, loc Locations) *System {
	return &System{loc: loc, procs: procs}
}

// Running returns the lower-cased image names of all running processes:
// on Linux and macOS every name a process goes by (see owner.Program).
func (s *System) Running() map[string]bool {
	if s.procs == nil {
		return running()
	}
	out := map[string]bool{}
	progs, _ := s.procs.Programs() // unreadable: nothing found runs
	for _, p := range progs {
		for _, n := range p.Names {
			out[strings.ToLower(n)] = true
		}
	}
	return out
}

// RunningExe returns the full path of a running process with one of the
// given image names, or "" when none runs or the OS won't tell.
func (s *System) RunningExe(names []string) string {
	if s.procs == nil {
		return runningExe(names)
	}
	progs, _ := s.procs.Programs()
	for _, p := range progs {
		if p.Exe == "" || !anyFold(p.Names, names) {
			continue
		}
		if fi, err := os.Stat(p.Exe); err == nil && !fi.IsDir() {
			return p.Exe
		}
	}
	return ""
}

// ExeIcon returns the main icon of a program, rendered at size×size.
func (s *System) ExeIcon(path string, size int) (image.Image, error) { return exeIcon(path, size) }

// anyFold reports whether a and b share a name, ignoring case.
func anyFold(a, b []string) bool {
	return slices.ContainsFunc(a, func(x string) bool {
		return slices.ContainsFunc(b, func(y string) bool { return strings.EqualFold(x, y) })
	})
}

// SteamLibraries returns every steamapps/common directory we can find: the
// Steam client's own and every library listed in its libraryfolders.vdf.
func (s *System) SteamLibraries() []string {
	var roots []string
	for _, base := range s.loc.Steam {
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
		key := dirKey(dir)
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
// appcache with each game's library artwork lives), each once.
func (s *System) SteamRoots() []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range s.loc.Steam {
		if k := dirKey(dir); !seen[k] && isDir(filepath.Join(dir, "appcache")) {
			seen[k] = true
			out = append(out, dir)
		}
	}
	return out
}

var vdfPath = regexp.MustCompile(`"path"\s+"([^"]+)"`)

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
