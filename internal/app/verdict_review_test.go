package app

import (
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
)

// offlineLine is the fake home line with its default route gone.
type offlineLine struct{ *fakeHomeLine }

func (offlineLine) DefaultRoute() (diag.Route, error) { return diag.Route{}, diag.ErrNoRoute }

func TestVerdictAPIOffline(t *testing.T) {
	line := offlineLine{newFakeHomeLine()}
	ts, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
	})
	if err := srv.store.SetBloat(lossyWiFiBloat(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	d := getDiagnosis(t, func(p string) (int, string) { return do(t, ts, "GET", p, "", nil) })
	if d.Verdict.ID != diag.VerdictOffline || d.Verdict.Confidence != diag.ConfidenceHigh {
		t.Fatalf("offline: verdict %s (%s) %q", d.Verdict.ID, d.Verdict.Confidence, d.Verdict.Title)
	}
	_, rep := do(t, ts, "GET", "/api/diag/verdict/report", "", nil)
	if !strings.Contains(rep, "VERDICT: No internet connection") {
		t.Errorf("report:\n%s", rep)
	}
}

// A trace remembers the connection it ran on, so the verdict can drop it
// after a switch from Wi-Fi to a cable.
func TestTraceRecordsItsConnection(t *testing.T) {
	line := newFakeHomeLine()
	ts, _ := newRealServer(t, newFake(t), func(c *Config) {
		c.PingTTL, c.Names = (&fakePath{}).ping, noNames
		c.TraceCount, c.TraceInterval = 5, 3*time.Millisecond
		c.NetStats, c.BloatLoader, c.Ping = line, line, line.ping
	})
	if code, body := do(t, ts, "POST", "/api/diag/trace", `{"host":"203.0.113.9"}`, csrf); code != 202 {
		t.Fatalf("start: %d %s", code, body)
	}
	r := waitTrace(t, ts).Result
	if r == nil || r.Interface != "Wi-Fi" || r.Link != diag.LinkWiFi {
		t.Fatalf("trace connection: %+v", r)
	}
}
