package diag

import (
	"net/netip"
	"strings"
	"testing"
	"time"
)

var vNow = time.Date(2026, 10, 26, 20, 0, 0, 0, time.UTC)

func wifiLink() *Route { return &Route{Index: 7, Name: "WLAN", Kind: LinkWiFi} }
func cableLink() *Route {
	return &Route{Index: 3, Name: "Ethernet", Kind: LinkEthernet}
}

// bloatOn is a graded test on link, finished ago, whose idle phase pinged
// the router sent times with lost unanswered and the given p95.
func bloatOn(link *Route, ago time.Duration, grade string, sent, lost int, p95 float64) *BloatResult {
	return &BloatResult{
		FinishedAt: vNow.Add(-ago), Grade: grade, Interface: link.Name, Link: link.Kind,
		IdleMS: ptr(18.0), DownIncreaseMS: ptr(40.0), UpIncreaseMS: ptr(12.0),
		Gateway: "Gateway 192.168.1.1 (ICMP)",
		Phases:  []PhaseResult{{Phase: PhaseIdle, Gateway: &LatencyStats{Sent: sent, Lost: lost, P50: ptr(2.0), P95: ptr(p95)}}},
	}
}

// traceWith builds a reached trace of n hops whose persisting loss starts
// at hop from (0: none), losing dest percent at the destination.
func traceWith(n, from int, dest float64) *TraceResult {
	t := &TraceResult{FinishedAt: vNow.Add(-10 * time.Minute), Reached: true, Rounds: 200, Planned: 200, Host: "Cloudflare DNS", Target: "198.51.100.0", Mode: TraceICMP}
	for i := 1; i <= n; i++ {
		h := TraceHop{TTL: i, Addr: "198.51.100.0", Sent: 200, Recv: 200, Probe: "icmp"}
		if i == 1 {
			h.Addr, h.Private = "192.168.1.1", true
		}
		if from > 0 && i >= from {
			h.LossPct, h.Recv = dest, 200-int(dest*2)
		}
		t.Hops = append(t.Hops, h)
	}
	t.DestLossPct = ptr(dest)
	if from > 0 {
		t.LossFrom = ptr(from)
	}
	return t
}

func boostPath(direct, relay PathStats, serverMS *float64) *PathEvidence {
	return &PathEvidence{Source: PathBoost, At: vNow, Game: "Example Game", Direct: &direct, Relay: &relay, ServerMS: serverMS}
}

func diagnose(ev Evidence) Diagnosis {
	ev.At = vNow
	return NewVerdictEngine(VerdictConfig{}).Diagnose(ev)
}

func check(t *testing.T, d Diagnosis, rule string) Check {
	t.Helper()
	for _, c := range d.Checks {
		if c.Rule == rule {
			return c
		}
	}
	t.Fatalf("no %s check in %+v", rule, d.Checks)
	return Check{}
}

func wantVerdict(t *testing.T, d Diagnosis, id string, conf Confidence) {
	t.Helper()
	if d.Verdict.ID != id || d.Verdict.Confidence != conf {
		t.Fatalf("verdict = %s (%s), want %s (%s): %s / %v\nchecks: %+v", d.Verdict.ID, d.Verdict.Confidence, id, conf, d.Verdict.Title, d.Verdict.Facts, d.Checks)
	}
}

func TestVerdictNoEvidence(t *testing.T) {
	d := diagnose(Evidence{})
	wantVerdict(t, d, VerdictUnknown, ConfidenceLow)
	if len(d.Checks) != 5 {
		t.Fatalf("checks = %d, want 5", len(d.Checks))
	}
	for _, c := range d.Checks {
		if c.Status != CheckUnknown || c.Summary == "" {
			t.Errorf("%s: %s %q, want unknown with a reason", c.Rule, c.Status, c.Summary)
		}
	}
	var ids []string
	for _, n := range d.Next {
		ids = append(ids, n.ID)
	}
	if strings.Join(ids, ",") != "bloat,trace,measure" {
		t.Errorf("next = %v", ids)
	}
}

