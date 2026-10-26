package netsim

import (
	"math"
	"testing"
	"time"
)

func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

func TestGilbertConstructors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		g        GilbertElliott
		loss     float64
		run      float64 // mean loss run, packets
		step     time.Duration
		badSteps float64
	}{
		{"3% in bursts of 8", Gilbert(0.03, 8), 0.03, 8, 0, 8},
		{"10% in pairs", Gilbert(0.10, 2), 0.10, 2, 0, 2},
		{"burst under one packet is one", Gilbert(0.05, 0.3), 0.05, 1, 0, 1},
		// One-packet bursts can't make more than half the packets lost.
		{"unreachable loss clamps", Gilbert(0.9, 1), 0.5, 1, 0, 1},
		{"roadmap Wi-Fi: 3%, 8 ms", GilbertTime(0.03, 8*time.Millisecond, 0), 0.03, 8, time.Millisecond, 8},
		{"2 ms steps", GilbertTime(0.05, 20*time.Millisecond, 2*time.Millisecond), 0.05, 10, 2 * time.Millisecond, 10},
	} {
		if got := tc.g.MeanLoss(); !near(got, tc.loss, 1e-12) {
			t.Errorf("%s: MeanLoss %v, want %v", tc.name, got, tc.loss)
		}
		if got := tc.g.MeanLossRun(); !near(got, tc.run, 1e-9) {
			t.Errorf("%s: MeanLossRun %v, want %v", tc.name, got, tc.run)
		}
		if got := tc.g.MeanBadSteps(); !near(got, tc.badSteps, 1e-9) {
			t.Errorf("%s: MeanBadSteps %v, want %v", tc.name, got, tc.badSteps)
		}
		if tc.g.Step != tc.step || tc.g.LossGood != 0 || tc.g.LossBad != 1 {
			t.Errorf("%s: %+v", tc.name, tc.g)
		}
	}

	if g := Gilbert(0, 8); g != (GilbertElliott{}) || g.active() {
		t.Errorf("no loss: %+v", g)
	}
	if g := GilbertTime(0, 8*time.Millisecond, 0); g != (GilbertElliott{}) {
		t.Errorf("no loss, time: %+v", g)
	}
	if g := Gilbert(1, 8); g.MeanLoss() != 1 || g.BadAfter(1000, true) != 1 {
		t.Errorf("total loss: %+v, mean %v", g, g.MeanLoss())
	}
}

