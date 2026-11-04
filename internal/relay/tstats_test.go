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

// recvFull waits for a full TStats report (one with lane blocks) of flow
// on any of conns whose report has settled at least seqs numbers. When
// wait runs out it returns the last full report, if there was one.
func recvFull(t *testing.T, conns []*net.UDPConn, flow uint64, seqs uint32, wait time.Duration) (proto.Header, tstats.Stats) {
	t.Helper()
	end := time.Now().Add(wait)
	var (
		lastH proto.Header
		last  tstats.Stats
		seen  bool
	)
	for time.Now().Before(end) {
		for _, c := range conns {
			h, p, err := recv(c, 20*time.Millisecond)
			if err != nil || h.Type != proto.TStats || h.Flow != flow {
				continue
			}
			s, _, err := tstats.Parse(p)
			if err != nil {
				t.Fatalf("TStats payload: %v", err)
			}
			if len(s.Lanes) == 0 {
				continue // a short report
			}
			lastH, last, seen = h, s, true
			if s.Seqs >= seqs {
				return h, s
			}
		}
	}
	if !seen {
		t.Fatalf("no full TStats for flow %d within %v", flow, wait)
	}
	t.Logf("last full report within %v settled %d numbers, not %d", wait, last.Seqs, seqs)
	return lastH, last
}

