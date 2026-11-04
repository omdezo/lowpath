package relay

import (
	"time"

	"lowpath/internal/tstats"
)

// reportKind is what one lane of a flow gets in one TStats interval.
type reportKind uint8

const (
	reportNone  reportKind = iota // nothing this interval
	reportShort                   // self, seqs and lost; no lane or pair blocks
	reportFull                    // every lane and pair block
)

// selfEvery is the longest a lane goes without a report naming it, when
// another lane carries the full one. The client maps lanes to its paths,
// and notices a lane that gets nothing, from self, and expects it at least
// every 5 s; with 1 s reports a short one is due every 3 s, so even a tick
// that comes late leaves it at 4 s.
const selfEvery = 3 * time.Second

// A lane takes the full report over from the lane holding it once it has
// won clearly more of the flow's upstream races (first copies of a number)
// for takeoverRuns reports in a row: at least takeoverFactor times as many.
// Lanes about as fast as each other split the races, and moving the full
// report back and forth between them would make both pay for it.
const (
	takeoverRuns   = 3
	takeoverFactor = 2
)

// reportScheduler decides, for one flow, which of its lanes gets the
// full TStats report each interval and which get a short one.
//
// #208 sent the full report down every lane, so each could learn its own
// index. On a USB-tethered phone that is 0.6 to 1.2 MB of its downlink an
// hour, a big share of the "under 5 MB per clean hour" budget, and it buys
// nothing: the counters are cumulative, so one copy a second is enough;
// only self has to reach every lane. So the full report goes down one
// lane, and the others get a 13-byte short report every selfEvery.
//
// The full lane is sticky rather than rotated: rotating spreads the cost
// over every lane, the metered one included, while keeping it on one
// leaves the others at about 25 bytes a second. The lane chosen is the one
// winning the most upstream races, the path the game's packets actually
// ride; a tethered phone is usually the slower path, so it usually loses
// the races and is spared. The full lane changes when it stops delivering
// upstream while another lane still does (dropped by the client, or
// broken), when it is no longer a live return address, or when another
// lane outruns it (see takeoverRuns).
//
// The zero value is ready to use. Its methods are called with the flow's
// mutex held.
type reportScheduler struct {
	full    int  // lane carrying the full report, when hasFull
	hasFull bool // false until the first plan, and after the full lane is forgotten
	// lastSelf is when each lane last got a report of either kind; zero =
	// never, so a new lane gets one at the next plan.
	lastSelf [tstats.MaxLanes]time.Time
	// Upstream copies each lane delivered since the last plan, and how
	// many of them were a number's first copy (races the lane won).
	copies, firsts [tstats.MaxLanes]uint32
	ahead          [tstats.MaxLanes]uint8 // plans in a row the lane clearly outran the full lane
}

// delivered records an upstream copy that came in on lane; first is
// whether it was the first copy of its number.
func (r *reportScheduler) delivered(lane int, first bool) {
	if lane < 0 || lane >= tstats.MaxLanes {
		return
	}
	r.copies[lane]++
	if first {
		r.firsts[lane]++
	}
}

// forget clears a lane slot whose return address the flow dropped: the
// next lane in the slot is a different path, so it is new to the client
// and gets its self at the next plan, and it can't inherit the full
// report.
func (r *reportScheduler) forget(lane int) {
	if lane < 0 || lane >= tstats.MaxLanes {
		return
	}
	r.lastSelf[lane] = time.Time{}
	r.copies[lane], r.firsts[lane], r.ahead[lane] = 0, 0, 0
	if r.hasFull && r.full == lane {
		r.hasFull = false
	}
}

// plan decides what each lane in live (the flow's live return addresses'
// lanes) gets in the report sent at now, appending one kind per lane to
// kinds in the same order. Exactly one live lane gets the full report.
// It starts a new interval for delivered.
func (r *reportScheduler) plan(now time.Time, live []int, kinds []reportKind) []reportKind {
	r.pickFull(live)
	for _, l := range live {
		k := reportNone
		switch {
		case r.hasFull && l == r.full:
			k = reportFull
		case r.lastSelf[l].IsZero() || now.Sub(r.lastSelf[l]) >= selfEvery:
			k = reportShort
		}
		if k != reportNone {
			r.lastSelf[l] = now
		}
		kinds = append(kinds, k)
	}
	r.copies, r.firsts = [tstats.MaxLanes]uint32{}, [tstats.MaxLanes]uint32{}
	return kinds
}

// pickFull settles which live lane carries the full report this interval.
func (r *reportScheduler) pickFull(live []int) {
	working := false // some live lane delivered upstream this interval
	holder := false  // the full lane is live
	for _, l := range live {
		working = working || r.copies[l] > 0
		holder = holder || r.hasFull && l == r.full
	}
	// A holder that went quiet while another lane kept delivering was
	// dropped by the client or broke: its downlink is likely gone too. When
	// every lane is quiet (the game paused) nothing is learned, so it stays.
	if holder && working && r.copies[r.full] == 0 {
		holder = false
	}
	if !holder {
		r.hasFull = false
		for _, l := range live {
			if !r.hasFull || r.better(l, r.full) {
				r.full, r.hasFull = l, true
			}
		}
		r.ahead = [tstats.MaxLanes]uint8{}
		return
	}
	next := r.full
	ahead := r.ahead
	r.ahead = [tstats.MaxLanes]uint8{} // a run ends unless the lane is live and ahead again
	for _, l := range live {
		if l == r.full || r.firsts[l] <= takeoverFactor*r.firsts[r.full] {
			continue
		}
		r.ahead[l] = ahead[l] + 1
		if r.ahead[l] >= takeoverRuns && (next == r.full || r.better(l, next)) {
			next = l
		}
	}
	if next != r.full {
		r.full = next
		r.ahead = [tstats.MaxLanes]uint8{}
	}
}

// better reports whether lane a is a better full lane than b this
// interval: more races won, then more copies delivered, then the lower
// slot, so the choice is the same however live is ordered.
func (r *reportScheduler) better(a, b int) bool {
	if r.firsts[a] != r.firsts[b] {
		return r.firsts[a] > r.firsts[b]
	}
	if r.copies[a] != r.copies[b] {
		return r.copies[a] > r.copies[b]
	}
	return a < b
}
