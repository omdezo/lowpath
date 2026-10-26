package diag

import (
	"time"
)

// PassiveConfig tunes the passive detector. Zero values get the defaults:
// the roadmap's rule "relay RTT +20 ms while non-game throughput > 70% of
// line rate", held for 3 of 5 seconds before it fires and 5 clean seconds
// before it clears.
type PassiveConfig struct {
	Rise     time.Duration // RTT above the baseline that counts as queueing (20 ms)
	Share    float64       // share of the line rate other traffic must use (0.7)
	MinBps   float64       // other traffic below this (bytes/s) never counts (250 kB/s = 2 Mbit/s)
	Baseline time.Duration // RTT baseline = minimum over this long (2 min)
	Raise    int           // seconds with the condition ...
	RaiseOf  int           // ... out of the last RaiseOf seconds to fire (3 of 5)
	Clear    int           // clean seconds in a row to clear (5)
	MinBase  int           // RTT samples needed before the baseline is trusted (5)
}

func (c *PassiveConfig) defaults() {
	if c.Rise <= 0 {
		c.Rise = 20 * time.Millisecond
	}
	if c.Share <= 0 || c.Share > 1 {
		c.Share = 0.7
	}
	if c.MinBps <= 0 {
		c.MinBps = 250e3
	}
	if c.Baseline <= 0 {
		c.Baseline = 2 * time.Minute
	}
	if c.RaiseOf <= 0 {
		c.RaiseOf = 5
	}
	if c.Raise <= 0 || c.Raise > c.RaiseOf {
		c.Raise = min(3, c.RaiseOf)
	}
	if c.Clear <= 0 {
		c.Clear = 5
	}
	if c.MinBase <= 0 {
		c.MinBase = 5
	}
}

// LineRate is what the internet line carries at most, in bytes/s per
// direction (0 = unknown), from the last bufferbloat test.
type LineRate struct {
	DownBps float64
	UpBps   float64
}

// PassiveSample is one second of observations.
type PassiveSample struct {
	At time.Time
	// RTT the game's route sees now (Lowpath's relay probes, without the
	// relay's own leg to the game server).
	RTT      time.Duration
	RTTKnown bool
	// NIC throughput of the default interface, bytes/s.
	Rx, Tx    float64
	RateKnown bool
	// Lowpath's own tunnel traffic (the game), bytes/s; subtracted from the
	// NIC totals to get "other" traffic.
	OwnRx, OwnTx float64
}

// PassiveState is what the UI shows. Nil numbers are unknown.
type PassiveState struct {
	// Active: something other than the game is filling the line and the
	// game's latency rose with it.
	Active    bool       `json:"active"`
	Direction string     `json:"direction,omitempty"` // download | upload | both
	Since     *time.Time `json:"since,omitempty"`
	// Watching is false while the detector lacks an input; Reason says
	// which one.
	Watching      bool     `json:"watching"`
	Reason        string   `json:"reason,omitempty"`
	RTTMS         *float64 `json:"rtt_ms"`
	BaselineMS    *float64 `json:"baseline_ms"`
	OtherDownMbps *float64 `json:"other_down_mbps"`
	OtherUpMbps   *float64 `json:"other_up_mbps"`
	LineDownMbps  *float64 `json:"line_down_mbps"`
	LineUpMbps    *float64 `json:"line_up_mbps"`
	// LineSource says where the line rate comes from: "test" (the last
	// bufferbloat test) or "peak" (the most this PC has moved since
	// Lowpath started, used only when there was no test).
	LineSource string `json:"line_source,omitempty"`
}

type rttAt struct {
	at  time.Time
	rtt time.Duration
}

// PassiveDetector watches latency and throughput during a session and
// flags when other traffic on the line (a launcher download, a stream, a
// cloud backup) is making the game lag. Not safe for concurrent use: one
// goroutine feeds it once a second.
type PassiveDetector struct {
	cfg      PassiveConfig
	test     LineRate
	peakDown float64
	peakUp   float64
	base     []rttAt
	seen     int // baseline samples since Reset
	hist     []bool
	clean    int
	active   bool
	dir      string
	since    time.Time
}

// NewPassiveDetector starts with the line rate from the last test (zero
// if none).
func NewPassiveDetector(cfg PassiveConfig, line LineRate) *PassiveDetector {
	cfg.defaults()
	return &PassiveDetector{cfg: cfg, test: line}
}

// SetLineRate replaces the line rate after a new bufferbloat test.
func (d *PassiveDetector) SetLineRate(l LineRate) { d.test = l }

