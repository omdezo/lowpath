// Package detect finds which supported games are installed or running.
package detect

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Running returns the lower-cased image names of all running processes.
func Running() map[string]bool { return running() }

// SteamLibraries returns every steamapps/common directory we can find.
func SteamLibraries() []string {
	var roots []string
	for _, base := range []string{
		os.Getenv("ProgramFiles(x86)") + `\Steam`,
		os.Getenv("ProgramFiles") + `\Steam`,
	} {
		if base == `\Steam` {
			continue
		}
		roots = append(roots, base)
		vdf, err := os.ReadFile(filepath.Join(base, "steamapps", "libraryfolders.vdf"))
		if err != nil {
			continue
		}
		for _, m := range vdfPath.FindAllStringSubmatch(string(vdf), -1) {
			roots = append(roots, strings.ReplaceAll(m[1], `\\`, `\`))
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

// SteamRoots returns the Steam client install folders that exist (where the
// appcache with each game's library artwork lives).
func SteamRoots() []string {
	var out []string
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles")} {
		if base == "" {
			continue
		}
		dir := filepath.Join(base, "Steam")
		if fi, err := os.Stat(filepath.Join(dir, "appcache")); err == nil && fi.IsDir() {
			out = append(out, dir)
		}
	}
	return out
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
