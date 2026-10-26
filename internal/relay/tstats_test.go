package relay

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"lowpath/internal/netsim"
	"lowpath/internal/proto"
	"lowpath/internal/tstats"
)

// recvStats waits for a TStats packet of flow on c whose report has
// settled at least seqs numbers, skipping everything else. When wait runs
// out it returns the last report, if there was one.
func recvStats(t *testing.T, c *net.UDPConn, flow uint64, seqs uint32, wait time.Duration) (proto.Header, tstats.Stats, int) {
	t.Helper()
	end := time.Now().Add(wait)
	var (
		lastH proto.Header
		last  tstats.Stats
		self  = -1
	)
	for time.Now().Before(end) {
		h, p, err := recv(c, time.Until(end))
		if err != nil {
			break
		}
		if h.Type != proto.TStats || h.Flow != flow {
			continue
		}
		s, me, err := tstats.Parse(p)
		if err != nil {
			t.Fatalf("TStats payload: %v", err)
		}
		lastH, last, self = h, s, me
		if s.Seqs >= seqs {
			return h, s, me
		}
	}
	if self < 0 {
		t.Fatalf("no TStats for flow %d within %v", flow, wait)
	}
	t.Logf("last report within %v settled %d numbers, not %d", wait, last.Seqs, seqs)
	return lastH, last, self
}

// Two lanes through netsim links with their own Gilbert bursts, on IPv4
// and IPv6 loopback: the reports the egress sends down each lane name that
// lane, and their counts match the losses the links' seeds make (the same
// choices replayed by netsim.Sampler) to 0.1% of the packets, the margin
// for a datagram the OS itself drops.
func TestTStatsMatchNetsimLanes(t *testing.T) {
	for _, loop := range []string{"127.0.0.1", "::1"} {
		t.Run(loop, func(t *testing.T) {
			host := netip.MustParseAddr(loop)
			srv, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(host, 0)))
			if err != nil {
				t.Skipf("no %s loopback: %v", loop, err)
			}
			t.Cleanup(func() { srv.Close() }) // a game server that never answers: only data goes through the links
			e, err := ListenEgressOpts(netip.AddrPortFrom(host, 0).String(), Options{AllowPrivate: true, StatsInterval: 100 * time.Millisecond}, quiet)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go e.Serve(ctx)

			profiles := []netsim.Profile{
				{Burst: netsim.Gilbert(0.04, 3), Seed: 41},
				{Burst: netsim.Gilbert(0.02, 2), Loss: 0.01, Seed: 42},
			}
			conns := make([]*net.UDPConn, len(profiles))
			for i, p := range profiles {
				l, err := netsim.Listen(netip.AddrPortFrom(host, 0).String(), e.Addr(), p)
				if err != nil {
					t.Fatal(err)
				}
				go l.Serve(ctx)
				c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(l.Addr()))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				conns[i] = c
			}

			const n = 3000
			h := proto.Header{Type: proto.TData, Token: 77, Flow: 9, Addr: udpAddr(srv)}
			start := time.Now()
			for s := 1; s <= n; s++ {
				h.Seq, h.TS = uint32(s), uint32(time.Since(start).Milliseconds())
				for _, c := range conns {
					send(t, c, h, []byte("game"))
				}
				if s%20 == 0 {
					time.Sleep(2 * time.Millisecond) // pace it, so loopback buffers don't overflow
				}
			}

			// Ground truth: the links' choices replayed in packet order.
			delivered := make([][]bool, len(profiles))
			for i, p := range profiles {
				sm := netsim.NewSampler(p)
				delivered[i] = make([]bool, n+1)
				for s := 1; s <= n; s++ {
					lost, _ := sm.Decide(0)
					delivered[i][s] = !lost
				}
			}
			span := func(i int) (first, last int) {
				for s := 1; s <= n; s++ {
					if delivered[i][s] {
						if first == 0 {
							first = s
						}
						last = s
					}
				}
				return first, last
			}
			f0, l0 := span(0)
			f1, l1 := span(1)
			var seqs, lostAll, joint, both uint32
			lanesLost := make([]uint32, 2)
			for s := 1; s <= n; s++ {
				in0, in1 := s >= f0 && s <= l0, s >= f1 && s <= l1
				if in0 || in1 {
					seqs++
					if !delivered[0][s] && !delivered[1][s] {
						lostAll++
					}
				}
				if in0 && !delivered[0][s] {
					lanesLost[0]++
				}
				if in1 && !delivered[1][s] {
					lanesLost[1]++
				}
				if in0 && in1 {
					both++
					if !delivered[0][s] && !delivered[1][s] {
						joint++
					}
				}
			}

			const slack = n / 1000
			nearCount := func(what string, got, want uint32) {
				t.Helper()
				if d := int(got) - int(want); d < -slack || d > slack {
					t.Errorf("%s = %d, want %d ± %d", what, got, want, slack)
				}
			}
			selves := map[int]bool{}
			for i, c := range conns {
				hdr, s, self := recvStats(t, c, 9, seqs, 5*time.Second)
				if hdr.Token != 77 || hdr.Addr != udpAddr(srv) || hdr.Seq == 0 || s.Dir != tstats.Up {
					t.Errorf("lane %d report header %+v dir %d", i, hdr, s.Dir)
				}
				if self < 0 || self >= tstats.MaxLanes || selves[self] {
					t.Fatalf("lane %d report names lane %d (seen %v)", i, self, selves)
				}
				selves[self] = true
				nearCount("seqs", s.Seqs, seqs)
				nearCount("lost on every lane", s.Lost, lostAll)
				l, ok := s.Lane(self)
				if !ok {
					t.Fatalf("report on lane %d lacks its own lane %d: %+v", i, self, s)
				}
				fi, la := span(i)
				nearCount("lane expected", l.Expected, uint32(la-fi+1))
				nearCount("lane lost", l.Lost, lanesLost[i])
				p, ok := s.Pair(0, 1)
				if !ok {
					t.Fatalf("no pair in %+v", s)
				}
				nearCount("both", p.Both, both)
				nearCount("joint", p.Joint, joint)
				t.Logf("lane %d: p %.4f (truth %.4f) p01 %.4f p10 %.4f jitter %v; p_AB %.5f (truth %.5f)",
					i, l.Loss(), float64(lanesLost[i])/float64(la-fi+1), l.P01(), l.P10(), l.Jitter(), p.PAB(), float64(joint)/float64(both))
			}
			if st := e.Stats(); st.Settled < uint64(seqs-slack) || st.Missing == 0 || st.Reports == 0 {
				t.Errorf("relay stats %+v", st)
			}
		})
	}
}

