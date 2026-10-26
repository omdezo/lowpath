package netsim

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// echoServer answers every datagram with the same bytes, like gamesim.
func echoServer(t *testing.T, ctx context.Context, addr string) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr)))
	if err != nil {
		t.Skipf("can't listen on %s: %v", addr, err) // no IPv6 loopback here
	}
	go func() { <-ctx.Done(); c.Close() }()
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			c.WriteToUDPAddrPort(buf[:n], src)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

// roundTrip sends msg through the link from a fresh client socket and
// reports whether the echo came back within wait.
func roundTrip(t *testing.T, l *Link, msg string, wait time.Duration) bool {
	t.Helper()
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(l.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	if string(buf[:n]) != msg {
		t.Fatalf("echo %q, want %q", buf[:n], msg)
	}
	return true
}

// TestLinksBehindSegment runs real links on IPv4 and IPv6 loopback: two
// lanes to one server forward both ways, a lossy shared segment cuts both
// at once, healing it brings both back, and a lane taken off the segment
// works while the other is still cut.
func TestLinksBehindSegment(t *testing.T) {
	for _, family := range []struct{ name, loop string }{
		{"IPv4", "127.0.0.1"},
		{"IPv6", "::1"},
	} {
		t.Run(family.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := echoServer(t, ctx, netip.AddrPortFrom(netip.MustParseAddr(family.loop), 0).String())
			listen := netip.AddrPortFrom(netip.MustParseAddr(family.loop), 0).String()
			lanes := make([]*Link, 2)
			for i := range lanes {
				l, err := Listen(listen, server, Profile{Delay: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				go l.Serve(ctx)
				lanes[i] = l
			}
			seg := NewSegment(Profile{})
			for _, l := range lanes {
				l.SetSegment(seg)
				if !roundTrip(t, l, "hello "+family.name, 2*time.Second) {
					t.Fatalf("%v: no echo through a clean segment", l.Addr())
				}
			}

			seg.SetProfile(Profile{Loss: 1})
			for _, l := range lanes {
				if roundTrip(t, l, "faded", 200*time.Millisecond) {
					t.Fatalf("%v: a packet crossed a fully lossy segment", l.Addr())
				}
			}

			lanes[0].SetSegment(nil)
			if !roundTrip(t, lanes[0], "off the Wi-Fi", 2*time.Second) {
				t.Error("a lane taken off the segment is still cut")
			}
			if roundTrip(t, lanes[1], "still on it", 200*time.Millisecond) {
				t.Error("the other lane got through the lossy segment")
			}

			seg.SetProfile(Profile{})
			if !roundTrip(t, lanes[1], "healed", 2*time.Second) {
				t.Error("no echo after the segment healed")
			}
		})
	}
}
