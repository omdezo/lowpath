package capture

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
)

func TestLastAddr(t *testing.T) {
	for p, want := range map[string]string{
		"10.0.0.0/8":     "10.255.255.255",
		"100.64.0.0/10":  "100.127.255.255",
		"172.16.0.0/12":  "172.31.255.255",
		"192.168.0.0/16": "192.168.255.255",
		"1.2.3.4/32":     "1.2.3.4",
	} {
		if got := lastAddr(netip.MustParsePrefix(p)).String(); got != want {
			t.Errorf("lastAddr(%s) = %s, want %s", p, got, want)
		}
	}
}

func TestBuildFilterExcludes(t *testing.T) {
	relay := netip.MustParseAddrPort("198.51.100.4:40000")
	f := buildFilter([]netip.AddrPort{
		relay,
		relay, // duplicate
		netip.MustParseAddrPort("[::ffff:198.51.100.5]:40001"), // mapped
		netip.MustParseAddrPort("198.51.100.6:0"),              // whole host
		netip.MustParseAddrPort("127.0.0.1:40000"),             // loopback: base filter covers it
		netip.MustParseAddrPort("192.168.1.5:40000"),           // LAN: same
		netip.MustParseAddrPort("[2001:db8::1]:40000"),         // IPv6: never matched
	})
	for _, want := range []string{
		"outbound and !loopback and ip and udp and udp.DstPort != 53 and ip.DstAddr < 224.0.0.0",
		"(ip.DstAddr < 172.16.0.0 or ip.DstAddr > 172.31.255.255)",
		"(ip.DstAddr < 100.64.0.0 or ip.DstAddr > 100.127.255.255)",
		"(ip.DstAddr != 198.51.100.4 or udp.DstPort != 40000)",
		"(ip.DstAddr != 198.51.100.5 or udp.DstPort != 40001)",
		"ip.DstAddr != 198.51.100.6",
	} {
		if !strings.Contains(f, want) {
			t.Errorf("filter lacks %q:\n%s", want, f)
		}
	}
	if strings.Count(f, "198.51.100.4") != 1 || strings.Contains(f, "127.0.0.1") ||
		strings.Contains(f, "192.168.1.5") || strings.Contains(f, "2001") {
		t.Errorf("unneeded excludes in filter:\n%s", f)
	}

	var many []netip.AddrPort
	for i := range maxExcludes + 10 {
		many = append(many, netip.MustParseAddrPort(fmt.Sprintf("198.51.%d.%d:40000", i/250, i%250+1)))
	}
	if n := strings.Count(buildFilter(many), "udp.DstPort != 40000)"); n != maxExcludes {
		t.Errorf("%d excludes written, want the cap %d", n, maxExcludes)
	}
}