// A flow that settles nothing new gets no more reports, and a relay with a
// negative StatsInterval sends none.
func TestTStatsOnlyWhenSomethingSettled(t *testing.T) {
	srv := silentServer(t)
	e := startEgress(t, &Options{AllowPrivate: true, StatsInterval: 50 * time.Millisecond})
	c := dialEgress(t, e)
	for s := uint32(1); s <= 5; s++ {
		send(t, c, proto.Header{Type: proto.TData, Flow: 3, Seq: s, Addr: udpAddr(srv)}, nil)
	}
	recvStats(t, c, 3, 5, 3*time.Second)
	if h, _, err := recv(c, 300*time.Millisecond); err == nil {
		t.Fatalf("a second report with nothing new: %+v", h)
	}

	off := startEgress(t, &Options{AllowPrivate: true, StatsInterval: -1})
	c2 := dialEgress(t, off)
	send(t, c2, proto.Header{Type: proto.TData, Flow: 4, Seq: 1, Addr: udpAddr(srv)}, nil)
	if h, _, err := recv(c2, 500*time.Millisecond); err == nil {
		t.Fatalf("got %+v from a relay with reports off", h)
	}
}

// Each return address of a flow holds a lane slot; a slot freed by a stale
// address goes to the next newcomer as a new lane.
func TestFlowLaneSlots(t *testing.T) {
	f := &flow{paths: map[netip.AddrPort]time.Time{}, lanes: map[netip.AddrPort]int{}}
	now := time.Now()
	addr := func(i int) netip.AddrPort {
		return netip.AddrPortFrom(netip.MustParseAddr("192.0.2.1"), uint16(1000+i))
	}
	seen := map[int]bool{}
	for i := range maxPaths {
		at := now
		if i == 0 {
			at = now.Add(-pathTTL) // already stale when the table fills
		}
		l := f.addPath(addr(i), at)
		if l < 0 || seen[l] {
			t.Fatalf("address %d got lane %d (taken: %v)", i, l, seen)
		}
		seen[l] = true
		f.lanesUp.Add(l, uint32(i+1), 0, 0)
	}
	if f.addPath(addr(0), now) != f.lanes[addr(0)] {
		t.Fatal("a known address changed lanes")
	}
	f.paths[addr(0)] = now.Add(-pathTTL)
	old, _ := f.lanesUp.Report(0).Lane(f.lanes[addr(0)])

	l := f.addPath(addr(99), now)
	if l != int(old.Index) {
		t.Fatalf("newcomer got lane %d, want the stale address's %d", l, old.Index)
	}
	if _, ok := f.lanes[addr(0)]; ok {
		t.Fatal("stale address kept its lane")
	}
	f.lanesUp.Add(l, 100, 0, 0)
	if nl, _ := f.lanesUp.Report(0).Lane(l); nl.ID == old.ID {
		t.Fatalf("the reused slot kept the old lane's id %d", nl.ID)
	}
	if got := f.addPath(addr(100), now); got != -1 {
		t.Fatalf("address past the cap got lane %d", got)
	}
}
