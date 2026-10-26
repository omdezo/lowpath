package icmp

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// extErr builds the data of an IP_RECVERR control message as Linux lays
// it out: struct sock_extended_err in host byte order, then the offender
// as a sockaddr_in or sockaddr_in6 (a zero family when off is invalid).
func extErr(errno uint32, origin, typ, code byte, off netip.Addr) []byte {
	b := make([]byte, 16)
	binary.NativeEndian.PutUint32(b, errno)
	b[4], b[5], b[6] = origin, typ, code
	switch {
	case off.Is4():
		sa := make([]byte, 16)
		binary.NativeEndian.PutUint16(sa, linuxAFInet)
		a := off.As4()
		copy(sa[4:], a[:])
		b = append(b, sa...)
	case off.Is6():
		sa := make([]byte, 28)
		binary.NativeEndian.PutUint16(sa, linuxAFInet6)
		a := off.As16()
		copy(sa[8:], a[:])
		binary.NativeEndian.PutUint32(sa[24:], 3) // scope ID
		b = append(b, sa...)
	default:
		b = append(b, make([]byte, 16)...) // AF_UNSPEC
	}
	return b
}

func TestParseExtendedErr(t *testing.T) {
	router4 := netip.MustParseAddr("192.0.2.1")
	router6 := netip.MustParseAddr("2001:db8::1")
	const ehostunreach = 113
	cases := []struct {
		name     string
		data     []byte
		ok       bool
		offender netip.Addr
		kind     ReplyKind // 0: no answer to the probe
	}{
		{"ipv4 time exceeded", extErr(ehostunreach, eeOriginICMP, 11, 0, router4), true, router4, TimeExceeded},
		{"ipv4 host unreachable", extErr(ehostunreach, eeOriginICMP, 3, 1, router4), true, router4, Unreachable},
		{"ipv4 admin prohibited", extErr(13, eeOriginICMP, 3, 13, router4), true, router4, Unreachable},
		{"ipv4 fragmentation needed", extErr(90, eeOriginICMP, 3, 4, router4), true, router4, 0},
		{"ipv4 parameter problem", extErr(71, eeOriginICMP, 12, 0, router4), true, router4, 0},
		{"ipv6 time exceeded", extErr(ehostunreach, eeOriginICMP6, 3, 0, router6), true, router6, TimeExceeded},
		{"ipv6 no route", extErr(101, eeOriginICMP6, 1, 0, router6), true, router6, Unreachable},
		{"ipv6 packet too big", extErr(90, eeOriginICMP6, 2, 0, router6), true, router6, 0},
		{"ipv6 type read as ipv4", extErr(ehostunreach, eeOriginICMP, 1, 0, router4), true, router4, 0},
		{"local error", extErr(90, 1, 0, 0, netip.Addr{}), true, netip.Addr{}, 0},
		{"unknown offender", extErr(ehostunreach, eeOriginICMP, 11, 0, netip.Addr{}), true, netip.Addr{}, 0},
		{"no offender", extErr(ehostunreach, eeOriginICMP, 11, 0, router4)[:16], true, netip.Addr{}, 0},
		{"truncated offender", extErr(ehostunreach, eeOriginICMP6, 3, 0, router6)[:30], true, netip.Addr{}, 0},
		{"too short", extErr(ehostunreach, eeOriginICMP, 11, 0, router4)[:15], false, netip.Addr{}, 0},
		{"empty", nil, false, netip.Addr{}, 0},
	}
	for _, c := range cases {
		e, ok := parseExtendedErr(c.data)
		if ok != c.ok || e.Offender != c.offender {
			t.Errorf("%s: got %+v, %v; want offender %v, %v", c.name, e, ok, c.offender, c.ok)
			continue
		}
		kind, answers := e.kind()
		if answers != (c.kind != 0) || kind != c.kind {
			t.Errorf("%s: kind %v, %v; want %v", c.name, kind, answers, c.kind)
		}
	}
	if e, _ := parseExtendedErr(extErr(ehostunreach, eeOriginICMP, 11, 0, router4)); e.Errno != ehostunreach || e.Origin != eeOriginICMP || e.Type != 11 {
		t.Errorf("fields: %+v", e)
	}
}
