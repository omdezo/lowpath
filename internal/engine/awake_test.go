package engine

import (
	"testing"
	"time"
)

// beats runs c's heartbeat from `from` up to before `to` (after base).
func beats(c *awakeClock, base time.Time, from, to time.Duration) {
	for at := from; at < to; at += heartbeatEvery {
		c.beat(base.Add(at))
	}
}

// A heartbeat gap of stallMin or more is a stall, plus the settle time for
// the readers; shorter gaps are scheduling noise. Ages leave stalls out.
func TestAwakeClockStalls(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	at := func(d time.Duration) time.Time { return base.Add(d) }
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }

	c := &awakeClock{}
	beats(c, base, 0, ms(1000)) // last beat at 950 ms
	c.beat(at(ms(1350)))        // 400 ms late: a busy PC, no stall
	c.beat(at(ms(2650)))        // stalled 1.3 s: [1350, 2650+settle)
	beats(c, base, ms(2700), ms(3000))
	c.beat(at(ms(2000))) // a stale time changes nothing

	for _, tc := range []struct {
		name      string
		from, now time.Duration
		want      time.Duration
	}{
		{"before the stall", ms(100), ms(1100), ms(1000)},
		{"across the stall", ms(1000), ms(3000), ms(2000) - (ms(1300) + stallSettle)},
		{"sent during it", ms(2000), ms(2900), ms(900) - (ms(650) + stallSettle)},
		{"on resume", ms(1050), ms(2660), ms(300)},
		{"after the settle", ms(2900), ms(3000), ms(100)},
		{"now before t", ms(500), ms(400), -ms(100)},
	} {
		if got := c.age(at(tc.from), at(tc.now)); got != tc.want {
			t.Errorf("%s: age = %v, want %v", tc.name, got, tc.want)
		}
	}

	var wall *awakeClock
	wall.beat(at(0))
	if got := wall.age(at(0), at(ms(5000))); got != ms(5000) {
		t.Errorf("nil clock: age = %v, want wall time", got)
	}
}

// Stalls during the previous one's settle merge; old stalls are forgotten.
func TestAwakeClockMergesAndForgets(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	c := &awakeClock{}
	c.beat(base)
	c.beat(base.Add(time.Second))                        // [0, 1 s + settle)
	c.beat(base.Add(time.Second + 100*time.Millisecond)) // settling
	c.beat(base.Add(1600 * time.Millisecond))            // [1.1 s, 1.6 s + settle): overlaps
	if len(c.stalls) != 1 || !c.stalls[0].from.Equal(base) || !c.stalls[0].to.Equal(base.Add(1600*time.Millisecond+stallSettle)) {
		t.Fatalf("stalls = %+v, want one merged stall", c.stalls)
	}
	later := base.Add(stallKeep + 5*time.Second)
	beats(c, base, 1650*time.Millisecond, later.Add(-time.Second).Sub(base))
	c.beat(later) // a new stall, and the first is past stallKeep
	if len(c.stalls) != 1 || !c.stalls[0].to.Equal(later.Add(stallSettle)) {
		t.Fatalf("stalls = %+v, want only the new one", c.stalls)
	}
}

// A machine that never stops stalling still lets probes time out: past
// maxStallCredit, wall time counts again (fail-open).
func TestAwakeClockCreditIsBounded(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	c := &awakeClock{}
	for at := time.Duration(0); at <= 30*time.Second; at += 600 * time.Millisecond {
		c.beat(base.Add(at)) // every gap is a stall, and they all merge
	}
	now := base.Add(30 * time.Second)
	if got, want := c.age(base, now), 30*time.Second-maxStallCredit; got != want {
		t.Errorf("age = %v, want %v", got, want)
	}
}

// A busy PC whose heartbeat is often a few hundred milliseconds late is not
// stalled: ages stay wall time, so dead routes fall back as fast as ever.
func TestAwakeClockBusyPCIsNotStalled(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	c := &awakeClock{}
	for at := time.Duration(0); at <= 20*time.Second; at += 450 * time.Millisecond {
		c.beat(base.Add(at))
	}
	if len(c.stalls) != 0 {
		t.Errorf("stalls = %+v, want none", c.stalls)
	}
	if got := c.age(base, base.Add(20*time.Second)); got != 20*time.Second {
		t.Errorf("age = %v, want wall time", got)
	}
}

// A reply to a probe sent after a stall and read within its settle time is
// a real round trip; one whose round trip spans the stall is not.
func TestTrackerRTTAroundAStall(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	c := &awakeClock{}
	beats(c, base, 0, time.Second) // last beat at 950 ms
	resume := base.Add(2500 * time.Millisecond)
	c.beat(resume) // stalled [950 ms, 2.5 s + settle)
	tr := tracker{awake: c}
	queued := tr.send(base.Add(900 * time.Millisecond))
	tr.recv(queued, resume.Add(time.Millisecond)) // drained on resume: 1.6 s "round trip"
	fresh := tr.send(resume.Add(50 * time.Millisecond))
	tr.recv(fresh, resume.Add(70*time.Millisecond)) // 20 ms, inside the settle time
	s := tr.stats(resume.Add(1200 * time.Millisecond))
	if s.Loss != 0 || !s.Up || s.Samples != 1 || s.RTT != 20 {
		t.Errorf("stats = %+v, want one 20 ms sample and no loss", s)
	}
}
