// Package sockdiag builds and parses sock_diag requests for the TCP socket
// table: what `ss -ti` asks the kernel, so per-connection byte counters can
// be read without root. The layouts are linux/sock_diag.h,
// linux/inet_diag.h and linux/tcp.h (sock_diag(7)); numbers are in host
// byte order except ports and addresses, which are in network byte order.
//
// The kernel answers every user's dump, and fills in struct tcp_info
// (INET_DIAG_INFO) for each socket without checking capabilities: only the
// socket mark and the cgroup id need CAP_NET_ADMIN (net/ipv4/inet_diag.c).
package sockdiag

import (
	"encoding/binary"
	"net/netip"

	"lowpath/internal/netlink"
)

// Message type and request values.
const (
	typeByFamily = 20 // SOCK_DIAG_BY_FAMILY
	afInet       = 2
	afInet6      = 10
	ipprotoTCP   = 6
	// extInfo asks for INET_DIAG_INFO (2): idiag_ext bit 1 << (2-1).
	extInfo  = 1 << (2 - 1)
	attrInfo = 2

	reqLen = 56 // struct inet_diag_req_v2
	msgLen = 72 // struct inet_diag_msg
)

// TCP states (include/net/tcp_states.h).
const (
	stateEstablished = 1
	stateSynSent     = 2
	stateSynRecv     = 3
	stateFinWait1    = 4
	stateFinWait2    = 5
	stateTimeWait    = 6
	stateClose       = 7
	stateCloseWait   = 8
	stateLastAck     = 9
	stateListen      = 10
	stateClosing     = 11
)

// moving are the states in which a connection can still move data: not
// listening sockets, and not TIME_WAIT or CLOSE, which the kernel keeps
// without a tcp_info.
const moving = 1<<stateEstablished | 1<<stateSynSent | 1<<stateSynRecv | 1<<stateFinWait1 |
	1<<stateFinWait2 | 1<<stateCloseWait | 1<<stateLastAck | 1<<stateClosing

// Offsets in struct tcp_info of the RFC 4898 byte counters, both added in
// Linux 4.1; an older kernel sends a shorter tcp_info.
const (
	offBytesAcked    = 120 // tcpi_bytes_acked
	offBytesReceived = 128 // tcpi_bytes_received
)

// DumpTCP asks for every TCP connection of one address family in this
// network namespace, with its tcp_info. IPv4 peers of dual-stack sockets
// come in the IPv6 dump as v4-mapped addresses, and only there.
func DumpTCP(v6 bool) netlink.Message {
	b := make([]byte, reqLen)
	b[0] = afInet
	if v6 {
		b[0] = afInet6
	}
	b[1] = ipprotoTCP
	b[2] = extInfo
	binary.NativeEndian.PutUint32(b[4:], moving)
	// The socket id (b[8:56]) stays zero: a dump matches every socket.
	op := "dump IPv4 TCP sockets"
	if v6 {
		op = "dump IPv6 TCP sockets"
	}
	return netlink.Message{Type: typeByFamily, Data: b, Op: op}
}

// Socket is one TCP socket from a dump.
type Socket struct {
	Local, Remote netip.AddrPort
	State         uint8
	UID           uint32
	Inode         uint32 // 0: no process holds the socket any more
	Cookie        uint64 // unique for the socket's lifetime in this namespace
	// BytesAcked is the payload the peer acknowledged, BytesReceived the
	// payload received in order; valid only with HasBytes (Linux 4.1+).
	BytesAcked, BytesReceived uint64
	HasBytes                  bool
}

// Parse decodes one reply of a DumpTCP request. ok is false for anything
// else and for truncated messages.
func Parse(r netlink.Reply) (s Socket, ok bool) {
	b := r.Data
	if r.Type != typeByFamily || len(b) < msgLen {
		return Socket{}, false
	}
	var alen int
	switch b[0] {
	case afInet:
		alen = 4
	case afInet6:
		alen = 16
	default:
		return Socket{}, false
	}
	// struct inet_diag_sockid starts at 4: sport, dport (big-endian),
	// src[4], dst[4] (32-bit words, network order), if, cookie[2].
	addr := func(at int) netip.Addr {
		if alen == 4 {
			return netip.AddrFrom4([4]byte(b[at : at+4]))
		}
		return netip.AddrFrom16([16]byte(b[at : at+16]))
	}
	s.State = b[1]
	s.Local = netip.AddrPortFrom(addr(8), binary.BigEndian.Uint16(b[4:]))
	s.Remote = netip.AddrPortFrom(addr(24), binary.BigEndian.Uint16(b[6:]))
	s.Cookie = uint64(binary.NativeEndian.Uint32(b[44:])) | uint64(binary.NativeEndian.Uint32(b[48:]))<<32
	s.UID = binary.NativeEndian.Uint32(b[64:])
	s.Inode = binary.NativeEndian.Uint32(b[68:])
	for _, a := range netlink.ParseAttrs(b[msgLen:]) {
		if a.Type == attrInfo && len(a.Data) >= offBytesReceived+8 {
			s.BytesAcked = binary.NativeEndian.Uint64(a.Data[offBytesAcked:])
			s.BytesReceived = binary.NativeEndian.Uint64(a.Data[offBytesReceived:])
			s.HasBytes = true
		}
	}
	return s, true
}
