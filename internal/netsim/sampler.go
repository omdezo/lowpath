package netsim

import "time"

// Sampler makes the same random choices as a Link or a Segment with
// profile p, without sockets or a wall clock: the caller says when each
// packet is sent. It is for statistics tests of code that measures loss and
// jitter, which need many packets and an exact ground truth. Ask one
// Sampler for every lane behind a shared hop (at the same at, copies share
// a time-stepped burst state, as on a Segment) and one per lane for its own
// link. With the same non-zero Seed and packets in the same order, a
// per-packet profile (no Step, spikes or jitter timing) loses exactly the
// packets a Link with that profile does.
type Sampler struct {
	p   Profile
	imp *impairer
}

// NewSampler returns a sampler for profile p, seeded with p.Seed (0 =
// random).
func NewSampler(p Profile) *Sampler { return &Sampler{p: p, imp: newImpairer(p.Seed)} }

// Decide says whether a packet sent at at (time since the sampler started;
// it must not go backwards) is lost, and if not, how long it is held.
func (s *Sampler) Decide(at time.Duration) (lost bool, delay time.Duration) {
	return s.imp.decide(&s.p, at)
}
