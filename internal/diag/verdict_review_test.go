package diag

import (
	"strings"
	"testing"
)

// A trace taken on Wi-Fi says nothing about the cable plugged in since:
// the field test of #14 (Diagnostics on Wi-Fi, then on Ethernet).
func TestVerdictTraceOnAnotherConnection(t *testing.T) {
	tr := traceWith(8, 1, 5) // loss from the router on
	tr.Interface, tr.Link = "WLAN", LinkWiFi
	d := diagnose(Evidence{Link: wifiLink(), Trace: tr})
	if d.Verdict.ID != VerdictWiFi {
		t.Fatalf("on the trace's own Wi-Fi: verdict %s, want %s", d.Verdict.ID, VerdictWiFi)
	}
	d = diagnose(Evidence{Link: cableLink(), Trace: tr})
	if d.Verdict.ID == VerdictHome || d.Verdict.ID == VerdictWiFi {
		t.Fatalf("on a cable, a Wi-Fi trace still blames the home network: %s %v", d.Verdict.Title, d.Verdict.Facts)
	}
	c := check(t, d, ruleISP)
	if c.Status != CheckUnknown || !strings.Contains(c.Summary, "ran on Wi-Fi (WLAN)") {
		t.Errorf("isp = %s %q", c.Status, c.Summary)
	}
	// A trace saved before traces recorded their connection is still used.
	tr.Interface, tr.Link = "", ""
	if d = diagnose(Evidence{Link: cableLink(), Trace: tr}); d.Verdict.ID != VerdictHome {
		t.Errorf("trace without a connection: verdict %s, want %s", d.Verdict.ID, VerdictHome)
	}
}

func TestVerdictOffline(t *testing.T) {
	ev := Evidence{Offline: true, Bloat: bloatOn(wifiLink(), 0, "A", 50, 0, 3), Trace: traceWith(8, 0, 0)}
	d := diagnose(ev)
	wantVerdict(t, d, VerdictOffline, ConfidenceHigh)
	if len(d.Checks) != 5 {
		t.Errorf("checks = %d, want the 5 rows from the earlier tests", len(d.Checks))
	}
	if len(d.Next) != 0 {
		t.Errorf("offline, no test can run, yet next = %+v", d.Next)
	}
	if d.Verdict.LowpathHelps {
		t.Error("a relay can't help an offline PC")
	}
}

// Hop 2 in the shared address space is the ISP's carrier-grade NAT
// (RFC 6598), not a second router at home.
func TestVerdictCGNATHopIsTheISP(t *testing.T) {
	tr := traceWith(8, 2, 5)
	tr.Hops[1].Addr, tr.Hops[1].Private = "100.64.12.0", true
	d := diagnose(Evidence{Link: cableLink(), Trace: tr})
	wantVerdict(t, d, VerdictISP, ConfidenceHigh)

	// A private second hop outside it is still a router at home.
	tr.Hops[1].Addr = "192.168.0.0"
	d = diagnose(Evidence{Link: cableLink(), Trace: tr})
	if d.Verdict.ID != VerdictHome {
		t.Errorf("192.168.0.x at hop 2: verdict %s, want %s", d.Verdict.ID, VerdictHome)
	}
}

func TestTraceHopInHome(t *testing.T) {
	for _, tc := range []struct {
		h    TraceHop
		want bool
	}{
		{TraceHop{TTL: 1, Addr: "100.64.0.1", Private: true}, true},
		{TraceHop{TTL: 2, Addr: "10.0.0.1", Private: true}, true},
		{TraceHop{TTL: 2, Addr: "100.100.0.0", Private: true}, false},
		{TraceHop{TTL: 2, Addr: "198.51.100.1"}, false},
		{TraceHop{TTL: 3, Addr: "192.168.2.1", Private: true}, false},
		{TraceHop{TTL: 0, Addr: "192.168.2.1", Private: true}, false},
	} {
		if got := tc.h.inHome(); got != tc.want {
			t.Errorf("%+v: inHome = %v, want %v", tc.h, got, tc.want)
		}
	}
}

func TestVerdictISPAdviceDuringBoost(t *testing.T) {
	p := boostPath(PathStats{RTTMS: 40, LossPct: 6, JitterMS: 4, Samples: 50}, PathStats{RTTMS: 12, JitterMS: 1, Samples: 50}, ptr(25.0))
	d := diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictISP, ConfidenceMedium)
	if !strings.Contains(d.Verdict.Advice, "Keep the boost on") {
		t.Errorf("advice during a boost: %q", d.Verdict.Advice)
	}
	for _, n := range d.Next {
		if strings.Contains(n.Text, "Boost the game") {
			t.Errorf("next step during a boost: %q", n.Text)
		}
	}
	// A trace's ISP verdict while boosting: the same.
	d = diagnose(Evidence{Link: cableLink(), Trace: traceWith(8, 4, 5), Boost: boostPath(PathStats{Samples: 5}, PathStats{Samples: 5}, nil)})
	wantVerdict(t, d, VerdictISP, ConfidenceHigh)
	if !strings.Contains(d.Verdict.Advice, "Keep the boost on") {
		t.Errorf("trace advice during a boost: %q", d.Verdict.Advice)
	}
	// Without a boost: start one.
	d = diagnose(Evidence{Link: cableLink(), Trace: traceWith(8, 4, 5)})
	if !strings.Contains(d.Verdict.Advice, "Boost the game") {
		t.Errorf("advice without a boost: %q", d.Verdict.Advice)
	}
}

// Direct lossy, relay leg clean, relay->server lossy: the loss is at the
// server's end both routes share, so it is not the ISP's route.
func TestVerdictLossyServerLegIsNotTheISP(t *testing.T) {
	p := boostPath(PathStats{RTTMS: 40, LossPct: 6, JitterMS: 4, Samples: 50}, PathStats{RTTMS: 12, LossPct: 0.2, JitterMS: 1, Samples: 50}, ptr(25.0))
	p.ServerLossPct = ptr(5.0)
	d := diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictServer, ConfidenceHigh)
	if c := check(t, d, ruleISP); c.Status == CheckFound {
		t.Errorf("isp = %s %q", c.Status, c.Summary)
	}
}

// The relay reporting no time to the server is all that is known: the
// hint says so, and doesn't claim that nobody can fix it.
func TestVerdictServerHintWording(t *testing.T) {
	p := boostPath(PathStats{RTTMS: 35, LossPct: 8, JitterMS: 6, Samples: 60}, PathStats{RTTMS: 10, LossPct: 0.2, JitterMS: 1, Samples: 60}, nil)
	d := diagnose(Evidence{Link: cableLink(), Boost: p})
	wantVerdict(t, d, VerdictServer, ConfidenceLow)
	all := d.Verdict.Title + " " + d.Verdict.Detail + " " + strings.Join(d.Verdict.Facts, " ")
	for _, bad := range []string{"no steady answer", "Neither you nor Lowpath"} {
		if strings.Contains(all, bad) {
			t.Errorf("hint claims %q: %s", bad, all)
		}
	}
	if !strings.Contains(all, "reports no time") || !strings.Contains(d.Verdict.Title, "may be") {
		t.Errorf("hint wording: %s", all)
	}
}