func TestGilbertElliottClosedForms(t *testing.T) {
	g := GilbertElliott{P: 0.01, R: 0.2, LossGood: 0.002, LossBad: 0.6}
	b := 0.01 / 0.21
	if !near(g.BadShare(), b, 1e-15) {
		t.Errorf("BadShare %v, want %v", g.BadShare(), b)
	}
	if want := (1-b)*0.002 + b*0.6; !near(g.MeanLoss(), want, 1e-15) {
		t.Errorf("MeanLoss %v, want %v", g.MeanLoss(), want)
	}
	// One step is the transition matrix itself.
	if !near(g.BadAfter(1, true), 1-g.R, 1e-15) || !near(g.BadAfter(1, false), g.P, 1e-15) {
		t.Errorf("BadAfter(1) = %v / %v", g.BadAfter(1, true), g.BadAfter(1, false))
	}
	if g.BadAfter(0, true) != 1 || g.BadAfter(0, false) != 0 {
		t.Error("BadAfter(0) must be where it starts")
	}
	// Far apart, the chain forgets where it was.
	if !near(g.BadAfter(10000, true), b, 1e-12) || !near(g.BadAfter(10000, false), b, 1e-12) {
		t.Error("BadAfter(10000) is not the stationary share")
	}
	// Two steps by hand: A^2[B][B] = (1-r)^2 + r·p.
	if want := (1-g.R)*(1-g.R) + g.R*g.P; !near(g.BadAfter(2, true), want, 1e-15) {
		t.Errorf("BadAfter(2, bad) %v, want %v", g.BadAfter(2, true), want)
	}
	// p + r > 1 alternates; still exact.
	alt := GilbertElliott{P: 0.9, R: 0.8, LossBad: 1}
	if want := (1-alt.R)*(1-alt.R) + alt.R*alt.P; !near(alt.BadAfter(2, true), want, 1e-15) {
		t.Errorf("alternating BadAfter(2) %v, want %v", alt.BadAfter(2, true), want)
	}

	w := Gilbert(0.03, 8)
	if !near(w.JointLoss(0), 0.03, 1e-15) {
		t.Errorf("Gilbert copies in the same step die together: %v", w.JointLoss(0))
	}
	if !near(w.JointLoss(1), 0.03*(1-w.R), 1e-15) {
		t.Errorf("JointLoss(1) %v", w.JointLoss(1))
	}
	if !near(w.JointLoss(5000), 0.03*0.03, 1e-12) {
		t.Errorf("JointLoss far apart %v, want independent %v", w.JointLoss(5000), 0.03*0.03)
	}
	for n := 1; n < 60; n++ {
		if w.JointLoss(n) > w.JointLoss(n-1) {
			t.Fatalf("JointLoss grows from %d to %d steps", n-1, n)
		}
	}
	// Losses in Good alone are independent, whatever the chain does.
	flat := GilbertElliott{LossGood: 0.02}
	if !near(flat.JointLoss(0), 0.0004, 1e-15) || !near(flat.MeanLossRun(), 1/0.98, 1e-12) {
		t.Errorf("independent loss: joint %v, run %v", flat.JointLoss(0), flat.MeanLossRun())
	}
	if !math.IsInf((GilbertElliott{}).MeanLossRun(), 1) {
		t.Error("a model that never loses has no runs")
	}
}

// traceStats is a loss trace's mean loss and mean run of consecutive
// losses, each with its standard error from batch means: the trace is cut
// into batches long enough to be nearly independent, so the spread of their
// means measures the error whatever the model's correlation.
type traceStats struct {
	loss, lossErr float64
	run, runErr   float64
}

// lossTrace runs the model for n packets, one per step.
func lossTrace(g GilbertElliott, n int, seed uint64) traceStats {
	const batches = 20
	rng := newRand(seed)
	var c geChain
	var lost, runs [batches]float64
	prev := false
	for i := range n {
		b := i * batches / n
		d := c.drop(g, time.Duration(i)*g.Step, rng)
		if d {
			lost[b]++
			if !prev {
				runs[b]++
			}
		}
		prev = d
	}
	var st traceStats
	var totalLost, totalRuns float64
	for b := range batches {
		totalLost += lost[b]
		totalRuns += runs[b]
	}
	st.loss, st.run = totalLost/float64(n), totalLost/totalRuns
	for b := range batches {
		dl := lost[b]/(float64(n)/batches) - st.loss
		dr := lost[b]/runs[b] - st.run
		st.lossErr += dl * dl
		st.runErr += dr * dr
	}
	st.lossErr = math.Sqrt(st.lossErr / (batches - 1) / batches)
	st.runErr = math.Sqrt(st.runErr / (batches - 1) / batches)
	return st
}

