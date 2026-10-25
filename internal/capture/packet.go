package capture

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// IPv4 and UDP wire format. WinDivert hands over raw IP packets and takes raw
// IP packets back, so the capture path parses and builds them itself.

const (
	ipv4HeaderLen = 20
	udpHeaderLen  = 8
	protoUDP      = 17
	injectTTL     = 64

	// maxUDPPayload is the most a single IPv4 datagram can carry.
	maxUDPPayload = 0xFFFF - ipv4HeaderLen - udpHeaderLen
)

var (
	errNotIPv4  = errors.New("capture: endpoints must be IPv4")
	errTooLarge = errors.New("capture: payload too large for one IPv4 datagram")
)

// parseUDP4 returns the endpoints and payload of an unfragmented IPv4 UDP
// packet. ok is false for anything else (IPv6, other protocols, fragments,
// malformed headers); the caller passes those through untouched.
func parseUDP4(pkt []byte) (src, dst netip.AddrPort, payload []byte, ok bool) {
	if len(pkt) < ipv4HeaderLen || pkt[0]>>4 != 4 || pkt[9] != protoUDP {
		return
	}
	ihl := int(pkt[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(pkt[2:4]))
	if ihl < ipv4HeaderLen || total < ihl+udpHeaderLen || total > len(pkt) {
		return
	}
	// MF or a non-zero offset: one piece of a larger datagram. Only the first
	// piece has the UDP header and none has the whole payload.
	if binary.BigEndian.Uint16(pkt[6:8])&0x3fff != 0 {
		return
	}
	u := pkt[ihl:total]
	ulen := int(binary.BigEndian.Uint16(u[4:6]))
	if ulen < udpHeaderLen || ulen > len(u) {
		return
	}
	src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(pkt[12:16])), binary.BigEndian.Uint16(u[0:2]))
	dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(pkt[16:20])), binary.BigEndian.Uint16(u[2:4]))
	return src, dst, u[udpHeaderLen:ulen], true
}

// appendUDP4 appends an IPv4+UDP packet from src to dst carrying payload, with
// both checksums filled in.
func appendUDP4(b []byte, src, dst netip.AddrPort, id uint16, payload []byte) ([]byte, error) {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	if !s.Is4() || !d.Is4() {
		return b, errNotIPv4
	}
	if len(payload) > maxUDPPayload {
		return b, errTooLarge
	}
	ulen := udpHeaderLen + len(payload)
	start := len(b)
	b = append(b, make([]byte, ipv4HeaderLen+udpHeaderLen)...)
	b = append(b, payload...)
	ip := b[start:]
	sa, da := s.As4(), d.As4()

	ip[0] = 0x45 // version 4, 20-byte header
	binary.BigEndian.PutUint16(ip[2:], uint16(ipv4HeaderLen+ulen))
	binary.BigEndian.PutUint16(ip[4:], id)
	ip[8] = injectTTL
	ip[9] = protoUDP
	copy(ip[12:16], sa[:])
	copy(ip[16:20], da[:])
	binary.BigEndian.PutUint16(ip[10:], ^fold(sum16(0, ip[:ipv4HeaderLen])))

	u := ip[ipv4HeaderLen:]
	binary.BigEndian.PutUint16(u[0:], src.Port())
	binary.BigEndian.PutUint16(u[2:], dst.Port())
	binary.BigEndian.PutUint16(u[4:], uint16(ulen))
	// The UDP checksum also covers a pseudo-header of the IP addresses,
	// protocol and UDP length.
	sum := sum16(0, ip[12:20])
	sum += protoUDP + uint32(ulen)
	c := ^fold(sum16(sum, u))
	if c == 0 {
		c = 0xffff // 0 means "no checksum" in UDP over IPv4
	}
	binary.BigEndian.PutUint16(u[6:], c)
	return b, nil
}

// sum16 adds b to a ones'-complement sum as big-endian 16-bit words. A uint32
// cannot overflow for anything up to a full 64 KiB datagram.
func sum16(sum uint32, b []byte) uint32 {
	for len(b) >= 2 {
		sum += uint32(b[0])<<8 | uint32(b[1])
		b = b[2:]
	}
	if len(b) == 1 {
		sum += uint32(b[0]) << 8
	}
	return sum
}

func fold(sum uint32) uint16 {
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return uint16(sum)
}
