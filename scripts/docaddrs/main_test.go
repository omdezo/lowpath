package main

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

// This file is scanned too, so its public examples are allow-listed
// resolvers; others are built at run time.
var (
	realV4   = netip.AddrFrom4([4]byte{5, 6, 7, 8})
	pastPriv = netip.AddrFrom4([4]byte{172, 32, 0, 1})
)

func TestPublic(t *testing.T) {
	for s, want := range map[string]bool{
		"192.0.2.1":            false, // documentation
		"198.51.100.7":         false,
		"203.0.113.9":          false,
		"2001:db8::1":          false,
		"3fff::1":              false,
		"10.0.0.1":             false,
		"100.72.1.1":           false, // CGNAT
		"127.0.0.1":            false,
		"224.0.0.251":          false, // multicast
		"ff02::1":              false,
		"fe80::1":              false,
		"fd00::1":              false,
		"64:ff9b::a00:5":       false, // NAT64 of 10.0.0.5
		"2002:a00:5::1":        false, // 6to4 of 10.0.0.5
		"::ffff:10.0.0.5":      false,
		"::ffff:0:a00:5":       false, // IPv4-translated 10.0.0.5
		"64:ff9b::808:808":     true,  // NAT64 of 8.8.8.8
		"2002:808:808::1":      true,
		"::ffff:1.1.1.1":       true,
		"8.8.8.8":              true,
		"2001:4860:4860::8888": true,
	} {
		if got := public(netip.MustParseAddr(s)); got != want {
			t.Errorf("public(%s) = %v, want %v", s, got, want)
		}
	}
	if !public(realV4) || !public(pastPriv) {
		t.Error("an ordinary public address is not public")
	}
}

func TestAddrs(t *testing.T) {
	got := addrs(`dial 1.1.1.1:443 then [2606:4700:4700::1111]:19302, prefix 198.51.100.0/24, ends 8.8.8.8. Not 12:30:45, v1.2.3 or aa:bb:cc:dd:ee:ff`)
	want := []string{"1.1.1.1", "2606:4700:4700::1111", "198.51.100.0", "8.8.8.8"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestScan(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/x/x_test.go", "a := \"198.51.100.2\"\nb := \""+realV4.String()+"\"\nc := \"8.8.8.8\"\nd := \"64:ff9b::808:808\"\n")
	write("internal/x/x.go", "e := \""+realV4.String()+"\"\n") // not a test file
	write("docs/guide.md", "Ping "+pastPriv.String()+" to see.\n")
	write("internal/capture/divert_windows_test.go", "{dst: \""+pastPriv.String()+":9000\"}\n") // allowed here only
	write("e2e/node_modules/pkg/index.js", "\""+realV4.String()+"\"\n")
	list, err := walkFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := scan(root, list)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].file != "docs/guide.md" || got[0].addr != pastPriv ||
		got[1].file != "internal/x/x_test.go" || got[1].line != 2 || got[1].addr != realV4 {
		t.Fatalf("found %+v", got)
	}
}
