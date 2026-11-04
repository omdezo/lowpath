package netsim

import (
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// stream tells apart the random sources made from one Seed: the down
// direction of a link (or of a segment split by SetProfiles) draws from its
// own, so its choices don't mirror the up direction's.
type stream byte

const (
	// upStream is the source every impairer drew from before directions
	// were split, so up-direction (and shared-segment) runs with a given
	// Seed still lose exactly the packets they did.
	upStream stream = iota
	downStream
)

// newRand is the random source for a seed. ChaCha8 rather than PCG: in the
// statistics tests, PCG seeded 1, 2, 3... with a fixed stream gave loss
// rates that spread about 1.4x wider from seed to seed than chance allows,
// and tests with fixed seeds rely on each seed being a fair sample.
func newRand(seed uint64) *rand.Rand { return newStreamRand(seed, upStream) }

// newStreamRand is newRand for one stream of a seed. The stream goes in a
// key byte the seed never reaches, not into the seed itself, so no seed's
// down stream is another seed's up stream.
func newStreamRand(seed uint64, s stream) *rand.Rand {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	copy(key[8:], "lowpath netsim")
	key[31] = byte(s)
	return rand.New(rand.NewChaCha8(key))
}

// impairer makes the random choices for one direction of a link, or for a
// segment: loss, spikes, burst loss and jitter. One lock covers the random
// source and the burst chain because the links behind a segment, and the
// two directions of a half-duplex one, send from different goroutines.
type impairer struct {
	mu     sync.Mutex
	stream stream
	seed   uint64
	rng    *rand.Rand
	burst  geChain
}

// newImpairer seeds from seed, or from the global source when it is 0.
func newImpairer(seed uint64) *impairer { return newStreamImpairer(seed, upStream) }

func newStreamImpairer(seed uint64, s stream) *impairer {
	im := &impairer{stream: s}
	im.reseed(seed)
	return im
}

func (im *impairer) reseed(seed uint64) {
	im.seed = seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	im.rng = newStreamRand(seed, im.stream)
	im.burst = geChain{}
}

// decide says whether a packet sent now (time since the link or segment
// started) is lost under p, and if not, how long it is held. A profile
// with a new non-zero Seed starts its random choices over, so a test can
// replay the same losses.
func (im *impairer) decide(p *Profile, now time.Duration) (drop bool, delay time.Duration) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if p.Seed != 0 && p.Seed != im.seed {
		im.reseed(p.Seed)
	}
	loss := p.Loss
	delay = p.Delay
	if p.SpikeEvery > 0 && p.SpikeLen > 0 && now%p.SpikeEvery < p.SpikeLen {
		delay += p.SpikeExtra
		loss += p.SpikeLoss
	}
	// The burst chain sees every packet, lost to the other causes or not,
	// so its statistics don't depend on them.
	drop = im.burst.drop(p.Burst, now, im.rng)
	if loss > 0 && im.rng.Float64() < loss {
		drop = true
	}
	if drop {
		return true, 0
	}
	if p.Jitter > 0 {
		delay += time.Duration(im.rng.NormFloat64() * float64(p.Jitter))
	}
	return false, delay
}

// direction is one way through a link or segment: the profile it applies
// and the impairer making its choices. A link has two; a segment has two
// but sends both ways through the first until SetProfiles splits it.
type direction struct {
	profile atomic.Pointer[Profile]
	imp     *impairer
}

func (d *direction) init(p Profile, s stream) {
	d.imp = newStreamImpairer(p.Seed, s)
	d.set(p)
}

func (d *direction) get() Profile { return *d.profile.Load() }

func (d *direction) set(p Profile) { d.profile.Store(&p) }

// decide is impairer.decide with this direction's current profile.
func (d *direction) decide(now time.Duration) (drop bool, delay time.Duration) {
	return d.imp.decide(d.profile.Load(), now)
}
