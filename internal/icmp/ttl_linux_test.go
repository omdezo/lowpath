package icmp

import (
	"errors"
	"net/netip"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestPingSocketLoopback sends a TTL-limited echo through the ping socket
// itself, whatever the privileges. It runs where net.ipv4.ping_group_range
// covers one of the test's groups (most systemd distributions, Docker
// containers); a router's answer through the error queue needs a real
// route: LOWPATH_INTERNET=1 go test -run TestInternetTrace ./internal/diag.
func TestPingSocketLoopback(t *testing.T) {
	for _, c := range []struct {
		addr string
		v6   bool
	}{{"127.0.0.1", false}, {"::1", true}} {
		t.Run(c.addr, func(t *testing.T) {
			r, err := pingSocket{v6: c.v6}.sendTTL(netip.MustParseAddr(c.addr), 1, time.Second)
			if errors.Is(err, os.ErrPermission) || errors.Is(err, ErrNoRoute) || errors.Is(err, errors.ErrUnsupported) {
				t.Skipf("no ping socket here (see net.ipv4.ping_group_range): %v", err)
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Echo || r.From != netip.MustParseAddr(c.addr) || r.RTT < 0 || r.RTT > time.Second {
				t.Fatalf("want an echo reply from %s, got %+v", c.addr, r)
			}
		})
	}
}

func TestLinuxTTLSenders(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		s := ttlSenders(v6)
		if len(s) != 2 || s[0] != (rawSocket{v6: v6}) || s[1] != (pingSocket{v6: v6}) {
			t.Errorf("v6=%v: senders %#v, want the raw socket, then the ping socket", v6, s)
		}
	}
}

func TestPingQueueQuotesProbe(t *testing.T) {
	echo := func(typ byte, seq uint16) []byte {
		return []byte{typ, 0, 0xab, 0xcd, 0x12, 0x34, byte(seq >> 8), byte(seq)}
	}
	cases := []struct {
		name string
		v6   bool
		data []byte
		want bool
	}{
		{"ipv4 probe", false, echo(8, 0xbeef), true},
		{"ipv4 probe with payload", false, append(echo(8, 0xbeef), payloadTTL[:]...), true},
		{"ipv4 other sequence", false, echo(8, 0xbeee), false},
		{"ipv4 echo reply", false, echo(0, 0xbeef), false},
		{"ipv6 probe", true, echo(128, 0xbeef), true},
		{"ipv4 type on ipv6", true, echo(8, 0xbeef), false},
		{"truncated", false, echo(8, 0xbeef)[:7], false},
		{"empty", false, nil, false},
	}
	for _, c := range cases {
		if got := (pingQueue{v6: c.v6, seq: 0xbeef}).quotesProbe(c.data); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSockaddr(t *testing.T) {
	sa, err := sockaddr(netip.MustParseAddr("192.0.2.7"))
	if v, ok := sa.(*unix.SockaddrInet4); err != nil || !ok || v.Addr != [4]byte{192, 0, 2, 7} {
		t.Fatalf("ipv4: %#v, %v", sa, err)
	}
	sa, err = sockaddr(netip.MustParseAddr("fe80::1%7"))
	if v, ok := sa.(*unix.SockaddrInet6); err != nil || !ok || v.ZoneId != 7 || v.Addr != netip.MustParseAddr("fe80::1").As16() {
		t.Fatalf("numeric zone: %#v, %v", sa, err)
	}
	sa, err = sockaddr(netip.MustParseAddr("fe80::1%lo"))
	if v, ok := sa.(*unix.SockaddrInet6); err != nil || !ok || v.ZoneId == 0 {
		t.Fatalf("named zone: %#v, %v", sa, err)
	}
	if _, err := sockaddr(netip.MustParseAddr("fe80::1%no-such-interface0")); err == nil {
		t.Fatal("unknown zone: want an error")
	}
	for in, want := range map[unix.Sockaddr]string{
		&unix.SockaddrInet4{Addr: [4]byte{198, 51, 100, 2}}:                       "198.51.100.2",
		&unix.SockaddrInet6{Addr: netip.MustParseAddr("2001:db8::2").As16()}:      "2001:db8::2",
		&unix.SockaddrInet6{Addr: netip.MustParseAddr("::ffff:192.0.2.3").As16()}: "192.0.2.3",
	} {
		if got := sockaddrAddr(in); got.String() != want {
			t.Errorf("%#v: %v, want %s", in, got, want)
		}
	}
	if sockaddrAddr(nil).IsValid() {
		t.Error("no address: want an invalid one")
	}
}
