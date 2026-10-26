// Command docaddrs fails when a test, e2e or doc file names a public IP
// address outside the documentation ranges (RFC 5737: 192.0.2.0/24,
// 198.51.100.0/24, 203.0.113.0/24; RFC 3849 and RFC 9637: 2001:db8::/32,
// 3fff::/20). Examples and fixtures must not point at a real network, so
// only the allow-list below may name one, each entry with its reason.
//
// Usage, from the repository root (scripts/ci.ps1 and scripts/ci.sh run it):
//
//	go run ./scripts/docaddrs [dir]
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// allowed are the real public addresses tests and docs may name.
var allowed = []struct {
	prefix string
	file   string // "" for any file, else the only file (slash path) that may use it
	why    string
}{
	{"1.1.1.1/32", "", "Cloudflare DNS: the route-trace and bufferbloat target"},
	{"1.0.0.1/32", "", "Cloudflare DNS"},
	{"2606:4700:4700::1111/128", "", "Cloudflare DNS: the IPv6 interface probe"},
	{"2606:4700:4700::1001/128", "", "Cloudflare DNS"},
	{"8.8.8.8/32", "", "Google Public DNS"},
	{"8.8.4.4/32", "", "Google Public DNS"},
	{"2001:4860:4860::8888/128", "", "Google Public DNS"},
	{"2001:4860:4860::8844/128", "", "Google Public DNS"},
	{"9.9.9.9/32", "", "Quad9 DNS"},
	{"149.112.112.112/32", "", "Quad9 DNS"},
	{"2620:fe::fe/128", "", "Quad9 DNS"},
	{"193.0.6.139/32", "internal/diag/internet_test.go", "RIPE NCC's own server: the live RIPEstat test needs an announced address"},
	{"2001:67c:2e8:22::c100:68b/128", "internal/diag/internet_test.go", "RIPE NCC's own server, IPv6"},
	{"185.25.183.163/32", "internal/catalog/sdr_test.go", "Valve relay in the captured live CS2 SDR config (testdata/sdr-730.json)"},
	// The first and last addresses outside the private and shared ranges:
	// the capture filter's range edges can only be tested with them.
	{"11.0.0.1/32", "internal/capture/divert_windows_test.go", "just past 10.0.0.0/8"},
	{"100.128.0.1/32", "internal/capture/divert_windows_test.go", "just past 100.64.0.0/10"},
	{"172.15.255.255/32", "internal/capture/divert_windows_test.go", "just before 172.16.0.0/12"},
	{"172.32.0.1/32", "internal/capture/divert_windows_test.go", "just past 172.16.0.0/12"},
	{"223.255.255.254/32", "internal/capture/divert_windows_test.go", "just before multicast"},
}

// documentation ranges, and special-purpose blocks that netip doesn't flag
// and that are not reachable on the internet anyway.
var notPublic = mustPrefixes(
	"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32", "3fff::/20",
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4",
	"100::/64", "fec0::/10",
)

// embedding are IPv6 blocks that carry an IPv4 address (NAT64, IPv4-
// compatible and -translated, 6to4, Teredo's server): the IPv4 address
// decides, at byte offset at.
var embedding = []struct {
	prefix netip.Prefix
	at     int
}{
	{netip.MustParsePrefix("64:ff9b::/96"), 12},
	{netip.MustParsePrefix("64:ff9b:1::/48"), 12},
	{netip.MustParsePrefix("::/96"), 12},
	{netip.MustParsePrefix("::ffff:0:0:0/96"), 12}, // IPv4-translated (mapped ones are unmapped first)
	{netip.MustParsePrefix("2002::/16"), 2},
	{netip.MustParsePrefix("2001::/32"), 4},
}

func mustPrefixes(ss ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ss))
	for i, s := range ss {
		out[i] = netip.MustParsePrefix(s)
	}
	return out
}

// effective is the address that decides: the IPv4 address inside the IPv6
// forms that embed one, else a itself.
func effective(a netip.Addr) netip.Addr {
	a = a.Unmap().WithZone("")
	if a.Is6() {
		for _, e := range embedding {
			if e.prefix.Contains(a) {
				b := a.As16()
				return netip.AddrFrom4([4]byte(b[e.at : e.at+4]))
			}
		}
	}
	return a
}

