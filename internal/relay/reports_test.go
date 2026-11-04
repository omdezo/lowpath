package relay

import (
	"testing"
	"time"
)

// deliver feeds r one interval of upstream copies: n numbers, each sent
// on every lane in order, the first lane in order winning the race.
func deliver(r *reportScheduler, n int, order ...int) {
	for range n {
		for i, l := range order {
			r.delivered(l, i == 0)
		}
	}
}

// planMap runs one plan and returns what each live lane got.
func planMap(t *testing.T, r *reportScheduler, now time.Time, live ...int) map[int]reportKind {
	t.Helper()
	kinds := r.plan(now, live, nil)
	if len(kinds) != len(live) {
		t.Fatalf("plan returned %d kinds for %d lanes", len(kinds), len(live))
	}
	got := map[int]reportKind{}
	full := 0
	for i, l := range live {
		got[l] = kinds[i]
		if kinds[i] == reportFull {
			full++
		}
	}
	if len(live) > 0 && full != 1 {
		t.Fatalf("%d full reports among lanes %v: %v", full, live, got)
	}
	return got
}

func fullLane(got map[int]reportKind) int {
	for l, k := range got {
		if k == reportFull {
			return l
		}
	}
	return -1
}

// Four steady lanes: the one winning the races carries the full report
// every interval; the others get a short one when they are new and every
// selfEvery after that, nothing in between.
func TestSchedulerSteadyLanes(t *testing.T) {
	var r reportScheduler
	t0 := time.Unix(1_700_000_000, 0)
	last := map[int]time.Time{}
	for sec := range 60 {
		now := t0.Add(time.Duration(sec) * time.Second)
		deliver(&r, 64, 2, 0, 3, 1)
		got := planMap(t, &r, now, 0, 1, 2, 3)
		if fullLane(got) != 2 {
			t.Fatalf("second %d: full report on lane %d, want the fastest lane 2", sec, fullLane(got))
		}
		for l, k := range got {
			if k == reportNone {
				if now.Sub(last[l]) >= selfEvery {
					t.Fatalf("second %d: lane %d got nothing %v after its last report", sec, l, now.Sub(last[l]))
				}
				continue
			}
			if k == reportShort && sec > 0 && now.Sub(last[l]) != selfEvery {
				t.Fatalf("second %d: lane %d got a short report %v after its last", sec, l, now.Sub(last[l]))
			}
			last[l] = now
		}
	}
}

// A lane that appears gets its self at the next report, and a lane that
// carries the full report and then leaves the live set hands it to the
// lane winning the most races at once.
func TestSchedulerLanesComeAndGo(t *testing.T) {
	var r reportScheduler
	t0 := time.Unix(1_700_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	deliver(&r, 50, 0)
	if got := planMap(t, &r, at(0), 0); got[0] != reportFull {
		t.Fatalf("a lone lane got %v", got)
	}
	deliver(&r, 50, 0, 1)
	if got := planMap(t, &r, at(1), 0, 1); got[0] != reportFull || got[1] != reportShort {
		t.Fatalf("new lane 1: %v", got)
	}
	deliver(&r, 50, 0, 1)
	if got := planMap(t, &r, at(2), 0, 1); got[1] != reportNone {
		t.Fatalf("lane 1 a second after its self: %v", got)
	}
	// Lane 0 stops being a live return address; lanes 1 and 5 carry on, 5
	// winning more races.
	deliver(&r, 20, 1, 5)
	deliver(&r, 30, 5, 1)
	got := planMap(t, &r, at(3), 1, 5)
	if got[5] != reportFull || got[1] != reportNone {
		t.Fatalf("after the full lane left: %v", got)
	}

	// The full lane's slot is freed and taken by a new path: it doesn't
	// inherit the full report, and gets its self at once.
	r.forget(5)
	deliver(&r, 50, 1, 5)
	got = planMap(t, &r, at(4), 1, 5)
	if got[1] != reportFull || got[5] != reportShort {
		t.Fatalf("slot 5 reused: %v", got)
	}
	// A short lane whose slot is reused is new too: no waiting selfEvery.
	r.forget(5)
	deliver(&r, 50, 1, 5)
	if got := planMap(t, &r, at(5), 1, 5); got[5] != reportShort {
		t.Fatalf("slot 5 reused again a second later: %v", got)
	}
	// No live lanes: nothing to plan, and no full lane is kept for later.
	if got := planMap(t, &r, at(6)); len(got) != 0 {
		t.Fatalf("no lanes: %v", got)
	}
}

// A full lane that stops delivering upstream while another lane carries
// on loses the full report at the next plan, even while it is still a live
// return address; when every lane is quiet it keeps it.
func TestSchedulerQuietFullLane(t *testing.T) {
	var r reportScheduler
	t0 := time.Unix(1_700_000_000, 0)
	deliver(&r, 50, 0, 1)
	if got := planMap(t, &r, t0, 0, 1); got[0] != reportFull {
		t.Fatalf("%v", got)
	}
	if got := planMap(t, &r, t0.Add(time.Second), 0, 1); got[0] != reportFull {
		t.Fatalf("every lane quiet: %v", got)
	}
	deliver(&r, 50, 1)
	if got := planMap(t, &r, t0.Add(2*time.Second), 0, 1); got[1] != reportFull {
		t.Fatalf("lane 0 quiet, lane 1 delivering: %v", got)
	}
}

// The first lane to show up (say a tethered phone, slower than the home
// line) gets the full report first; a lane that clearly outruns it takes
// it over after takeoverRuns intervals. Lanes about as fast as each other
// don't trade it back and forth.
func TestSchedulerTakeover(t *testing.T) {
	var r reportScheduler
	t0 := time.Unix(1_700_000_000, 0)
	deliver(&r, 50, 0)
	planMap(t, &r, t0, 0)
	for i := 1; i <= takeoverRuns; i++ {
		deliver(&r, 50, 1, 0)
		got := planMap(t, &r, t0.Add(time.Duration(i)*time.Second), 0, 1)
		want := 0
		if i == takeoverRuns {
			want = 1
		}
		if fullLane(got) != want {
			t.Fatalf("interval %d: full on lane %d, want %d", i, fullLane(got), want)
		}
	}

	// Two lanes splitting the races 30/20 either way: the holder stays.
	var even reportScheduler
	moves, prev := 0, -1
	for sec := range 60 {
		a, b := 2, 3
		if sec%3 == 1 {
			a, b = b, a
		}
		deliver(&even, 30, a, b)
		deliver(&even, 20, b, a)
		got := planMap(t, &even, t0.Add(time.Duration(sec)*time.Second), 2, 3)
		if l := fullLane(got); prev >= 0 && l != prev {
			moves++
		}
		prev = fullLane(got)
	}
	if moves != 0 {
		t.Fatalf("full report moved %d times between equally fast lanes", moves)
	}
}
