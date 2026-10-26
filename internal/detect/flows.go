package detect

import (
	"encoding/binary"
	"errors"
	"net/netip"
)

// FlowEvent is one packet, or one send or receive call, between a local
// process and a remote endpoint, as the OS reports it: addresses, ports and
// size only, never the payload.
type FlowEvent struct {
	PID      uint32
	Proto    string // "udp" or "tcp"
	Local    netip.AddrPort
	Remote   netip.AddrPort
	Bytes    uint32
	Outbound bool // local → remote
}

// FlowWatch is a running flow watcher.
type FlowWatch interface {
	// Close stops the watch. Events stop arriving before it returns.
	Close() error
	// Done is closed once events stop: after Close, or when the OS ended
	// the watch by itself (Windows: the ETW session was stopped from
	// outside, or its consumer failed). The owner then closes the watch
	// and may start a new one.
	Done() <-chan struct{}
}

// ErrNeedAdmin is returned when the OS lets only administrators watch
// network events (Windows: ETW real-time sessions need administrator
// rights or the Performance Log Users group).
var ErrNeedAdmin = errors.New("watching network flows needs administrator rights")

// Event ids of the Microsoft-Windows-Kernel-Network provider that carry a
// flow, from its manifest (version 0 of each event): TCP data sent and
// received, and UDP data sent and received, over IPv4 and IPv6.
const (
	knTCPSent4 = 10
	knTCPRecv4 = 11
	knTCPSent6 = 26
	knTCPRecv6 = 27
	knUDPSent4 = 42
	knUDPRecv4 = 43
	knUDPSent6 = 58
	knUDPRecv6 = 59
)

// parseKernelNetwork decodes the user data of a Microsoft-Windows-Kernel-
// Network event. Every flow event starts with the same fields:
//
//	PID uint32, size uint32, daddr, saddr, dport uint16, sport uint16, ...
//
// where the addresses are a UInt32 (IPv4) or 16 bytes (IPv6) in network
// byte order and the ports are in network byte order too (outType
// win:Port). The kernel names the remote end "d" and the local end "s" for
// both directions; a consumer that finds the pair the other way round for
// receives can still match them by the remote it learned from sends (see
// the app's flow learner). ok is false for other events and short data.
func parseKernelNetwork(id uint16, data []byte) (ev FlowEvent, ok bool) {
	var v6 bool
	switch id {
	case knTCPSent4, knTCPRecv4:
		ev.Proto = "tcp"
	case knTCPSent6, knTCPRecv6:
		ev.Proto, v6 = "tcp", true
	case knUDPSent4, knUDPRecv4:
		ev.Proto = "udp"
	case knUDPSent6, knUDPRecv6:
		ev.Proto, v6 = "udp", true
	default:
		return FlowEvent{}, false
	}
	ev.Outbound = id == knTCPSent4 || id == knTCPSent6 || id == knUDPSent4 || id == knUDPSent6
	alen := 4
	if v6 {
		alen = 16
	}
	need := 8 + 2*alen + 4
	if len(data) < need {
		return FlowEvent{}, false
	}
	ev.PID = binary.LittleEndian.Uint32(data[0:])
	ev.Bytes = binary.LittleEndian.Uint32(data[4:])
	addr := func(b []byte) netip.Addr {
		if v6 {
			return netip.AddrFrom16([16]byte(b)).Unmap()
		}
		return netip.AddrFrom4([4]byte(b))
	}
	d := addr(data[8 : 8+alen])
	s := addr(data[8+alen : 8+2*alen])
	p := 8 + 2*alen
	dport := binary.BigEndian.Uint16(data[p:])
	sport := binary.BigEndian.Uint16(data[p+2:])
	ev.Remote = netip.AddrPortFrom(d, dport)
	ev.Local = netip.AddrPortFrom(s, sport)
	return ev, true
}
