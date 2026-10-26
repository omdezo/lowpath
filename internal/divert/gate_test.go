package divert

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallGate(t *testing.T) {
	for _, c := range []struct {
		name       string
		before     int // calls that enter before shut
		exitBefore int // of those, how many exit before shut
		inFlight   int64
	}{
		{"idle", 0, 0, 0},
		{"all returned", 3, 3, 0},
		{"one running", 1, 0, 1},
		{"two of three running", 3, 1, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := newCallGate()
			for range c.before {
				if !g.enter() {
					t.Fatal("enter failed on an open gate")
				}
			}
			for range c.exitBefore {
				g.exit()
			}
			n, first := g.shut()
			if n != c.inFlight || !first {
				t.Fatalf("shut = %d, %v; want %d, true", n, first, c.inFlight)
			}
			if g.enter() {
				t.Fatal("enter succeeded after shut")
			}
			if _, first := g.shut(); first {
				t.Fatal("second shut reported first")
			}
			if left := g.wait(time.Millisecond); left != c.inFlight {
				t.Fatalf("wait with calls running = %d, want %d", left, c.inFlight)
			}
			for range c.before - c.exitBefore {
				g.exit()
			}
			if left := g.wait(time.Hour); left != 0 {
				t.Fatalf("wait after the last exit = %d, want 0", left)
			}
		})
	}
}

// wait returns as soon as the last call leaves, not when the bound runs out.
func TestCallGateWaitWakesOnLastExit(t *testing.T) {
	g := newCallGate()
	g.enter()
	g.enter()
	g.shut()
	go func() { g.exit(); g.exit() }()
	start := time.Now()
	if left := g.wait(time.Hour); left != 0 {
		t.Fatalf("wait = %d, want 0", left)
	}
	if d := time.Since(start); d > time.Minute {
		t.Fatalf("wait took %v", d)
	}
}

// Many callers race one closer: no call may still be between enter and exit
// once the closer has been told nothing is left, which is when Close
// releases the handle.
func TestCallGateNoCallAfterRelease(t *testing.T) {
	for round := range 200 {
		g := newCallGate()
		var released atomic.Bool
		var late, calls atomic.Int64
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for g.enter() {
					calls.Add(1)
					if released.Load() {
						late.Add(1)
					}
					g.exit()
				}
			}()
		}
		for calls.Load() < 50 {
			time.Sleep(10 * time.Microsecond)
		}
		g.shut()
		if left := g.wait(time.Hour); left != 0 {
			t.Fatalf("round %d: wait = %d", round, left)
		}
		released.Store(true)
		wg.Wait()
		if n := late.Load(); n != 0 {
			t.Fatalf("round %d: %d calls ran after the release", round, n)
		}
	}
}
