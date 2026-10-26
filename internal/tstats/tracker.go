package tstats

import (
	"math"
	"time"
)

// Tracker is the delivery bitmap of one flow in one direction: for each
// open sequence number, the lanes whose copy arrived. The zero value is
// ready to use (direction Up). It is not safe for concurrent use; the
// caller holds its flow's lock.
//
// A number settles once Settle has passed since its first copy arrived
// (or, if no copy did, since a later number's first copy did) and every
// lane has either moved past it or stopped for Hold. Settling decides it
// for every lane carrying the flow at that number: from the lane's first
// copy to its newest one, which is how the relay tells "lost on this lane"
// from "the client wasn't using this lane" without the client saying which
// lanes it sends on.
type Tracker struct {
	Dir Dir

	started   bool
	next      uint32 // oldest open number
	top       uint32 // newest number seen
	okThrough uint32 // a number after next whose first copy is Settle old; numbers before it need no more waiting
	closed    bool   // a number has settled since the numbering (re)started
	ring      *ring  // allocated with the first copy: a flow costs nothing until it carries data

	lanes [MaxLanes]lane
	pairs [npairs]Pair
	ids   uint32 // lane IDs handed out
	seqs  uint32
	lost  uint32
}

type ring struct {
	at  [ringSize]uint32 // receiver clock in ms when the number's first copy came
	got [ringSize]uint8  // lanes whose copy came; 0 = none yet
}

type lane struct {
	c        Lane // counters; Index and ID set while the slot is in use
	on       bool // carrying the flow: first..last is its current span
	first    uint32
	last     uint32
	lastAt   time.Duration
	prevLost bool // whether the previous number in the span was lost
	hasPrev  bool

	// RFC 3550 interarrival jitter, in microseconds.
	jitter float64
	prevAt time.Duration
	prevTS uint32
	hasTS  bool
}

const (
	mask     = ringSize - 1
	settleMS = int32(Settle / time.Millisecond)
)

func ms(d time.Duration) uint32 { return uint32(d / time.Millisecond) }

// Add records a copy of seq that arrived on lane i at time at (the
// receiver's clock, any fixed origin); ts is the sender's timestamp in
// milliseconds, as in the overlay header. It reports whether the copy came
// after its number had settled (and so counts as lost on its lane).
//
// At most ringSize numbers stay open: past that the oldest settle at once,
// with lanes that haven't reached them taken as not carrying them. A jump
// of ringSize or more, forward or (4 times that) backward, is taken as the
// sender starting its numbering over: open numbers settle and the gap is
// not counted.
func (t *Tracker) Add(i int, seq, ts uint32, at time.Duration) (late bool) {
	if i < 0 || i >= MaxLanes {
		return false
	}
	l := &t.lanes[i]
	if l.c.ID == 0 {
		t.ids++
		if t.ids == 0 {
			t.ids = 1
		}
		l.c = Lane{Index: uint8(i), ID: t.ids}
	}
	l.addJitter(ts, at)
	if t.ring == nil {
		t.ring = new(ring)
	}
	// Settle first, so whether a copy is late depends on when it came, not
	// on when the caller last asked for a report.
	t.settle(at)
	switch {
	case !t.started:
		t.restart(seq)
	case int32(seq-t.top) > 0:
		if seq-t.top >= ringSize {
			t.settleAll(at)
			t.restart(seq)
			break
		}
		t.top = seq
		for t.top-t.next >= ringSize {
			t.close(at)
		}
	case int32(seq-t.next) < 0:
		if t.next-seq >= restartGap {
			t.settleAll(at)
			t.restart(seq)
			break
		}
		if !t.closed && t.top-seq < ringSize {
			// Nothing has settled yet: the first copy came down a faster
			// lane than this earlier number's, so open the window back to it.
			t.next, t.okThrough = seq, seq
			break
		}
		l.c.Late++
		if l.on {
			l.lastAt = at
		}
		return true
	}

	k := seq & mask
	if t.ring.got[k] == 0 {
		t.ring.at[k] = ms(at)
	}
	t.ring.got[k] |= 1 << i

	switch {
	case !l.on || at-l.lastAt >= Hold:
		l.on, l.first, l.last, l.hasPrev = true, seq, seq, false
	case int32(seq-l.last) > 0:
		l.last = seq
	case int32(seq-l.first) < 0:
		l.first = seq // a reordered copy from before the span's first
	}
	l.lastAt = at
	return false
}

// addJitter updates the lane's RFC 3550 jitter (section 6.4.1, appendix
// A.8) with a copy sent at ts (ms) that arrived at at: J += (|D| - J)/16,
// D being the change in transit time from the previous copy in arrival
// order. The difference of the two timestamps is taken first, so neither
// clock's origin matters and the sender's 32-bit milliseconds may wrap.
func (l *lane) addJitter(ts uint32, at time.Duration) {
	if l.hasTS {
		d := float64(at-l.prevAt)/float64(time.Microsecond) - float64(int32(ts-l.prevTS))*1000
		l.jitter += (math.Abs(d) - l.jitter) / 16
	}
	l.prevAt, l.prevTS, l.hasTS = at, ts, true
}

