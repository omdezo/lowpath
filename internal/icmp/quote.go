package icmp

import (
	"encoding/binary"
	"net/netip"
)

// quotedEcho reads the original packet an ICMP error message quotes (the
// IP header plus at least the first 8 bytes of what followed it) and, when
// that was an echo request, returns its destination, ID and sequence
// number, so a reply can be matched to the probe that caused it.
// IPv6 packets with extension headers before ICMPv6 are not matched: the
// trace never sends them.
func quotedEcho(data []byte, v6 bool) (dst netip.Addr, id, seq uint16, ok bool) {
	if v6 {
		const hdr = 40
		if len(data) < hdr+8 || data[0]>>4 != 6 || data[6] != 58 || data[hdr] != 128 {
			return netip.Addr{}, 0, 0, false
		}
		dst = netip.AddrFrom16([16]byte(data[24:40]))
		return dst, binary.BigEndian.Uint16(data[hdr+4:]), binary.BigEndian.Uint16(data[hdr+6:]), true
	}
	if len(data) < 20 || data[0]>>4 != 4 {
		return netip.Addr{}, 0, 0, false
	}
	hdr := int(data[0]&0x0f) * 4
	if hdr < 20 || len(data) < hdr+8 || data[9] != 1 || data[hdr] != 8 {
		return netip.Addr{}, 0, 0, false
	}
	dst = netip.AddrFrom4([4]byte(data[16:20]))
	return dst, binary.BigEndian.Uint16(data[hdr+4:]), binary.BigEndian.Uint16(data[hdr+6:]), true
}
