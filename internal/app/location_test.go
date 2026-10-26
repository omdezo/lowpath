package app

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"lowpath/internal/diag"
)

// fakeLocator returns a fixed location and records how it was asked.
type fakeLocator struct {
	mu        sync.Mutex
	loc       diag.Location
	err       error
	have      bool
	refreshes int
	calls     int
}

func (f *fakeLocator) Locate(ctx context.Context, refresh bool) (diag.Location, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if refresh {
		f.refreshes++
	}
	if f.err != nil {
		return diag.Location{}, f.err
	}
	f.have = true
	return f.loc, nil
}

func (f *fakeLocator) Last() (diag.Location, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loc, f.have
}

func muscat() diag.Location {
	return diag.Location{
		State: diag.StateOK,
		IPv4: &diag.Endpoint{
			IP: netip.MustParseAddr("89.147.159.43"), Source: "stun stun.l.google.com:19302",
			NetworkInfo: diag.NetworkInfo{ASN: 204170, ASName: "awasr", ISP: "Awaser Oman LLC", Country: "OM", City: "Muscat", Prefix: netip.MustParsePrefix("89.147.159.0/24")},
		},
		IPv6Note: "This connection has no public IPv6 address (common, and fine for Lowpath).",
	}
}

func TestLocationEndpoint(t *testing.T) {
	fl := &fakeLocator{loc: muscat()}
	ts, _ := newRealServer(t, &fakeCapturer{}, func(c *Config) { c.Locator = fl })

	// The diagnostics report has no location before one was looked up.
	var rep map[string]any
	if code := call(t, ts, "GET", "/api/diagnostics", "", &rep); code != 200 || rep["location"] != nil {
		t.Fatalf("report before lookup: %d %v", code, rep["location"])
	}

	var got struct {
		State string `json:"state"`
		IPv4  struct {
			IP      string `json:"ip"`
			ASN     uint32 `json:"asn"`
			ISP     string `json:"isp"`
			City    string `json:"city"`
			Country string `json:"country"`
			Prefix  string `json:"prefix"`
		} `json:"ipv4"`
		IPv6     *struct{} `json:"ipv6"`
		IPv6Note string    `json:"ipv6_note"`
	}
	if code := call(t, ts, "GET", "/api/location", "", &got); code != 200 {
		t.Fatalf("location: %d", code)
	}
	if got.State != "ok" || got.IPv4.IP != "89.147.159.43" || got.IPv4.ASN != 204170 || got.IPv4.ISP != "Awaser Oman LLC" ||
		got.IPv4.City != "Muscat" || got.IPv4.Prefix != "89.147.159.0/24" || got.IPv6 != nil || got.IPv6Note == "" {
		t.Fatalf("got %+v", got)
	}
	call(t, ts, "GET", "/api/location?refresh=1", "", nil)
	if fl.refreshes != 1 || fl.calls != 2 {
		t.Fatalf("refreshes %d, calls %d", fl.refreshes, fl.calls)
	}

	// The report carries the ISP but not the full address.
	_, body := do(t, ts, "GET", "/api/diagnostics", "", nil)
	if strings.Contains(body, "89.147.159.43") || !strings.Contains(body, "89.147.159.0") || !strings.Contains(body, "Awaser Oman LLC") {
		t.Fatalf("report location not redacted: %s", body)
	}
}

func TestLocationEndpointErrors(t *testing.T) {
	fl := &fakeLocator{err: context.DeadlineExceeded}
	ts, _ := newRealServer(t, &fakeCapturer{}, func(c *Config) { c.Locator = fl })
	if code, body := do(t, ts, "GET", "/api/location", "", nil); code != 504 || !strings.Contains(body, "location_timeout") {
		t.Fatalf("timeout: %d %s", code, body)
	}
	fl.mu.Lock()
	fl.err = errors.New("boom")
	fl.mu.Unlock()
	if code, body := do(t, ts, "GET", "/api/location", "", nil); code != 503 || !strings.Contains(body, "location_failed") {
		t.Fatalf("failure: %d %s", code, body)
	}
	// Like every API call it needs the session.
	if code, _ := do(t, ts, "GET", "/api/location", "", map[string]string{"Cookie": ""}); code != 401 {
		t.Fatalf("no session: %d", code)
	}
}
