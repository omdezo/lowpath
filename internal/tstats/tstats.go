// Package tstats keeps per-lane delivery accounting for one direction of a
// multipath flow. A lane is one relay path a flow's copies travel; every
// copy of a sequence number is recorded with the lane it came in on, and
// once a number is settled it says, for each lane that was carrying the
// flow, whether that lane delivered it. From that come each lane's loss
// (p_A, p_B), the loss two lanes share (p_AB) and how far it is from
// independent (Rho), how bursty a lane's loss is (Gilbert p01 and p10) and
// each lane's RFC 3550 interarrival jitter.
//
// The egress relay runs a Tracker per flow on the copies it receives from
// the client (upstream) and sends the result back once a second in a
// TStats packet (see Stats.Append); a client can run the same Tracker on
// the copies it receives (downstream). Counters are cumulative, as in RTCP
// receiver reports (RFC 3550 section 6.4.1), so a lost report loses
// nothing and the reader picks its own window with Stats.Since.
package tstats

import (
	"math"
	"time"
)

// MaxLanes is how many lanes a Tracker tells apart. The egress relay
// replies to at most 8 return addresses per flow, and a client sends at
// most 4 copies of a packet.
const MaxLanes = 8

// Settle is how long a sequence number stays open for more copies after a
// copy of it, or of a later number, arrived. A copy later than that is no
// use to a game, so it counts as lost on its lane (and as Late).
const Settle = 250 * time.Millisecond

// Hold is how long settling waits for a lane that stopped delivering. If
// it delivers again within Hold, the numbers it missed are losses; if not,
// they are numbers it was never sent: the client stopped using it, which
// the relay can't tell from an outage. A lane that comes back after Hold
// starts a new span.
const Hold = time.Second

// ringSize is how many sequence numbers can be open at once. At 128
// packets a second it spans 8 s, far more than Settle and Hold; at higher
// rates the oldest open numbers are settled early (see Tracker.Add).
const ringSize = 1024

// restartGap is how far behind the settled numbers a copy must be to mean
// the sender started its numbering over, as dedup.RestartGap.
const restartGap = 4 * ringSize

// npairs is the number of unordered lane pairs.
const npairs = MaxLanes * (MaxLanes - 1) / 2

// Dir is the direction a Stats describes.
type Dir uint8

const (
	Up   Dir = 0 // client -> game server, measured by the egress relay
	Down Dir = 1 // game server -> client, measured by the client
)

// Lane is one lane's cumulative counters.
type Lane struct {
	Index uint8  `json:"index"` // the lane's slot, 0..MaxLanes-1
	ID    uint32 `json:"id"`    // changes when the slot is given to another lane, so windows don't mix them
	// Expected counts the settled numbers this lane was carrying: from its
	// first copy to its newest one (per span, see Hold).
	Expected uint32 `json:"expected"`
	Lost     uint32 `json:"lost"` // of Expected, not delivered within Settle
	Late     uint32 `json:"late"` // copies that came after their number settled (counted in Lost)
	// N00..N11 count transitions between consecutive expected numbers,
	// 0 = delivered, 1 = lost: N01 is delivered then lost. They are what
	// the Gilbert estimates P01 and P10 come from.
	N00 uint32 `json:"n00"`
	N01 uint32 `json:"n01"`
	N10 uint32 `json:"n10"`
	N11 uint32 `json:"n11"`
	// JitterUS is the RFC 3550 interarrival jitter in microseconds: the
	// smoothed change in transit time between consecutive copies in arrival
	// order. It is the current value, not a counter.
	JitterUS uint32 `json:"jitter_us"`
}

// Pair is the cumulative joint counters of two lanes, over the numbers both
// were carrying.
type Pair struct {
	A     uint8  `json:"a"` // lane indexes, A < B
	B     uint8  `json:"b"`
	Both  uint32 `json:"both"`   // settled numbers both lanes were carrying
	LostA uint32 `json:"lost_a"` // of Both, lost on A
	LostB uint32 `json:"lost_b"` // of Both, lost on B
	Joint uint32 `json:"joint"`  // of Both, lost on both
}

// Stats is a Tracker's report.
type Stats struct {
	Dir   Dir    `json:"dir"`
	Seqs  uint32 `json:"seqs"` // settled numbers at least one lane was carrying
	Lost  uint32 `json:"lost"` // of Seqs, delivered by no lane: what the game lost
	Lanes []Lane `json:"lanes"`
	Pairs []Pair `json:"pairs"`
}

// ratio is n/d, or 0 when d is 0.
func ratio(n, d uint32) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

// Loss is the share of the flow's packets no lane delivered (0..1).
func (s Stats) Loss() float64 { return ratio(s.Lost, s.Seqs) }

