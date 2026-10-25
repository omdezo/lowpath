package capture

import (
	"fmt"
	"net/netip"
	"strings"
)

// lanPrefixes are destinations a game server never has: private LANs,
// link-local and carrier-grade NAT space. Loopback and >= 224.0.0.0
// (multicast, broadcast, reserved) are excluded separately.
var lanPrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
}

// maxExcludes bounds the relay endpoints written into the filter, which
// WinDivert compiles into a fixed-size program. Leaving some out is safe:
// tunnel packets come from this process, which is never captured.
const maxExcludes = 64

var firstReserved = netip.MustParseAddr("224.0.0.0")

// buildFilter returns the WinDivert filter for outbound IPv4 UDP that could
// be game traffic. It only narrows what crosses into user mode; the
// per-process check decides what is captured. DNS (53) never is.
//
// WinDivert 2.2 cannot negate a parenthesized expression, so exclusions are
// written in their De Morgan form: "not in [lo, hi]" is "< lo or > hi".
func buildFilter(exclude []netip.AddrPort) string {
	var b strings.Builder
	b.WriteString("outbound and !loopback and ip and udp and udp.DstPort != 53 and ip.DstAddr < 224.0.0.0")
	for _, p := range lanPrefixes {
		fmt.Fprintf(&b, " and (ip.DstAddr < %s or ip.DstAddr > %s)", p.Addr(), lastAddr(p))
	}
	n := 0
	seen := map[netip.AddrPort]bool{}
	for _, e := range exclude {
		a := netip.AddrPortFrom(e.Addr().Unmap(), e.Port())
		if seen[a] || !reachable(a.Addr()) {
			continue
		}
		if n++; n > maxExcludes {
			break
		}
		seen[a] = true
		if a.Port() == 0 { // the whole host
			fmt.Fprintf(&b, " and ip.DstAddr != %s", a.Addr())
		} else {
			fmt.Fprintf(&b, " and (ip.DstAddr != %s or udp.DstPort != %d)", a.Addr(), a.Port())
		}
	}
	return b.String()
}

// reachable reports whether the base filter can match packets to a at all.
func reachable(a netip.Addr) bool {
	if !a.Is4() || a.IsLoopback() || a.Compare(firstReserved) >= 0 {
		return false
	}
	for _, p := range lanPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// lastAddr is the highest address in an IPv4 prefix.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		a[i/8] |= 0x80 >> (i % 8)
	}
	return netip.AddrFrom4(a)
}
