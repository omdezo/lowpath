package engine

import (
	"context"
	"sync"
	"time"
)

// awakeClock tells how long the engine was able to run from how long it
// has been. When the whole process stalls (a host that deschedules the
// machine, a badly overloaded PC), replies that arrive meanwhile wait in the
// path sockets until the readers run again. Judged on wall time, the probes
// they answer would count as lost and their paths as broken, which the
// network never said (#214). The clock notices a stall as a gap in a
// heartbeat that does nothing else, and leaves the stall, plus a moment for
// the readers to drain what queued up, out of a probe's age.
//
// A nil *awakeClock is wall time.
type awakeClock struct {
	mu     sync.Mutex
	last   time.Time // latest heartbeat
	stalls []span    // recent stalls, oldest first, not overlapping
}

// span is a stretch of time from `from` up to `to`.
type span struct{ from, to time.Time }

const (
	// heartbeatEvery is how often the heartbeat runs while nothing stalls.
	heartbeatEvery = probeInterval / 2
	// stallMin is the heartbeat gap that counts as a stall: half the probe
	// timeout. A shorter gap can't by itself hold a reply back past its
	// timeout, and counting it would let a merely busy PC, whose heartbeat
	// is often a few hundred milliseconds late, look stalled all the time.
	stallMin = probeTimeout / 2
	// stallSettle is how long after a stall the readers get to drain the
	// replies queued during it before a probe's age runs on again.
	stallSettle = 250 * time.Millisecond
	// maxStallCredit bounds how much stalled time is taken out of an age.
	// On a machine that stalls again and again, the stalls merge and awake
	// time would stand still: probes would never time out and routes that
	// are really dead would never fall back. Past this credit wall time
	// counts again, so fail-open holds whatever the machine does, at most
	// this much later than without stalls. It is the same bound as the
	// start grace: stalls of up to about this long don't read as loss.
	maxStallCredit = startGrace
	// stallKeep is how long a stall is remembered: longer than the oldest
	// probe a tracker still looks at (ringSize probes) plus the credit.
	stallKeep = ringSize*probeInterval + probeTimeout + maxStallCredit
)

// run beats every heartbeatEvery until ctx ends.
func (c *awakeClock) run(ctx context.Context) {
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Not the tick's value: a ticker sends the time the tick was
			// scheduled, which after a stall hides how late it is.
			c.beat(time.Now())
		}
	}
}

// beat records that the engine ran at now. A gap of stallMin or more since
// the previous beat is a stall: from that beat until now, plus stallSettle.
// Anyone about to judge probes beats first with time.Now(), so a stall is
// on record before the probes it delayed are judged, whichever goroutine
// the scheduler wakes first on resume. Times before the latest beat are
// ignored.
func (c *awakeClock) beat(now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.last.IsZero() && now.Sub(c.last) >= stallMin {
		s := span{from: c.last, to: now.Add(stallSettle)}
		if n := len(c.stalls); n > 0 && !s.from.After(c.stalls[n-1].to) {
			c.stalls[n-1].to = s.to // stalled again while settling: one stall
		} else {
			c.stalls = append(c.stalls, s)
		}
		cut := now.Add(-stallKeep)
		i := 0
		for i < len(c.stalls) && c.stalls[i].to.Before(cut) {
			i++
		}
		c.stalls = c.stalls[i:]
	}
	if now.After(c.last) {
		c.last = now
	}
}

// age is how long the engine was awake from t to now: now-t without the
// stalls in between, up to maxStallCredit of them.
func (c *awakeClock) age(t, now time.Time) time.Duration {
	return now.Sub(t) - c.stalled(t, now)
}

// stalled is how much of [t, now) the engine spent stalled, at most
// maxStallCredit.
func (c *awakeClock) stalled(t, now time.Time) time.Duration {
	if c == nil || !now.After(t) {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var d time.Duration
	for _, s := range c.stalls {
		from, to := s.from, s.to
		if from.Before(t) {
			from = t
		}
		if to.After(now) {
			to = now
		}
		if to.After(from) {
			d += to.Sub(from)
		}
	}
	return min(d, maxStallCredit)
}
