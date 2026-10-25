// Package icmp sends single ICMP echo requests ("ping"). The client uses it to
// measure the direct route to a game server (the "before" number) and relays
// use it to measure their own leg to the game server.
//
// Windows uses IcmpSendEcho (no admin rights needed). Other platforms use a raw
// ICMP socket when running as root, else an unprivileged ping socket.
package icmp

import (
	"errors"
	"net/netip"
	"time"
)

var (
	ErrTimeout     = errors.New("icmp: no reply")
	ErrUnsupported = errors.New("icmp: only IPv4 is supported")
)

// Ping sends one echo request to dst and returns the round trip time.
func Ping(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	dst = dst.Unmap()
	if !dst.Is4() {
		return 0, ErrUnsupported
	}
	return ping4(dst, timeout)
}
