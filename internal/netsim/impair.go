package netsim

import (
	"encoding/binary"
	"math/rand/v2"
	"sync"
	"time"
)

// newRand is the random source for a seed. ChaCha8 rather than PCG: in the
// statistics tests, PCG seeded 1, 2, 3... with a fixed stream gave loss
// rates that spread about 1.4x wider from seed to seed than chance allows,
// and tests with fixed seeds rely on each seed being a fair sample.
func newRand(seed uint64) *rand.Rand {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	copy(key[8:], "lowpath netsim")
	return rand.New(rand.NewChaCha8(key))
}

// impairer makes the random choices for one link or segment: loss, spikes,
// burst loss and jitter. One lock covers the random source and the burst
// chain because a link's two directions send from different goroutines.
type impairer struct {
	mu    sync.Mutex
	seed  uint64
	rng   *rand.Rand
	burst geChain
}

// newImpairer seeds from seed, or from the global source when it is 0.
func newImpairer(seed uint64) *impairer {
	im := &impairer{}
	im.reseed(seed)
	return im
}

func (im *impairer) reseed(seed uint64) {
	im.seed = seed
	if seed == 0 {
		seed = rand.Uint64()
	}
	im.rng = newRand(seed)
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
