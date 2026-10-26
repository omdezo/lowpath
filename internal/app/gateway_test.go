package app

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
)

func getGateway(t *testing.T, get func(string) (int, string)) diag.GatewayView {
	t.Helper()
	code, body := get("/api/diag/gateway")
	if code != 200 {
		t.Fatalf("GET gateway: %d %s", code, body)
	}
	var v diag.GatewayView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// waitGateway polls the pinger, as the Diagnostics tab does, until ok.
func waitGateway(t *testing.T, get func(string) (int, string), ok func(diag.GatewayView) bool) diag.GatewayView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := getGateway(t, get)
		if ok(v) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway view never got there: %+v", v)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestGatewayAPIFeedsTheVerdict(t *testing.T) {
	line := newFakeHomeLine() // Wi-Fi, router 192.168.1.1 answering in 2 ms
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
		c.GatewayInterval = 2 * time.Millisecond
	})
	get := func(path string) (int, string) { return do(t, ts, "GET", path, "", nil) }

	// Nothing pings the router until someone looks.
	time.Sleep(50 * time.Millisecond)
	line.mu.Lock()
	n := line.pinged[line.gw]
	line.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d router pings before anyone looked", n)
	}
	if v := getGateway(t, get); v.State != diag.GatewayMeasuring {
		t.Errorf("first look: %s", v.State)
	}
	v := waitGateway(t, get, func(v diag.GatewayView) bool { return v.State == diag.GatewayOn && v.Stats.Sent >= 30 })
	if v.Gateway != "192.168.1.1" || v.Link != diag.LinkWiFi || *v.Stats.P50MS != 2 || v.Stats.Lost != 0 {
		t.Fatalf("view %+v stats %+v", v, *v.Stats)
	}

	d := getDiagnosis(t, get)
	home := d.Checks[0]
	if home.Rule != "home" || home.Status != diag.CheckClear || !strings.Contains(strings.Join(home.Facts, " "), "live router pings") {
		t.Fatalf("home check %+v", home)
	}
	if len(d.Segments) != 4 || d.Segments[0].ID != diag.SegmentHome || d.Segments[0].Status != diag.SegmentGood || d.Segments[0].RTTMS == nil {
		t.Fatalf("segments %+v", d.Segments)
	}
	_, report := get("/api/diag/verdict/report")
	if !strings.Contains(report, "ROUTE, PART BY PART") || !strings.Contains(report, "Router pings (live router pings") {
		t.Errorf("report:\n%s", report)
	}
	_, export := get("/api/diagnostics")
	if !strings.Contains(export, `"gateway":{"state":"on"`) {
		t.Errorf("diagnostics export lacks the gateway: %.300s", export)
	}

	// The bufferbloat test pauses it: the test pings the router itself.
	srv.bloat.tune = fastBloat
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", map[string]string{"X-Lowpath": "1"}); code != 202 {
		t.Fatalf("start bloat: %d %s", code, body)
	}
	v = waitGateway(t, get, func(v diag.GatewayView) bool { return v.State == diag.GatewayPaused })
	if !strings.Contains(v.Message, "bufferbloat test") {
		t.Errorf("paused: %q", v.Message)
	}
	waitGateway(t, get, func(v diag.GatewayView) bool { return v.State == diag.GatewayOn })
}
