package icmp

import (
	"fmt"
	"net/netip"
	"syscall"
	"time"
	"unsafe"
)

var (
	iphlpapi            = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreateFile  = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpCloseHandle = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho    = iphlpapi.NewProc("IcmpSendEcho")
)

// icmpEchoReply mirrors ICMP_ECHO_REPLY on 64-bit Windows.
type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Options       [16]byte
}

const ipSuccess = 0

func ping4(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	h, _, err := procIcmpCreateFile.Call()
	if h == uintptr(syscall.InvalidHandle) {
		return 0, fmt.Errorf("icmp: IcmpCreateFile: %w", err)
	}
	defer procIcmpCloseHandle.Call(h)

	a := dst.As4()
	ipaddr := uint32(a[0]) | uint32(a[1])<<8 | uint32(a[2])<<16 | uint32(a[3])<<24 // network order in memory
	payload := [32]byte{'l', 'o', 'w', 'p', 'a', 't', 'h'}
	var reply [unsafe.Sizeof(icmpEchoReply{}) + 32 + 8 + 64]byte
	ms := uint32(max(timeout.Milliseconds(), 1))

	start := time.Now()
	n, _, _ := procIcmpSendEcho.Call(h, uintptr(ipaddr),
		uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(ms))
	rtt := time.Since(start)
	if n == 0 {
		return 0, ErrTimeout
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	if r.Status != ipSuccess {
		return 0, ErrTimeout
	}
	// Our wall clock has sub-millisecond resolution; the API's field does not.
	// Trust the API if the call itself was slowed down by scheduling.
	if api := time.Duration(r.RoundTripTime) * time.Millisecond; rtt > api+2*time.Millisecond {
		rtt = api
	}
	return rtt, nil
}