func TestVerdictWiFiFromRouterLoss(t *testing.T) {
	l := wifiLink()
	// 4 of 50 router pings lost while the line was idle.
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	// No signal reading: Wi-Fi, but not more than medium.
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
	if d.Verdict.Segment != SegmentHome || d.Verdict.LowpathHelps {
		t.Errorf("segment %q helps %v", d.Verdict.Segment, d.Verdict.LowpathHelps)
	}
	facts := strings.Join(d.Verdict.Facts, "\n")
	for _, want := range []string{"4 of 50 pings lost (8.0%)", "doesn't read the Wi-Fi signal"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts lack %q:\n%s", want, facts)
		}
	}
	if d.Interface != "WLAN" || d.Link != LinkWiFi {
		t.Errorf("link %q %q", d.Link, d.Interface)
	}

	weak := -76
	d = diagnose(Evidence{Link: l, WiFi: &WiFiSignal{RSSI: &weak, Band: "5"}, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceHigh)
	d = diagnose(Evidence{Link: l, WiFi: &WiFiSignal{Band: "2.4"}, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceHigh)

	// A strong 5 GHz signal points at the router or the network instead.
	strong := -52
	d = diagnose(Evidence{Link: l, WiFi: &WiFiSignal{RSSI: &strong, Band: "5"}, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 4, 6)})
	wantVerdict(t, d, VerdictHome, ConfidenceMedium)
}

func TestVerdictHomeOnCable(t *testing.T) {
	l := cableLink()
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A+", 50, 3, 4)})
	wantVerdict(t, d, VerdictHome, ConfidenceHigh)
	if !strings.Contains(d.Verdict.Detail, "cable") {
		t.Errorf("detail %q", d.Verdict.Detail)
	}
}

func TestVerdictRouterNoise(t *testing.T) {
	l := cableLink()
	// One lost ping is 2% of 50, but one probe proves nothing.
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 1, 4)})
	if c := check(t, d, ruleHome); c.Status != CheckClear {
		t.Fatalf("home = %s %q", c.Status, c.Summary)
	}
	// A router that ignores ping isn't losing anything.
	d = diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 50, 4)})
	if c := check(t, d, ruleHome); c.Status != CheckUnknown || !strings.Contains(strings.Join(c.Facts, " "), "ignore ping") {
		t.Fatalf("home = %s %q %v", c.Status, c.Summary, c.Facts)
	}
	// Slow replies alone are less certain: routers answer ping at low priority.
	d = diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 0, 40)})
	wantVerdict(t, d, VerdictHome, ConfidenceMedium)
}

