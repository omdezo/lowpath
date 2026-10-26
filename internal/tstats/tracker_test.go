package tstats

import (
	"math"
	"testing"
	"time"
)

const ms1 = time.Millisecond

// cp is one copy: seq on lane at a time in ms (sender stamp = seq ms
// unless ts is set).
type cp struct {
	lane int
	seq  uint32
	at   int // ms
	ts   int // ms; 0 = seq
}

func feed(tr *Tracker, cps []cp) (late int) {
	for _, c := range cps {
		ts := uint32(c.seq)
		if c.ts != 0 {
			ts = uint32(c.ts)
		}
		if tr.Add(c.lane, c.seq, ts, time.Duration(c.at)*ms1) {
			late++
		}
	}
	return late
}

// run sends seq from..to on lane every 10 ms starting at start ms, except
// the numbers in skip.
func run(lane int, from, to uint32, start int, skip ...uint32) []cp {
	var out []cp
	for s := from; int32(to-s) >= 0; s++ {
		at := start + 10*int(s-from)
		missing := false
		for _, k := range skip {
			missing = missing || k == s
		}
		if !missing {
			out = append(out, cp{lane: lane, seq: s, at: at})
		}
	}
	return out
}

// merge orders copies by arrival.
func merge(runs ...[]cp) []cp {
	var out []cp
	for _, r := range runs {
		out = append(out, r...)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].at < out[j-1].at; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func laneOf(t *testing.T, s Stats, i int) Lane {
	t.Helper()
	l, ok := s.Lane(i)
	if !ok {
		t.Fatalf("no lane %d in %+v", i, s)
	}
	return l
}

func TestTrackerAccounting(t *testing.T) {
	type want struct {
		seqs, lost uint32
		lanes      []Lane // Index, Expected, Lost, Late and transitions are compared
		pair       *Pair
		late       int
	}
	tests := []struct {
		name string
		cps  []cp
		at   int // report time, ms
		want want
	}{
		{
			name: "two lanes, independent misses",
			cps:  merge(run(0, 1, 10, 0, 3, 4), run(1, 1, 10, 0, 4, 7)),
			at:   2000,
			want: want{seqs: 10, lost: 1,
				lanes: []Lane{
					{Index: 0, Expected: 10, Lost: 2, N00: 6, N01: 1, N11: 1, N10: 1},
					{Index: 1, Expected: 10, Lost: 2, N00: 5, N01: 2, N10: 2},
				},
				pair: &Pair{A: 0, B: 1, Both: 10, LostA: 2, LostB: 2, Joint: 1}},
		},
		{
			name: "a copy reordered within Settle is delivered",
			cps:  []cp{{0, 1, 0, 0}, {0, 2, 10, 0}, {0, 4, 30, 0}, {0, 3, 130, 0}, {0, 5, 140, 0}},
			at:   2000,
			want: want{seqs: 5, lanes: []Lane{{Index: 0, Expected: 5, N00: 4}}},
		},
		{
			name: "a copy after Settle is late and lost",
			cps:  merge([]cp{{0, 1, 0, 0}, {0, 2, 10, 0}, {0, 4, 30, 0}, {0, 3, 400, 0}}, run(1, 1, 4, 0)),
			at:   2000,
			want: want{seqs: 4, late: 1,
				lanes: []Lane{
					{Index: 0, Expected: 4, Lost: 1, Late: 1, N00: 1, N01: 1, N10: 1},
					{Index: 1, Expected: 4, N00: 3},
				},
				pair: &Pair{A: 0, B: 1, Both: 4, LostA: 1}},
		},
		{
			name: "a slower lane's copies from before the first copy count",
			cps:  merge(run(0, 1, 10, 0, 1, 2), run(1, 1, 10, 25)),
			at:   2000,
			want: want{seqs: 10,
				lanes: []Lane{{Index: 0, Expected: 8, N00: 7}, {Index: 1, Expected: 10, N00: 9}},
				pair:  &Pair{A: 0, B: 1, Both: 8}},
		},
		{
			name: "a lane that joins later isn't charged before its first copy",
			cps:  merge(run(0, 1, 10, 0), run(1, 6, 10, 50)),
			at:   2000,
			want: want{seqs: 10,
				lanes: []Lane{{Index: 0, Expected: 10, N00: 9}, {Index: 1, Expected: 5, N00: 4}},
				pair:  &Pair{A: 0, B: 1, Both: 5}},
		},
		{
			name: "a burst on a lane that comes back within Hold is loss",
			cps:  merge(run(0, 1, 20, 0), run(1, 1, 20, 0, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15)),
			at:   2000,
			want: want{seqs: 20,
				lanes: []Lane{{Index: 0, Expected: 20, N00: 19}, {Index: 1, Expected: 20, Lost: 10, N00: 8, N01: 1, N11: 9, N10: 1}},
				pair:  &Pair{A: 0, B: 1, Both: 20, LostB: 10}},
		},
		{
			name: "a lane the client stopped using isn't charged after Hold",
			cps:  merge(run(0, 1, 200, 0), run(1, 1, 10, 0)),
			at:   4000,
			want: want{seqs: 200,
				lanes: []Lane{{Index: 0, Expected: 200, N00: 199}, {Index: 1, Expected: 10, N00: 9}},
				pair:  &Pair{A: 0, B: 1, Both: 10}},
		},
		{
			name: "a lane back after Hold starts a new span",
			cps:  merge(run(0, 1, 300, 0), run(1, 1, 10, 0), run(1, 251, 300, 2500)),
			at:   6000,
			want: want{seqs: 300,
				lanes: []Lane{{Index: 0, Expected: 300, N00: 299}, {Index: 1, Expected: 60, N00: 58}},
				pair:  &Pair{A: 0, B: 1, Both: 60}},
		},
		{
			name: "numbering wraps around",
			cps:  run(0, math.MaxUint32-4, 5, 0, 0, 1),
			at:   2000,
			want: want{seqs: 11, lost: 2, lanes: []Lane{{Index: 0, Expected: 11, Lost: 2, N00: 7, N01: 1, N11: 1, N10: 1}}},
		},
		{
			name: "a sender that starts over is not a gap",
			cps:  append(run(0, 1000000, 1000009, 0), run(0, 1, 10, 200)...),
			at:   2000,
			want: want{seqs: 20, lanes: []Lane{{Index: 0, Expected: 20, N00: 18}}},
		},
		{
			name: "nothing settles before Settle",
			cps:  run(0, 1, 10, 0),
			at:   int(Settle/ms1) + 5,
			want: want{seqs: 1, lanes: []Lane{{Index: 0, Expected: 1}}},
		},
		{
			name: "a lane in a burst holds settling until Hold",
			cps:  merge(run(0, 1, 50, 0), run(1, 1, 10, 0)),
			at:   90 + int(Hold/ms1) - 1,
			want: want{seqs: 10,
				lanes: []Lane{{Index: 0, Expected: 10, N00: 9}, {Index: 1, Expected: 10, N00: 9}},
				pair:  &Pair{A: 0, B: 1, Both: 10}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tr Tracker
			late := feed(&tr, tc.cps)
			s := tr.Report(time.Duration(tc.at) * ms1)
			if s.Seqs != tc.want.seqs || s.Lost != tc.want.lost || late != tc.want.late {
				t.Errorf("seqs %d lost %d late %d, want %d %d %d", s.Seqs, s.Lost, late, tc.want.seqs, tc.want.lost, tc.want.late)
			}
			if len(s.Lanes) != len(tc.want.lanes) {
				t.Fatalf("lanes %+v, want %+v", s.Lanes, tc.want.lanes)
			}
			for _, w := range tc.want.lanes {
				g := laneOf(t, s, int(w.Index))
				g.ID, g.JitterUS = 0, 0
				if g != w {
					t.Errorf("lane %d = %+v, want %+v", w.Index, g, w)
				}
			}
			if tc.want.pair != nil {
				if p, ok := s.Pair(1, 0); !ok || p != *tc.want.pair {
					t.Errorf("pair = %+v, want %+v", p, *tc.want.pair)
				}
			}
		})
	}
}

func TestTrackerOverflowSettlesOldest(t *testing.T) {
	var tr Tracker
	for s := uint32(1); s <= 3000; s++ {
		tr.Add(0, s, s, 0) // all at once: nothing is Settle old yet
	}
	if s := tr.Report(0); s.Seqs != 3000-ringSize {
		t.Fatalf("settled %d at once, want the %d that overflowed the ring", s.Seqs, 3000-ringSize)
	}
	if s := tr.Report(time.Second); s.Seqs != 3000 || s.Lost != 0 {
		t.Fatalf("settled %d lost %d, want 3000 and 0", s.Seqs, s.Lost)
	}
}

func TestTrackerCloseFreesTheSlot(t *testing.T) {
	var tr Tracker
	feed(&tr, merge(run(0, 1, 10, 0), run(1, 1, 10, 0, 5)))
	s := tr.Report(2 * time.Second)
	old := laneOf(t, s, 1)
	if len(s.Pairs) != 1 || old.Lost != 1 {
		t.Fatalf("before close: %+v", s)
	}
	tr.Close(1)
	if s := tr.Report(2 * time.Second); len(s.Lanes) != 1 || len(s.Pairs) != 0 {
		t.Fatalf("after close: %+v, want lane 0 only", s)
	}
	feed(&tr, merge(run(0, 11, 20, 2000), run(1, 11, 20, 2000)))
	s2 := tr.Report(4 * time.Second)
	nl := laneOf(t, s2, 1)
	if nl.ID == old.ID || nl.Expected != 10 || nl.Lost != 0 {
		t.Fatalf("new lane in slot 1: %+v (old id %d)", nl, old.ID)
	}
	if p, _ := s2.Pair(0, 1); p.Both != 10 {
		t.Fatalf("new pair %+v, want 10 numbers in common", p)
	}
	// A window across the close starts the new lane from zero.
	w := s2.Since(s)
	if wl := laneOf(t, w, 1); wl.Expected != 10 {
		t.Fatalf("window lane 1 %+v, want the new lane's 10", wl)
	}
	if wl := laneOf(t, w, 0); wl.Expected != 10 || w.Seqs != 10 {
		t.Fatalf("window lane 0 %+v seqs %d, want 10", wl, w.Seqs)
	}
}

func TestTrackerIgnoresBadLane(t *testing.T) {
	var tr Tracker
	tr.Add(-1, 1, 1, 0)
	tr.Add(MaxLanes, 1, 1, 0)
	tr.Close(MaxLanes)
	if s := tr.Report(time.Second); s.Seqs != 0 || len(s.Lanes) != 0 {
		t.Fatalf("%+v", s)
	}
}

func TestJitterRFC3550(t *testing.T) {
	// Constant transit: no jitter, whatever the clocks' origins.
	var tr Tracker
	for i := range 100 {
		tr.Add(0, uint32(i+1), uint32(5000+20*i), time.Duration(20*i)*ms1+7*time.Microsecond)
	}
	if j := laneOf(t, tr.Report(0), 0).JitterUS; j != 0 {
		t.Fatalf("steady path jitter %d us, want 0", j)
	}
	// Transit alternating 0 and 2 ms: |D| = 2 ms every time, so after n
	// differences J = 2 ms · (1 - (15/16)^n).
	tr = Tracker{}
	const n = 40
	for i := range n + 1 {
		at := time.Duration(20*i) * ms1
		if i%2 == 1 {
			at += 2 * ms1
		}
		tr.Add(0, uint32(i+1), uint32(20*i), at)
	}
	want := 2000 * (1 - math.Pow(15.0/16, n))
	if j := float64(laneOf(t, tr.Report(0), 0).JitterUS); math.Abs(j-want) > 1 {
		t.Fatalf("jitter %.0f us, want %.0f", j, want)
	}
	// The sender's 32-bit millisecond clock may wrap.
	tr = Tracker{}
	tr.Add(0, 1, math.MaxUint32-9, 0)
	tr.Add(0, 2, 10, 20*ms1+16*ms1)
	if j := laneOf(t, tr.Report(0), 0).JitterUS; j != 1000 {
		t.Fatalf("jitter across the wrap %d us, want 16 ms / 16", j)
	}
}

func TestDerivedRates(t *testing.T) {
	l := Lane{Expected: 1000, Lost: 30, N00: 940, N01: 30, N10: 29, N11: 0}
	if l.Loss() != 0.03 || math.Abs(l.P01()-30.0/970) > 1e-12 || l.P10() != 1 || l.MeanBurst() != 1 {
		t.Fatalf("loss %v p01 %v p10 %v burst %v", l.Loss(), l.P01(), l.P10(), l.MeanBurst())
	}
	if (Lane{}).MeanBurst() != 0 || !math.IsInf(Lane{N11: 3}.MeanBurst(), 1) || (Lane{}).Loss() != 0 {
		t.Fatal("empty lane rates")
	}
	if (Lane{JitterUS: 1500}).Jitter() != 1500*time.Microsecond {
		t.Fatal("jitter duration")
	}
	tests := []struct {
		p        Pair
		rho, phi float64
		ok       bool
	}{
		{Pair{Both: 10000, LostA: 100, LostB: 100, Joint: 1}, 1, 0, true},     // independent: p_AB = p_A·p_B
		{Pair{Both: 10000, LostA: 100, LostB: 100, Joint: 100}, 100, 1, true}, // always together
		{Pair{Both: 10000, LostA: 100, LostB: 100, Joint: 5}, 5, 0.0404, true},
		{Pair{Both: 10000, LostA: 100}, 0, 0, false}, // B lost nothing
	}
	for _, tc := range tests {
		rho, ok := tc.p.Rho()
		phi, ok2 := tc.p.Phi()
		if ok != tc.ok || ok2 != tc.ok || math.Abs(rho-tc.rho) > 1e-9 || math.Abs(phi-tc.phi) > 1e-3 {
			t.Errorf("%+v: rho %v %v phi %v %v, want %v %v", tc.p, rho, ok, phi, ok2, tc.rho, tc.phi)
		}
	}
	p := Pair{Both: 200, LostA: 10, LostB: 4, Joint: 2}
	if p.PA() != 0.05 || p.PB() != 0.02 || p.PAB() != 0.01 {
		t.Fatalf("%v %v %v", p.PA(), p.PB(), p.PAB())
	}
	if (Stats{Seqs: 400, Lost: 2}).Loss() != 0.005 || (Stats{}).Loss() != 0 {
		t.Fatal("stats loss")
	}
}

func TestWireRoundTrip(t *testing.T) {
	var tr Tracker
	tr.Dir = Down
	feed(&tr, merge(run(0, 1, 30, 0, 4), run(3, 1, 30, 3, 4, 9), run(7, 5, 30, 1)))
	s := tr.Report(2 * time.Second)
	if len(s.Lanes) != 3 || len(s.Pairs) != 3 {
		t.Fatalf("%+v", s)
	}
	b := s.Append([]byte{0xEE}, 3)[1:]
	if len(b) != headLen+3*laneLen+3*pairLen {
		t.Fatalf("encoded %d bytes", len(b))
	}
	got, self, err := Parse(append(b, 1, 2, 3)) // a later version's extra fields are skipped
	if err != nil || self != 3 || got.Dir != Down || got.Seqs != s.Seqs || got.Lost != s.Lost {
		t.Fatalf("parse: %+v self %d %v", got, self, err)
	}
	for i := range s.Lanes {
		if got.Lanes[i] != s.Lanes[i] {
			t.Errorf("lane %d: %+v, want %+v", i, got.Lanes[i], s.Lanes[i])
		}
	}
	for i := range s.Pairs {
		if got.Pairs[i] != s.Pairs[i] {
			t.Errorf("pair %d: %+v, want %+v", i, got.Pairs[i], s.Pairs[i])
		}
	}
	if _, self, err := Parse(s.Append(nil, -1)); err != nil || self != NoLane {
		t.Fatalf("no self: %d %v", self, err)
	}

	bad := map[string][]byte{
		"short head":    b[:headLen-1],
		"short blocks":  b[:len(b)-1],
		"version":       append([]byte{2}, b[1:]...),
		"self":          append([]byte{1, 9}, b[2:]...),
		"lane count":    append([]byte{1, 0, 0, MaxLanes + 1}, b[4:]...),
		"lane index":    func() []byte { c := append([]byte(nil), b...); c[headLen] = MaxLanes; return c }(),
		"pair order":    func() []byte { c := append([]byte(nil), b...); o := headLen + 3*laneLen; c[o], c[o+1] = 3, 0; return c }(),
		"pair index":    func() []byte { c := append([]byte(nil), b...); c[headLen+3*laneLen+1] = MaxLanes; return c }(),
		"empty payload": nil,
	}
	for name, p := range bad {
		if _, _, err := Parse(p); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestPairIndexCoversEveryPair(t *testing.T) {
	seen := map[int]bool{}
	for a := range MaxLanes {
		for b := a + 1; b < MaxLanes; b++ {
			i := pairIndex(a, b)
			if i < 0 || i >= npairs || seen[i] {
				t.Fatalf("pair (%d,%d) -> %d", a, b, i)
			}
			seen[i] = true
		}
	}
}
