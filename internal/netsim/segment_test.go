package netsim

import (
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// clockAt is a fake clock the test moves by hand.
type clockAt struct{ now time.Duration }

func (c *clockAt) read() time.Duration { return c.now }

// liveClock is a fake clock that timers may read while the test moves it.
type liveClock struct{ now atomic.Int64 }

func (c *liveClock) read() time.Duration  { return time.Duration(c.now.Load()) }
func (c *liveClock) set(at time.Duration) { c.now.Store(int64(at)) }

// testLink is a Link without sockets: send runs the real impairment path,
// on the test's clock.
func testLink(p Profile, clock func() time.Duration) *Link {
	l := &Link{clock: clock, imp: newImpairer(p.Seed)}
	l.profile.Store(&p)
	return l
}

func testSegment(p Profile, clock func() time.Duration) *Segment {
	s := NewSegment(p)
	s.clock = clock
	return s
}

// delivered sends one packet from the client over l and reports whether it
// got through (zero-delay profiles deliver synchronously).
func delivered(l *Link) bool {
	got := false
	l.send([]byte{1}, false, func([]byte) { got = true })
	return got
}

// TestSharedSegmentCorrelatesLanes puts two lanes behind one Wi-Fi-like
// segment (Gilbert-Elliott, 3% loss in 8 ms bursts, the roadmap's L2 case)
// and sends a copy on each, the second one shifted by 0 to 50 ms. Lanes
// behind one segment lose copies together; the same lanes on separate
// segments don't. The measured loss of both copies must match
// GilbertElliott.JointLoss.
func TestSharedSegmentCorrelatesLanes(t *testing.T) {
	wifi := Profile{Burst: GilbertTime(0.03, 8*time.Millisecond, time.Millisecond), Seed: 11}
	const pairs = 200_000
	const gap = 100 * time.Millisecond // between pairs: 100 steps, nearly independent
	type result struct{ lostA, lostB, both float64 }
	run := func(shift time.Duration, shared bool) result {
		c := &clockAt{}
		a, b := testLink(Profile{Seed: 21}, c.read), testLink(Profile{Seed: 22}, c.read)
		segA := testSegment(wifi, c.read)
		segB := segA
		if !shared {
			other := wifi
			other.Seed = 12
			segB = testSegment(other, c.read)
		}
		a.SetSegment(segA)
		b.SetSegment(segB)
		var r result
		for i := range pairs {
			c.now = time.Duration(i) * gap
			okA := delivered(a)
			c.now += shift
			okB := delivered(b)
			if !okA {
				r.lostA++
			}
			if !okB {
				r.lostB++
			}
			if !okA && !okB {
				r.both++
			}
		}
		return result{r.lostA / pairs, r.lostB / pairs, r.both / pairs}
	}

	g := wifi.Burst
	simultaneous := g.JointLoss(0)
	for _, tc := range []struct {
		name   string
		shift  time.Duration
		shared bool
		want   float64 // P(both copies lost)
	}{
		{"shared, same instant", 0, true, simultaneous},
		{"shared, 4 ms apart", 4 * time.Millisecond, true, g.JointLoss(4)},
		{"shared, 8 ms apart (one mean burst)", 8 * time.Millisecond, true, g.JointLoss(8)},
		{"shared, 20 ms apart", 20 * time.Millisecond, true, g.JointLoss(20)},
		{"shared, 50 ms apart", 50 * time.Millisecond, true, g.JointLoss(50)},
		{"separate segments, same instant", 0, false, g.MeanLoss() * g.MeanLoss()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run(tc.shift, tc.shared)
			// Pairs are 100 steps apart, so close to independent trials.
			bothTol := 5 * math.Sqrt(tc.want/pairs)
			laneTol := 5 * math.Sqrt(0.03*3/pairs) // bursts: about 3x binomial
			t.Logf("lane A %.4f, lane B %.4f, both %.5f (want %.5f ± %.5f, %.1fx better than simultaneous)",
				r.lostA, r.lostB, r.both, tc.want, bothTol, simultaneous/math.Max(r.both, 1e-9))
			if !near(r.lostA, 0.03, laneTol) || !near(r.lostB, 0.03, laneTol) {
				t.Errorf("each lane should still lose 3%%: A %.4f, B %.4f", r.lostA, r.lostB)
			}
			if !near(r.both, tc.want, bothTol) {
				t.Errorf("both copies lost %.5f, want %.5f ± %.5f", r.both, tc.want, bothTol)
			}
		})
	}

	// The L2 acceptance needs ≥ 5x less loss than simultaneous copies. A
	// shift of one mean burst (8 ms) gives under 3x; 13 ms is the shortest
	// that gets there on this segment (#196).
	for _, tc := range []struct {
		steps    int
		min, max float64
	}{{8, 2.8, 2.9}, {12, 4.6, 4.7}, {13, 5.2, 5.3}, {20, 10.9, 11}} {
		if r := simultaneous / g.JointLoss(tc.steps); r < tc.min || r > tc.max {
			t.Errorf("%d ms shift: %.2fx better than simultaneous copies, want %.1f to %.1f", tc.steps, r, tc.min, tc.max)
		}
	}
}

