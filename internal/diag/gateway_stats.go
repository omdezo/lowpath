package diag

import (
	"math"
	"slices"
	"time"
)

// gwResult is one ping to the router: when it was sent, and its round trip
// or that it was lost.
type gwResult struct {
	sent time.Time
	rtt  time.Duration
	lost bool
}

// GatewayStats summarizes the router pings in the window. Times are nil
// when no ping was answered; P99MS also while fewer than 100 were, since a
// 99th percentile of fewer samples is just the maximum.
type GatewayStats struct {
	WindowS  float64  `json:"window_s"` // the time the pings cover
	Sent     int      `json:"sent"`
	Lost     int      `json:"lost"`
	LossPct  float64  `json:"loss_pct"`
	P50MS    *float64 `json:"p50_ms"`
	P95MS    *float64 `json:"p95_ms"`
	P99MS    *float64 `json:"p99_ms"`
	MinMS    *float64 `json:"min_ms"`
	MaxMS    *float64 `json:"max_ms"`
	JitterMS *float64 `json:"jitter_ms"` // mean change between consecutive replies (RFC 3550's D, unsmoothed)
	// Spikes counts replies of spikeMS or more.
	Spikes int `json:"spikes"`
	// MaxLossRun is the longest run of lost pings in a row: at 20 Hz, 6
	// lost in a row is a 300 ms stall.
	MaxLossRun int `json:"max_loss_run"`
	// Limited: the router answers the same few pings every second and
	// drops the rest, so it limits its replies; the loss isn't the link's.
	Limited bool `json:"limited"`
}

// GatewaySecond is one second of router pings, for the chart.
type GatewaySecond struct {
	T     int64    `json:"t"` // Unix time of the second's start
	Sent  int      `json:"sent"`
	Lost  int      `json:"lost"`
	P50MS *float64 `json:"p50_ms"`
	MaxMS *float64 `json:"max_ms"`
}

// minP99Replies: below this many replies the 99th percentile isn't shown.
const minP99Replies = 100

// gatewayStats summarizes the results sent before settled (later ones may
// still have pings in flight, whose loss would be missing).
func gatewayStats(rs []gwResult, settled time.Time) GatewayStats {
	var st GatewayStats
	var rtts []float64
	var prev *float64
	var jsum float64
	jn, run := 0, 0
	var first, last time.Time
	for _, r := range rs {
		if !r.sent.Before(settled) {
			break
		}
		if first.IsZero() {
			first = r.sent
		}
		last = r.sent
		st.Sent++
		if r.lost {
			st.Lost++
			run++
			st.MaxLossRun = max(st.MaxLossRun, run)
			continue
		}
		run = 0
		v := ms(r.rtt)
		rtts = append(rtts, v)
		if v >= spikeMS {
			st.Spikes++
		}
		if prev != nil {
			jsum += math.Abs(v - *prev)
			jn++
		}
		prev = &v
	}
	if st.Sent == 0 {
		return st
	}
	st.WindowS = last.Sub(first).Seconds()
	st.LossPct = 100 * float64(st.Lost) / float64(st.Sent)
	if len(rtts) > 0 {
		slices.Sort(rtts)
		st.MinMS, st.MaxMS = ptr(rtts[0]), ptr(rtts[len(rtts)-1])
		st.P50MS = ptr(percentile(rtts, 0.50))
		st.P95MS = ptr(percentile(rtts, 0.95))
		if len(rtts) >= minP99Replies {
			st.P99MS = ptr(percentile(rtts, 0.99))
		}
	}
	if jn > 0 {
		st.JitterMS = ptr(jsum / float64(jn))
	}
	st.Limited, _ = limitedPattern(rs, settled)
	return st
}

// gatewaySeries buckets the results sent from from to settled by second.
func gatewaySeries(rs []gwResult, from, settled time.Time) []GatewaySecond {
	var out []GatewaySecond
	var rtts []float64
	flush := func() {
		if len(out) == 0 {
			return
		}
		s := &out[len(out)-1]
		if len(rtts) > 0 {
			slices.Sort(rtts)
			s.P50MS, s.MaxMS = ptr(percentile(rtts, 0.5)), ptr(rtts[len(rtts)-1])
		}
		rtts = rtts[:0]
	}
	for _, r := range rs {
		if r.sent.Before(from) {
			continue
		}
		if !r.sent.Before(settled) {
			break
		}
		t := r.sent.Unix()
		if len(out) == 0 || out[len(out)-1].T != t {
			flush()
			out = append(out, GatewaySecond{T: t})
		}
		s := &out[len(out)-1]
		s.Sent++
		if r.lost {
			s.Lost++
		} else {
			rtts = append(rtts, ms(r.rtt))
		}
	}
	flush()
	return out
}

// Limiter detection: over at least limitSeconds whole seconds, at least
// limitLossPct of the pings lost, loss in nearly every second, and the
// same number of replies (±1) every second. Wi-Fi loss comes in bursts
// and leaves most seconds clean; a reply rate limit leaves a fixed number
// of replies per second and drops the rest.
const (
	limitSeconds   = 10
	limitLossPct   = 10
	limitLossyFrac = 0.9
)

// limitedPattern reports whether the results settled before settled look
// rate limited, and how many replies a second the router sends then.
func limitedPattern(rs []gwResult, settled time.Time) (bool, int) {
	type sec struct{ sent, ok int }
	var secs []sec
	var cur int64
	firstSec := int64(math.MinInt64)
	for _, r := range rs {
		t := r.sent.Unix()
		if firstSec == math.MinInt64 {
			firstSec = t
		}
		// The first second is partial; so is any second not over yet.
		if t == firstSec || !time.Unix(t+1, 0).Before(settled) {
			continue
		}
		if len(secs) == 0 || t != cur {
			cur = t
			secs = append(secs, sec{})
		}
		s := &secs[len(secs)-1]
		s.sent++
		if !r.lost {
			s.ok++
		}
	}
	if len(secs) < limitSeconds {
		return false, 0
	}
	sent, ok, lossy := 0, 0, 0
	lo, hi := math.MaxInt, 0
	for _, s := range secs {
		sent += s.sent
		ok += s.ok
		if s.ok < s.sent {
			lossy++
		}
		lo, hi = min(lo, s.ok), max(hi, s.ok)
	}
	lossPct := 100 * float64(sent-ok) / float64(sent)
	if lossPct < limitLossPct || float64(lossy) < limitLossyFrac*float64(len(secs)) || lo == 0 || hi-lo > 1 {
		return false, 0
	}
	return true, int(math.Round(float64(ok) / float64(len(secs))))
}
