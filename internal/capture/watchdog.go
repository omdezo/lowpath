package capture

import (
	"sync/atomic"
	"time"
)

const (
	// DefaultStallLimit is how long one packet may sit in the handler
	// (owner lookup plus OnPacket, normally microseconds) before the
	// watchdog decides the packet path is stuck and stops capturing, so the
	// game's traffic goes out directly instead of waiting in the queue.
	DefaultStallLimit = 500 * time.Millisecond
	// watchEvery is how often the watchdog looks at the workers.
	watchEvery = 100 * time.Millisecond
	// abortWait bounds how long a watchdog stop waits for the workers before
	// it closes the handle anyway: a stuck worker never returns.
	abortWait = 200 * time.Millisecond
)

// workerState is one receive worker's progress counter: it is odd while the
// worker handles a packet and even while it waits for the next one, and it
// changes with every packet. Two atomic adds per packet and no clock, so the
// hot path stays allocation-free and cheap.
type workerState struct{ n atomic.Uint64 }

func (w *workerState) enter() { w.n.Add(1) }
func (w *workerState) leave() { w.n.Add(1) }

// watchInterval is how often to look for a stall limit: often enough to
// catch it a fraction of the limit late.
func watchInterval(limit time.Duration) time.Duration {
	return min(watchEvery, max(limit/5, time.Millisecond))
}

// stallWatch decides from periodic looks at the workers' counters whether
// one of them has been inside the same packet for too long. It keeps its
// own clock (the times passed to check), so it is tested without sleeping.
type stallWatch struct {
	limit   time.Duration
	every   time.Duration // planned time between looks
	workers []*workerState
	seen    []uint64    // counter at the last look
	since   []time.Time // when the counter was first seen at that value
	last    time.Time   // previous look
}

func newStallWatch(limit time.Duration, workers []*workerState, now time.Time) *stallWatch {
	if limit <= 0 {
		limit = DefaultStallLimit
	}
	w := &stallWatch{
		limit: limit, every: watchInterval(limit), workers: workers, last: now,
		seen: make([]uint64, len(workers)), since: make([]time.Time, len(workers)),
	}
	for i, ws := range workers {
		w.seen[i], w.since[i] = ws.n.Load(), now
	}
	return w
}

// check looks at the workers at time now and reports how long the longest
// stuck one has held its packet, and whether that is over the limit.
//
// A look that comes much later than planned means this process (or the
// whole PC: sleep, hibernation, a debugger) was paused; the packets didn't
// get a chance to move, so the clock restarts instead of blaming the
// handler for the pause.
func (w *stallWatch) check(now time.Time) (time.Duration, bool) {
	paused := now.Sub(w.last) > 3*w.every
	w.last = now
	var worst time.Duration
	for i, ws := range w.workers {
		n := ws.n.Load()
		if n != w.seen[i] || n%2 == 0 || paused {
			w.seen[i], w.since[i] = n, now
			continue
		}
		worst = max(worst, now.Sub(w.since[i]))
	}
	return worst, worst >= w.limit
}