// TestSegmentAddsToLink: the segment's loss and delay come on top of the
// link's own, it can change at runtime for every link behind it, and a
// detached link no longer sees it.
func TestSegmentAddsToLink(t *testing.T) {
	c := &clockAt{}
	l := testLink(Profile{Delay: 20 * time.Millisecond}, c.read)
	seg := testSegment(Profile{Delay: 3 * time.Millisecond}, c.read)
	l.SetSegment(seg)
	if l.Segment() != seg {
		t.Fatal("Segment() doesn't return the attached segment")
	}
	if drop, d := l.imp.decide(l.profile.Load(), 0); drop || d != 20*time.Millisecond {
		t.Fatalf("link alone: drop %v delay %v", drop, d)
	}
	sent := make(chan time.Time, 1)
	start := time.Now()
	l.send([]byte{1}, false, func([]byte) { sent <- time.Now() })
	if got := (<-sent).Sub(start); got < 23*time.Millisecond {
		t.Errorf("delivered after %v, want at least link 20 ms + segment 3 ms", got)
	}

	seg.SetProfile(Profile{Loss: 1})
	for range 100 {
		if delivered(l) {
			t.Fatal("a fully lossy segment let a packet through")
		}
	}
	l.SetSegment(nil)
	l.SetProfile(Profile{})
	if !delivered(l) {
		t.Fatal("a detached link still sees the segment")
	}
	if seg.Profile().Loss != 1 {
		t.Error("Profile() doesn't return the segment's profile")
	}
}

// TestSegmentMeetsRepliesOnArrival: the segment is on the client's side,
// so a packet from the client meets it when sent and a reply only once the
// link's delay is over. The segment here loses everything in the first
// 10 ms of each hour; the link takes 200 ms.
func TestSegmentMeetsRepliesOnArrival(t *testing.T) {
	const hold = 200 * time.Millisecond
	fade := Profile{SpikeEvery: time.Hour, SpikeLen: 10 * time.Millisecond, SpikeLoss: 1}
	c := &liveClock{}
	l := testLink(Profile{Delay: hold}, c.read)
	l.SetSegment(testSegment(fade, c.read))
	got := func(reply bool, then time.Duration) bool {
		done := make(chan struct{}, 1)
		l.send([]byte{1}, reply, func([]byte) { done <- struct{}{} })
		c.set(then) // well before the link's 200 ms are over
		select {
		case <-done:
			return true
		case <-time.After(hold + 300*time.Millisecond):
			return false
		}
	}

	c.set(0)
	if got(false, 50*time.Millisecond) {
		t.Error("a packet from the client crossed the segment during its fade")
	}
	c.set(0)
	if !got(true, 50*time.Millisecond) {
		t.Error("a reply sent during the fade was lost, though it reached the segment after it")
	}
	c.set(50 * time.Millisecond)
	if got(true, time.Hour+5*time.Millisecond) {
		t.Error("a reply that reached the segment during the next fade got through")
	}
}

