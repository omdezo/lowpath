package icmp

import (
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"
)

func TestPingTTLLoopback(t *testing.T) {
	for _, a := range []string{"127.0.0.1", "::1"} {
		t.Run(a, func(t *testing.T) {
			r, err := PingTTL(netip.MustParseAddr(a), 1, time.Second)
			if errors.Is(err, os.ErrPermission) || errors.Is(err, ErrNoRoute) {
				t.Skipf("TTL-limited ICMP not available here: %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			// The loopback destination is zero hops away: TTL 1 reaches it.
			if r.Kind != Echo || r.From != netip.MustParseAddr(a) {
				t.Fatalf("want an echo reply from %s, got %+v", a, r)
			}
			if r.RTT < 0 || r.RTT > time.Second {
				t.Fatalf("implausible RTT %v", r.RTT)
			}
		})
	}
}

func TestPingTTLRejectsBadInput(t *testing.T) {
	for _, ttl := range []int{0, -1, 256} {
		if _, err := PingTTL(netip.MustParseAddr("192.0.2.1"), ttl, time.Second); err == nil {
			t.Errorf("TTL %d: want an error", ttl)
		}
	}
	if _, err := PingTTL(netip.Addr{}, 1, time.Second); err == nil {
		t.Error("no address: want an error")
	}
}

func TestReplyKindString(t *testing.T) {
	for k, want := range map[ReplyKind]string{Echo: "echo", TimeExceeded: "time exceeded", Unreachable: "unreachable", 0: "unknown"} {
		if k.String() != want {
			t.Errorf("%d: %q, want %q", k, k.String(), want)
		}
	}
}

// quoted4 builds the start of an IPv4 echo request to dst, as a router
// quotes it in a time exceeded message, with optWords 32-bit option words.
func quoted4(dst netip.Addr, proto, icmpType byte, id, seq uint16, optWords int) []byte {
	hdr := 20 + 4*optWords
	b := make([]byte, hdr+8)
	b[0] = 0x40 | byte(hdr/4)
	b[8] = 1 // TTL left when it expired
	b[9] = proto
	copy(b[12:16], []byte{192, 0, 2, 10})
	a := dst.As4()
	copy(b[16:20], a[:])
	b[hdr] = icmpType
	b[hdr+4], b[hdr+5] = byte(id>>8), byte(id)
	b[hdr+6], b[hdr+7] = byte(seq>>8), byte(seq)
	return b
}

func quoted6(dst netip.Addr, next, icmpType byte, id, seq uint16) []byte {
	b := make([]byte, 48)
	b[0] = 0x60
	b[6] = next
	a := dst.As16()
	copy(b[24:40], a[:])
	b[40] = icmpType
	b[44], b[45] = byte(id>>8), byte(id)
	b[46], b[47] = byte(seq>>8), byte(seq)
	return b
}

func TestQuotedEcho(t *testing.T) {
	v4 := netip.MustParseAddr("198.51.100.7")
	v6 := netip.MustParseAddr("2001:db8::7")
	cases := []struct {
		name    string
		data    []byte
		v6      bool
		ok      bool
		dst     netip.Addr
		id, seq uint16
	}{
		{"ipv4", quoted4(v4, 1, 8, 0x1234, 0xbeef, 0), false, true, v4, 0x1234, 0xbeef},
		{"ipv4 with options", quoted4(v4, 1, 8, 7, 9, 2), false, true, v4, 7, 9},
		{"ipv4 udp", quoted4(v4, 17, 8, 7, 9, 0), false, false, netip.Addr{}, 0, 0},
		{"ipv4 echo reply", quoted4(v4, 1, 0, 7, 9, 0), false, false, netip.Addr{}, 0, 0},
		{"ipv4 truncated", quoted4(v4, 1, 8, 7, 9, 0)[:26], false, false, netip.Addr{}, 0, 0},
		{"ipv4 bad header length", append([]byte{0x41}, quoted4(v4, 1, 8, 7, 9, 0)[1:]...), false, false, netip.Addr{}, 0, 0},
		{"ipv6", quoted6(v6, 58, 128, 0xabcd, 3), true, true, v6, 0xabcd, 3},
		{"ipv6 extension header", quoted6(v6, 0, 128, 1, 1), true, false, netip.Addr{}, 0, 0},
		{"ipv6 echo reply", quoted6(v6, 58, 129, 1, 1), true, false, netip.Addr{}, 0, 0},
		{"ipv6 truncated", quoted6(v6, 58, 128, 1, 1)[:47], true, false, netip.Addr{}, 0, 0},
		{"ipv6 data read as ipv4", quoted6(v6, 58, 128, 1, 1), false, false, netip.Addr{}, 0, 0},
		{"empty", nil, false, false, netip.Addr{}, 0, 0},
	}
	for _, c := range cases {
		dst, id, seq, ok := quotedEcho(c.data, c.v6)
		if ok != c.ok || dst != c.dst || id != c.id || seq != c.seq {
			t.Errorf("%s: got %v %d %d %v, want %v %d %d %v", c.name, dst, id, seq, ok, c.dst, c.id, c.seq, c.ok)
		}
	}
}
