package diag

import (
	"fmt"
	"strings"
)

// Segments split the way from this PC to the game server into the parts a
// verdict can blame (roadmap §3.7: "segmented Home/ISP/Lowpath/Server
// bar"), each with what was measured on it and how it looks. They come
// from the same evidence and checks as the verdict, so the bar and the
// verdict never disagree, and a part nothing measured says so instead of
// showing a number.

// Segment statuses.
const (
	SegmentGood    = "good"
	SegmentWarn    = "warn"    // measured, not bad enough to blame
	SegmentBad     = "bad"     // a check found its problem here
	SegmentUnknown = "unknown" // not measured; Summary says what to run
	SegmentOff     = "off"     // not part of the route right now (no relay in use)
)

// Segment is one part of the route.
type Segment struct {
	ID      string   `json:"id"` // SegmentHome, SegmentISP, SegmentLowpath, SegmentServer
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	RTTMS   *float64 `json:"rtt_ms"`   // nil: not measured
	LossPct *float64 `json:"loss_pct"` // nil: not measured, or not usable
	Source  string   `json:"source,omitempty"`
	Summary string   `json:"summary"`
	// Blamed: the verdict points at this segment.
	Blamed bool `json:"blamed"`
}

func segments(v *view, checks []Check, vd Verdict) []Segment {
	byRule := map[string]Check{}
	for _, c := range checks {
		byRule[c.Rule] = c
	}
	out := []Segment{homeSegment(v, byRule), ispSegment(v, byRule, vd), lowpathSegment(v, vd), serverSegment(v, byRule)}
	if vd.ID == VerdictOffline {
		// No route at all: the home end is where it breaks, and router
		// pings from before the connection dropped say nothing about now.
		out[0] = Segment{ID: SegmentHome, Name: "Home", Status: SegmentBad, Summary: vd.Title + "."}
	}
	for i := range out {
		out[i].Blamed = vd.Segment != "" && vd.Segment == out[i].ID && out[i].Status == SegmentBad
	}
	return out
}

// checkStatus maps a check onto a segment status.
func checkStatus(c Check) string {
	switch c.Status {
	case CheckFound:
		return SegmentBad
	case CheckClear:
		return SegmentGood
	}
	return SegmentUnknown
}

func homeSegment(v *view, checks map[string]Check) Segment {
	home, line := checks[ruleHome], checks[ruleLine]
	s := Segment{ID: SegmentHome, Name: "Home", Status: checkStatus(home), Summary: home.Summary}
	if g := v.gateway; g != nil && (g.Lost < g.Sent || g.Stopped) {
		s.RTTMS, s.Source = g.P50MS, upperFirst(g.Source)
		if !g.Limited {
			s.LossPct = ptr(g.lossPct())
		}
		if s.Status == SegmentGood && g.Spikes > 0 {
			s.Status = SegmentWarn
			s.Summary = fmt.Sprintf("Your router answers without loss, but %d %s took %d ms or more.", g.Spikes, plural(g.Spikes, "reply", "replies"), spikeMS)
		}
	}
	if s.Status == SegmentGood || s.Status == SegmentWarn {
		// Clean router pings: the radio link loses nothing now, but a weak
		// or crowded one is the first part to fail, so the bar doesn't call
		// Home OK while the Wi-Fi check's facts say the signal is weak.
		if weak, known, fact := wifiSignal(v.ev.WiFi); known && weak && v.linkKind() == LinkWiFi {
			s.Status, s.Summary = SegmentWarn, s.Summary+" "+fact
		}
	}
	if line.Status == CheckFound {
		// A full line queues in the home router: the home end of the route.
		s.Status, s.Summary = SegmentBad, line.Summary
	}
	return s
}

