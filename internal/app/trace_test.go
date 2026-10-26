package app

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/icmp"
)

// fakePath is a two-router route to any destination; the destination
// answers ping unless it is in silent.
type fakePath struct {
	silent map[netip.Addr]bool
	slow   time.Duration // every probe takes this long
}

var (
	traceHop1 = netip.MustParseAddr("192.168.1.1")
	traceHop2 = netip.MustParseAddr("198.51.100.1")
)

func (f *fakePath) ping(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error) {
	if f.slow > 0 {
		time.Sleep(min(f.slow, timeout))
	}
	switch {
	case dst.IsLoopback():
		return icmp.Reply{From: dst, RTT: time.Millisecond / 10, Kind: icmp.Echo}, nil
	case ttl == 1:
		return icmp.Reply{From: traceHop1, RTT: time.Millisecond, Kind: icmp.TimeExceeded}, nil
	case ttl == 2:
		return icmp.Reply{From: traceHop2, RTT: 7 * time.Millisecond, Kind: icmp.TimeExceeded}, nil
	case f.silent[dst]:
		return icmp.Reply{}, icmp.ErrTimeout
	}
	return icmp.Reply{From: dst, RTT: 15 * time.Millisecond, Kind: icmp.Echo}, nil
}

func noNames(context.Context, netip.Addr) string { return "" }

func newTraceServer(t *testing.T, path *fakePath, count int, interval time.Duration) (*httptest.Server, *Server) {
	t.Helper()
	return newRealServer(t, newFake(t), func(c *Config) {
		c.PingTTL, c.Names = path.ping, noNames
		c.TraceCount, c.TraceInterval = count, interval
	})
}

func traceStatus(t *testing.T, ts *httptest.Server) traceView {
	t.Helper()
	code, body := do(t, ts, "GET", "/api/diag/trace", "", nil)
	if code != 200 {
		t.Fatalf("GET trace: %d %s", code, body)
	}
	var v traceView
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitTrace(t *testing.T, ts *httptest.Server) traceView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := traceStatus(t, ts)
		if !v.Running {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("trace still running: %+v", v.Progress)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTraceAPIRunsAndSavesTheResult(t *testing.T) {
	ts, _ := newTraceServer(t, &fakePath{}, 30, 3*time.Millisecond)

	v := traceStatus(t, ts)
	if v.Running || v.Result != nil || v.Defaults.Count != 30 {
		t.Fatalf("before any trace: %+v", v)
	}
	if len(v.Targets) != 1 || v.Targets[0].ID != publicTraceTarget.ID || v.Targets[0].UDP {
		t.Fatalf("with no relays only the public target is offered: %+v", v.Targets)
	}
	if code, _ := do(t, ts, "GET", "/api/diag/trace/report", "", nil); code != 404 {
		t.Fatalf("report before any trace: %d", code)
	}
	code, body := do(t, ts, "POST", "/api/diag/trace", `{"host":" 203.0.113.9 ","mode":"icmp"}`, csrf)
	if code != 202 || !strings.Contains(body, `"running":true`) {
		t.Fatalf("start: %d %s", code, body)
	}
	v = waitTrace(t, ts)
	r := v.Result
	if r == nil || v.Error != "" {
		t.Fatalf("no result: %+v", v)
	}
	if !r.Reached || len(r.Hops) != 3 || r.Hops[2].Addr != "203.0.113.9" || r.Rounds != 30 || r.Host != "203.0.113.9" {
		t.Fatalf("result %+v", r)
	}
	if r.LossFrom != nil || !strings.Contains(r.Verdict, "clean") {
		t.Fatalf("verdict %q", r.Verdict)
	}

	code, rep := do(t, ts, "GET", "/api/diag/trace/report", "", nil)
	if code != 200 || !strings.Contains(rep, "Lowpath route trace to 203.0.113.9") || !strings.Contains(rep, "198.51.100.1") {
		t.Fatalf("report: %d\n%s", code, rep)
	}
	// The diagnostics report, which users post publicly, has no full
	// addresses.
	_, diagBody := do(t, ts, "GET", "/api/diagnostics", "", nil)
	if !strings.Contains(diagBody, `"route_trace"`) || strings.Contains(diagBody, "198.51.100.1\"") || strings.Contains(diagBody, "203.0.113.9") {
		t.Fatalf("diagnostics report leaks hop addresses: %s", diagBody)
	}
}

func TestTraceAPIUDPModeToRelay(t *testing.T) {
	ts, _ := newTraceServer(t, &fakePath{}, 20, 3*time.Millisecond)
	if code := call(t, ts, "PUT", "/api/settings", `{"loopback_relay":true}`, nil); code != 200 {
		t.Fatalf("enable loopback relay: %d", code)
	}
	v := traceStatus(t, ts)
	if len(v.Targets) != 2 || v.Targets[0].ID != "relay:"+loopbackID || !v.Targets[0].UDP || v.Targets[0].Addr != "127.0.0.1" {
		t.Fatalf("targets %+v", v.Targets)
	}
	code, body := do(t, ts, "POST", "/api/diag/trace", `{"target_id":"relay:loopback","mode":"udp"}`, csrf)
	if code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	r := waitTrace(t, ts).Result
	if r == nil || r.Mode != diag.TraceUDP || r.End == nil || r.End.Recv == 0 || !r.Reached {
		t.Fatalf("UDP trace to the loopback relay: %+v", r)
	}
	if !strings.Contains(r.EndName, "Loopback test relay") || r.Host != "Loopback test relay" {
		t.Fatalf("names: %q %q", r.EndName, r.Host)
	}
}

func TestTraceAPIRejectsBadRequests(t *testing.T) {
	ts, _ := newTraceServer(t, &fakePath{}, 5, time.Millisecond)
	for _, body := range []string{
		`{}`,
		`{"host":"   "}`,
		`{"host":"203.0.113.9","mode":"tcp"}`,
		`{"host":"203.0.113.9","mode":"udp"}`,
		`{"target_id":"cloudflare","mode":"udp"}`,
		`{"target_id":"relay:gone"}`,
		`{"host":"224.0.0.1"}`,
		`{"host":"0.0.0.0"}`,
		`{"host":"255.255.255.255"}`,
		`{"host":"no spaces allowed"}`,
		`{"host":"http://example.com/x"}`,
		`not json`,
	} {
		if code, resp := do(t, ts, "POST", "/api/diag/trace", body, csrf); code != 400 {
			t.Errorf("%s: %d %s, want 400", body, code, resp)
		}
	}
	if traceStatus(t, ts).Running {
		t.Fatal("a rejected request started a trace")
	}
}

func TestTraceAPIStopKeepsPartialResult(t *testing.T) {
	ts, _ := newTraceServer(t, &fakePath{}, 1000, 20*time.Millisecond)
	if code, body := do(t, ts, "POST", "/api/diag/trace", `{"target_id":"cloudflare"}`, csrf); code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/diag/trace", `{"target_id":"cloudflare"}`, csrf); code != 409 || !strings.Contains(body, "trace_running") {
		t.Fatalf("second start: %d %s", code, body)
	}
	// The bufferbloat test would fill the line under the trace.
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 409 || !strings.Contains(body, "trace_running") {
		t.Fatalf("bufferbloat test during a trace: %d %s", code, body)
	}
	time.Sleep(150 * time.Millisecond)
	if p := traceStatus(t, ts).Progress; p == nil || !p.Running || p.Target != "1.1.1.1" {
		t.Fatalf("progress %+v", p)
	}
	code, body := do(t, ts, "DELETE", "/api/diag/trace", "", csrf)
	if code != 200 || !strings.Contains(body, `"running":false`) {
		t.Fatalf("stop: %d %s", code, body)
	}
	r := traceStatus(t, ts).Result
	if r == nil || !r.Stopped || r.Rounds == 0 || r.Rounds >= 1000 || r.Host != "Cloudflare DNS" {
		t.Fatalf("partial result %+v", r)
	}
}

func TestTraceAPIRefusedDuringBloatTest(t *testing.T) {
	line := newFakeHomeLine()
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
		c.PingTTL, c.Names = (&fakePath{}).ping, noNames
	})
	srv.bloat.tune = fastBloat
	if code, body := do(t, ts, "POST", "/api/diag/bloat", "", csrf); code != 202 {
		t.Fatalf("start bufferbloat test: %d %s", code, body)
	}
	if code, body := do(t, ts, "POST", "/api/diag/trace", `{"target_id":"cloudflare"}`, csrf); code != 409 || !strings.Contains(body, "bloat_running") {
		t.Fatalf("trace during the bufferbloat test: %d %s", code, body)
	}
	srv.bloat.stop("")
}

