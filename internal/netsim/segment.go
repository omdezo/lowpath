package netsim

import (
	"sync"
	"sync/atomic"
	"time"
)

// Segment is a hop that several links share, such as the player's Wi-Fi or
// home router in front of every lane. Its profile applies to each packet of
// every link behind it, in both directions, from one shared state, on top
// of the link's own profile. It stands on the client's side of each link,
// so replies meet it once the link's delay is over. Two lanes behind one
// bursty Segment lose packets together, as they do at home: duplicates
// sent at the same instant don't survive a Wi-Fi fade, and a copy shifted
// in time gets through only once the shift spans a few mean bursts
// (roadmap L2; see GilbertElliott.JointLoss).
//
// Use a time-stepped Burst (GilbertTime) for a shared radio: a per-packet
// chain would step on each lane's packets in turn.
//
// By default the segment is one radio, half duplex: a fade hits packets
// going either way, so both directions share one state. SetProfiles splits
// it into two channels with a state each, as on a wired, full-duplex hop
// where the download can be lossy while the upload is clean; SetProfile
// makes it one channel again.
type Segment struct {
	clock func() time.Duration
	up    direction // packets from the clients, and replies too unless split
	down  direction // replies, once split
	split atomic.Bool

	mu sync.Mutex // one setter at a time, so split and the profiles agree
}

// NewSegment returns a half-duplex segment with profile p; attach links
// with Link.SetSegment.
func NewSegment(p Profile) *Segment {
	s := &Segment{clock: sinceNow()}
	s.up.init(p, upStream)
	s.down.init(p, downStream)
	return s
}

// Profile is the up direction's profile: both directions' unless
// SetProfiles split the segment.
func (s *Segment) Profile() Profile { return s.up.get() }

// Profiles is each direction's profile: up from the clients, down for the
// replies.
func (s *Segment) Profiles() (up, down Profile) { return s.up.get(), s.down.get() }

// Split reports whether each direction has its own state (SetProfiles)
// rather than one shared by both (SetProfile, the default).
func (s *Segment) Split() bool { return s.split.Load() }

// SetProfile changes the segment's impairment at runtime, for every link
// behind it, and makes it one half-duplex channel again if SetProfiles
// split it: both directions go back to the up direction's state.
func (s *Segment) SetProfile(p Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.set(p)
	s.down.set(p)
	s.split.Store(false)
}

// SetProfiles gives each direction its own profile and its own state, a
// full-duplex hop: losses one way don't line up with losses the other way,
// even when up and down are equal. Replies draw from their own stream of
// down's Seed, so a seeded run repeats.
func (s *Segment) SetProfiles(up, down Profile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.up.set(up)
	s.down.set(down)
	s.split.Store(true)
}

// decide impairs one packet crossing the segment; reply is true for the
// target's direction.
func (s *Segment) decide(reply bool) (bool, time.Duration) {
	d := &s.up
	if reply && s.split.Load() {
		d = &s.down
	}
	return d.decide(s.clock())
}