// TestImpairerSeedRepeats: the same Seed gives the same losses and jitter
// every run, a new Seed set at runtime starts over, and Seed 0 is random.
func TestImpairerSeedRepeats(t *testing.T) {
	p := Profile{Delay: 30 * time.Millisecond, Jitter: 4 * time.Millisecond, Loss: 0.05,
		Burst: Gilbert(0.05, 4), Seed: 99}
	trace := func(im *impairer, p Profile) []time.Duration {
		out := make([]time.Duration, 0, 5000)
		for i := range 5000 {
			drop, d := im.decide(&p, time.Duration(i)*time.Millisecond)
			if drop {
				d = -1
			}
			out = append(out, d)
		}
		return out
	}
	same := func(a, b []time.Duration) bool {
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	first := trace(newImpairer(99), p)
	if !same(first, trace(newImpairer(99), p)) {
		t.Fatal("the same seed gave different packets")
	}
	other := p
	other.Seed = 100
	if same(first, trace(newImpairer(100), other)) {
		t.Fatal("different seeds gave the same packets")
	}
	// A link made with one seed, then given another, replays the new one.
	im := newImpairer(100)
	trace(im, other)
	if !same(first, trace(im, p)) {
		t.Fatal("setting Seed 99 at runtime didn't start over")
	}
	// Seed 0 keeps the current stream instead of restarting it.
	zero := p
	zero.Seed = 0
	if same(first, trace(im, zero)) {
		t.Fatal("Seed 0 restarted the stream")
	}
	if a, b := trace(newImpairer(0), zero), trace(newImpairer(0), zero); same(a, b) {
		t.Fatal("two random seeds gave the same packets")
	}
}

// TestImpairerProfile: delay, jitter, plain loss and spikes behave as
// before, now on the link's clock.
func TestImpairerProfile(t *testing.T) {
	ms := time.Millisecond
	im := newImpairer(5)
	spiky := Profile{Delay: 40 * ms, SpikeEvery: 10 * time.Second, SpikeLen: 2 * time.Second, SpikeExtra: 70 * ms}
	for _, tc := range []struct {
		at   time.Duration
		want time.Duration
	}{
		{0, 110 * ms}, {1999 * ms, 110 * ms}, {2 * time.Second, 40 * ms}, {9 * time.Second, 40 * ms},
		{10 * time.Second, 110 * ms}, {33 * time.Second, 40 * ms},
	} {
		if drop, d := im.decide(&spiky, tc.at); drop || d != tc.want {
			t.Errorf("at %v: drop %v delay %v, want %v", tc.at, drop, d, tc.want)
		}
	}
	all := Profile{SpikeEvery: 10 * time.Second, SpikeLen: time.Second, SpikeLoss: 1}
	if drop, _ := im.decide(&all, 500*ms); !drop {
		t.Error("spike loss 1 let a packet through")
	}
	if drop, _ := im.decide(&all, 1500*ms); drop {
		t.Error("lost a packet outside the spike")
	}

	const n = 200_000
	jit := Profile{Delay: 30 * ms, Jitter: 5 * ms, Loss: 0.02}
	var lost int
	var sum, sq float64
	for i := range n {
		drop, d := im.decide(&jit, time.Duration(i)*ms)
		if drop {
			lost++
			continue
		}
		sum += float64(d)
		sq += float64(d) * float64(d)
	}
	kept := float64(n - lost)
	mean := sum / kept
	sd := math.Sqrt(sq/kept - mean*mean)
	if got := float64(lost) / n; !near(got, 0.02, 5*math.Sqrt(0.02*0.98/n)) {
		t.Errorf("loss %.4f, want 0.02", got)
	}
	if !near(mean, float64(30*ms), float64(100*time.Microsecond)) || !near(sd, float64(5*ms), float64(100*time.Microsecond)) {
		t.Errorf("delay mean %v sd %v, want 30ms and 5ms", time.Duration(mean), time.Duration(sd))
	}
}
