package relay

import (
	"math"
	"time"
)

const (
	// legWindowSize is how many of a server's latest pings its loss is
	// taken over: a minute at the default 1 s interval. The client weighs
	// the leg against its own route over seconds and blames the server only
	// at 2% loss (diag's routeLossPct), so the window must be long enough
	// that a single lost ping (1/60 = 1.7%) can't do that on its own, and
	// short enough to describe the current minute of play rather than the
	// whole session, which a server change already starts afresh.
	legWindowSize = 60
	// legMinPings is how many pings with an outcome it takes before loss is
	// reported, and how many replies compared with the one before them
	// before jitter is: the same 20
	// samples the verdict asks of every route (diag's minPathSamples), so
	// the leg is never judged on less than the routes it is compared with.
	// By then the RFC 3550 estimate, which starts at 0, has also come most
	// of the way to its steady value (1 - (15/16)^20 ≈ 72%).
	legMinPings = 20
)

// legWindow keeps the outcomes of the latest pings to one game server: how
// many were lost, and the RFC 3550 jitter of the round trips that came back.
// It is not safe for concurrent use; the monitor guards it.
type legWindow struct {
	lost  [legWindowSize]bool // ring of outcomes, true = no reply
	n     int                 // outcomes held, up to legWindowSize
	next  int                 // ring slot of the next outcome
	nLost int                 // true entries among the n held

	jitter float64       // RFC 3550 estimate, nanoseconds
	prev   time.Duration // last reply's round trip
	pairs  int           // replies that followed another reply (capped)
	any    bool          // prev is set
}

// add records one ping: its round trip, or ok false for no reply.
func (w *legWindow) add(rtt time.Duration, ok bool) {
	if w.n == legWindowSize && w.lost[w.next] {
		w.nLost--
	}
	w.lost[w.next] = !ok
	if !ok {
		w.nLost++
	}
	w.next = (w.next + 1) % legWindowSize
	w.n = min(w.n+1, legWindowSize)
	if ok {
		w.addJitter(rtt)
	}
}

// addJitter folds a reply into the RFC 3550 interarrival jitter (section
// 6.4.1): J += (|D| - J)/16, where D is the change in transit time from the
// previous reply. Each ping times its own round trip, so D is just the
// difference of two round trips, with no clock offsets to cancel; a lost
// ping in between is skipped, as the RFC skips lost packets. The estimate
// is an exponential average over about the last 16 replies, which already
// fits inside the window, so it is kept running rather than recomputed.
func (w *legWindow) addJitter(rtt time.Duration) {
	if w.any {
		d := math.Abs(float64(rtt - w.prev))
		w.jitter += (d - w.jitter) / 16
		w.pairs = min(w.pairs+1, legMinPings)
	}
	w.prev, w.any = rtt, true
}

// loss is the share of the held pings that got no reply, in percent; ok is
// false until legMinPings have an outcome.
func (w *legWindow) loss() (pct float64, ok bool) {
	if w.n < legMinPings {
		return 0, false
	}
	return 100 * float64(w.nLost) / float64(w.n), true
}

// jitterEstimate is the RFC 3550 jitter; ok is false until legMinPings
// replies have been compared with the one before them.
func (w *legWindow) jitterEstimate() (time.Duration, bool) {
	if w.pairs < legMinPings {
		return 0, false
	}
	return time.Duration(math.Round(w.jitter)), true
}
