package capture

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

var (
	gameLocal  = netip.MustParseAddrPort("192.168.1.10:50000")
	gameServer = netip.MustParseAddrPort("203.0.113.7:27015")
)

// refChecksum is an independent RFC 1071 implementation: sum 16-bit words in
// a uint64, fold once at the end. Over data that includes a correct checksum
// it returns 0.
func refChecksum(chunks ...[]byte) uint16 {
	var all []byte
	for _, c := range chunks {
		all = append(all, c...)
	}
	if len(all)%2 == 1 {
		all = append(all, 0)
	}
	var s uint64
	for i := 0; i < len(all); i += 2 {
		s += uint64(binary.BigEndian.Uint16(all[i:]))
	}
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return ^uint16(s)
}

func verifyChecksums(t *testing.T, pkt []byte) {
	t.Helper()
	ihl := int(pkt[0]&0x0f) * 4
	if c := refChecksum(pkt[:ihl]); c != 0 {
		t.Fatalf("IPv4 header checksum wrong (residue %#04x)", c)
	}
	u := pkt[ihl:binary.BigEndian.Uint16(pkt[2:])]
	pseudo := make([]byte, 12)
	copy(pseudo, pkt[12:20])
	pseudo[9] = protoUDP
	binary.BigEndian.PutUint16(pseudo[10:], uint16(len(u)))
	if c := refChecksum(pseudo, u); c != 0 {
		t.Fatalf("UDP checksum wrong (residue %#04x)", c)
	}
}

func TestChecksumKnownVector(t *testing.T) {
	// The classic example header (192.168.0.1 -> 192.168.0.199) sums to 0xb861.
	h := []byte{0x45, 0x00, 0x00, 0x73, 0x00, 0x00, 0x40, 0x00, 0x40, 0x11,
		0x00, 0x00, 0xc0, 0xa8, 0x00, 0x01, 0xc0, 0xa8, 0x00, 0xc7}
	if c := ^fold(sum16(0, h)); c != 0xb861 {
		t.Fatalf("checksum = %#04x, want 0xb861", c)
	}
}

func TestBuildParseRoundTrip(t *testing.T) {
	for _, payload := range [][]byte{{}, []byte("x"), []byte("hello game"), bytes.Repeat([]byte{0xff}, 1401)} {
		prefix := []byte("keep")
		pkt, err := appendUDP4(prefix, gameServer, gameLocal, 0x1234, payload)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pkt[:4], []byte("keep")) {
			t.Fatal("prefix overwritten")
		}
		pkt = pkt[4:]
		if len(pkt) != 28+len(payload) || pkt[0] != 0x45 || pkt[8] != injectTTL ||
			binary.BigEndian.Uint16(pkt[4:]) != 0x1234 || binary.BigEndian.Uint16(pkt[6:]) != 0 {
			t.Fatalf("bad header % x", pkt[:20])
		}
		verifyChecksums(t, pkt)
		src, dst, got, ok := parseUDP4(pkt)
		if !ok || src != gameServer || dst != gameLocal || !bytes.Equal(got, payload) {
			t.Fatalf("parse = %v %v %q %v", src, dst, got, ok)
		}
	}
}

func TestBuildRejects(t *testing.T) {
	v6 := netip.MustParseAddrPort("[2001:db8::1]:5")
	if _, err := appendUDP4(nil, v6, gameLocal, 1, nil); err != errNotIPv4 {
		t.Fatalf("IPv6 source: %v", err)
	}
	mapped := netip.AddrPortFrom(netip.AddrFrom16(gameServer.Addr().As16()), gameServer.Port())
	if _, err := appendUDP4(nil, mapped, gameLocal, 1, nil); err != nil {
		t.Fatalf("IPv4-mapped source: %v", err)
	}
	if _, err := appendUDP4(nil, gameServer, gameLocal, 1, make([]byte, maxUDPPayload+1)); err != errTooLarge {
		t.Fatalf("oversize: %v", err)
	}
	if pkt, err := appendUDP4(nil, gameServer, gameLocal, 1, make([]byte, maxUDPPayload)); err != nil || len(pkt) != 0xFFFF {
		t.Fatalf("max size: len %d, %v", len(pkt), err)
	}
}

// withOptions rebuilds pkt with a 4-byte IP options field (IHL 6).
func withOptions(pkt []byte) []byte {
	out := append([]byte{}, pkt[:20]...)
	out = append(out, 0x01, 0x01, 0x01, 0x00) // NOP NOP NOP EOL
	out = append(out, pkt[20:]...)
	out[0] = 0x46
	binary.BigEndian.PutUint16(out[2:], uint16(len(out)))
	binary.BigEndian.PutUint16(out[10:], 0)
	binary.BigEndian.PutUint16(out[10:], ^fold(sum16(0, out[:24])))
	return out
}

func TestParse(t *testing.T) {
	good, _ := appendUDP4(nil, gameLocal, gameServer, 7, []byte("payload"))
	mod := func(f func(p []byte) []byte) []byte { return f(append([]byte{}, good...)) }

	opts := withOptions(good)
	verifyChecksums(t, opts)
	if src, dst, p, ok := parseUDP4(opts); !ok || src != gameLocal || dst != gameServer || string(p) != "payload" {
		t.Fatalf("IHL 6: %v %v %q %v", src, dst, p, ok)
	}
	// Ethernet padding after the datagram is not payload.
	if _, _, p, ok := parseUDP4(append(append([]byte{}, good...), 0, 0, 0)); !ok || string(p) != "payload" {
		t.Fatalf("padded: %q %v", p, ok)
	}
	// DF is fine.
	if _, _, _, ok := parseUDP4(mod(func(p []byte) []byte { p[6] = 0x40; return p })); !ok {
		t.Fatal("DF packet rejected")
	}

	bad := map[string][]byte{
		"empty":         nil,
		"truncated":     good[:25],
		"short total":   mod(func(p []byte) []byte { p = p[:len(p)-1]; return p }),
		"IPv6":          mod(func(p []byte) []byte { p[0] = 0x60; return p }),
		"TCP":           mod(func(p []byte) []byte { p[9] = 6; return p }),
		"IHL 4":         mod(func(p []byte) []byte { p[0] = 0x44; return p }),
		"more frags":    mod(func(p []byte) []byte { p[6] = 0x20; return p }),
		"frag offset":   mod(func(p []byte) []byte { p[7] = 0x01; return p }),
		"udp len big":   mod(func(p []byte) []byte { binary.BigEndian.PutUint16(p[24:], 100); return p }),
		"udp len small": mod(func(p []byte) []byte { binary.BigEndian.PutUint16(p[24:], 7); return p }),
	}
	for name, pkt := range bad {
		if _, _, _, ok := parseUDP4(pkt); ok {
			t.Errorf("%s: accepted", name)
		}
	}
}
