package relay

import (
	"net/netip"
	"testing"
)

func TestIsPublic(t *testing.T) {
	cases := map[string]bool{
		"8.8.8.8":              true,
		"9.9.9.9":              true,
		"203.0.113.7":          true, // documentation ranges are routable as far as we care
		"2606:4700:4700::1111": true,
		"::ffff:8.8.8.8":       true,
		"0.0.0.0":              false,
		"0.1.2.3":              false,
		"127.0.0.1":            false,
		"127.8.9.10":           false,
		"10.1.2.3":             false,
		"172.16.0.1":           false,
		"172.31.255.255":       false,
		"192.168.1.1":          false,
		"169.254.169.254":      false, // cloud metadata service
		"100.64.0.1":           false,
		"100.127.255.254":      false,
		"224.0.0.251":          false,
		"239.255.255.250":      false,
		"255.255.255.255":      false,
		"240.0.0.1":            false,
		"::":                   false,
		"::1":                  false,
		"fe80::1":              false,
		"fc00::1":              false,
		"fd12:3456::1":         false,
		"fec0::1":              false,
		"ff02::1":              false,
		"::ffff:192.168.0.1":   false,
		"::ffff:127.0.0.1":     false,
		"64:ff9b::a00:5":       false, // NAT64 of 10.0.0.5
		"64:ff9b::808:808":     false, // NAT64 even of a public IPv4: use the IPv4 address itself
		"64:ff9b:1::a00:5":     false,
		"2002:a00:5::1":        false, // 6to4
		"2001:0:a00:5::1":      false, // Teredo
		"::a00:5":              false, // IPv4-compatible
		"::ffff:0:a00:5":       false, // IPv4-translated
		"2001:4860:4860::8888": true,  // ordinary public IPv6 next to Teredo's /32
	}
	for s, want := range cases {
		if got := IsPublic(netip.MustParseAddr(s)); got != want {
			t.Errorf("IsPublic(%s) = %v, want %v", s, got, want)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("zero Addr is public")
	}
}
