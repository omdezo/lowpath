package netsim

import (
	"sync/atomic"
	"time"
)

// Segment is a hop that several links share, such as the player's Wi-Fi or
// home router in front of every lane. Its profile applies to each packet of
// every link behind it, in both directions, from one shared state, on top
// of the link's own profile. So two lanes behind one bursty Segment lose
// packets together, as they do at home: duplicates sent at the same
// instant don't survive a Wi-Fi fade, and a copy shifted in time gets
// through only once the shift spans a few mean bursts (roadmap L2; see
// GilbertElliott.JointLoss).
//
// Use a time-stepped Burst (GilbertTime) for a shared radio: a per-packet
// chain would step on each lane's packets in turn.
type Segment struct {
	profile atomic.Pointer[Profile]
	clock   func() time.Duration
	imp     *impairer
}

// NewSegment returns a segment with profile p; attach links with
// Link.SetSegment.
func NewSegment(p Profile) *Segment {
	s := &Segment{clock: sinceNow(), imp: newImpairer(p.Seed)}
	s.profile.Store(&p)
	return s
}

func (s *Segment) Profile() Profile { return *s.profile.Load() }

// SetProfile changes the segment's impairment at runtime, for every link
// behind it.
func (s *Segment) SetProfile(p Profile) { s.profile.Store(&p) }

func (s *Segment) decide() (bool, time.Duration) {
	return s.imp.decide(s.profile.Load(), s.clock())
}
