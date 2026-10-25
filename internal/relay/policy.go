package relay

import (
	"net"
	"net/netip"
)

// Ranges that are not covered by netip's Is* helpers but must never be
// reachable through a public relay: anything here leads into the relay
// provider's network or nowhere at all.
var reserved = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT (RFC 6598)
	netip.MustParsePrefix("240.0.0.0/4"),   // reserved, includes 255.255.255.255
	netip.MustParsePrefix("fec0::/10"),     // deprecated site-local

	// IPv6 forms that carry an IPv4 address the network may translate back
	// to: through them a relay on an IPv6-only host with NAT64 (or a 6to4 or
	// Teredo gateway) would reach private IPv4 space. No game server needs
	// them; games are reached by their plain IPv4 or IPv6 address.
	netip.MustParsePrefix("::/96"),           // IPv4-compatible (deprecated)
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated (SIIT)
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 well-known prefix
	netip.MustParsePrefix("64:ff9b:1::/48"),  // NAT64 local-use
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("2001::/32"),       // Teredo
}

// IsPublic reports whether a is an address a public relay may send game
// traffic to. Loopback, private, link-local, CGNAT, multicast, broadcast and
// unspecified addresses are not: an open or leaked relay would otherwise be a
// door into the relay host's own network.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() ||
		a.IsLinkLocalUnicast() || a.IsMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// addrSet is the relay host's own IPs. Sending "game traffic" to ourselves
// could loop packets through the relay or reach services bound to them.
type addrSet map[netip.Addr]struct{}

func localAddrs() addrSet {
	s := addrSet{}
	ifaddrs, err := net.InterfaceAddrs()
	if err != nil {
		return s
	}
	for _, ia := range ifaddrs {
		ipn, ok := ia.(*net.IPNet)
		if !ok {
			continue
		}
		if a, ok := netip.AddrFromSlice(ipn.IP); ok {
			s[a.Unmap()] = struct{}{}
		}
	}
	return s
}
