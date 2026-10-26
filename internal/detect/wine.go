package detect

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// maxManifest bounds the launcher files read: they are a few kilobytes,
// and a stray huge file must not stall detection.
const maxManifest = 4 << 20

// WinePrefixes returns the drive_c folder of every Wine prefix found: the
// Locations' prefix patterns, Proton's steamapps/compatdata/<app>/pfx in
// each Steam library, and the prefixes Lutris and Heroic list per game.
// Each folder once, only those that exist.
func (s *System) WinePrefixes() []string {
	var cands []string
	for _, pat := range s.loc.Prefixes {
		m, _ := filepath.Glob(pat)
		cands = append(cands, m...)
	}
	for _, lib := range s.SteamLibraries() {
		m, _ := filepath.Glob(filepath.Join(filepath.Dir(lib), "compatdata", "*", "pfx"))
		cands = append(cands, m...)
	}
	for _, dir := range s.loc.Lutris {
		files, _ := filepath.Glob(filepath.Join(dir, "*.yml"))
		for _, f := range files {
			if b, err := readSmall(f); err == nil {
				cands = append(cands, lutrisPrefixes(b)...)
			}
		}
	}
	for _, dir := range s.loc.Heroic {
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, f := range files {
			if b, err := readSmall(f); err == nil {
				cands = append(cands, heroicPrefixes(b)...)
			}
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range cands {
		dc := filepath.Join(c, "drive_c")
		if k := dirKey(dc); !seen[k] && isDir(dc) {
			seen[k] = true
			out = append(out, dc)
		}
	}
	return out
}

// LauncherInstalls returns the game folders launchers say they installed
// (Legendary's install_path, which Heroic uses for Epic games), each once,
// only those that exist.
func (s *System) LauncherInstalls() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range s.loc.Legendary {
		b, err := readSmall(f)
		if err != nil {
			continue
		}
		for _, p := range legendaryInstalls(b) {
			if k := dirKey(p); !seen[k] && isDir(p) {
				seen[k] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// lutrisPrefixes reads the "prefix:" values of a Lutris game file:
//
//	game:
//	  exe: /home/player/Games/x/drive_c/Program Files/X/x.exe
//	  prefix: /home/player/Games/x
//
// A line scan is enough for these flat files and keeps a YAML parser out.
func lutrisPrefixes(b []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		v, ok := strings.CutPrefix(line, "prefix:")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if filepath.IsAbs(v) {
			out = append(out, v)
		}
	}
	return out
}

// heroicPrefixes reads a Heroic GamesConfig file: {"<app>": {"winePrefix":
// "/home/player/Games/Heroic/Prefixes/default/Game", ...}}.
func heroicPrefixes(b []byte) []string { return jsonField(b, "winePrefix") }

// legendaryInstalls reads Legendary's installed.json: {"<app>":
// {"install_path": "/home/player/Games/Heroic/rocketleague", ...}}.
func legendaryInstalls(b []byte) []string { return jsonField(b, "install_path") }

// jsonField collects the absolute paths in field of every entry of a JSON
// object of objects. Entries are decoded one by one, so one odd entry
// doesn't hide the others.
func jsonField(b []byte, field string) []string {
	var entries map[string]json.RawMessage
	if json.Unmarshal(b, &entries) != nil {
		return nil
	}
	var out []string
	for _, raw := range entries {
		var e map[string]json.RawMessage
		var v string
		if json.Unmarshal(raw, &e) != nil || json.Unmarshal(e[field], &v) != nil {
			continue
		}
		if filepath.IsAbs(v) {
			out = append(out, v)
		}
	}
	slices.Sort(out) // map order is random; keep scans repeatable
	return out
}

// readSmall reads a launcher file, refusing one larger than maxManifest.
func readSmall(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	if err == nil && len(b) > maxManifest {
		err = fmt.Errorf("detect: %s is larger than %d bytes", path, maxManifest)
	}
	return b, err
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// dirKey identifies a folder for de-duplication: ~/.steam/steam is a link
// to ~/.local/share/Steam, and Windows paths differ in case.
func dirKey(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return strings.ToLower(filepath.Clean(p))
}
