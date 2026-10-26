// Package icmp sends single ICMP echo requests ("ping"). The client uses it to
// measure the direct route to a game server (the "before" number) and relays
// use it to measure their own leg to the game server. PingTTL sends an echo
// that may only cross a given number of routers, which is how the route
// trace (MTR) finds and measures every hop.
//
// Windows uses IcmpSendEcho, IcmpSendEcho2Ex and Icmp6SendEcho2 (no admin
// rights needed). Other platforms use a raw ICMP socket when running as
// root, else an unprivileged ping socket for Ping. PingTTL needs the raw
// socket there: a ping socket never returns the routers' "time exceeded"
// replies as messages (Linux only queues them as socket errors).
package icmp

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
)

var (
	ErrTimeout     = errors.New("icmp: no reply")
	ErrUnsupported = errors.New("icmp: only IPv4 is supported")
	// ErrNoRoute: this PC has no route to the destination (offline, or no
	// IPv6 on this network), so nothing was sent.
	ErrNoRoute = errors.New("icmp: no route to the destination")
)

// Ping sends one echo request to dst and returns the round trip time.
func Ping(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	dst = dst.Unmap()
	if !dst.Is4() {
		return 0, ErrUnsupported
	}
	return ping4(dst, timeout)
}

// ReplyKind says who answered a TTL-limited echo.
type ReplyKind int

const (
	// Echo: the destination itself answered.
	Echo ReplyKind = iota + 1
	// TimeExceeded: a router on the way dropped the probe because its TTL
	// (IPv6: hop limit) ran out, and said so. From is that router.
	TimeExceeded
	// Unreachable: a router (or the destination) answered that the
	// destination can't be reached.
	Unreachable
)

func (k ReplyKind) String() string {
	switch k {
	case Echo:
		return "echo"
	case TimeExceeded:
		return "time exceeded"
	case Unreachable:
		return "unreachable"
	}
	return "unknown"
}

// Reply is the answer to one TTL-limited echo.
type Reply struct {
	From netip.Addr
	RTT  time.Duration
	Kind ReplyKind
}

// payloadTTL is the PingTTL payload: 64 bytes, WinMTR's default size, so
// a route trace compares with a WinMTR report of the same target.
var payloadTTL = func() (b [64]byte) {
	copy(b[:], "lowpath route trace")
	return
}()

// MaxTTL is the largest TTL PingTTL accepts.
const MaxTTL = 255

// PingTTL sends one echo request to dst that may cross at most ttl routers
// and waits up to timeout for whoever answers: the router where the TTL
// ran out, or dst itself. It returns ErrTimeout when nobody answered (many
// routers never do), ErrNoRoute when the PC can't send toward dst, and an
// error matching os.ErrPermission when the OS needs privileges for it.
func PingTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	if ttl < 1 || ttl > MaxTTL {
		return Reply{}, fmt.Errorf("icmp: TTL %d out of range 1..%d", ttl, MaxTTL)
	}
	if !dst.IsValid() {
		return Reply{}, errors.New("icmp: no destination address")
	}
	if dst.Is4In6() {
		dst = dst.Unmap()
	}
	timeout = max(timeout, time.Millisecond)
	if dst.Is4() {
		return pingTTL4(dst, ttl, timeout)
	}
	return pingTTL6(dst, ttl, timeout)
}
