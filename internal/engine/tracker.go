package engine

import (
	"math"
	"sync"
	"time"
)

// Stats is what the UI shows for one path (or for the combined result).
type Stats struct {
	RTT     float64 `json:"rtt_ms"` // includes Server when the egress relay measured it
	Server  float64 `json:"server_ms"`
	Loss    float64 `json:"loss_pct"`
	Jitter  float64 `json:"jitter_ms"`
	Up      bool    `json:"up"`
	Samples int     `json:"samples"`
}

// Score ranks paths: lower is better. Loss and jitter are weighted heavily
// because they are what players actually feel as lag spikes.
func (s Stats) Score() float64 {
	if !s.Up || s.Samples == 0 {
		return math.Inf(1)
	}
	return s.RTT + 3*s.Jitter + 8*s.Loss
}

const (
	ringSize     = 128
	statsWindow  = 50 // probes considered per live snapshot (~5 s at 10 Hz)
	probeTimeout = 1 * time.Second
)

type rec struct {
	seq   uint32
	sent  time.Time
	rtt   time.Duration // total, server leg included
	leg   time.Duration // relay->server part of rtt; -1 = not measured
	got   bool
	valid bool
}

// tracker records probe send/receive times and derives RTT, loss and jitter.
type tracker struct {
	mu   sync.Mutex
	ring [ringSize]rec
	next uint32
}

func (t *tracker) send(now time.Time) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.next++
	t.ring[t.next%ringSize] = rec{seq: t.next, sent: now, leg: -1, valid: true}
	return t.next
}

// recv records a reply that arrived at now. It returns false for unknown or
// duplicate replies.
func (t *tracker) recv(seq uint32, now time.Time) bool { return t.recvLeg(seq, now, -1) }

// recvLeg is recv for a reply that also reports the egress relay's own round
// trip to the game server (leg >= 0); that leg is part of what the game feels,
// so it is added to the sample.
func (t *tracker) recvLeg(seq uint32, now time.Time, leg time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.pending(seq)
	if r == nil {
		return false
	}
	r.got = true
	r.rtt = now.Sub(r.sent)
	if leg >= 0 {
		r.rtt += leg
		r.leg = leg
	}
	return true
}

// recvRTT records a reply whose round trip was measured by someone else (the
// ICMP pinger times its own echo more precisely than our goroutine could).
func (t *tracker) recvRTT(seq uint32, rtt time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.pending(seq)
	if r == nil {
		return false
	}
	r.got = true
	r.rtt = rtt
	return true
}

func (t *tracker) pending(seq uint32) *rec {
	r := &t.ring[seq%ringSize]
	if !r.valid || r.seq != seq || r.got {
		return nil
	}
	return r
}

// reset forgets every sample (the measured target changed) but keeps the
// sequence counter, so late replies to old probes can't match new ones.
func (t *tracker) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ring = [ringSize]rec{}
}

func (t *tracker) stats(now time.Time) Stats { return t.statsN(now, statsWindow) }

func (t *tracker) statsN(now time.Time, window int) Stats {
	t.mu.Lock()
	defer t.mu.Unlock()
	var rtts []float64
	sent, lost := 0, 0
	var legSum float64
	legs := 0
	var lastGot time.Time
	for i := 0; i < ringSize && sent < window; i++ {
		r := t.ring[(t.next-uint32(i))%ringSize]
		if !r.valid || r.seq != t.next-uint32(i) {
			break
		}
		if !r.got && now.Sub(r.sent) < probeTimeout {
			continue // still in flight, don't count it yet
		}
		sent++
		if r.got {
			rtts = append(rtts, float64(r.rtt)/float64(time.Millisecond))
			if r.leg >= 0 {
				legSum += float64(r.leg) / float64(time.Millisecond)
				legs++
			}
			if r.sent.After(lastGot) {
				lastGot = r.sent
			}
		} else {
			lost++
		}
	}
	s := Stats{Samples: len(rtts)}
	if sent > 0 {
		s.Loss = 100 * float64(lost) / float64(sent)
	}
	if legs > 0 {
		s.Server = legSum / float64(legs)
	}
	if len(rtts) > 0 {
		var sum float64
		for _, v := range rtts {
			sum += v
		}
		s.RTT = sum / float64(len(rtts))
		var sq float64
		for _, v := range rtts {
			sq += (v - s.RTT) * (v - s.RTT)
		}
		s.Jitter = math.Sqrt(sq / float64(len(rtts)))
	}
	s.Up = !lastGot.IsZero() && now.Sub(lastGot) < probeTimeout+2*time.Second
	return s
}