// public reports whether a is an address on the real internet.
func public(a netip.Addr) bool {
	a = effective(a)
	if !a.IsGlobalUnicast() || a.IsPrivate() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

func isAllowed(file string, a netip.Addr) bool {
	a = effective(a)
	for _, e := range allowed {
		if (e.file == "" || e.file == file) && netip.MustParsePrefix(e.prefix).Contains(a) {
			return true
		}
	}
	return false
}

// addrLike matches runs that may be an IP address, with or without a port.
var addrLike = regexp.MustCompile(`[0-9A-Fa-f.:]*[.:][0-9A-Fa-f.:]*`)

// addrs returns the IP addresses written in line.
func addrs(line string) []netip.Addr {
	var out []netip.Addr
	for _, loc := range addrLike.FindAllStringIndex(line, -1) {
		start, m := loc[0], line[loc[0]:loc[1]]
		// A label in front ("stun:", "IP:") leaves a lone ':' that no
		// address starts with.
		if strings.HasPrefix(m, ":") && !strings.HasPrefix(m, "::") {
			start, m = start+1, m[1:]
		}
		// A run that starts inside a word is part of a name or a version
		// ("std::vector", "v1.2.3.4"), not an address.
		if inWord(line, start) {
			continue
		}
		// Trailing punctuation (a sentence's ".", a label's ":") is not
		// part of the address.
		for _, c := range []string{m, strings.TrimRight(m, ".:")} {
			if ap, err := netip.ParseAddrPort(c); err == nil {
				out = append(out, ap.Addr())
				break
			}
			if a, err := netip.ParseAddr(c); err == nil {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

// inWord says whether line[i] continues a word: it follows a letter or '_'
// that does not end a "\n" or "\t" escape in a string literal.
func inWord(line string, i int) bool {
	if i == 0 {
		return false
	}
	c := line[i-1]
	word := c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
	return word && (i < 2 || line[i-2] != '\\')
}

// checked says whether a file (slash path relative to the root) is a test,
// e2e or doc source.
func checked(file string) bool {
	switch {
	case strings.HasSuffix(file, "_test.go"):
		return true
	case strings.HasPrefix(file, "e2e/"):
		ext := path.Ext(file)
		return (ext == ".mjs" || ext == ".js" || ext == ".ts" || ext == ".json" || ext == ".md") &&
			!strings.HasSuffix(file, "package-lock.json")
	case strings.HasPrefix(file, "docs/"):
		return path.Ext(file) == ".md"
	}
	return false
}

// files lists the files under root that git tracks or would track (new
// files included, ignored output such as e2e/out left out). Outside a git
// work tree it walks root instead.
func files(root string) ([]string, error) {
	if list, err := gitFiles(root); err == nil {
		return list, nil
	}
	return walkFiles(root)
}

func gitFiles(root string) ([]string, error) {
	cmd := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	// A git hook (pre-push runs the CI) sets GIT_DIR and friends, which
	// would override -C.
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX":
		default:
			cmd.Env = append(cmd.Env, kv)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var list []string
	for _, f := range strings.Split(string(out), string(rune(0))) {
		if f != "" {
			list = append(list, f)
		}
	}
	return list, nil
}

// walkFiles lists the files under root, skipping skipDir.
func walkFiles(root string) ([]string, error) {
	var list []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err == nil {
			list = append(list, filepath.ToSlash(rel))
		}
		return err
	})
	return list, err
}

// skipDir are directories with no sources of ours.
var skipDir = map[string]bool{".git": true, "node_modules": true, "dist": true, "bin": true, ".ci": true, "out": true}

type finding struct {
	file string
	line int
	addr netip.Addr
}

// scan checks the files in list (slash paths relative to root).
func scan(root string, list []string) ([]finding, error) {
	var out []finding
	for _, rel := range list {
		if !checked(rel) {
			continue
		}
		found, err := scanFile(root, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func scanFile(root, rel string) ([]finding, error) {
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil // deleted but not yet committed
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []finding
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		for _, a := range addrs(sc.Text()) {
			if public(a) && !isAllowed(rel, a) {
				out = append(out, finding{rel, n, a})
			}
		}
	}
	return out, sc.Err()
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	list, err := files(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docaddrs:", err)
		os.Exit(2)
	}
	found, err := scan(root, list)
	if err != nil {
		fmt.Fprintln(os.Stderr, "docaddrs:", err)
		os.Exit(2)
	}
	for _, f := range found {
		fmt.Printf("%s:%d: %s is a real public address\n", f.file, f.line, f.addr)
	}
	if len(found) > 0 {
		fmt.Println("Tests, e2e and docs use documentation addresses: 192.0.2.0/24, 198.51.100.0/24,")
		fmt.Println("203.0.113.0/24 and 2001:db8::/32 (AS64496-AS64511 for AS numbers). A real")
		fmt.Println("address that is truly needed goes in the allow-list in scripts/docaddrs/main.go.")
		os.Exit(1)
	}
	fmt.Println("No real public addresses in tests, e2e or docs.")
}
