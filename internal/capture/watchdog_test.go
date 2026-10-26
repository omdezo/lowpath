package capture

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestStallWatch(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ws := []*workerState{{}, {}}
	w := newStallWatch(500*time.Millisecond, ws, t0)
	at := func(ms int) time.Time { return t0.Add(time.Duration(ms) * time.Millisecond) }

	// Idle workers (even counters) are never stuck, however long they wait.
	for ms := 100; ms <= 2000; ms += 100 {
		if d, stuck := w.check(at(ms)); stuck || d != 0 {
			t.Fatalf("idle at %d ms: %v %v", ms, d, stuck)
		}
	}
	// Worker 0 takes a packet and keeps it.
	ws[0].enter()
	if _, stuck := w.check(at(2100)); stuck {
		t.Fatal("stuck the moment it started")
	}
	for ms := 2200; ms < 2600; ms += 100 {
		if d, stuck := w.check(at(ms)); stuck {
			t.Fatalf("stuck after %v, under the limit", d)
		}
	}
	if d, stuck := w.check(at(2600)); !stuck || d != 500*time.Millisecond {
		t.Fatalf("at the limit: %v %v", d, stuck)
	}

	// A busy worker that keeps moving (new packet each look) is fine.
	ws[0].leave()
	for ms := 2700; ms < 4000; ms += 100 {
		ws[1].enter()
		ws[1].leave()
		ws[1].enter() // in the middle of the next packet at every look
		if d, stuck := w.check(at(ms)); stuck {
			t.Fatalf("moving worker called stuck after %v", d)
		}
		ws[1].leave()
	}
}

// A look that comes far too late means the process or PC was paused (sleep,
// hibernation): that pause must not count against the packet in hand.
func TestStallWatchIgnoresPauses(t *testing.T) {
	t0 := time.Unix(1000, 0)
	ws := []*workerState{{}}
	w := newStallWatch(500*time.Millisecond, ws, t0)
	ws[0].enter()
	w.check(t0.Add(100 * time.Millisecond))
	if d, stuck := w.check(t0.Add(30 * time.Minute)); stuck {
		t.Fatalf("a 30 min sleep counted as a %v stall", d)
	}
	// After the pause the clock runs again from the resume.
	resume := t0.Add(30 * time.Minute)
	for ms := 100; ms < 500; ms += 100 {
		w.check(resume.Add(time.Duration(ms) * time.Millisecond))
	}
	if _, stuck := w.check(resume.Add(500 * time.Millisecond)); !stuck {
		t.Fatal("a packet still stuck 500 ms after the resume was not caught")
	}
}

func TestWatchInterval(t *testing.T) {
	if got := watchInterval(DefaultStallLimit); got != watchEvery {
		t.Errorf("default: every %v", got)
	}
	if got := watchInterval(50 * time.Millisecond); got != 10*time.Millisecond {
		t.Errorf("50 ms limit: every %v", got)
	}
	if got := watchInterval(time.Nanosecond); got != time.Millisecond {
		t.Errorf("tiny limit: every %v", got)
	}
}

// waitDone waits for a capture to stop on its own and returns how long
// that took.
func waitDone(t *testing.T, c *Capture, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	select {
	case <-c.Done():
		return time.Since(start)
	case <-time.After(within):
		t.Fatalf("capture still running after %v", within)
		return 0
	}
}

// The acceptance case for C5 inside the process: the engine deadlocks in
// Send, and within a second the handle is closed (the driver then sends
// queued and new packets directly).
func TestStuckHandlerFailsOpen(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{}, 8)
	h.c.cfg.OnPacket = func(_, _ netip.AddrPort, _ []byte) bool {
		entered <- struct{}{}
		<-release // a deadlocked engine
		return true
	}
	h.c.run(2)
	h.dev.in <- recvd{udp(t, gameLocal, gameServer, "game"), outboundOn(7, 3)}
	<-entered
	took := waitDone(t, h.c, 3*time.Second)
	if !h.dev.closed.Load() {
		t.Fatal("handle still open")
	}
	limit := DefaultStallLimit + watchEvery + abortWait
	if took > limit+300*time.Millisecond { // slack for a loaded CI machine
		t.Errorf("failed open after %v, want about %v", took, limit)
	}
	err := h.c.Err()
	if err == nil || !strings.Contains(err.Error(), "stuck in the handler") {
		t.Fatalf("Err = %v", err)
	}
	if s := h.c.Stats(); !strings.Contains(s.LastError, "stuck") {
		t.Errorf("stats last error %q", s.LastError)
	}
	// Close after failing open is harmless and keeps the reason.
	if err := h.c.Close(); err != nil {
		t.Fatal(err)
	}
	if h.c.Err() == nil {
		t.Fatal("Close cleared the reason")
	}
	if err := h.c.Inject(gameLocal, gameServer, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Inject after failing open: %v", err)
	}
}

func TestStallLimitConfigurable(t *testing.T) {
	h := newHarness(t)
	h.c.cfg.StallLimit = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	h.c.cfg.OnPacket = func(_, _ netip.AddrPort, _ []byte) bool { <-release; return true }
	h.c.run(1)
	h.dev.in <- recvd{udp(t, gameLocal, gameServer, "game"), outboundOn(7, 3)}
	if took := waitDone(t, h.c, 2*time.Second); took > 50*time.Millisecond+abortWait+400*time.Millisecond {
		t.Errorf("50 ms limit: stopped after %v", took)
	}
}

// A slow but moving packet path is not a stall.
func TestSlowHandlerKeepsCapturing(t *testing.T) {
	h := newHarness(t)
	h.c.cfg.StallLimit = 200 * time.Millisecond
	h.c.cfg.OnPacket = func(_, _ netip.AddrPort, _ []byte) bool { time.Sleep(20 * time.Millisecond); return true }
	h.c.run(1)
	for range 30 {
		h.dev.in <- recvd{udp(t, gameLocal, gameServer, "game"), outboundOn(7, 3)}
	}
	waitFor(t, func() bool { return h.c.Stats().Captured == 30 })
	select {
	case <-h.c.Done():
		t.Fatalf("stopped: %v", h.c.Err())
	default:
	}
	h.c.Close()
	if h.c.Err() != nil {
		t.Fatalf("Err after a plain Close: %v", h.c.Err())
	}
}

// A panic in the engine must not take the app down with the capture still
// open: it is recovered, capture stops, and the reason names the crash.
func TestPanickingHandlerFailsOpen(t *testing.T) {
	h := newHarness(t)
	h.c.cfg.OnPacket = func(_, _ netip.AddrPort, _ []byte) bool { panic("engine bug") }
	h.c.run(2)
	h.dev.in <- recvd{udp(t, gameLocal, gameServer, "game"), outboundOn(7, 3)}
	waitDone(t, h.c, 2*time.Second)
	if !h.dev.closed.Load() {
		t.Fatal("handle still open")
	}
	if err := h.c.Err(); err == nil || !strings.Contains(err.Error(), "crashed") || !strings.Contains(err.Error(), "engine bug") {
		t.Fatalf("Err = %v", err)
	}
}

func TestCloseIsNotAFailure(t *testing.T) {
	h := newHarness(t)
	h.c.run(2)
	select {
	case <-h.c.Done():
		t.Fatal("done before Close")
	default:
	}
	h.c.Close()
	select {
	case <-h.c.Done():
	default:
		t.Fatal("Done not closed by Close")
	}
	if h.c.Err() != nil {
		t.Fatalf("Err after Close: %v", h.c.Err())
	}
}
