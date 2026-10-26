package icmp

import (
	"encoding/binary"
	"net/netip"
)

// Linux reports an ICMP error for a datagram socket with IP_RECVERR (IPv6:
// IPV6_RECVERR) set as a message on the socket's error queue: a control
// message holding struct sock_extended_err, followed by the address of the
// router that sent the error (SO_EE_OFFENDER). See IP_RECVERR(2const).
// The layout is parsed here, without build tags, so its tests run on every
// OS; ttl_linux.go reads the queue.

// SO_EE_ORIGIN_* from linux/errqueue.h, and the address families of the
// offender (AF_INET, AF_INET6 on Linux).
const (
	eeOriginICMP  = 2
	eeOriginICMP6 = 3

	linuxAFInet  = 2
	linuxAFInet6 = 10
)

// extendedErr is one queued error: struct sock_extended_err and the
// offender.
type extendedErr struct {
	Errno    uint32
	Origin   uint8
	Type     uint8 // the ICMP type, for Origin eeOriginICMP or eeOriginICMP6
	Code     uint8
	Offender netip.Addr // invalid when the kernel doesn't know it (AF_UNSPEC)
}

// parseExtendedErr reads the data of an IP_RECVERR or IPV6_RECVERR control
// message. Fields are in host byte order, the offender's address in network
// order. It reports false for data too short to hold the structure.
func parseExtendedErr(b []byte) (extendedErr, bool) {
	// u32 ee_errno, u8 ee_origin, ee_type, ee_code, ee_pad, u32 ee_info,
	// u32 ee_data: 16 bytes, then the offender's sockaddr.
	const size = 16
	if len(b) < size {
		return extendedErr{}, false
	}
	e := extendedErr{
		Errno:  binary.NativeEndian.Uint32(b),
		Origin: b[4], Type: b[5], Code: b[6],
	}
	sa := b[size:]
	if len(sa) < 2 {
		return e, true
	}
	switch binary.NativeEndian.Uint16(sa) {
	case linuxAFInet: // sockaddr_in: family, port, 4-byte address
		if len(sa) >= 8 {
			e.Offender = netip.AddrFrom4([4]byte(sa[4:8]))
		}
	case linuxAFInet6: // sockaddr_in6: family, port, flow info, 16-byte address, scope
		if len(sa) >= 24 {
			e.Offender = netip.AddrFrom16([16]byte(sa[8:24]))
		}
	}
	return e, true
}

// kind says what a queued error means for a TTL-limited echo: a router's
// "time exceeded" or someone's "destination unreachable". Other errors
// (a "packet too big", a parameter problem, local errors) are no answer to
// the probe.
func (e extendedErr) kind() (ReplyKind, bool) {
	if !e.Offender.IsValid() {
		return 0, false
	}
	switch {
	case e.Origin == eeOriginICMP && e.Type == 11, e.Origin == eeOriginICMP6 && e.Type == 3:
		return TimeExceeded, true
	// IPv4 code 4 is "fragmentation needed", IPv6's "packet too big".
	case e.Origin == eeOriginICMP && e.Type == 3 && e.Code != 4, e.Origin == eeOriginICMP6 && e.Type == 1:
		return Unreachable, true
	}
	return 0, false
}
