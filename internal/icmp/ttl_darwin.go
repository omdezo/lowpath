package icmp

import (
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
)

// ttlNeeds is what macOS wants when no socket can be opened; the datagram
// socket below needs nothing, so a trace should never get there.
const ttlNeeds = "root"

// dgramSocket sends through a macOS datagram ICMP socket (SOCK_DGRAM,
// IPPROTO_ICMP or IPPROTO_ICMPV6), which any user may open. It may send
// echo requests only, set IP_TTL and IPV6_UNICAST_HOPS, and, unlike a
// Linux ping socket, it is a raw socket underneath: it receives every ICMP
// message, routers' "time exceeded" and "unreachable" included, with the
// echo ID left as sent. So the raw socket's matching applies unchanged
// (golang.org/x/net/icmp sets IP_STRIPHDR, so IPv4 messages arrive without
// their IP header, as on a raw socket read through it). See icmp(4) and
// icmp_dgram_attach and icmp6_dgram_attach in xnu.
type dgramSocket struct{ v6 bool }

func unprivilegedTTL(v6 bool) ttlSender { return dgramSocket{v6: v6} }

func (d dgramSocket) sendTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	network, laddr := "udp4", "0.0.0.0"
	if d.v6 {
		network, laddr = "udp6", "::"
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		return Reply{}, socketError(err)
	}
	defer c.Close()
	return echoTTL(c, &net.UDPAddr{IP: dst.AsSlice(), Zone: dst.Zone()}, dst, ttl, timeout, d.v6)
}