func TestVerdictGatewayPingerWins(t *testing.T) {
	l := wifiLink()
	g := &GatewaySample{Source: "gateway pings", At: vNow.Add(-time.Minute), Sent: 400, Lost: 8, P50MS: ptr(3.0), TailMS: ptr(30.0), TailPct: 99}
	// The bufferbloat test's router pings are clean, the dedicated ones not.
	d := diagnose(Evidence{Link: l, Gateway: g, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 0, 4)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
	if !strings.Contains(strings.Join(d.Verdict.Facts, " "), "8 of 400") {
		t.Errorf("facts %v", d.Verdict.Facts)
	}
}

func TestVerdictBloatOnAnotherConnection(t *testing.T) {
	// Tested on Wi-Fi, now on a cable: the old test says nothing about this link.
	d := diagnose(Evidence{Link: cableLink(), Bloat: bloatOn(wifiLink(), 5*time.Minute, "F", 50, 10, 60)})
	wantVerdict(t, d, VerdictUnknown, ConfidenceLow)
	for _, r := range []string{ruleHome, ruleLine} {
		if c := check(t, d, r); c.Status != CheckUnknown || !strings.Contains(c.Summary, "Wi-Fi (WLAN)") {
			t.Errorf("%s = %s %q", r, c.Status, c.Summary)
		}
	}
}

func TestVerdictEvidenceAge(t *testing.T) {
	l := cableLink()
	// Two hours old: still used, one step less sure.
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 2*time.Hour, "D", 50, 0, 4)})
	wantVerdict(t, d, VerdictLine, ConfidenceMedium)
	// Three days old: ignored.
	d = diagnose(Evidence{Link: l, Bloat: bloatOn(l, 72*time.Hour, "D", 50, 0, 4)})
	if c := check(t, d, ruleLine); c.Status != CheckUnknown || !strings.Contains(c.Summary, "3 days old") {
		t.Fatalf("line = %s %q", c.Status, c.Summary)
	}
	// Unknown connection: used, but less sure, and the link kind comes from the test.
	d = diagnose(Evidence{Bloat: bloatOn(wifiLink(), 5*time.Minute, "A", 50, 5, 4)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
}

func TestVerdictOverloadedLine(t *testing.T) {
	l := cableLink()
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "C", 50, 0, 4)})
	wantVerdict(t, d, VerdictLine, ConfidenceHigh)
	if !strings.Contains(d.Verdict.Detail, "grade C") || !strings.Contains(strings.Join(d.Verdict.Facts, " "), "+40 ms downloading") {
		t.Errorf("verdict %q %v", d.Verdict.Detail, d.Verdict.Facts)
	}
	for _, g := range []string{"A+", "A", "B"} {
		d = diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, g, 50, 0, 4)})
		if c := check(t, d, ruleLine); c.Status != CheckClear {
			t.Errorf("grade %s: line = %s", g, c.Status)
		}
	}
	// The passive detector during a boost, with no test at all.
	p := &PassiveState{Active: true, Watching: true, Direction: "download", RTTMS: ptr(61.0), BaselineMS: ptr(22.0),
		OtherDownMbps: ptr(88.0), LineDownMbps: ptr(100.0)}
	d = diagnose(Evidence{Link: l, Passive: p})
	wantVerdict(t, d, VerdictLine, ConfidenceHigh)
	if f := strings.Join(d.Verdict.Facts, " "); !strings.Contains(f, "88 of 100 Mbit/s downloading") || !strings.Contains(f, "from 22 to 61 ms") {
		t.Errorf("facts %q", f)
	}
	d = diagnose(Evidence{Link: l, Passive: &PassiveState{Watching: true}})
	if c := check(t, d, ruleLine); c.Status != CheckClear {
		t.Errorf("quiet boost: line = %s", c.Status)
	}
	// A test without a grade says why.
	d = diagnose(Evidence{Link: l, Bloat: &BloatResult{FinishedAt: vNow, Interface: l.Name, Link: l.Kind, Note: "Nothing answered the latency probes."}})
	if c := check(t, d, ruleLine); c.Status != CheckUnknown || !strings.Contains(c.Summary, "Nothing answered") {
		t.Errorf("no grade: line = %s %q", c.Status, c.Summary)
	}
}

func TestVerdictFirstRuleWins(t *testing.T) {
	l := wifiLink()
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "F", 50, 6, 30), Trace: traceWith(9, 5, 4)})
	wantVerdict(t, d, VerdictWiFi, ConfidenceMedium)
	// The other problems still show in their rows.
	for _, r := range []string{ruleHome, ruleLine, ruleISP} {
		if c := check(t, d, r); c.Status != CheckFound {
			t.Errorf("%s = %s", r, c.Status)
		}
	}
}

