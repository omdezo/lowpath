package diag

import (
	"strings"
	"testing"
	"time"
)

func segment(t *testing.T, d Diagnosis, id string) Segment {
	t.Helper()
	for _, s := range d.Segments {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no %s segment in %+v", id, d.Segments)
	return Segment{}
}

// livePings is a minute of 20 Hz router pings on link.
func livePings(link *Route, lost int, p99 float64) *GatewaySample {
	return &GatewaySample{Source: "live router pings, 20 times a second", At: vNow.Add(-time.Second), Sent: 1200, Lost: lost,
		P50MS: ptr(2.0), TailMS: ptr(p99), TailPct: 99, Link: link.Kind, Interface: link.Name}
}

func TestSegmentsNothingMeasured(t *testing.T) {
	d := diagnose(Evidence{Link: wifiLink()})
	if len(d.Segments) != 4 {
		t.Fatalf("%d segments", len(d.Segments))
	}
	for _, s := range d.Segments {
		want := SegmentUnknown
		if s.ID == SegmentLowpath {
			want = SegmentOff
		}
		if s.Status != want || s.RTTMS != nil || s.LossPct != nil || s.Blamed || s.Summary == "" {
			t.Errorf("%s: %+v", s.ID, s)
		}
	}
	if h := segment(t, d, SegmentHome); !strings.Contains(h.Summary, "Diagnostics open") {
		t.Errorf("home says %q", h.Summary)
	}
}

func TestSegmentsWiFiLossBlamesHome(t *testing.T) {
	d := diagnose(Evidence{Link: wifiLink(), Gateway: livePings(wifiLink(), 36, 9)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
	h := segment(t, d, SegmentHome)
	if h.Status != SegmentBad || !h.Blamed || *h.RTTMS != 2 || *h.LossPct != 3 || !strings.Contains(h.Source, "Live router pings, 20 times a second") {
		t.Fatalf("home %+v", h)
	}
	for _, id := range []string{SegmentISP, SegmentLowpath, SegmentServer} {
		if segment(t, d, id).Blamed {
			t.Errorf("%s blamed too", id)
		}
	}
}

func TestSegmentsCleanRouterWithSpikes(t *testing.T) {
	g := livePings(cableLink(), 0, 4)
	g.Spikes = 3
	d := diagnose(Evidence{Link: cableLink(), Gateway: g})
	h := segment(t, d, SegmentHome)
	if h.Status != SegmentWarn || h.Blamed || !strings.Contains(h.Summary, "3 replies took 100 ms") {
		t.Fatalf("home %+v", h)
	}
	if c := check(t, d, ruleHome); c.Status != CheckClear || !strings.Contains(c.Facts[0], "3 replies of 100 ms or more") {
		t.Errorf("home check %+v", c)
	}
}

func TestSegmentsRateLimitedRouterIsNotLoss(t *testing.T) {
	g := livePings(wifiLink(), 600, 5)
	g.Limited = true
	d := diagnose(Evidence{Link: wifiLink(), Gateway: g})
	c := check(t, d, ruleHome)
	if c.Status != CheckClear || !strings.Contains(strings.Join(c.Facts, " "), "limits how many pings") {
		t.Fatalf("home check %+v", c)
	}
	if h := segment(t, d, SegmentHome); h.LossPct != nil || h.Status != SegmentGood {
		t.Errorf("home %+v", h)
	}
}

func TestGatewayPingsFromAnotherConnection(t *testing.T) {
	// Lossy pings on Wi-Fi, then the cable is plugged in: they say nothing
	// about the cable.
	d := diagnose(Evidence{Link: cableLink(), Gateway: livePings(wifiLink(), 36, 9)})
	if c := check(t, d, ruleHome); c.Status != CheckUnknown {
		t.Fatalf("home check %+v", c)
	}
	// With a bufferbloat test on the cable, that one is used instead.
	d = diagnose(Evidence{Link: cableLink(), Gateway: livePings(wifiLink(), 36, 9), Bloat: bloatOn(cableLink(), time.Minute, "A", 50, 0, 3)})
	if c := check(t, d, ruleHome); c.Status != CheckClear || !strings.Contains(c.Facts[0], "idle phase") {
		t.Fatalf("home check with a cable test %+v", c)
	}
	// The connection in use unknown: used, one step less sure.
	d = diagnose(Evidence{Gateway: livePings(wifiLink(), 36, 9)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
}

func TestSegmentsLineBlamesHome(t *testing.T) {
	d := diagnose(Evidence{Link: cableLink(), Gateway: livePings(cableLink(), 0, 3), Bloat: bloatOn(cableLink(), time.Minute, "D", 50, 0, 3)})
	wantVerdict(t, d, VerdictLine, ConfidenceHigh)
	h := segment(t, d, SegmentHome)
	if h.Status != SegmentBad || !h.Blamed || !strings.Contains(h.Summary, "busy") {
		t.Fatalf("home %+v", h)
	}
}

func TestSegmentsISPTrace(t *testing.T) {
	tr := traceWith(6, 3, 4)
	for i := range tr.Hops {
		tr.Hops[i].AvgMS = ptr(float64(5 * (i + 1)))
		if i >= 2 {
			tr.Hops[i].PersistPct = ptr(4.0)
		} else {
			tr.Hops[i].PersistPct = ptr(0.0)
		}
	}
	d := diagnose(Evidence{Link: cableLink(), Gateway: livePings(cableLink(), 0, 3), Trace: tr})
	wantVerdict(t, d, VerdictISP, ConfidenceHigh)
	s := segment(t, d, SegmentISP)
	// Hop 2 is the first past home and still clean; the loss starts at 3
	// and persists: the ISP part shows that loss, not hop 2's 0%.
	if s.Status != SegmentBad || !s.Blamed || *s.RTTMS != 30 || s.LossPct == nil || *s.LossPct != 4 || !strings.Contains(s.Source, "this PC to Cloudflare DNS") {
		t.Fatalf("isp %+v", s)
	}
	if h := segment(t, d, SegmentHome); h.Status != SegmentGood {
		t.Errorf("home %+v", h)
	}
}

func TestSegmentsISPTraceCleanAndHomeLoss(t *testing.T) {
	clean := traceWith(6, 0, 0)
	for i := range clean.Hops {
		clean.Hops[i].PersistPct = ptr(0.0)
	}
	s := segment(t, diagnose(Evidence{Link: cableLink(), Trace: clean}), SegmentISP)
	if s.Status != SegmentGood || s.LossPct == nil || *s.LossPct != 0 {
		t.Errorf("clean trace: %+v", s)
	}
	// Loss from hop 1 persists through the ISP's hops: it is the home's,
	// so the ISP part shows no loss figure of its own.
	home := traceWith(6, 1, 5)
	for i := range home.Hops {
		home.Hops[i].PersistPct = ptr(5.0)
	}
	s = segment(t, diagnose(Evidence{Link: cableLink(), Trace: home}), SegmentISP)
	if s.Status != SegmentUnknown || s.LossPct != nil {
		t.Errorf("loss from home: %+v", s)
	}
}

func TestSegmentsOffline(t *testing.T) {
	// Clean router pings from before the connection dropped.
	d := diagnose(Evidence{Link: cableLink(), Offline: true, Gateway: livePings(cableLink(), 0, 3)})
	wantVerdict(t, d, VerdictOffline, ConfidenceHigh)
	h := segment(t, d, SegmentHome)
	if h.Status != SegmentBad || !h.Blamed || h.RTTMS != nil || h.LossPct != nil || !strings.Contains(h.Summary, "No internet") {
		t.Fatalf("home while offline %+v", h)
	}
}

func TestSegmentsRouterStoppedAnswering(t *testing.T) {
	g := livePings(wifiLink(), 1200, 0)
	g.P50MS, g.TailMS, g.TailPct, g.Stopped = nil, nil, 0, true
	d := diagnose(Evidence{Link: wifiLink(), Gateway: g})
	if d.Verdict.ID != VerdictWiFi {
		t.Fatalf("verdict %s, want %s", d.Verdict.ID, VerdictWiFi)
	}
	h := segment(t, d, SegmentHome)
	if h.Status != SegmentBad || !h.Blamed || h.RTTMS != nil || h.LossPct == nil || *h.LossPct != 100 {
		t.Fatalf("home %+v", h)
	}
	if !strings.Contains(strings.Join(d.Checks[0].Facts, " "), "stopped answering") {
		t.Errorf("facts %q", d.Checks[0].Facts)
	}
	// The same series from a router that never answered: not loss.
	g.Stopped = false
	if d := diagnose(Evidence{Link: wifiLink(), Gateway: g}); d.Verdict.ID == VerdictWiFi || segment(t, d, SegmentHome).Status != SegmentUnknown {
		t.Errorf("never answered: %s %+v", d.Verdict.ID, segment(t, d, SegmentHome))
	}
}

func TestSegmentsBoost(t *testing.T) {
	p := boostPath(PathStats{RTTMS: 60, LossPct: 0.5, Samples: 60}, PathStats{RTTMS: 20, LossPct: 0, JitterMS: 1, Samples: 60}, ptr(35.0))
	d := diagnose(Evidence{Link: cableLink(), Gateway: livePings(cableLink(), 0, 3), Boost: p})
	l := segment(t, d, SegmentLowpath)
	if l.Status != SegmentGood || *l.RTTMS != 20 || *l.LossPct != 0 || !strings.Contains(l.Source, "Example Game boost") {
		t.Fatalf("lowpath %+v", l)
	}
	sv := segment(t, d, SegmentServer)
	if *sv.RTTMS != 35 || sv.LossPct != nil || !strings.Contains(sv.Source, "the relay to the server") {
		t.Fatalf("server %+v", sv)
	}
	// A lossy relay leg.
	p.Relay.LossPct = 3
	if l := segment(t, diagnose(Evidence{Link: cableLink(), Boost: p}), SegmentLowpath); l.Status != SegmentBad {
		t.Errorf("lossy relay leg: %+v", l)
	}
	// No time from the relay to the server: the direct ping.
	p.Relay.LossPct, p.ServerMS = 0, nil
	if sv := segment(t, diagnose(Evidence{Link: cableLink(), Boost: p}), SegmentServer); *sv.RTTMS != 60 || *sv.LossPct != 0.5 {
		t.Errorf("server from the direct ping: %+v", sv)
	}
}

func TestSegmentsInReport(t *testing.T) {
	ev := Evidence{At: vNow, Link: wifiLink(), Gateway: livePings(wifiLink(), 36, 9)}
	d := NewVerdictEngine(VerdictConfig{}).Diagnose(ev)
	txt := DiagnosisReport{Diagnosis: d, Evidence: ev}.Text()
	for _, want := range []string{"ROUTE, PART BY PART", "[bad] Home: 2.0 ms, 3.0% loss (the verdict).", "[off] Lowpath relay: —.", "Router pings (live router pings, 20 times a second"} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
	for _, l := range strings.Split(txt, "\n") {
		if n := len([]rune(l)); n > reportWidth {
			t.Errorf("line of %d columns: %q", n, l)
		}
	}
	none := DiagnosisReport{Diagnosis: diagnose(Evidence{}), Evidence: Evidence{}}.Text()
	if !strings.Contains(none, "Router pings: none yet") {
		t.Errorf("no gateway pings:\n%s", none)
	}
}
