package tstats

import (
	"math"
	"sort"
	"testing"
	"time"

	"lowpath/internal/netsim"
)

// These tests run the estimators on loss and delay drawn from netsim's
// models with fixed seeds, many packets at a time, without sockets. Each
// first checks the Tracker's counts against the ground truth of the same
// run exactly, then the estimates against the model's closed forms within
// five standard errors. Fixed seeds make every run the same, so they
// can't flake. Before settling the margins they ran over 40 seeds each:
// the counts matched the truth exactly every time, the worst estimate was
// 4.2 standard errors off, and the ranges of Rho are noted below.

// simLane is one lane of a simulated flow: its own link, and an offset of
// its copies from the packet's send time (a time-shifted duplicate).
type simLane struct {
	link  netsim.Profile
	shift time.Duration
}

type arrival struct {
	lane int
	seq  uint32
	sent time.Duration
	at   time.Duration
}

// simulate sends n packets every interval down each lane (behind seg when
// it isn't nil), feeds the copies that arrive to a Tracker in arrival
// order with a report every 100 ms, and returns the final report and which
// copies arrived.
func simulate(n int, every time.Duration, seg *netsim.Sampler, lanes []simLane) (Stats, [][]bool) {
	links := make([]*netsim.Sampler, len(lanes))
	for i, l := range lanes {
		links[i] = netsim.NewSampler(l.link)
	}
	type send struct {
		lane int
		seq  int
		at   time.Duration
	}
	sends := make([]send, 0, n*len(lanes))
	for s := range n {
		for i, l := range lanes {
			sends = append(sends, send{i, s, time.Duration(s)*every + l.shift})
		}
	}
	// The shared hop sees copies in the order they are sent.
	sort.SliceStable(sends, func(a, b int) bool { return sends[a].at < sends[b].at })
	got := make([][]bool, len(lanes))
	for i := range got {
		got[i] = make([]bool, n)
	}
	var arr []arrival
	for _, s := range sends {
		lost, delay := links[s.lane].Decide(s.at)
		if seg != nil {
			segLost, segDelay := seg.Decide(s.at)
			lost = lost || segLost
			delay += segDelay
		}
		if lost {
			continue
		}
		got[s.lane][s.seq] = true
		// The sender stamps whole milliseconds, as the engine does.
		sent := time.Duration(s.seq) * every
		arr = append(arr, arrival{s.lane, uint32(s.seq + 1), sent, s.at + delay})
	}
	sort.SliceStable(arr, func(a, b int) bool { return arr[a].at < arr[b].at })

	var tr Tracker
	next := 100 * time.Millisecond
	for _, a := range arr {
		for a.at >= next {
			tr.Report(next)
			next += 100 * time.Millisecond
		}
		tr.Add(a.lane, a.seq, uint32(a.sent/time.Millisecond), a.at)
	}
	return tr.Report(arr[len(arr)-1].at + 2*Hold), got
}

// truth counts what the Tracker should report for got: a lane carries the
// numbers from its first delivered copy to its last (nothing at the run's
// ends tells the relay it was sending before or after that).
func truth(got [][]bool) Stats {
	span := make([][2]int, len(got))
	for i, g := range got {
		span[i] = [2]int{-1, -1}
		for s, ok := range g {
			if ok {
				if span[i][0] < 0 {
					span[i][0] = s
				}
				span[i][1] = s
			}
		}
	}
	carries := func(i, s int) bool { return span[i][0] >= 0 && s >= span[i][0] && s <= span[i][1] }
	var st Stats
	for i := range got {
		st.Lanes = append(st.Lanes, Lane{Index: uint8(i)})
		for j := i + 1; j < len(got); j++ {
			st.Pairs = append(st.Pairs, Pair{A: uint8(i), B: uint8(j)})
		}
	}
	prev := make([]int, len(got)) // 0 none, 1 delivered, 2 lost
	for s := range got[0] {
		any, delivered := false, false
		for i := range got {
			l := &st.Lanes[i]
			if !carries(i, s) {
				prev[i] = 0
				continue
			}
			any = true
			delivered = delivered || got[i][s]
			l.Expected++
			lost := !got[i][s]
			if lost {
				l.Lost++
			}
			switch {
			case prev[i] == 1 && !lost:
				l.N00++
			case prev[i] == 1 && lost:
				l.N01++
			case prev[i] == 2 && !lost:
				l.N10++
			case prev[i] == 2 && lost:
				l.N11++
			}
			prev[i] = 1
			if lost {
				prev[i] = 2
			}
		}
		if any {
			st.Seqs++
			if !delivered {
				st.Lost++
			}
		}
		k := 0
		for i := range got {
			for j := i + 1; j < len(got); j++ {
				p := &st.Pairs[k]
				k++
				if !carries(i, s) || !carries(j, s) {
					continue
				}
				p.Both++
				if !got[i][s] {
					p.LostA++
				}
				if !got[j][s] {
					p.LostB++
				}
				if !got[i][s] && !got[j][s] {
					p.Joint++
				}
			}
		}
	}
	return st
}

