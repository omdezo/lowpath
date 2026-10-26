package netsim

import (
	"math"
	"math/rand/v2"
	"time"
)

// GilbertElliott is the two-state burst loss model: a Markov chain between a
// Good and a Bad state, each losing packets independently at its own rate.
// The parameters follow Hasslinger and Hohlfeld ("The Gilbert-Elliott Model
// for Packet Loss in Real Time Services on the Internet", MMB 2008) and
// Linux netem's "loss gemodel p r 1-h 1-k":
//
//	P        p    Good -> Bad, per step
//	R        r    Bad -> Good, per step
//	LossGood 1-k  loss probability in Good
//	LossBad  1-h  loss probability in Bad
//
// Gilbert's original model is LossGood 0, LossBad 1 (see Gilbert and
// GilbertTime). Each step the chain moves first and the packet is then lost
// with the rate of the state it is in.
//
// With Step 0 the chain takes one step per packet, like netem. With Step > 0
// it takes one step per Step of time whether packets flow or not, so packets
// in the same Step see the same state. That is how a shared radio behaves:
// a Wi-Fi fade hits every packet in the air at that moment, whichever lane
// sent it (see Segment).
type GilbertElliott struct {
	P        float64       `json:"p"`
	R        float64       `json:"r"`
	LossGood float64       `json:"lossGood"`
	LossBad  float64       `json:"lossBad"`
	Step     time.Duration `json:"step,omitempty"`
}

// Gilbert is the classic Gilbert model, stepping once per packet, with the
// given mean loss (0..1) and mean burst length in packets (>= 1). A short
// burst limits the loss it can reach (one-packet bursts reach at most 50%):
// MeanLoss says what the model really gives.
func Gilbert(loss, burst float64) GilbertElliott {
	if loss <= 0 {
		return GilbertElliott{}
	}
	if loss >= 1 {
		return GilbertElliott{P: 1, LossBad: 1}
	}
	r := 1 / max(burst, 1)
	return GilbertElliott{P: min(r*loss/(1-loss), 1), R: r, LossBad: 1}
}

// GilbertTime is Gilbert with the burst given as time: the chain steps
// every step (1 ms when 0) and a Bad spell lasts burst on average. The
// roadmap's Wi-Fi case, "ABL 8 ms, 3%", is GilbertTime(0.03, 8*ms, 0).
func GilbertTime(loss float64, burst, step time.Duration) GilbertElliott {
	if step <= 0 {
		step = time.Millisecond
	}
	g := Gilbert(loss, float64(burst)/float64(step))
	if g.P > 0 {
		g.Step = step
	}
	return g
}

// active reports whether the model can lose a packet at all.
func (g GilbertElliott) active() bool { return g.LossGood > 0 || (g.P > 0 && g.LossBad > 0) }

// BadShare is the stationary probability of the Bad state, p / (p + r).
func (g GilbertElliott) BadShare() float64 {
	if g.P+g.R == 0 {
		return 0 // never leaves Good, where it starts
	}
	return g.P / (g.P + g.R)
}

// MeanLoss is the long-run loss rate, (1-k)·πG + (1-h)·πB.
func (g GilbertElliott) MeanLoss() float64 {
	b := g.BadShare()
	return (1-b)*g.LossGood + b*g.LossBad
}

// MeanBadSteps is how long a Bad spell lasts on average, 1/r steps
// (Step/r of time when Step > 0). Infinite when r is 0.
func (g GilbertElliott) MeanBadSteps() float64 { return 1 / g.R }

// MeanLossRun is the mean number of packets in a run of consecutive losses
// when every step carries one packet: the loss rate divided by the rate at
// which runs start, P(kept, then lost). For Gilbert's model it is 1/r; with
// LossGood > 0 or LossBad < 1 runs are shorter than a Bad spell.
func (g GilbertElliott) MeanLossRun() float64 {
	b := g.BadShare()
	pi := [2]float64{1 - b, b}
	loss := [2]float64{g.LossGood, g.LossBad}
	move := [2][2]float64{{1 - g.P, g.P}, {g.R, 1 - g.R}}
	var starts float64
	for s := range 2 {
		for t := range 2 {
			starts += pi[s] * (1 - loss[s]) * move[s][t] * loss[t]
		}
	}
	if starts == 0 {
		return math.Inf(1)
	}
	return g.MeanLoss() / starts
}

// BadAfter is the probability of being in Bad n steps after being in Bad
// (bad true) or Good: πB + (x - πB)·(1-p-r)^n.
func (g GilbertElliott) BadAfter(n int, bad bool) float64 {
	if n <= 0 || g.P+g.R == 0 {
		if bad {
			return 1
		}
		return 0
	}
	b := g.BadShare()
	x := 0.0
	if bad {
		x = 1
	}
	return b + (x-b)*math.Pow(1-g.P-g.R, float64(n))
}

// JointLoss is the probability that two packets n steps apart are both
// lost; with n 0 they share a step, as two lanes' copies sent at once
// through a Segment do. Without bursts it would be MeanLoss squared, so
// JointLoss(n) / MeanLoss() is how likely the second copy is to be lost
// too, which is what a time shift between duplicates (roadmap L2) is
// chosen against.
func (g GilbertElliott) JointLoss(n int) float64 {
	b := g.BadShare()
	pi := [2]float64{1 - b, b}
	loss := [2]float64{g.LossGood, g.LossBad}
	var p float64
	for s := range 2 {
		toBad := g.BadAfter(n, s == 1)
		p += pi[s] * loss[s] * ((1-toBad)*loss[0] + toBad*loss[1])
	}
	return p
}

// geChain is a running GilbertElliott model. It is not safe for concurrent
// use; the impairer that owns it holds a lock.
type geChain struct {
	started bool
	bad     bool
	step    int64         // index of the current Step, time mode only
	unit    time.Duration // the Step that step counts in
}

// drop advances the chain to now (time since the link or segment started;
// only used when Step > 0) and decides one packet. The first packet finds
// the chain in its stationary state, so short runs aren't biased toward
// Good.
func (c *geChain) drop(m GilbertElliott, now time.Duration, rng *rand.Rand) bool {
	if !m.active() {
		return false
	}
	switch {
	case !c.started:
		c.started = true
		c.bad = rng.Float64() < m.BadShare()
		if m.Step > 0 {
			c.step, c.unit = int64(now/m.Step), m.Step
		}
	case m.Step > 0:
		s := int64(now / m.Step)
		if m.Step != c.unit {
			// The profile changed the step size: count on from here in the
			// new one, or an index in the old one would hold the chain still.
			c.step, c.unit = s, m.Step
		}
		if s > c.step { // concurrent senders may arrive slightly out of order: same step then
			c.bad = rng.Float64() < m.BadAfter(int(min(s-c.step, math.MaxInt32)), c.bad)
			c.step = s
		}
	case c.bad:
		c.bad = rng.Float64() >= m.R
	default:
		c.bad = rng.Float64() < m.P
	}
	loss := m.LossGood
	if c.bad {
		loss = m.LossBad
	}
	return loss > 0 && rng.Float64() < loss
}