// Close frees lane i's slot: the lane is gone (the relay forgot its return
// address). Its counters, its pairs and its copies of open numbers go with
// it, and the next lane in the slot gets a new ID.
func (t *Tracker) Close(i int) {
	if i < 0 || i >= MaxLanes {
		return
	}
	t.lanes[i] = lane{}
	for j := range MaxLanes {
		if j != i {
			t.pairs[pairIndex(min(i, j), max(i, j))] = Pair{}
		}
	}
	if t.ring != nil {
		for k := range t.ring.got {
			t.ring.got[k] &^= 1 << i
		}
	}
}

// Report settles what is ready at now and returns the counters.
func (t *Tracker) Report(now time.Duration) Stats {
	t.settle(now)
	s := Stats{Dir: t.Dir, Seqs: t.seqs, Lost: t.lost}
	for i := range t.lanes {
		l := &t.lanes[i]
		if l.c.ID == 0 {
			continue
		}
		c := l.c
		c.JitterUS = uint32(min(math.Round(l.jitter), math.MaxUint32))
		s.Lanes = append(s.Lanes, c)
	}
	for a := range MaxLanes {
		for b := a + 1; b < MaxLanes; b++ {
			if t.lanes[a].c.ID != 0 && t.lanes[b].c.ID != 0 {
				p := t.pairs[pairIndex(a, b)]
				p.A, p.B = uint8(a), uint8(b)
				s.Pairs = append(s.Pairs, p)
			}
		}
	}
	return s
}

func (t *Tracker) restart(seq uint32) {
	t.started = true
	t.next, t.top, t.okThrough = seq, seq, seq
	t.closed = false
	for i := range t.lanes {
		t.lanes[i].on = false
	}
}

// settle closes the open numbers that are ready, oldest first.
func (t *Tracker) settle(now time.Duration) {
	for t.started && int32(t.top-t.next) >= 0 && t.ready(now) {
		t.close(now)
	}
}

func (t *Tracker) settleAll(now time.Duration) {
	for t.started && int32(t.top-t.next) >= 0 {
		t.close(now)
	}
}

// ready reports whether the oldest open number can settle at now. Ages are
// signed: a report taken just before a copy was recorded may pass a now
// slightly older than that copy.
func (t *Tracker) ready(now time.Duration) bool {
	nowMS := ms(now)
	if k := t.next & mask; t.ring.got[k] != 0 {
		if int32(nowMS-t.ring.at[k]) < settleMS {
			return false
		}
	} else if int32(t.okThrough-t.next) <= 0 {
		// No copy yet: it may still be on its way until a later number's
		// first copy is Settle old. The newest number always has a copy,
		// unless its lane was closed since.
		for s := t.next + 1; int32(t.top-s) >= 0; s++ {
			if k := s & mask; t.ring.got[k] != 0 {
				if int32(nowMS-t.ring.at[k]) < settleMS {
					return false
				}
				t.okThrough = s
				break
			}
		}
		if int32(t.okThrough-t.next) <= 0 {
			return false
		}
	}
	for i := range t.lanes {
		l := &t.lanes[i]
		if l.on && int32(l.last-t.next) < 0 && now-l.lastAt < Hold {
			return false // may be in a loss burst; wait for its next copy, or for Hold
		}
	}
	return true
}

// close settles the oldest open number.
func (t *Tracker) close(now time.Duration) {
	k := t.next & mask
	got := t.ring.got[k]
	exp := got
	for i := range t.lanes {
		l := &t.lanes[i]
		if !l.on {
			continue
		}
		if int32(t.next-l.first) >= 0 && int32(l.last-t.next) >= 0 {
			exp |= 1 << i
		} else if int32(l.last-t.next) < 0 && now-l.lastAt >= Hold {
			l.on = false // stopped: a copy from now on starts a new span
		}
	}
	if exp != 0 {
		t.seqs++
		if got == 0 {
			t.lost++
		}
	}
	for i := range t.lanes {
		l := &t.lanes[i]
		bit := uint8(1) << i
		if exp&bit == 0 {
			l.hasPrev = false
			continue
		}
		lost := got&bit == 0
		l.c.Expected++
		if lost {
			l.c.Lost++
		}
		if l.hasPrev {
			switch {
			case !l.prevLost && !lost:
				l.c.N00++
			case !l.prevLost && lost:
				l.c.N01++
			case l.prevLost && !lost:
				l.c.N10++
			default:
				l.c.N11++
			}
		}
		l.prevLost, l.hasPrev = lost, true
		for j := i + 1; j < MaxLanes; j++ {
			if exp&(1<<j) == 0 {
				continue
			}
			p := &t.pairs[pairIndex(i, j)]
			lostB := got&(1<<j) == 0
			p.Both++
			if lost {
				p.LostA++
			}
			if lostB {
				p.LostB++
			}
			if lost && lostB {
				p.Joint++
			}
		}
	}
	t.ring.got[k], t.ring.at[k] = 0, 0
	t.next++
	t.closed = true
}