// Two lanes through netsim links with their own Gilbert bursts, on IPv4
// and IPv6 loopback: the reports the egress sends down each lane name that
// lane, and the full report's counts match the losses the links' seeds make (the same
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
			// Every lane's first report, short or full, names it; the full
			// counters come down one of them.
			selves := map[int]bool{}
			lanes := make([]int, len(conns))
			for i, c := range conns {
				_, _, self := recvStats(t, c, 9, 0, 5*time.Second)
				if self < 0 || self >= tstats.MaxLanes || selves[self] {
					t.Fatalf("lane %d report names lane %d (seen %v)", i, self, selves)
				}
				selves[self] = true
				lanes[i] = self
			}
			hdr, s := recvFull(t, conns, 9, seqs, 5*time.Second)
			if hdr.Token != 77 || hdr.Addr != udpAddr(srv) || hdr.Seq == 0 || s.Dir != tstats.Up {
				t.Errorf("report header %+v dir %d", hdr, s.Dir)
			}
			nearCount("seqs", s.Seqs, seqs)
			nearCount("lost on every lane", s.Lost, lostAll)
			for i, self := range lanes {
				l, ok := s.Lane(self)
				if !ok {
					t.Fatalf("full report lacks lane %d's counters (lane %d): %+v", i, self, s)
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
			if st := e.Stats(); st.Settled < uint64(seqs-slack) || st.Missing == 0 || st.Reports == 0 || st.ReportBytes < st.Reports*proto.FixedLen {
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

// Four lanes for 60 s on a fake clock, one report interval a second:
// every lane gets a report naming it at least every 5 s, the full counters
// come down some lane every second, and the TStats traffic on every lane
// but the one carrying the full report stays under 0.3 MB an hour,
// counted as the issue (#213) does: each datagram plus 28 bytes of IPv4
// and UDP header. The lane that shows up first is a tethered phone, the
// fastest lane for the first second and the slowest after that, so it
// gets the full report at first and must hand it over.
func TestTStatsFourLanesBytes(t *testing.T) {
	srv := silentServer(t)
	e, err := ListenEgressOpts("127.0.0.1:0", Options{AllowPrivate: true, StatsInterval: -1}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.Close()
		e.mu.Lock()
		for _, f := range e.flows {
			f.upstream.Close()
		}
		e.mu.Unlock()
	})
	clock := e.start
	e.now = func() time.Time { return clock }

	const lanes, phone = 4, 0
	clients := make([]*net.UDPConn, lanes)
	for i := range clients {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		clients[i] = c
	}

	// 32 packets a second, each copied down every lane 4 ms apart; the
	// test drives the egress itself (handle, sendStats), so nothing sleeps.
	const rate = 32
	h := proto.Header{Type: proto.TData, Flow: 21, Addr: udpAddr(srv)}
	var out []byte
	for sec := 1; sec <= 60; sec++ {
		order := []int{1, 2, 3, phone}
		if sec == 1 {
			order = []int{phone, 1, 2, 3}
		}
		for k := range rate {
			h.Seq++
			sent := e.start.Add(time.Duration(sec-1)*time.Second + time.Duration(k)*time.Second/rate)
			h.TS = uint32(sent.Sub(e.start).Milliseconds())
			for i, c := range order {
				clock = sent.Add(time.Duration(i) * 4 * time.Millisecond)
				out = e.handle(proto.Append(nil, h, []byte("game")), udpAddr(clients[c]), out)
			}
		}
		clock = e.start.Add(time.Duration(sec) * time.Second)
		e.sendStats(clock)
	}

	type report struct {
		sec   int
		self  int
		full  bool
		bytes int
	}
	got := make([][]report, lanes)
	var sent, sentBytes uint64
	for i, c := range clients {
		buf := make([]byte, 2048)
		for {
			c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
			n, err := c.Read(buf)
			if err != nil {
				break
			}
			hd, p, err := proto.Parse(buf[:n])
			if err != nil || hd.Type != proto.TStats || hd.Flow != 21 {
				t.Fatalf("lane %d got %+v %v", i, hd, err)
			}
			s, self, err := tstats.Parse(p)
			if err != nil {
				t.Fatalf("TStats payload: %v", err)
			}
			full := len(s.Lanes) > 0
			if full && (len(s.Lanes) != lanes || len(s.Pairs) != lanes*(lanes-1)/2) {
				t.Fatalf("full report with %d lanes, %d pairs", len(s.Lanes), len(s.Pairs))
			}
			if !full && len(s.Pairs) > 0 {
				t.Fatalf("short report with pairs: %+v", s)
			}
			if hd.TS%1000 != 0 {
				t.Fatalf("report stamped %d ms, not on a tick", hd.TS)
			}
			got[i] = append(got[i], report{sec: int(hd.TS / 1000), self: self, full: full, bytes: n})
			sent++
			sentBytes += uint64(n)
		}
	}

	fullAt := map[int]int{} // second -> lane that got the full report
	expensive := 0
	for i, rs := range got {
		if len(rs) == 0 {
			t.Fatalf("lane %d got no reports", i)
		}
		prev, wire, fulls := 0, 0, 0
		for _, r := range rs {
			if r.self != rs[0].self {
				t.Fatalf("lane %d named %d, then %d", i, rs[0].self, r.self)
			}
			if r.sec-prev > 5 {
				t.Errorf("lane %d: no self from %d s to %d s", i, prev, r.sec)
			}
			prev = r.sec
			if r.full {
				if l, dup := fullAt[r.sec]; dup {
					t.Errorf("second %d: full report on lanes %d and %d", r.sec, l, i)
				}
				fullAt[r.sec] = i
				fulls++
			}
			wire += r.bytes + 20 + 8 // IPv4 + UDP headers
		}
		if rs[0].sec != 1 {
			t.Errorf("lane %d first named at %d s", i, rs[0].sec)
		}
		if 60-prev >= 5 {
			t.Errorf("lane %d: no self from %d s to the end", i, prev)
		}
		perHour := float64(wire) * 3600 / 60 / 1e6
		t.Logf("lane %d (slot %d): %d reports, %d full, %d bytes on the wire in 60 s = %.3f MB/h", i, rs[0].self, len(rs), fulls, wire, perHour)
		if perHour >= 0.3 {
			expensive++
			if i == phone {
				t.Errorf("the phone lane, the slowest, pays %.3f MB/h", perHour)
			}
		}
	}
	for sec := 1; sec <= 60; sec++ {
		if _, ok := fullAt[sec]; !ok {
			t.Errorf("second %d: no full report on any lane", sec)
		}
	}
	if expensive > 1 {
		t.Errorf("%d lanes at 0.3 MB/h or more, want at most 1", expensive)
	}
	if st := e.Stats(); st.Reports != sent || st.ReportBytes != sentBytes {
		t.Errorf("relay counted %d reports, %d bytes; lanes got %d, %d", st.Reports, st.ReportBytes, sent, sentBytes)
	}
}