// Reset forgets the RTT baseline and the alarm (new session, new route).
// Peak throughput is kept: the line didn't change.
func (d *PassiveDetector) Reset() {
	d.base, d.seen, d.hist, d.clean, d.active, d.dir = nil, 0, nil, 0, false, ""
}

// Observe adds one second of data and returns the current state.
func (d *PassiveDetector) Observe(s PassiveSample) PassiveState {
	var st PassiveState
	otherRx, otherTx := max(s.Rx-s.OwnRx, 0), max(s.Tx-s.OwnTx, 0)
	if s.RateKnown {
		d.peakDown, d.peakUp = max(d.peakDown, otherRx), max(d.peakUp, otherTx)
		st.OtherDownMbps, st.OtherUpMbps = ptr(otherRx*8/1e6), ptr(otherTx*8/1e6)
	}
	// A finished test measured the internet line itself and wins. The peak
	// is only a stand-in without one: the adapter's counters also carry LAN
	// traffic (a copy to a NAS, a game streamed to a TV), and a peak learned
	// from that would hide every later internet download for good.
	lineDown, lineUp := orPeak(d.test.DownBps, d.peakDown), orPeak(d.test.UpBps, d.peakUp)
	if lineDown > 0 {
		st.LineDownMbps = ptr(lineDown * 8 / 1e6)
	}
	if lineUp > 0 {
		st.LineUpMbps = ptr(lineUp * 8 / 1e6)
	}
	switch {
	case d.test.DownBps > 0:
		st.LineSource = "test"
	case lineDown > 0:
		st.LineSource = "peak"
	}

	full := func(other, line float64) bool { return other >= d.cfg.MinBps && line > 0 && other >= d.cfg.Share*line }
	downFull := s.RateKnown && full(otherRx, lineDown)
	upFull := s.RateKnown && full(otherTx, lineUp)

	// The baseline learns only from seconds when the line isn't full, so a
	// long download can't raise it until the alarm clears by itself.
	if s.RTTKnown {
		st.RTTMS = ptr(ms(s.RTT))
		if !downFull && !upFull {
			d.base = append(d.base, rttAt{s.At, s.RTT})
			d.seen++
		}
	}
	d.pruneBase(s.At)
	base, baseOK := d.baseline()
	if baseOK {
		st.BaselineMS = ptr(ms(base))
	}

	switch {
	case !s.RTTKnown:
		st.Reason = "no latency measurement right now"
	case !s.RateKnown:
		st.Reason = "this PC's network counters can't be read"
	case !baseOK:
		st.Reason = "learning your normal latency"
	default:
		st.Watching = true
	}

	cond := st.Watching && s.RTT >= base+d.cfg.Rise && (downFull || upFull)
	d.hist = append(d.hist, cond)
	if len(d.hist) > d.cfg.RaiseOf {
		d.hist = d.hist[len(d.hist)-d.cfg.RaiseOf:]
	}
	if cond {
		d.clean = 0
		switch {
		case downFull && upFull:
			d.dir = "both"
		case downFull:
			d.dir = "download"
		default:
			d.dir = "upload"
		}
	} else {
		d.clean++
	}
	hits := 0
	for _, h := range d.hist {
		if h {
			hits++
		}
	}
	if !d.active && hits >= d.cfg.Raise {
		d.active, d.since = true, s.At
	} else if d.active && d.clean >= d.cfg.Clear {
		d.active, d.dir = false, ""
	}
	if d.active {
		st.Active, st.Direction = true, d.dir
		since := d.since
		st.Since = &since
	}
	return st
}

// orPeak is the test's rate when there is one, else the peak seen.
func orPeak(test, peak float64) float64 {
	if test > 0 {
		return test
	}
	return peak
}

// pruneBase drops RTT samples older than the window, but always keeps the
// newest one so a long busy stretch doesn't erase the baseline.
func (d *PassiveDetector) pruneBase(now time.Time) {
	cut := 0
	for cut < len(d.base)-1 && now.Sub(d.base[cut].at) > d.cfg.Baseline {
		cut++
	}
	d.base = d.base[cut:]
}

func (d *PassiveDetector) baseline() (time.Duration, bool) {
	// After a long busy stretch only the newest sample may be left; that
	// is still a baseline. At startup a few samples are needed first.
	if len(d.base) == 0 || d.seen < d.cfg.MinBase {
		return 0, false
	}
	low := d.base[0].rtt
	for _, r := range d.base[1:] {
		low = min(low, r.rtt)
	}
	return low, true
}