// Lane returns the lane in slot i.
func (s Stats) Lane(i int) (Lane, bool) {
	for _, l := range s.Lanes {
		if int(l.Index) == i {
			return l, true
		}
	}
	return Lane{}, false
}

// Pair returns the joint counters of lanes a and b, in either order.
func (s Stats) Pair(a, b int) (Pair, bool) {
	if a > b {
		a, b = b, a
	}
	for _, p := range s.Pairs {
		if int(p.A) == a && int(p.B) == b {
			return p, true
		}
	}
	return Pair{}, false
}

// Loss is the lane's own loss, p_A (0..1).
func (l Lane) Loss() float64 { return ratio(l.Lost, l.Expected) }

// P01 estimates the Gilbert model's Good -> Bad probability: how often a
// delivered packet is followed by a lost one. For Gilbert's model (no loss
// in Good, all lost in Bad) the loss sequence is the chain itself, and this
// is the maximum likelihood estimate of its p.
func (l Lane) P01() float64 { return ratio(l.N01, l.N00+l.N01) }

// P10 estimates the Bad -> Good probability r: how often a lost packet is
// followed by a delivered one.
func (l Lane) P10() float64 { return ratio(l.N10, l.N10+l.N11) }

// MeanBurst is the mean number of consecutive losses, 1/P10; 0 when the
// lane lost nothing.
func (l Lane) MeanBurst() float64 {
	if l.N10 == 0 {
		if l.N11 > 0 {
			return math.Inf(1) // one loss run that hasn't ended
		}
		return 0
	}
	return float64(l.N10+l.N11) / float64(l.N10)
}

// Jitter is JitterUS as a duration.
func (l Lane) Jitter() time.Duration { return time.Duration(l.JitterUS) * time.Microsecond }

// PA is lane A's loss over the numbers both lanes carried.
func (p Pair) PA() float64 { return ratio(p.LostA, p.Both) }

// PB is lane B's loss over the numbers both lanes carried.
func (p Pair) PB() float64 { return ratio(p.LostB, p.Both) }

// PAB is the share of numbers both lanes lost: what two copies, one per
// lane, still lose.
func (p Pair) PAB() float64 { return ratio(p.Joint, p.Both) }

// Rho is p_AB / (p_A · p_B): 1 when the lanes lose packets independently,
// above 1 when their losses come together (a shared Wi-Fi or access line),
// up to 1/p when one loss always takes both copies. This is the ρ of the
// roadmap's redundancy policy. ok is false while either lane has lost
// nothing, when it is undefined.
func (p Pair) Rho() (rho float64, ok bool) {
	if p.LostA == 0 || p.LostB == 0 {
		return 0, false
	}
	return float64(p.Joint) * float64(p.Both) / (float64(p.LostA) * float64(p.LostB)), true
}

// Phi is the Pearson correlation of the two lanes' loss indicators (-1..1):
// 0 when independent, 1 when they always lose together. Unlike Rho it
// stays bounded when losses are rare. ok is false when a lane lost all or
// none of the numbers.
func (p Pair) Phi() (phi float64, ok bool) {
	n, a, b, j := float64(p.Both), float64(p.LostA), float64(p.LostB), float64(p.Joint)
	d := a * (n - a) * b * (n - b)
	if d <= 0 {
		return 0, false
	}
	return (n*j - a*b) / math.Sqrt(d), true
}

// Since returns the counters accumulated after prev, an earlier report of
// the same Tracker: the window between the two. A lane whose ID changed,
// and a pair with such a lane, starts from zero. Jitter stays the current
// value.
func (s Stats) Since(prev Stats) Stats {
	out := Stats{Dir: s.Dir, Seqs: s.Seqs - prev.Seqs, Lost: s.Lost - prev.Lost}
	same := map[uint8]bool{}
	for _, l := range s.Lanes {
		if p, ok := prev.Lane(int(l.Index)); ok && p.ID == l.ID {
			same[l.Index] = true
			l.Expected -= p.Expected
			l.Lost -= p.Lost
			l.Late -= p.Late
			l.N00 -= p.N00
			l.N01 -= p.N01
			l.N10 -= p.N10
			l.N11 -= p.N11
		}
		out.Lanes = append(out.Lanes, l)
	}
	for _, q := range s.Pairs {
		if p, ok := prev.Pair(int(q.A), int(q.B)); ok && same[q.A] && same[q.B] {
			q.Both -= p.Both
			q.LostA -= p.LostA
			q.LostB -= p.LostB
			q.Joint -= p.Joint
		}
		out.Pairs = append(out.Pairs, q)
	}
	return out
}

// pairIndex numbers the pair (a, b), a < b, from 0 to npairs-1.
func pairIndex(a, b int) int { return a*(2*MaxLanes-a-1)/2 + b - a - 1 }