func TestVerdictISPRoute(t *testing.T) {
	l := cableLink()
	clean := bloatOn(l, 5*time.Minute, "A", 50, 0, 3)
	tr := traceWith(10, 5, 3.5)
	tr.Hops[4].Name = "core1.example.net"
	d := diagnose(Evidence{Link: l, Bloat: clean, Trace: tr})
	wantVerdict(t, d, VerdictISP, ConfidenceHigh)
	if !d.Verdict.LowpathHelps || d.Verdict.Segment != SegmentISP {
		t.Errorf("helps %v segment %q", d.Verdict.LowpathHelps, d.Verdict.Segment)
	}
	if f := d.Verdict.Facts[0]; !strings.Contains(f, "hop 5 (core1.example.net)") || !strings.Contains(f, "3.5%") {
		t.Errorf("fact %q", f)
	}
	found := false
	for _, n := range d.Next {
		found = found || n.ID == NextMeasure
	}
	if !found {
		t.Errorf("next %v lacks a boost comparison", d.Next)
	}

	// A short trace is less sure.
	short := traceWith(10, 5, 3.5)
	short.Rounds, short.Stopped = 30, true
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: short})
	wantVerdict(t, d, VerdictISP, ConfidenceMedium)

	// Loss from hop 1 is the home network's.
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: traceWith(10, 1, 3)})
	wantVerdict(t, d, VerdictHome, ConfidenceHigh)
	if c := check(t, d, ruleISP); c.Status != CheckUnknown {
		t.Errorf("isp with home loss = %s", c.Status)
	}

	// Only the destination loses replies: the route is clean.
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: traceWith(10, 10, 5)})
	if c := check(t, d, ruleISP); c.Status != CheckClear {
		t.Errorf("destination-only loss: isp = %s %q", c.Status, c.Summary)
	}
	// Not reached: can't be confirmed.
	nr := traceWith(6, 0, 0)
	nr.Reached, nr.DestLossPct = false, nil
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: nr})
	if c := check(t, d, ruleISP); c.Status != CheckUnknown {
		t.Errorf("unreached: isp = %s", c.Status)
	}
	// UDP trace losing only at the relay's port: the relay's link.
	udp := traceWith(8, 0, 4)
	udp.Mode, udp.LossFrom, udp.End = TraceUDP, ptr(0), &TraceHop{Probe: "udp", Sent: 200, Recv: 192, LossPct: 4}
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: udp})
	wantVerdict(t, d, VerdictRelay, ConfidenceHigh)
	if d.Verdict.Segment != SegmentLowpath {
		t.Errorf("segment %q", d.Verdict.Segment)
	}
}

func TestVerdictISPFromBoost(t *testing.T) {
	p := boostPath(PathStats{RTTMS: 40, LossPct: 6, JitterMS: 4, Samples: 50}, PathStats{RTTMS: 12, LossPct: 0, JitterMS: 1, Samples: 50}, ptr(25.0))
	d := diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictISP, ConfidenceMedium)
	if f := strings.Join(d.Verdict.Facts, " "); !strings.Contains(f, "During the Example Game boost") || !strings.Contains(f, "ICMP") {
		t.Errorf("facts %q", f)
	}
	// From an analysis instead of a live boost: less sure.
	p.Source = PathAnalysis
	d = diagnose(Evidence{Link: cableLink(), Analysis: p})
	wantVerdict(t, d, VerdictISP, ConfidenceLow)
	// Too few samples say nothing.
	p = boostPath(PathStats{RTTMS: 40, LossPct: 6, Samples: 5}, PathStats{RTTMS: 12, Samples: 5}, ptr(25.0))
	d = diagnose(Evidence{Link: cableLink(), Boost: p})
	if c := check(t, d, ruleISP); c.Status != CheckUnknown {
		t.Errorf("few samples: isp = %s", c.Status)
	}
}