func matchTruth(t *testing.T, s Stats, got [][]bool) {
	t.Helper()
	w := truth(got)
	if s.Seqs != w.Seqs || s.Lost != w.Lost {
		t.Errorf("seqs %d lost %d, want %d %d", s.Seqs, s.Lost, w.Seqs, w.Lost)
	}
	for _, wl := range w.Lanes {
		g := laneOf(t, s, int(wl.Index))
		g.ID, g.JitterUS = 0, 0
		if g != wl {
			t.Errorf("lane %d = %+v, want %+v", wl.Index, g, wl)
		}
	}
	for _, wp := range w.Pairs {
		if g, _ := s.Pair(int(wp.A), int(wp.B)); g != wp {
			t.Errorf("pair = %+v, want %+v", g, wp)
		}
	}
}

// markovSE is the standard error of the mean of n steps of a two-state
// chain with stationary share b and eigenvalue lambda = 1-p-r: the
// binomial error grown by (1+lambda)/(1-lambda) for the correlation.
func markovSE(b, lambda float64, n float64) float64 {
	return math.Sqrt(b * (1 - b) / n * (1 + lambda) / (1 - lambda))
}

func near(t *testing.T, what string, got, want, se float64) {
	t.Helper()
	if math.Abs(got-want) > 5*se {
		t.Errorf("%s = %.5f, want %.5f ± 5 × %.5f", what, got, want, se)
	} else {
		t.Logf("%s = %.5f (model %.5f, %.1f standard errors)", what, got, want, math.Abs(got-want)/se)
	}
}

// Two lanes with their own per-packet Gilbert bursts: each lane's loss,
// p01, p10 and burst length come out as the model's, and the lanes lose
// copies together only as often as independence predicts (Rho near 1).
func TestEstimatesOnIndependentGilbertLanes(t *testing.T) {
	const n = 200000
	ga, gb := netsim.Gilbert(0.03, 4), netsim.Gilbert(0.02, 2)
	s, got := simulate(n, 5*time.Millisecond, nil, []simLane{
		{link: netsim.Profile{Delay: 20 * ms1, Burst: ga, Seed: 11}},
		{link: netsim.Profile{Delay: 35 * ms1, Burst: gb, Seed: 12}},
	})
	matchTruth(t, s, got)
	for i, g := range []netsim.GilbertElliott{ga, gb} {
		l := laneOf(t, s, i)
		b, lambda := g.BadShare(), 1-g.P-g.R
		near(t, "loss", l.Loss(), g.MeanLoss(), markovSE(b, lambda, n))
		// p01 and p10 are binomial over the visits to each state.
		near(t, "p01", l.P01(), g.P, math.Sqrt(g.P*(1-g.P)/(n*(1-b))))
		near(t, "p10", l.P10(), g.R, math.Sqrt(g.R*(1-g.R)/(n*b)))
		if mb := l.MeanBurst(); math.Abs(mb-g.MeanLossRun()) > 0.1*g.MeanLossRun() {
			t.Errorf("lane %d mean burst %.2f, want %.2f", i, mb, g.MeanLossRun())
		}
	}
	p, _ := s.Pair(0, 1)
	// Joint losses come in clusters as long as the shorter burst, so the
	// error of p_AB is about that much wider than binomial.
	want := ga.MeanLoss() * gb.MeanLoss()
	near(t, "p_AB", p.PAB(), want, math.Sqrt(want/n*2*gb.MeanLossRun()))
	// Rho rests on about 120 joint losses here, so it spreads wide around
	// 1 (0.62 to 1.39 over 40 seeds), but far from the policy's 5.
	if rho, ok := p.Rho(); !ok || rho < 0.5 || rho > 1.6 {
		t.Errorf("rho %.2f (%v), want about 1 for independent lanes", rho, ok)
	}
	if phi, _ := p.Phi(); math.Abs(phi) > 0.02 {
		t.Errorf("phi %.3f, want about 0", phi)
	}
	near(t, "delivered loss", s.Loss(), want, math.Sqrt(want/n*2*gb.MeanLossRun()))
}

