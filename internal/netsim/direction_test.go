package netsim

import (
	"context"
	"math"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// deliveredReply sends one reply from the target over l and reports
// whether it got through (zero-delay profiles deliver synchronously).
func deliveredReply(l *Link) bool {
	got := false
	l.send([]byte{1}, true, func([]byte) { got = true })
	return got
}

// TestLinkDirectionsApart: each direction of a link has its own profile
// and its own random state. The up direction loses exactly the packets a
// Sampler with its profile does, however many replies flow in between, so
// existing seeded runs replay unchanged; the down direction draws from a
// stream of its own under the same Seed.
func TestLinkDirectionsApart(t *testing.T) {
	p := Profile{Loss: 0.2, Burst: Gilbert(0.05, 4), Seed: 51}
	c := &clockAt{}
	l := testLink(p, c.read)
	if up, down := l.Profiles(); up != p || down != p || l.Profile() != p {
		t.Fatalf("Profiles() = %+v, %+v; Profile() = %+v, want %+v for both", up, down, l.Profile(), p)
	}
	want := NewSampler(p)
	var upLost, downLost [5000]bool
	for i := range upLost {
		c.now = time.Duration(i) * time.Millisecond
		upLost[i] = !delivered(l)
		for range i % 3 { // a varying number of replies between client packets
			downLost[i] = !deliveredReply(l) || downLost[i]
		}
		if lost, _ := want.Decide(c.now); lost != upLost[i] {
			t.Fatalf("packet %d: up lost %v, the sampler says %v: replies moved the up direction", i, upLost[i], lost)
		}
	}
	if upLost == downLost {
		t.Fatal("the down direction made the up direction's choices")
	}

	clean, lossy := Profile{Delay: 2 * time.Millisecond}, Profile{Loss: 1, Seed: 52}
	l.SetProfiles(clean, lossy)
	if up, down := l.Profiles(); up != clean || down != lossy || l.Profile() != clean {
		t.Fatalf("after SetProfiles: Profiles() = %+v, %+v; Profile() = %+v", up, down, l.Profile())
	}
	for range 100 {
		if deliveredReply(l) {
			t.Fatal("a reply crossed a fully lossy down direction")
		}
	}
	if drop, d := l.up.decide(0); drop || d != 2*time.Millisecond {
		t.Errorf("up direction: drop %v delay %v, want the clean 2 ms profile", drop, d)
	}
	l.SetProfile(Profile{})
	if !deliveredReply(l) || !delivered(l) {
		t.Error("SetProfile didn't reset both directions")
	}
}

// TestSegmentHalfAndFullDuplex: a segment is one radio by default, so a
// fade takes packets going both ways at once; SetProfiles splits it into
// a channel per direction, and SetProfile makes it one again.
func TestSegmentHalfAndFullDuplex(t *testing.T) {
	wifi := Profile{Burst: GilbertTime(0.1, 8*time.Millisecond, time.Millisecond), Seed: 53}
	c := &clockAt{}
	l := testLink(Profile{}, c.read)
	seg := testSegment(wifi, c.read)
	l.SetSegment(seg)
	// Count the steps where only one direction lost its packet.
	disagree := func() (lost, apart int) {
		for range 20_000 {
			c.now += 3 * time.Millisecond
			up, down := !delivered(l), !deliveredReply(l)
			if up {
				lost++
			}
			if up != down {
				apart++
			}
		}
		return lost, apart
	}
	if seg.Split() {
		t.Fatal("a new segment is split")
	}
	if lost, apart := disagree(); lost < 1000 || apart != 0 {
		t.Fatalf("half duplex: lost %d of 20000 up, %d steps where one direction alone lost, want ~2000 and 0", lost, apart)
	}

	// Same profile both ways, but two channels: losses no longer line up.
	other := wifi
	other.Seed = 54
	seg.SetProfiles(wifi, other)
	if up, down := seg.Profiles(); !seg.Split() || up != wifi || down != other || seg.Profile() != wifi {
		t.Fatalf("after SetProfiles: split %v, Profiles() = %+v, %+v", seg.Split(), up, down)
	}
	if lost, apart := disagree(); lost < 1000 || apart < 2000 {
		t.Fatalf("full duplex: lost %d up, %d steps where one direction alone lost, want ~2000 and ~3600", lost, apart)
	}

	// Download-only loss on a wired hop.
	seg.SetProfiles(Profile{}, Profile{Loss: 1})
	if !delivered(l) || deliveredReply(l) {
		t.Fatal("a segment lossy on the download only should pass the upload and cut replies")
	}
	seg.SetProfile(wifi)
	if seg.Split() {
		t.Fatal("SetProfile didn't make the segment one channel again")
	}
	if _, apart := disagree(); apart != 0 {
		t.Fatalf("half duplex again: %d steps where one direction alone lost", apart)
	}
}

// TestDownlinkOnlyLoss is roadmap L11's network: real links on IPv4 and
// IPv6 loopback with 3% loss on the download only. A counting echo server
// tells upload losses (it never saw the packet) from download ones (its
// echo never came back): the download loses 3% ± 5 standard errors, and
// exactly what the seed says; the upload loses nothing.
func TestDownlinkOnlyLoss(t *testing.T) {
	const n = 10_000
	const loss = 0.03
	for _, family := range []struct{ name, loop string }{
		{"IPv4", "127.0.0.1"},
		{"IPv6", "::1"},
	} {
		t.Run(family.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			loop := netip.MustParseAddr(family.loop)
			srv, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(loop, 0)))
			if err != nil {
				t.Skipf("no %s loopback: %v", family.loop, err)
			}
			go func() { <-ctx.Done(); srv.Close() }()
			var serverGot atomic.Int64
			go func() {
				buf := make([]byte, 2048)
				for {
					k, src, err := srv.ReadFromUDPAddrPort(buf)
					if err != nil {
						return
					}
					serverGot.Add(1)
					srv.WriteToUDPAddrPort(buf[:k], src)
				}
			}()

			down := Profile{Loss: loss, Seed: 61}
			l, err := Listen(netip.AddrPortFrom(loop, 0).String(), srv.LocalAddr().(*net.UDPAddr).AddrPort(), Profile{})
			if err != nil {
				t.Fatal(err)
			}
			l.SetProfiles(Profile{Seed: 61}, down)
			go l.Serve(ctx)

			client, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(l.Addr()))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			echoes := make(chan int, 1)
			go func() {
				got, buf := 0, make([]byte, 2048)
				for {
					client.SetReadDeadline(time.Now().Add(time.Second))
					if _, err := client.Read(buf); err != nil {
						echoes <- got
						return
					}
					got++
				}
			}()
			for i := range n {
				if _, err := client.Write([]byte{byte(i), byte(i >> 8)}); err != nil {
					t.Fatal(err)
				}
				if i%20 == 19 {
					time.Sleep(time.Millisecond) // pace it, so loopback buffers don't overflow
				}
			}
			back := <-echoes // a second after the last echo

			// Ground truth: the down direction's choices replayed in order.
			replay, seeded := newStreamImpairer(down.Seed, downStream), 0
			for range serverGot.Load() {
				if lost, _ := replay.decide(&down, 0); lost {
					seeded++
				}
			}
			upLost := n - int(serverGot.Load())
			downLost := int(serverGot.Load()) - back
			rate := float64(downLost) / float64(serverGot.Load())
			tol := 5 * math.Sqrt(loss*(1-loss)/float64(serverGot.Load()))
			t.Logf("up lost %d of %d, down lost %d (%.4f, want %.2f ± %.4f; the seed says %d)", upLost, n, downLost, rate, loss, tol, seeded)
			// 0.1% of the packets is the margin for a datagram the OS itself
			// drops, as in the relay's TStats test.
			const osDrops = n / 1000
			if upLost > osDrops {
				t.Errorf("the upload lost %d packets with no loss configured", upLost)
			}
			if !near(rate, loss, tol) {
				t.Errorf("download loss %.4f, want %.2f ± %.4f", rate, loss, tol)
			}
			if downLost < seeded || downLost > seeded+osDrops {
				t.Errorf("download lost %d packets, the seed says %d", downLost, seeded)
			}
		})
	}
}
