package owner

import (
	"bytes"
	"encoding/binary"
	"net/netip"
)

// Layout of the macOS proc_info structures the Darwin resolver reads,
// from xnu's bsd/sys/proc_info.h. They are the same on x86_64 and arm64
// (both LP64, little-endian). The parsers are here, without a build tag, so
// they are tested on every OS.
const (
	procInfoCallPIDInfo   = 2 // PROC_INFO_CALL_PIDINFO
	procInfoCallPIDFDInfo = 3 // PROC_INFO_CALL_PIDFDINFO

	procPIDListFDs      = 1  // PROC_PIDLISTFDS: array of struct proc_fdinfo
	procPIDPathInfo     = 11 // PROC_PIDPATHINFO: the program's path
	procPIDFDSocketInfo = 3  // PROC_PIDFDSOCKETINFO: struct socket_fdinfo

	procFDInfoLen        = 8    // struct proc_fdinfo {int32 proc_fd; uint32 proc_fdtype}
	proxFDTypeSocket     = 2    // PROX_FDTYPE_SOCKET
	socketFDInfoLen      = 792  // sizeof(struct socket_fdinfo)
	procPIDPathInfoMax   = 4096 // PROC_PIDPATHINFO_MAXSIZE (4*MAXPATHLEN)
	sockinfoIn           = 1    // SOCKINFO_IN: a UDP (or raw IP) socket
	ipprotoUDP           = 17
	afInetDarwin         = 2
	afInet6Darwin        = 30
	iniIPv4, iniIPv6     = 1, 2 // insi_vflag
	socketInfoOff        = 24   // struct socket_fdinfo: proc_fileinfo pfi (24 bytes), then socket_info psi
	soiProtocolOff       = socketInfoOff + 156
	soiFamilyOff         = socketInfoOff + 160
	soiKindOff           = socketInfoOff + 232
	insiOff              = socketInfoOff + 240 // soi_proto.pri_in, a struct in_sockinfo
	insiLportOff         = insiOff + 4         // int; the port in network byte order in its low 16 bits
	insiVflagOff         = insiOff + 24
	insiLaddrOff         = insiOff + 48 // union {in4in6_addr; in6_addr}
	insiLaddr4Off        = insiLaddrOff + 12
	socketFDInfoMinBytes = insiLaddrOff + 16
)

// parseProcFDs decodes a PROC_PIDLISTFDS reply into the descriptors that
// are sockets.
func parseProcFDs(b []byte) []int32 {
	var out []int32
	for ; len(b) >= procFDInfoLen; b = b[procFDInfoLen:] {
		if binary.LittleEndian.Uint32(b[4:]) == proxFDTypeSocket {
			out = append(out, int32(binary.LittleEndian.Uint32(b)))
		}
	}
	return out
}

// parseSocketFDInfo returns the local address of a UDP socket from a
// PROC_PIDFDSOCKETINFO reply; false for any other kind of socket. An IPv6
// socket that only carries IPv4 (insi_vflag INI_IPV4) reports the IPv4
// address, as the Windows table does.
func parseSocketFDInfo(b []byte) (netip.AddrPort, bool) {
	if len(b) < socketFDInfoMinBytes {
		return netip.AddrPort{}, false
	}
	le := binary.LittleEndian
	if le.Uint32(b[soiKindOff:]) != sockinfoIn || le.Uint32(b[soiProtocolOff:]) != ipprotoUDP {
		return netip.AddrPort{}, false
	}
	// insi_lport holds inp_lport, a network-order u_short, in an int: its
	// first two bytes in memory are the port, big-endian.
	port := binary.BigEndian.Uint16(b[insiLportOff:])
	family := le.Uint32(b[soiFamilyOff:])
	vflag := b[insiVflagOff]
	var a netip.Addr
	switch {
	case family == afInet6Darwin && vflag&iniIPv6 != 0:
		a = netip.AddrFrom16([16]byte(b[insiLaddrOff : insiLaddrOff+16]))
	case family == afInetDarwin || family == afInet6Darwin && vflag&iniIPv4 != 0:
		a = netip.AddrFrom4([4]byte(b[insiLaddr4Off : insiLaddr4Off+4]))
	default:
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(a, port), true
}

// parseProcArgs2 decodes the kern.procargs2 sysctl: an int32 argc, the
// executable path, NUL padding, argc arguments, then the environment, all
// NUL-terminated. Missing parts come back empty.
func parseProcArgs2(b []byte) (argv0 string, env []string) {
	if len(b) < 4 {
		return "", nil
	}
	argc := int(int32(binary.LittleEndian.Uint32(b)))
	b = b[4:]
	next := func() (string, bool) {
		i := bytes.IndexByte(b, 0)
		if i < 0 {
			return "", false
		}
		s := string(b[:i])
		b = b[i+1:]
		return s, true
	}
	if _, ok := next(); !ok { // exec path
		return "", nil
	}
	for len(b) > 0 && b[0] == 0 {
		b = b[1:]
	}
	for i := 0; i < argc; i++ {
		s, ok := next()
		if !ok {
			return argv0, nil
		}
		if i == 0 {
			argv0 = s
		}
	}
	for {
		s, ok := next()
		if !ok || s == "" {
			return argv0, env
		}
		env = append(env, s)
	}
}

// cString is the text before the first NUL.
func cString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}