// Two lanes behind one bursty hop, as on home Wi-Fi (3% loss in 8 ms
// bursts, each lane with 0.2% of its own): copies sent at once die
// together, so Rho is far above the roadmap policy's 5 that calls for
// time-shifted copies; shifted by one mean burst, the joint loss falls as
// JointLoss predicts; on separate hops the lanes are independent.
func TestEstimatesOnSharedSegment(t *testing.T) {
	const n = 200000
	wifi := netsim.GilbertTime(0.03, 8*ms1, ms1)
	own := 0.002
	tests := []struct {
		name   string
		shift  time.Duration
		shared bool
		rhoMin float64
		rhoMax float64
	}{
		// Over 40 seeds Rho ranged 27 to 33, 9.7 to 11.3 and 0.68 to 1.23.
		{"simultaneous copies", 0, true, 25, 40},
		{"copies one burst apart", 8 * ms1, true, 8, 16},
		{"separate hops", 0, false, 0.5, 1.6},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lanes := []simLane{
				{link: netsim.Profile{Delay: 10 * ms1, Loss: own, Seed: 21}},
				{link: netsim.Profile{Delay: 18 * ms1, Loss: own, Seed: 22}, shift: tc.shift},
			}
			var seg *netsim.Sampler
			if tc.shared {
				seg = netsim.NewSampler(netsim.Profile{Burst: wifi, Seed: 23})
			} else {
				// Each lane on its own Wi-Fi: fold the burst into the link.
				lanes[0].link.Burst, lanes[1].link.Burst = wifi, wifi
			}
			s, got := simulate(n, ms1, seg, lanes)
			matchTruth(t, s, got)

			m := wifi.MeanLoss()
			lambda := 1 - wifi.P - wifi.R
			laneLoss := 1 - (1-m)*(1-own)
			// Both lost: 1 - P(A ok) - P(B ok) + P(both ok), the hop's part
			// of P(both ok) being 1 - 2m + JointLoss(shift in steps).
			joint := m * m
			if tc.shared {
				joint = wifi.JointLoss(int(tc.shift / wifi.Step))
			}
			wantAB := 1 - 2*(1-m)*(1-own) + (1-own)*(1-own)*(1-2*m+joint)
			for i := range 2 {
				near(t, "lane loss", laneOf(t, s, i).Loss(), laneLoss, markovSE(wifi.BadShare(), lambda, n))
			}
			p, _ := s.Pair(0, 1)
			near(t, "p_AB", p.PAB(), wantAB, markovSE(wantAB, lambda, n))
			rho, _ := p.Rho()
			if rho < tc.rhoMin || rho > tc.rhoMax {
				t.Errorf("rho %.2f, want %.1f..%.1f (model %.2f)", rho, tc.rhoMin, tc.rhoMax, wantAB/(laneLoss*laneLoss))
			}
			t.Logf("p_A %.4f p_B %.4f p_AB %.5f rho %.2f (model %.2f)", p.PA(), p.PB(), p.PAB(), rho, wantAB/(laneLoss*laneLoss))
		})
	}
}

// Normal delay jitter of standard deviation sigma: consecutive transit
// times differ by N(0, 2 sigma^2), whose mean absolute value is
// 2 sigma / sqrt(pi), which is what the RFC 3550 estimate averages to.
func TestJitterOnNormalDelays(t *testing.T) {
	const n = 20000
	sigma := 2 * ms1
	every := 20 * ms1 // far apart enough that copies don't overtake each other
	link := netsim.NewSampler(netsim.Profile{Delay: 30 * ms1, Jitter: sigma, Seed: 31})
	var tr Tracker
	var sum float64
	var count int
	for s := range n {
		sent := time.Duration(s) * every
		_, d := link.Decide(sent)
		tr.Add(0, uint32(s+1), uint32(sent/time.Millisecond), sent+d)
		if s >= 100 { // past the filter's warm-up from 0
			sum += float64(tr.lanes[0].jitter)
			count++
		}
	}
	mean := sum / float64(count) * float64(time.Microsecond)
	want := 2 * float64(sigma) / math.Sqrt(math.Pi)
	if math.Abs(mean-want) > 0.03*want {
		t.Fatalf("mean jitter %v, want %v ± 3%%", time.Duration(mean), time.Duration(want))
	}
	t.Logf("mean jitter %v (model %v)", time.Duration(mean), time.Duration(want))
	if j := laneOf(t, tr.Report(time.Duration(n)*every), 0).Jitter(); j < sigma/2 || j > 2*sigma {
		t.Fatalf("final jitter %v, want near %v", j, time.Duration(want))
	}
}
