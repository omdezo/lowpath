//go:build !windows

package icmp

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/net/icmp"
)

// queuedWait is how long the tests wait for a loopback reply to be queued
// on the socket before reading it with a deadline that already passed, as
// after a stall.
const queuedWait = 50 * time.Millisecond

// past is a read deadline a stall outlasted.
func past() time.Time { return time.Now().Add(-time.Second) }

// udpPair is two loopback UDP sockets: a stand-in for an ICMP socket that
// needs no privileges, served by the same poller.
func udpPair(t *testing.T, network, addr string) (recv, send net.PacketConn) {
	t.Helper()
	recv, err := net.ListenPacket(network, addr)
	if err != nil {
		t.Skipf("no %s loopback here: %v", network, err)
	}
	t.Cleanup(func() { recv.Close() })
	send, err = net.ListenPacket(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { send.Close() })
	return recv, send
}

func TestReplyReaderReadsQueuedAfterDeadline(t *testing.T) {
	for _, c := range []struct{ network, addr string }{{"udp4", "127.0.0.1:0"}, {"udp6", "[::1]:0"}} {
		t.Run(c.network, func(t *testing.T) {
			recv, send := udpPair(t, c.network, c.addr)
			if _, err := send.WriteTo([]byte("reply"), recv.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			time.Sleep(queuedWait)
			buf := make([]byte, 64)
			n, _, err := newReplyReader(recv, past()).ReadFrom(buf)
			if err != nil || string(buf[:n]) != "reply" {
				t.Fatalf("got %q, %v; want the queued reply", buf[:n], err)
			}
		})
	}
}

// The drain reads everything already queued, so a caller skipping other
// messages still finds its own, but it waits drainGrace once at most: no
// later read waits again, nor reads at all.
func TestReplyReaderDrainIsBounded(t *testing.T) {
	recv, send := udpPair(t, "udp4", "127.0.0.1:0")
	for _, m := range []string{"other", "reply"} {
		if _, err := send.WriteTo([]byte(m), recv.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(queuedWait)
	r := newReplyReader(recv, past())
	buf := make([]byte, 64)
	for _, want := range []string{"other", "reply"} {
		if n, _, err := r.ReadFrom(buf); err != nil || string(buf[:n]) != want {
			t.Fatalf("got %q, %v; want %q", buf[:n], err, want)
		}
	}
	begin := time.Now()
	if _, _, err := r.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("empty socket: got %v, want the deadline", err)
	}
	if waited := time.Since(begin); waited > drainGrace+500*time.Millisecond {
		t.Fatalf("waited %v past the drain's deadline", waited)
	}
	if _, err := send.WriteTo([]byte("late"), recv.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(queuedWait)
	if _, _, err := r.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("after the drain: got %v, want the deadline", err)
	}
}

// A ping whose reply was queued before a stall outlasted the deadline
// returns the reply, not ErrTimeout.
func TestPingReadsReplyQueuedPastDeadline(t *testing.T) {
	c, raw, err := listen4()
	if err != nil {
		t.Skipf("ICMP not available here: %v", err)
	}
	defer c.Close()
	p := echo4{dst: netip.MustParseAddr("127.0.0.1"), raw: raw, id: 0x1234, seq: 0x5678}
	start, err := p.send(c)
	if err != nil {
		t.Skipf("ICMP not available here: %v", err)
	}
	time.Sleep(queuedWait)
	rtt, err := p.await(newReplyReader(c, past()), start)
	if err != nil {
		t.Fatalf("got %v, want the queued reply", err)
	}
	if rtt < queuedWait {
		t.Fatalf("RTT %v, want it timed at the read (≥ %v)", rtt, queuedWait)
	}
}

// The same for a TTL-limited echo through a socket the poller serves (a
// raw socket, or the macOS datagram one).
func TestEchoTTLReadsReplyQueuedPastDeadline(t *testing.T) {
	for _, tc := range []struct {
		addr string
		v6   bool
	}{{"127.0.0.1", false}, {"::1", true}} {
		t.Run(tc.addr, func(t *testing.T) {
			c, dgram := listenTTL(t, tc.v6)
			dst := netip.MustParseAddr(tc.addr)
			var to net.Addr = &net.IPAddr{IP: dst.AsSlice()}
			if dgram {
				to = &net.UDPAddr{IP: dst.AsSlice()}
			}
			p := ttlProbe{dst: dst, id: 0x1234, seq: 0x5678, v6: tc.v6}
			start, err := p.send(c, to, 64)
			if errors.Is(err, ErrNoRoute) {
				t.Skipf("no route to %s here: %v", tc.addr, err)
			}
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(queuedWait)
			got, err := p.await(newReplyReader(c, past()), start)
			if err != nil {
				t.Fatalf("got %v, want the queued reply", err)
			}
			if got.Kind != Echo || got.From != dst || got.RTT < queuedWait {
				t.Fatalf("got %+v, want an echo from %s timed at the read", got, dst)
			}
		})
	}
}

// listenTTL opens the socket echoTTL reads: a raw one, or on macOS the
// datagram one any user may open (dgram).
func listenTTL(t *testing.T, v6 bool) (c *icmp.PacketConn, dgram bool) {
	t.Helper()
	network, laddr := "ip4:icmp", "0.0.0.0"
	if v6 {
		network, laddr = "ip6:ipv6-icmp", "::"
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil && runtime.GOOS == "darwin" {
		network = "udp4"
		if v6 {
			network = "udp6"
		}
		c, err = icmp.ListenPacket(network, laddr)
		dgram = true
	}
	if err != nil {
		t.Skipf("no ICMP socket here: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, dgram
}
