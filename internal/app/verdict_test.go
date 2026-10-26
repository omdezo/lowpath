package app

import (
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/engine"
)

func newVerdictServer(t *testing.T) (*Server, func(path string) (int, string)) {
	t.Helper()
	line := newFakeHomeLine() // Wi-Fi, router 192.168.1.1
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
	})
	return srv, func(path string) (int, string) { return do(t, ts, "GET", path, "", nil) }
}

func getDiagnosis(t *testing.T, get func(string) (int, string)) diag.Diagnosis {
	t.Helper()
	code, body := get("/api/diag/verdict")
	if code != 200 {
		t.Fatalf("GET verdict: %d %s", code, body)
	}
	var d diag.Diagnosis
	if err := json.Unmarshal([]byte(body), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// lossyWiFiBloat is a test on the fake line whose idle phase lost 5 of 50
// router pings.
func lossyWiFiBloat(at time.Time) diag.BloatResult {
	f := func(v float64) *float64 { return &v }
	return diag.BloatResult{
		StartedAt: at.Add(-20 * time.Second), FinishedAt: at, Grade: "A", Interface: "Wi-Fi", Link: diag.LinkWiFi,
		Target: "Relay Example 203.0.113.50:4000 (UDP probe)", Gateway: "Gateway 192.168.1.1 (ICMP)",
		IdleMS: f(21), DownIncreaseMS: f(9), UpIncreaseMS: f(4),
		Phases: []diag.PhaseResult{{Phase: diag.PhaseIdle, Gateway: &diag.LatencyStats{Sent: 50, Lost: 5, P50: f(2), P95: f(9)}}},
	}
}

func TestVerdictAPINothingMeasured(t *testing.T) {
	_, get := newVerdictServer(t)
	d := getDiagnosis(t, get)
	if d.Verdict.ID != diag.VerdictUnknown || len(d.Checks) != 5 || len(d.Next) == 0 {
		t.Fatalf("diagnosis: %+v", d)
	}
	if d.Link != diag.LinkWiFi || d.Interface != "Wi-Fi" {
		t.Errorf("link %q %q", d.Link, d.Interface)
	}
}

func TestVerdictAPIWiFiAndReport(t *testing.T) {
	srv, get := newVerdictServer(t)
	if err := srv.store.SetBloat(lossyWiFiBloat(time.Now().Add(-3 * time.Minute))); err != nil {
		t.Fatal(err)
	}
	d := getDiagnosis(t, get)
	if d.Verdict.ID != diag.VerdictWiFi || d.Verdict.Confidence != diag.ConfidenceMedium {
		t.Fatalf("verdict %s (%s): %v", d.Verdict.ID, d.Verdict.Confidence, d.Verdict.Facts)
	}

	code, body := get("/api/diag/verdict/report")
	if code != 200 {
		t.Fatalf("report: %d %s", code, body)
	}
	for _, want := range []string{"Lowpath diagnosis", "VERDICT: Your Wi-Fi is the problem (confidence: medium)", "5 of 50 pings lost", "Connection: Wi-Fi."} {
		if !strings.Contains(body, want) {
			t.Errorf("report lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "203.0.113.50") {
		t.Errorf("report names the relay's address:\n%s", body)
	}

	// The JSON diagnostics export carries the same verdict.
	code, body = get("/api/diagnostics")
	var rep struct {
		Verdict diag.Diagnosis `json:"verdict"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &rep) != nil || rep.Verdict.Verdict.ID != diag.VerdictWiFi {
		t.Fatalf("diagnostics: %d %s", code, body)
	}
}

func TestVerdictAPIReportHeaders(t *testing.T) {
	line := newFakeHomeLine()
	ts, _ := newRealServer(t, newFake(t), func(c *Config) { c.NetStats = line })
	req, _ := http.NewRequest("GET", ts.URL+"/api/diag/verdict/report", nil)
	req.Header.Set("Cookie", sessionCookie+"="+testSession)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status %d", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type %q", ct)
	}
	if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "lowpath-diagnosis.txt") {
		t.Errorf("disposition %q", cd)
	}
}

func TestVerdictAPIISPRouteFromTrace(t *testing.T) {
	srv, get := newVerdictServer(t)
	// A clean router and line, then a trace to a game's server whose loss
	// starts at hop 3 and carries on.
	b := lossyWiFiBloat(time.Now().Add(-time.Minute))
	b.Phases[0].Gateway.Lost = 0
	if err := srv.store.SetBloat(b); err != nil {
		t.Fatal(err)
	}
	server := netip.MustParseAddrPort("198.51.100.20:27015")
	if err := srv.store.RecordServer("cs2", server); err != nil {
		t.Fatal(err)
	}
	f := func(v float64) *float64 { return &v }
	tr := diag.TraceResult{FinishedAt: time.Now().Add(-time.Minute), Target: "198.51.100.20", Host: "Counter-Strike 2 server", Mode: diag.TraceICMP,
		Rounds: 200, Planned: 200, Reached: true, DestLossPct: f(4), LossFrom: func() *int { v := 3; return &v }(),
		Hops: []diag.TraceHop{
			{TTL: 1, Addr: "192.168.1.1", Private: true, Sent: 200, Recv: 200},
			{TTL: 2, Addr: "198.51.100.1", Sent: 200, Recv: 200},
			{TTL: 3, Addr: "198.51.100.7", Name: "edge7.example.net", Sent: 200, Recv: 192, LossPct: 4},
			{TTL: 4, Addr: "198.51.100.20", Sent: 200, Recv: 192, LossPct: 4},
		}}
	if err := srv.store.SetTrace(tr); err != nil {
		t.Fatal(err)
	}
	if k := srv.traceKind("198.51.100.20"); k != diag.TraceKindServer {
		t.Errorf("trace kind %q", k)
	}
	d := getDiagnosis(t, get)
	if d.Verdict.ID != diag.VerdictISP || !d.Verdict.LowpathHelps {
		t.Fatalf("verdict %+v", d.Verdict)
	}
	// The trace is redacted before it reaches the verdict: no host names.
	if f := strings.Join(d.Verdict.Facts, " "); strings.Contains(f, "example.net") || !strings.Contains(f, "hop 3") {
		t.Errorf("facts %q", f)
	}
}

func TestVerdictTraceKind(t *testing.T) {
	srv, _ := newVerdictServer(t)
	for target, want := range map[string]string{"": diag.TraceKindPublic, "not an address": diag.TraceKindPublic, "192.0.2.99": diag.TraceKindPublic} {
		if k := srv.traceKind(target); k != want {
			t.Errorf("%q: %q, want %q", target, k, want)
		}
	}
	// The loopback test relay counts as a relay.
	if err := srv.relays.SetLoopback(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.relays.SetLoopback(false) })
	paths := srv.net.Paths("")
	if len(paths) == 0 {
		t.Fatal("no loopback relay")
	}
	if k := srv.traceKind(paths[0].Addr.Addr().String()); k != diag.TraceKindRelay {
		t.Errorf("relay: %q", k)
	}
}

func TestBoostEvidence(t *testing.T) {
	now := time.Now()
	snap := engine.Snapshot{
		Before: engine.Stats{RTT: 41, Loss: 3, Jitter: 2, Up: true, Samples: 50}, BeforeKnown: true,
		After: engine.Stats{RTT: 30, Server: 18, Loss: 0.5, Jitter: 1, Up: true, Samples: 50},
	}
	p := boostEvidence(snap, "Example Game", now)
	if p.Direct == nil || p.Direct.RTTMS != 41 || p.Direct.LossPct != 3 {
		t.Fatalf("direct %+v", p.Direct)
	}
	if p.Relay == nil || p.Relay.RTTMS != 12 || p.ServerMS == nil || *p.ServerMS != 18 {
		t.Fatalf("relay %+v server %v", p.Relay, p.ServerMS)
	}
	if p.SameMethod || p.Source != diag.PathBoost || p.Game != "Example Game" {
		t.Errorf("%+v", p)
	}
	// Only the latest server leg known.
	snap.After.Server, snap.ServerLeg, snap.ServerLegKnown = 0, 17, true
	if p := boostEvidence(snap, "", now); p.Relay.RTTMS != 30 || *p.ServerMS != 17 {
		t.Errorf("latest leg: relay %+v server %v", p.Relay, *p.ServerMS)
	}
	// The server ignores ping, and every relay is down (fallback).
	snap.BeforeKnown, snap.Fallback = false, true
	if p := boostEvidence(snap, "", now); p.Direct != nil || p.Relay != nil || p.ServerMS != nil {
		t.Errorf("fallback: %+v", p)
	}
}

func TestAnalysisEvidence(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	a := analysisResult{At: at, DirectKnown: true, Direct: engine.Stats{RTT: 25, Up: true, Samples: 30},
		Paths: map[string]engine.Stats{
			"far":  {RTT: 80, Server: 10, Up: true, Samples: 30},
			"near": {RTT: 24, Server: 9, Loss: 0, Up: true, Samples: 30},
			"down": {Up: false},
		}}
	p := analysisEvidence(a, "Example Game")
	if p.Source != diag.PathAnalysis || !p.At.Equal(at) || p.Direct == nil || p.Direct.RTTMS != 25 {
		t.Fatalf("%+v", p)
	}
	if p.Relay == nil || p.Relay.RTTMS != 15 || p.ServerMS == nil || *p.ServerMS != 9 {
		t.Fatalf("best relay %+v server %v", p.Relay, p.ServerMS)
	}
	a.DirectKnown, a.Paths = false, nil
	if p := analysisEvidence(a, ""); p.Direct != nil || p.Relay != nil {
		t.Errorf("nothing measured: %+v", p)
	}
}

func TestVerdictAPIAlreadyOptimalFromAnalysis(t *testing.T) {
	srv, get := newVerdictServer(t)
	srv.mu.Lock()
	srv.analysis["cs2"] = analysisResult{At: time.Now().Add(-time.Minute), DirectKnown: true,
		Direct: engine.Stats{RTT: 20, Jitter: 1, Up: true, Samples: 30},
		Paths:  map[string]engine.Stats{"r1": {RTT: 21, Server: 4, Jitter: 1, Up: true, Samples: 30}}}
	srv.mu.Unlock()
	d := getDiagnosis(t, get)
	if d.Verdict.ID != diag.VerdictOptimal {
		t.Fatalf("verdict %+v", d.Verdict)
	}
	if f := strings.Join(d.Verdict.Facts, " "); !strings.Contains(f, "Counter-Strike 2") {
		t.Errorf("facts %q", f)
	}
}
