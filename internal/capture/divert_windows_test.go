//go:build windows && amd64

package capture

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"testing"

	"lowpath/internal/divert"
)

// These tests use the real WinDivert.dll from the source tree. Its helpers
// (filter compiler and evaluator, checksums) run in user mode: no admin, no
// driver.
func loadDLL(t *testing.T) {
	t.Helper()
	if err := divert.Load("../divert/bin"); err != nil {
		t.Fatalf("load WinDivert.dll: %v", err)
	}
}

func TestFilterCompiles(t *testing.T) {
	loadDLL(t)
	if err := divert.CompileFilter(buildFilter(nil), divert.LayerNetwork); err != nil {
		t.Fatalf("base filter: %v", err)
	}
	// What a route-mode session passes: the loopback test relay, real relays
	// (one twice, one IPv6, one whole host) and more endpoints than the cap.
	appEx := []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:40000"),
		netip.MustParseAddrPort("203.0.113.10:40000"),
		netip.MustParseAddrPort("203.0.113.10:40000"),
		netip.MustParseAddrPort("[2001:db8::1]:40000"),
		netip.MustParseAddrPort("[::ffff:198.51.100.7]:40001"),
		netip.MustParseAddrPort("198.51.100.8:0"),
	}
	for i := range maxExcludes + 10 {
		appEx = append(appEx, netip.MustParseAddrPort(fmt.Sprintf("192.0.2.%d:%d", i%250+1, 40000+i)))
	}
	if err := divert.CompileFilter(buildFilter(appEx), divert.LayerNetwork); err != nil {
		t.Fatalf("filter for a route-mode session rejected: %v", err)
	}
	// The cap must fit in WinDivert's filter program with room to spare;
	// find the real limit to keep an eye on it.
	limit := 0
	for n := maxExcludes; n <= 512; n += 16 {
		var ex []netip.AddrPort
		for i := range n {
			ex = append(ex, netip.MustParseAddrPort(fmt.Sprintf("198.51.%d.%d:%d", i/250, i%250+1, 40000+i)))
		}
		f := buildFilterN(ex, n)
		if err := divert.CompileFilter(f, divert.LayerNetwork); err != nil {
			if n == maxExcludes {
				t.Fatalf("filter with %d excludes rejected: %v", n, err)
			}
			t.Logf("WinDivert rejects %d excludes (%d bytes): %v", n, len(f), err)
			break
		}
		limit = n
	}
	t.Logf("filter compiles with up to %d relay excludes (cap %d)", limit, maxExcludes)
}

// buildFilterN is buildFilter with a different cap, to probe the limit.
func buildFilterN(ex []netip.AddrPort, n int) string {
	f := buildFilter(nil)
	for _, e := range ex[:n] {
		f += fmt.Sprintf(" and (ip.DstAddr != %s or udp.DstPort != %d)", e.Addr(), e.Port())
	}
	return f
}

// TestFilterSemantics runs packets through WinDivert's own evaluator: the
// comparison operators on ip.DstAddr must behave as ranges.
func TestFilterSemantics(t *testing.T) {
	loadDLL(t)
	relay := netip.MustParseAddrPort("198.51.100.4:40000")
	host := netip.MustParseAddr("198.51.100.9")
	filter := buildFilter([]netip.AddrPort{relay, netip.AddrPortFrom(host, 0)})
	src := netip.MustParseAddrPort("192.168.1.10:50000")

	cases := []struct {
		dst      string
		inbound  bool
		loopback bool
		want     bool
	}{
		{dst: "203.0.113.7:27015", want: true},
		{dst: "1.0.0.1:3478", want: true},
		{dst: "223.255.255.254:9000", want: true},
		{dst: "203.0.113.7:27015", inbound: true, want: false},
		{dst: "203.0.113.7:27015", loopback: true, want: false},
		{dst: "8.8.8.8:53", want: false},
		{dst: "10.0.0.1:9000", want: false},
		{dst: "10.255.255.255:9000", want: false},
		{dst: "11.0.0.1:9000", want: true},
		{dst: "100.64.0.1:9000", want: false},
		{dst: "100.127.255.255:9000", want: false},
		{dst: "100.128.0.1:9000", want: true},
		{dst: "169.254.1.1:9000", want: false},
		{dst: "172.15.255.255:9000", want: true},
		{dst: "172.16.0.1:9000", want: false},
		{dst: "172.31.255.255:9000", want: false},
		{dst: "172.32.0.1:9000", want: true},
		{dst: "192.168.1.1:9000", want: false},
		{dst: "224.0.0.251:5353", want: false},
		{dst: "239.255.255.250:1900", want: false},
		{dst: "255.255.255.255:67", want: false},
		{dst: "198.51.100.4:40000", want: false}, // relay endpoint
		{dst: "198.51.100.4:40001", want: true},  // same relay host, other port
		{dst: "198.51.100.9:1234", want: false},  // excluded host
	}
	for _, c := range cases {
		pkt, err := appendUDP4(nil, src, netip.MustParseAddrPort(c.dst), 1, []byte("probe"))
		if err != nil {
			t.Fatal(err)
		}
		var a divert.Address
		a.SetOutbound(!c.inbound)
		a.SetLoopback(c.loopback)
		got, err := divert.EvalFilter(filter, pkt, &a)
		if err != nil || got != c.want {
			t.Errorf("dst %s inbound=%v loopback=%v: match = %v, %v; want %v",
				c.dst, c.inbound, c.loopback, got, err, c.want)
		}
	}
}

// TestChecksumsMatchDLL checks the Go checksums against WinDivert's.
func TestChecksumsMatchDLL(t *testing.T) {
	loadDLL(t)
	for _, n := range []int{0, 1, 2, 3, 100, 1401, 8000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte(i*7 + n)
		}
		ours, err := appendUDP4(nil, gameServer, gameLocal, uint16(n), payload)
		if err != nil {
			t.Fatal(err)
		}
		theirs := bytes.Clone(ours)
		var a divert.Address
		if err := divert.CalcChecksums(theirs, &a); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ours, theirs) {
			t.Fatalf("payload %d: Go % x\nDLL % x", n, ours[:28], theirs[:28])
		}
		if !a.IPChecksum() || !a.UDPChecksum() {
			t.Fatalf("DLL did not mark checksums valid")
		}
	}
}

func TestStartWithoutAdmin(t *testing.T) {
	if IsAdmin() {
		t.Skip("elevated: Start would really capture")
	}
	loadDLL(t)
	_, err := Start(Config{
		DriverDir: "../divert/bin",
		Processes: []string{"game.exe"},
		OnPacket:  func(_, _ netip.AddrPort, _ []byte) bool { return true },
	})
	if !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("Start without admin: %v, want ErrNeedAdmin", err)
	}
}

func BenchmarkOwnerLookupMiss(b *testing.B) {
	for b.Loop() {
		if _, err := udpSockets(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProcessList(b *testing.B) {
	for b.Loop() {
		if _, err := processNames(); err != nil {
			b.Fatal(err)
		}
	}
}