func TestTraceStoppedOnClose(t *testing.T) {
	path := &fakePath{slow: 30 * time.Millisecond}
	ts, srv := newTraceServer(t, path, 1000, 10*time.Millisecond)
	if code, body := do(t, ts, "POST", "/api/diag/trace", `{"target_id":"cloudflare"}`, csrf); code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() { srv.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited too long for the trace")
	}
	if srv.trace.isRunning() {
		t.Fatal("trace still running after Close")
	}
}

func TestResolveTraceHost(t *testing.T) {
	ctx := context.Background()
	for in, want := range map[string]string{
		"203.0.113.9":         "203.0.113.9",
		"[2001:db8::1]":       "2001:db8::1",
		"::ffff:198.51.100.4": "198.51.100.4",
		"2001:db8::1%eth0":    "2001:db8::1%eth0",
		"fe80::1%7":           "fe80::1%7",
	} {
		a, err := resolveTraceHost(ctx, in)
		if err != nil || a.String() != want {
			t.Errorf("%s: %v %v, want %s", in, a, err, want)
		}
	}
	// Names that can't be host names are refused before any lookup.
	for _, in := range []string{"no spaces", "http://example.com", "user@example.com", strings.Repeat("a", 254)} {
		if _, err := resolveTraceHost(ctx, in); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
}

func TestFirstPerFamilyPrefersIPv4ForTraces(t *testing.T) {
	got := firstPerFamily([]netip.Addr{netip.MustParseAddr("2001:db8::5"), netip.MustParseAddr("198.51.100.5")}, 0)
	if got[0].Addr() != netip.MustParseAddr("198.51.100.5") {
		t.Fatalf("got %v", got)
	}
}
