package owner

import (
	"path"
	"slices"
	"strings"
)

// procFacts is what a Unix process table tells about one process, before it
// is turned into a Program. Linux (/proc) and macOS (libproc, sysctl) fill
// it in; the rules that pick names and paths are the same on both, so they
// live here and are tested on every OS.
type procFacts struct {
	pid   uint32
	comm  string // kernel short name, possibly cut short
	exe   string // resolved program file, "" when unknown
	argv0 string // first argument, "" when unknown
	// env looks up the process's environment; nil when it can't be read
	// (another user's process). Only Wine processes need it.
	env func(key string) string
}

// winePath reports whether s is a Windows path with a drive letter
// ("C:\Games\x.exe", "z:/home/x.exe"), the argv[0] Wine gives a Windows
// program it runs.
func winePath(s string) bool {
	if len(s) < 3 || s[1] != ':' || (s[2] != '\\' && s[2] != '/') {
		return false
	}
	c := s[0] | 0x20
	return c >= 'a' && c <= 'z'
}

// lastElem is the file name at the end of a Unix or Windows path.
func lastElem(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		return s[i+1:]
	}
	return s
}

// wineLoader reports whether file is a program a Windows program runs from
// under Wine, Proton, CrossOver or Whisky.
func wineLoader(file string) bool {
	switch file {
	case "wine", "wine64", "wine-preloader", "wine64-preloader", "wineloader", "wineloader64":
		return true
	}
	return false
}

func (f procFacts) program() Program {
	p := Program{PID: f.pid, Exe: f.exe}
	file := lastElem(f.exe)
	wine := winePath(f.argv0)
	// Wine's argv[0] is one token of the Windows command line, or what was
	// passed to wine for the first process, so it may hold spaces: a drive
	// path, a Unix path ("wine /home/p/Games/x/drive_c/Program
	// Files/X/x.exe", as Lutris runs games) or a bare name ("x.exe").
	loaded := !wine && f.argv0 != "" && wineLoader(file)
	arg := lastElem(f.argv0)
	if !wine && !loaded && strings.ContainsAny(f.argv0, " \t") {
		// A program that rewrote its command line into one string
		// ("/opt/app/app --type=zygote"): the last element is not a name.
		arg = ""
	}
	switch {
	case wine && arg != "":
		// A Windows program under Wine or Proton: the program file is the
		// Wine loader, argv[0] the game.
		p.Name, p.Exe = arg, f.wineFile()
	case loaded && arg != "":
		// The same with a Unix path or a bare name in argv[0]. The game's
		// file is known only from an absolute path; the loader's is not it.
		p.Name, p.Exe = arg, ""
		if path.IsAbs(f.argv0) {
			p.Exe = path.Clean(f.argv0)
		}
	case file != "":
		p.Name = file
	case arg != "" && f.comm != "" && strings.HasPrefix(arg, f.comm):
		// No program path (another user's process), but argv[0] is the
		// uncut version of the short name.
		p.Name = arg
	default:
		p.Name = f.comm
	}
	for _, n := range []string{p.Name, f.comm, file, arg} {
		if n != "" && !slices.ContainsFunc(p.Names, func(have string) bool { return strings.EqualFold(have, n) }) {
			p.Names = append(p.Names, n)
		}
	}
	return p
}

// wineFile maps the Windows path in argv[0] to a Unix path through the Wine
// prefix's dosdevices links (c: is the prefix's drive_c, z: is /), so the
// file can be found for its icon. "" when the prefix is unknown.
func (f procFacts) wineFile() string {
	if f.env == nil {
		return ""
	}
	prefix := f.env("WINEPREFIX")
	if prefix == "" {
		home := f.env("HOME")
		if home == "" {
			return ""
		}
		prefix = path.Join(home, ".wine")
	}
	drive := string(f.argv0[0]|0x20) + ":"
	rest := strings.ReplaceAll(f.argv0[3:], `\`, "/")
	return path.Join(prefix, "dosdevices", drive, rest)
}

// envLookup looks a key up in KEY=VALUE pairs; the first match wins, as in
// getenv.
func envLookup(pairs []string) func(string) string {
	return func(key string) string {
		for _, kv := range pairs {
			if k, v, ok := strings.Cut(kv, "="); ok && k == key {
				return v
			}
		}
		return ""
	}
}