func TestVerdictGameServer(t *testing.T) {
	relay := PathStats{RTTMS: 10, LossPct: 0.2, JitterMS: 1, Samples: 60}
	p := boostPath(PathStats{RTTMS: 35, LossPct: 0.5, JitterMS: 2, Samples: 60}, relay, ptr(22.0))
	p.ServerLossPct, p.ServerJitterMS = ptr(4.0), ptr(3.0)
	// Direct is clean too, but the server leg's own loss is what counts here.
	p.Direct = nil
	d := diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictServer, ConfidenceHigh)
	if d.Verdict.LowpathHelps {
		t.Error("a relay can't fix the server")
	}

	// Both routes struggle to get answers from the server: a hint.
	p = boostPath(PathStats{RTTMS: 35, LossPct: 8, JitterMS: 6, Samples: 60}, relay, nil)
	d = diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictServer, ConfidenceLow)

	// A server answering the PC steadily is clear.
	p = boostPath(PathStats{RTTMS: 35, LossPct: 0, JitterMS: 2, Samples: 60}, relay, ptr(22.0))
	d = diagnose(Evidence{Link: cableLink(), Boost: p})
	if c := check(t, d, ruleServer); c.Status != CheckClear {
		t.Errorf("server = %s %q", c.Status, c.Summary)
	}
	// A trace where only the server drops replies: noted, not blamed.
	tr := traceWith(9, 9, 6)
	d = diagnose(Evidence{Link: cableLink(), Trace: tr, TraceKind: TraceKindServer})
	c := check(t, d, ruleServer)
	if c.Status != CheckUnknown || !strings.Contains(strings.Join(c.Facts, " "), "limit ping") {
		t.Errorf("server = %s %v", c.Status, c.Facts)
	}
}

func TestVerdictAlreadyOptimal(t *testing.T) {
	l := cableLink()
	clean := bloatOn(l, 5*time.Minute, "A+", 50, 0, 3)
	// Direct 21 ms vs relay 15 + 5 = 20 ms: within 2 ms, no lossier.
	p := boostPath(PathStats{RTTMS: 21, LossPct: 0, JitterMS: 1, Samples: 60}, PathStats{RTTMS: 15, LossPct: 0, JitterMS: 1, Samples: 60}, ptr(5.0))
	d := diagnose(Evidence{Link: l, Bloat: clean, Trace: traceWith(8, 0, 0), Boost: p})
	wantVerdict(t, d, VerdictOptimal, ConfidenceMedium)
	if !strings.Contains(strings.Join(d.Verdict.Facts, " "), "direct 21 ms with 0.0% loss, the relay route 20 ms") {
		t.Errorf("facts %v", d.Verdict.Facts)
	}
	p.SameMethod = true
	d = diagnose(Evidence{Link: l, Bloat: clean, Boost: p})
	wantVerdict(t, d, VerdictOptimal, ConfidenceHigh)

	// The relay is clearly better: Lowpath helps, nothing wrong found.
	p = boostPath(PathStats{RTTMS: 48, LossPct: 0.5, JitterMS: 3, Samples: 60}, PathStats{RTTMS: 15, LossPct: 0, JitterMS: 1, Samples: 60}, ptr(12.0))
	d = diagnose(Evidence{Link: l, Bloat: clean, Trace: traceWith(8, 0, 0), Boost: p})
	if c := check(t, d, ruleOptimal); c.Status != CheckClear {
		t.Fatalf("optimal = %s %q", c.Status, c.Summary)
	}
	// Home, line, ISP and server all measured and fine.
	wantVerdict(t, d, VerdictClean, ConfidenceHigh)

	// Without the relay's server leg, "direct is close enough" can still be
	// said (the relay route is at least its first leg), "relay is better"
	// can't.
	p = boostPath(PathStats{RTTMS: 16, Samples: 60}, PathStats{RTTMS: 15, Samples: 60}, nil)
	d = diagnose(Evidence{Link: l, Boost: p})
	wantVerdict(t, d, VerdictOptimal, ConfidenceMedium)
	p = boostPath(PathStats{RTTMS: 40, Samples: 60}, PathStats{RTTMS: 15, Samples: 60}, nil)
	d = diagnose(Evidence{Link: l, Boost: p})
	if c := check(t, d, ruleOptimal); c.Status != CheckUnknown {
		t.Errorf("no server leg: optimal = %s", c.Status)
	}
	// An old analysis is a weak basis.
	p = boostPath(PathStats{RTTMS: 21, Samples: 60}, PathStats{RTTMS: 15, Samples: 60}, ptr(5.0))
	p.Source, p.At = PathAnalysis, vNow.Add(-3*time.Hour)
	d = diagnose(Evidence{Link: l, Analysis: p})
	wantVerdict(t, d, VerdictOptimal, ConfidenceLow)
	p.At = vNow.Add(-30 * time.Hour)
	d = diagnose(Evidence{Link: l, Analysis: p})
	wantVerdict(t, d, VerdictUnknown, ConfidenceLow)
}

