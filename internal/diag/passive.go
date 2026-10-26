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
	// PeakWindow is how long the most traffic seen stands in for the line
	// rate when no bufferbloat test measured it (10 min): long enough to
	// remember the line's speed across a pause between downloads, short
	// enough that a burst to a LAN peer the counters couldn't tell apart
	// stops hiding internet downloads soon after it ends.
	PeakWindow time.Duration
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
	if c.PeakWindow <= 0 {
		c.PeakWindow = 10 * time.Minute
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
	// LAN traffic through the same adapter (a copy to a NAS, a game
	// streamed to a TV), bytes/s, from per-connection counters; subtracted
	// too, since it never crosses the internet line. LANKnown is false
	// where the OS gives no such counters without administrator rights:
	// LAN traffic then counts as other traffic.
	LANRx, LANTx float64
	LANKnown     bool
	// Talkers are the programs moving the most internet traffic in this
	// second, biggest first (none where the OS can't tell).
	Talkers []Talker
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
	// bufferbloat test) or "peak" (the most other traffic seen in the last
	// PeakWindow, used only when there was no test).
	LineSource string `json:"line_source,omitempty"`
	// NeedsTest: no bufferbloat test has measured the line, so its rate is
	// a guess from the traffic seen; the UI asks for a test once.
	NeedsTest bool `json:"needs_test,omitempty"`
	// LAN traffic left out of the other traffic (nil: this system can't
	// tell it apart).
	LANDownMbps *float64 `json:"lan_down_mbps"`
	LANUpMbps   *float64 `json:"lan_up_mbps"`
	// Top is the program moving the most internet traffic in the direction
	// that fills the line (nil: none, or the system can't tell).
	Top *TopTalker `json:"top,omitempty"`
}

// TopTalker is one program's internet traffic.
type TopTalker struct {
	Name     string  `json:"name"`
	DownMbps float64 `json:"down_mbps"`
	UpMbps   float64 `json:"up_mbps"`
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
	peakDown slidingMax
	peakUp   slidingMax
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
	return &PassiveDetector{cfg: cfg, test: line, peakDown: slidingMax{window: cfg.PeakWindow}, peakUp: slidingMax{window: cfg.PeakWindow}}
}

// SetLineRate replaces the line rate after a new bufferbloat test.
func (d *PassiveDetector) SetLineRate(l LineRate) { d.test = l }

// Reset forgets the RTT baseline and the alarm (new session, new route).
// The peaks are kept and age out on their own: the line didn't change.
func (d *PassiveDetector) Reset() {
	d.base, d.seen, d.hist, d.clean, d.active, d.dir = nil, 0, nil, 0, false, ""
}

// Observe adds one second of data and returns the current state.
func (d *PassiveDetector) Observe(s PassiveSample) PassiveState {
	var st PassiveState
	otherRx, otherTx := max(s.Rx-s.OwnRx, 0), max(s.Tx-s.OwnTx, 0)
	if s.RateKnown && s.LANKnown {
		otherRx, otherTx = max(otherRx-s.LANRx, 0), max(otherTx-s.LANTx, 0)
		st.LANDownMbps, st.LANUpMbps = ptr(s.LANRx*8/1e6), ptr(s.LANTx*8/1e6)
	}
	if s.RateKnown {
		d.peakDown.add(s.At, otherRx)
		d.peakUp.add(s.At, otherTx)
		st.OtherDownMbps, st.OtherUpMbps = ptr(otherRx*8/1e6), ptr(otherTx*8/1e6)
	}
	// A finished test measured the internet line itself and wins. The peak
	// is only a stand-in without one, and a sliding one: the adapter's
	// counters also carry LAN traffic that can't always be told apart (a
	// copy to a NAS, a game streamed to a TV), and a peak kept for good
	// would hide every later internet download until Lowpath restarts.
	lineDown, lineUp := orPeak(d.test.DownBps, d.peakDown.max(s.At)), orPeak(d.test.UpBps, d.peakUp.max(s.At))
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
	st.NeedsTest = d.test.DownBps <= 0 && d.test.UpBps <= 0

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
	case lineDown <= 0 && lineUp <= 0:
		// Nothing to compare other traffic with: no test, and no traffic
		// seen yet that could stand in for one.
		st.Reason = "your connection's speed isn't known yet"
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
	st.Top = topTalker(s.Talkers, st.Direction)
	return st
}

// topTalker is the program moving the most in the direction that fills the
// line, or in both directions together while nothing does.
func topTalker(ts []Talker, dir string) *TopTalker {
	score := func(t Talker) float64 {
		switch dir {
		case "download":
			return t.Rx
		case "upload":
			return t.Tx
		}
		return t.Rx + t.Tx
	}
	best := -1
	for i, t := range ts {
		if score(t) > 0 && (best < 0 || score(t) > score(ts[best])) {
			best = i
		}
	}
	if best < 0 {
		return nil
	}
	t := ts[best]
	return &TopTalker{Name: t.Name, DownMbps: t.Rx * 8 / 1e6, UpMbps: t.Tx * 8 / 1e6}
}

// orPeak is the test's rate when there is one, else the peak seen.
func orPeak(test, peak float64) float64 {
	if test > 0 {
		return test
	}
	return peak
}

// slidingMax is the largest value added in the last window. It is a
// monotonic queue: a value is dropped once a later one is at least as big,
// so it holds at most one entry per second of the window and an add costs
// O(1) amortized.
type slidingMax struct {
	window time.Duration
	q      []valueAt
}

type valueAt struct {
	at time.Time
	v  float64
}

func (m *slidingMax) add(at time.Time, v float64) {
	for len(m.q) > 0 && m.q[len(m.q)-1].v <= v {
		m.q = m.q[:len(m.q)-1]
	}
	m.q = append(m.q, valueAt{at, v})
}

// max is the largest value added within the window up to now (0: none).
func (m *slidingMax) max(now time.Time) float64 {
	cut := 0
	for cut < len(m.q) && now.Sub(m.q[cut].at) > m.window {
		cut++
	}
	m.q = m.q[cut:]
	if len(m.q) == 0 {
		return 0
	}
	return m.q[0].v
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