// TestGilbertElliottStatistics checks the generator against the closed
// forms: mean loss and mean burst (loss run) length, per packet and time
// stepped, for Gilbert's model and for the general one. Seeds are fixed, so
// the result is the same every run; the tolerance is five standard errors,
// so a change in Go's random source doesn't break the test either.
func TestGilbertElliottStatistics(t *testing.T) {
	const n = 2_000_000
	for _, tc := range []struct {
		name string
		g    GilbertElliott
	}{
		{"Gilbert 3%, bursts of 8", Gilbert(0.03, 8)},
		{"Gilbert 10%, pairs", Gilbert(0.10, 2)},
		{"Gilbert 1%, long bursts", Gilbert(0.01, 25)},
		{"Gilbert-Elliott, lossy Good, leaky Bad", GilbertElliott{P: 0.01, R: 0.2, LossGood: 0.002, LossBad: 0.6}},
		{"netem-like p 5% r 50% 1-h 90%", GilbertElliott{P: 0.05, R: 0.5, LossBad: 0.9}},
		{"independent loss in Good only", GilbertElliott{LossGood: 0.02}},
		{"time stepped, one packet per step", GilbertTime(0.03, 8*time.Millisecond, time.Millisecond)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := lossTrace(tc.g, n, 1)
			wantLoss, wantRun := tc.g.MeanLoss(), tc.g.MeanLossRun()
			t.Logf("loss %.5f ± %.5f (want %.5f), run %.3f ± %.3f (want %.3f)", st.loss, st.lossErr, wantLoss, st.run, st.runErr, wantRun)
			if !near(st.loss, wantLoss, 5*st.lossErr) {
				t.Errorf("loss %.5f, want %.5f ± %.5f", st.loss, wantLoss, 5*st.lossErr)
			}
			if !near(st.run, wantRun, 5*st.runErr) {
				t.Errorf("mean burst %.3f packets, want %.3f ± %.3f", st.run, wantRun, 5*st.runErr)
			}
		})
	}
}

// TestGilbertTimeStepsWithTheClock: in time mode the chain moves with the
// clock, not with packets. Sparse packets still see the stationary loss,
// packets in one step share its state, and a clock running backwards (two
// senders racing) doesn't move it.
func TestGilbertTimeStepsWithTheClock(t *testing.T) {
	g := GilbertTime(0.03, 8*time.Millisecond, time.Millisecond)
	rng := newRand(7)
	var c geChain
	const n = 400_000
	lost := 0
	for i := range n {
		// 3.7 ms apart: several steps between packets, not a whole number.
		if c.drop(g, time.Duration(i)*3700*time.Microsecond, rng) {
			lost++
		}
	}
	// Packets 3.7 steps apart are nearly independent: binomial error.
	if got, tol := float64(lost)/n, 4*math.Sqrt(0.03*0.97*3/n); !near(got, 0.03, tol) {
		t.Errorf("sparse packets lose %.5f, want 0.03 ± %.5f", got, tol)
	}

	// Everything in one step shares the state: with Gilbert's model all
	// lost or all kept.
	for i := range 2000 {
		at := time.Duration(i) * 50 * time.Millisecond
		first := c.drop(g, at, rng)
		for k := range 5 {
			if c.drop(g, at+time.Duration(k)*100*time.Microsecond, rng) != first {
				t.Fatalf("step %v: packets in one step disagree", at)
			}
		}
		// A sender a little behind lands in the step already decided.
		if c.drop(g, at-2*time.Millisecond, rng) != first {
			t.Fatalf("step %v: a late sender moved the chain", at)
		}
	}
}

// TestGilbertTimeStepChange: a profile that changes the step size at
// runtime keeps the chain moving in the new steps instead of waiting for
// the clock to catch up with an index counted in the old ones.
func TestGilbertTimeStepChange(t *testing.T) {
	rng := newRand(3)
	var c geChain
	fine := GilbertTime(0.03, 8*time.Millisecond, time.Millisecond)
	now := time.Duration(0)
	for ; now < 10*time.Second; now += time.Millisecond {
		c.drop(fine, now, rng)
	}
	// Ten times coarser from t = 10 s: the old index (10000) is ten times
	// the new one (1000).
	coarse := GilbertTime(0.03, 80*time.Millisecond, 10*time.Millisecond)
	const n = 5000 // one packet per step, 50 s
	runs, prev := 0, false
	for range n {
		d := c.drop(coarse, now, rng)
		if d && !prev {
			runs++
		}
		prev = d
		now += 10 * time.Millisecond
	}
	// About n·p = 19 bursts expected; a held chain gives none (or one
	// that never ends).
	if runs < 5 {
		t.Errorf("after the step change %d loss bursts in %d steps, want about %.0f", runs, n, n*coarse.P)
	}
}
