package netsim

import (
	"testing"
	"time"
)

// A Sampler makes the choices of an impairer with the same profile, and
// two copies asked at one instant share a time-stepped burst, as two lanes
// behind a Segment do.
func TestSampler(t *testing.T) {
	p := Profile{Delay: 30 * time.Millisecond, Jitter: 4 * time.Millisecond, Loss: 0.05, Burst: Gilbert(0.05, 4), Seed: 7}
	s, im := NewSampler(p), newImpairer(7)
	for i := range 5000 {
		at := time.Duration(i) * time.Millisecond
		lost, d := s.Decide(at)
		lost2, d2 := im.decide(&p, at)
		if lost != lost2 || d != d2 {
			t.Fatalf("packet %d: sampler %v %v, impairer %v %v", i, lost, d, lost2, d2)
		}
	}

	wifi := NewSampler(Profile{Burst: GilbertTime(0.1, 8*time.Millisecond, time.Millisecond), Seed: 8})
	lost := 0
	for i := range 20000 {
		at := time.Duration(i) * time.Millisecond
		la, _ := wifi.Decide(at)
		lb, _ := wifi.Decide(at)
		if la != lb {
			t.Fatalf("step %d: copies at one instant disagree", i)
		}
		if la {
			lost++
		}
	}
	if lost < 1000 || lost > 3000 {
		t.Fatalf("lost %d of 20000 at 10%%", lost)
	}
}