func TestVerdictCleanConfidence(t *testing.T) {
	l := cableLink()
	d := diagnose(Evidence{Link: l, Bloat: bloatOn(l, 5*time.Minute, "A", 50, 0, 3)})
	// Home and line measured: two of four.
	wantVerdict(t, d, VerdictClean, ConfidenceMedium)
	if d.Verdict.Advice == "" {
		t.Error("a partial clean verdict should say what is unmeasured")
	}
	d = diagnose(Evidence{Link: l, Passive: &PassiveState{Watching: true}})
	wantVerdict(t, d, VerdictClean, ConfidenceLow)
}

func TestDiagnosisReport(t *testing.T) {
	l := wifiLink()
	loc := Location{State: StateOK, IPv4: &Endpoint{IP: netip.MustParseAddr("203.0.113.77"), NetworkInfo: NetworkInfo{ASN: 64500, ISP: "Example Broadband", City: "Amsterdam", Country: "NL"}}}
	tr := traceWith(10, 5, 3.5)
	tr.Hops[4].Name = "203-0-113-9.example.net"
	tr.Verdict = "Loss starts at hop 5 (203-0-113-9.example.net) and persists to the destination: 3.5% of probes are lost there."
	ev := Evidence{At: vNow, Link: l, Bloat: bloatOn(l, 5*time.Minute, "C", 50, 4, 6), Trace: ptr(tr.Redacted())}
	d := NewVerdictEngine(VerdictConfig{}).Diagnose(ev)
	r := DiagnosisReport{Version: "0.2.0", OS: "windows", Diagnosis: d, Evidence: ev, Location: ptr(loc.Redacted())}
	txt := r.Text()
	for _, want := range []string{
		"Lowpath diagnosis", "Generated 2026-10-26T20:00:00Z by Lowpath 0.2.0 on windows.",
		"Connection: Wi-Fi (WLAN).", "Network: Example Broadband (AS64500, 203.0.113.0); Amsterdam (NL).",
		"VERDICT: Your Wi-Fi is the problem (confidence: medium)",
		"[problem] Wi-Fi and home network", "[problem] Internet line under load", "[problem] ISP route",
		"[not measured] Relay or direct", "Bufferbloat test (5 min ago, Wi-Fi (WLAN)): grade C",
		"Route trace to Cloudflare DNS", "FOR A SURER ANSWER",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("report lacks %q:\n%s", want, txt)
		}
	}
	for _, leak := range []string{"203.0.113.77", "203-0-113-9", "example.net"} {
		if strings.Contains(txt, leak) {
			t.Errorf("report leaks %q:\n%s", leak, txt)
		}
	}
	for i, ln := range strings.Split(txt, "\n") {
		if n := len([]rune(ln)); n > reportWidth {
			t.Errorf("line %d is %d wide: %q", i+1, n, ln)
		}
	}
}

func TestWrap(t *testing.T) {
	var b strings.Builder
	wrap(&b, "  - ", strings.Repeat("word ", 40))
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "  - word") || !strings.HasPrefix(lines[1], "    word") {
		t.Fatalf("wrap:\n%s", b.String())
	}
	for _, l := range lines {
		if len(l) > reportWidth {
			t.Errorf("%d wide: %q", len(l), l)
		}
	}
	b.Reset()
	wrap(&b, "", "")
	if b.String() != "\n" {
		t.Errorf("empty = %q", b.String())
	}
}