func ispSegment(v *view, checks map[string]Check, vd Verdict) Segment {
	isp := checks[ruleISP]
	s := Segment{ID: SegmentISP, Name: "ISP", Status: checkStatus(isp), Summary: isp.Summary}
	if vd.ID == VerdictRelay {
		// The trace found the routers on the way clean and the loss at the
		// relay's port: the ISP part is fine.
		s.Status, s.Summary = SegmentGood, "The routers on the way to your relay don't lose traffic."
	}
	if t := v.trace; t != nil && t.Reached {
		if d := traceDest(t); d != nil && d.AvgMS != nil {
			s.RTTMS = d.AvgMS
		}
		s.LossPct = ispTraceLoss(v)
		s.Source = "Route trace, this PC to " + traceHost(t)
		return s
	}
	if b := v.bloat; b != nil {
		for _, p := range b.Phases {
			if p.Phase == PhaseIdle && p.Internet.Sent > 0 && p.Internet.Sent > p.Internet.Lost {
				s.RTTMS = p.Internet.P50
				s.LossPct = ptr(100 * p.Internet.lossFrac())
				s.Source = "Bufferbloat test, idle pings to " + b.Target
			}
		}
	}
	return s
}

// ispTraceLoss is the trace's loss on the ISP's part, as the ISP rule
// reads it: the loss that starts past the home network and persists to the
// destination, else what persists from the first hop past home (a clean
// route's). Nil when loss already starts at home, so the ISP's share can't
// be told apart, or when no hop past home answered.
func ispTraceLoss(v *view) *float64 {
	t := v.trace
	if v.homeLossHop() != nil {
		return nil
	}
	if f := t.LossFrom; f != nil && *f >= 1 && *f <= len(t.Hops) && !(*f == len(t.Hops) && t.End == nil) {
		return t.DestLossPct
	}
	if h := firstISPHop(t); h != nil {
		return h.PersistPct
	}
	return nil
}

// traceDest is the row that measured the destination: the UDP row, else
// the last hop.
func traceDest(t *TraceResult) *TraceHop {
	if t.End != nil {
		return t.End
	}
	if len(t.Hops) == 0 {
		return nil
	}
	return &t.Hops[len(t.Hops)-1]
}

// firstISPHop is the first hop past the home network.
func firstISPHop(t *TraceResult) *TraceHop {
	for i := range t.Hops {
		if !t.Hops[i].inHome() {
			return &t.Hops[i]
		}
	}
	return nil
}

func lowpathSegment(v *view, vd Verdict) Segment {
	s := Segment{ID: SegmentLowpath, Name: "Lowpath relay"}
	p := v.path
	if vd.ID == VerdictRelay {
		s.Status, s.Summary = SegmentBad, vd.Title+"."
	}
	if p == nil || p.Relay == nil {
		if s.Status == "" {
			s.Status, s.Summary = SegmentOff, "Not in use: boost a game, or measure routes on its page, to compare a relay."
		}
		return s
	}
	r := p.Relay
	s.RTTMS, s.LossPct = ptr(r.RTTMS), ptr(r.LossPct)
	s.Source = pathWords(p, v) + ", this PC to the relay"
	if s.Status != "" {
		return s
	}
	switch {
	case r.Samples < minPathSamples:
		s.Status, s.Summary = SegmentUnknown, "Too few probes to the relay yet."
	case r.LossPct >= routeLossPct:
		s.Status, s.Summary = SegmentBad, fmt.Sprintf("The leg to the relay loses %.1f%% of probes.", r.LossPct)
	case r.LossPct >= cleanLossPct || r.JitterMS >= cleanJitterMS:
		s.Status, s.Summary = SegmentWarn, fmt.Sprintf("The leg to the relay loses %.1f%% with %.0f ms jitter.", r.LossPct, r.JitterMS)
	default:
		s.Status, s.Summary = SegmentGood, "The leg to the relay is clean."
	}
	return s
}

func serverSegment(v *view, checks map[string]Check) Segment {
	srv := checks[ruleServer]
	s := Segment{ID: SegmentServer, Name: "Game server", Status: checkStatus(srv), Summary: srv.Summary}
	p := v.path
	switch {
	case p == nil:
	case p.ServerMS != nil:
		s.RTTMS, s.LossPct = p.ServerMS, p.ServerLossPct
		s.Source = pathWords(p, v) + ", the relay to the server"
	case p.Direct != nil:
		s.RTTMS, s.LossPct = ptr(p.Direct.RTTMS), ptr(p.Direct.LossPct)
		s.Source = pathWords(p, v) + ", direct ping from this PC"
	}
	return s
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
