package icmp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"time"
	"unsafe"
)

// TTL-limited echo through the IP Helper API, which needs no administrator
// rights: IcmpSendEcho2Ex for IPv4 and Icmp6SendEcho2 for IPv6, the calls
// WinMTR and tracert use. Each call opens its own handle, so concurrent
// probes (one per hop) never share state.

var (
	procIcmpSendEcho2Ex = iphlpapi.NewProc("IcmpSendEcho2Ex")
	procIcmp6CreateFile = iphlpapi.NewProc("Icmp6CreateFile")
	procIcmp6SendEcho2  = iphlpapi.NewProc("Icmp6SendEcho2")
)

// ipOptionInformation mirrors IP_OPTION_INFORMATION in the native (64-bit)
// layout; the API reads only Ttl from it here.
type ipOptionInformation struct {
	Ttl         uint8
	Tos         uint8
	Flags       uint8
	OptionsSize uint8
	OptionsData uintptr
}

// sockaddrIn6 mirrors SOCKADDR_IN6.
type sockaddrIn6 struct {
	Family   uint16
	Port     uint16
	Flowinfo uint32
	Addr     [16]byte
	ScopeID  uint32
}

// IP_STATUS codes from ipexport.h.
const (
	ipDestNetUnreachable  = 11002 // IPv6: IP_DEST_NO_ROUTE
	ipDestHostUnreachable = 11003 // IPv6: IP_DEST_ADDR_UNREACHABLE
	ipDestProtUnreachable = 11004 // IPv6: IP_DEST_PROHIBITED
	ipDestPortUnreachable = 11005
	ipReqTimedOut         = 11010
	ipBadRoute            = 11012
	ipTTLExpiredTransit   = 11013 // IPv6: IP_HOP_LIMIT_EXCEEDED
	ipBadDestination      = 11018
	ipDestUnreachable     = 11040 // IPv6
	ipTimeExceeded        = 11041 // IPv6

	errorNetworkUnreachable = 1231
	errorHostUnreachable    = 1232
	afInet6                 = 23
)

// replyBuf is large enough for a reply structure, the echoed payload, the
// 8 bytes of an ICMP error, an IO_STATUS_BLOCK and IP options.
type replyBuf [256 + len(payloadTTL)]byte

func pingTTL4(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	h, _, err := procIcmpCreateFile.Call()
	if h == uintptr(syscall.InvalidHandle) {
		return Reply{}, fmt.Errorf("icmp: IcmpCreateFile: %w", err)
	}
	defer procIcmpCloseHandle.Call(h)

	a := dst.As4()
	ipaddr := uint32(a[0]) | uint32(a[1])<<8 | uint32(a[2])<<16 | uint32(a[3])<<24 // network order in memory
	opt := ipOptionInformation{Ttl: uint8(ttl)}
	var reply replyBuf
	start := time.Now()
	n, _, callErr := procIcmpSendEcho2Ex.Call(h, 0, 0, 0,
		0, uintptr(ipaddr),
		uintptr(unsafe.Pointer(&payloadTTL[0])), uintptr(len(payloadTTL)),
		uintptr(unsafe.Pointer(&opt)),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeoutMS(timeout)))
	rtt := time.Since(start)
	if n == 0 {
		return Reply{}, callError(callErr)
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	from := netip.AddrFrom4([4]byte{byte(r.Address), byte(r.Address >> 8), byte(r.Address >> 16), byte(r.Address >> 24)})
	return classify(r.Status, from, wallRTT(rtt, r.RoundTripTime))
}

func pingTTL6(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	h, _, err := procIcmp6CreateFile.Call()
	if h == uintptr(syscall.InvalidHandle) {
		return Reply{}, fmt.Errorf("icmp: Icmp6CreateFile: %w", err)
	}
	defer procIcmpCloseHandle.Call(h)

	src := sockaddrIn6{Family: afInet6}
	to := sockaddrIn6{Family: afInet6, Addr: dst.As16()}
	if z := dst.Zone(); z != "" {
		// Link-local hops need the interface; netip keeps it as the zone.
		if id, err := zoneIndex(z); err == nil {
			to.ScopeID = id
		}
	}
	opt := ipOptionInformation{Ttl: uint8(ttl)}
	var reply replyBuf
	start := time.Now()
	n, _, callErr := procIcmp6SendEcho2.Call(h, 0, 0, 0,
		uintptr(unsafe.Pointer(&src)), uintptr(unsafe.Pointer(&to)),
		uintptr(unsafe.Pointer(&payloadTTL[0])), uintptr(len(payloadTTL)),
		uintptr(unsafe.Pointer(&opt)),
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeoutMS(timeout)))
	rtt := time.Since(start)
	if n == 0 {
		return Reply{}, callError(callErr)
	}
	from, status, apiRTT := parseReply6(reply[:])
	return classify(status, from, wallRTT(rtt, apiRTT))
}

// parseReply6 reads an ICMPV6_ECHO_REPLY: a packed IPV6_ADDRESS_EX (port,
// flow info, the address as 8 big-endian words, scope id; 26 bytes), then
// Status and RoundTripTime.
func parseReply6(b []byte) (from netip.Addr, status, rttMS uint32) {
	var a [16]byte
	copy(a[:], b[6:22])
	from = netip.AddrFrom16(a)
	return from, binary.LittleEndian.Uint32(b[26:30]), binary.LittleEndian.Uint32(b[30:34])
}

// classify turns an IP_STATUS of a received reply into a Reply.
func classify(status uint32, from netip.Addr, rtt time.Duration) (Reply, error) {
	switch status {
	case ipSuccess:
		return Reply{From: from, RTT: rtt, Kind: Echo}, nil
	case ipTTLExpiredTransit, ipTimeExceeded:
		return Reply{From: from, RTT: rtt, Kind: TimeExceeded}, nil
	case ipDestNetUnreachable, ipDestHostUnreachable, ipDestProtUnreachable, ipDestPortUnreachable, ipDestUnreachable, ipBadRoute, ipBadDestination:
		return Reply{From: from, RTT: rtt, Kind: Unreachable}, nil
	case ipReqTimedOut:
		return Reply{}, ErrTimeout
	}
	return Reply{}, fmt.Errorf("icmp: reply status %d", status)
}

// callError maps the error of a call that returned no reply. The API
// reports a missing route (offline, no IPv6) through the same channel as
// a timeout.
func callError(err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return ErrTimeout
	}
	switch errno {
	case 0, ipReqTimedOut:
		return ErrTimeout
	case errorNetworkUnreachable, errorHostUnreachable, ipDestNetUnreachable, ipDestHostUnreachable, ipBadRoute, ipBadDestination:
		return ErrNoRoute
	}
	return fmt.Errorf("icmp: %w", err)
}

func timeoutMS(d time.Duration) uint32 { return uint32(min(max(d.Milliseconds(), 1), 60_000)) }

// wallRTT prefers the sub-millisecond wall clock unless scheduling slowed
// the call itself down (see ping4).
func wallRTT(wall time.Duration, apiMS uint32) time.Duration {
	if api := time.Duration(apiMS) * time.Millisecond; wall > api+2*time.Millisecond {
		return api
	}
	return wall
}

// zoneIndex resolves an IPv6 zone (interface name or number) to its index.
func zoneIndex(z string) (uint32, error) {
	if id, err := strconv.ParseUint(z, 10, 32); err == nil {
		return uint32(id), nil
	}
	ifi, err := net.InterfaceByName(z)
	if err != nil {
		return 0, err
	}
	return uint32(ifi.Index), nil
}
