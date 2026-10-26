package diag

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// fakeRIPE serves canned RIPEstat replies, shaped like the live API's
// (captured October 2026), and records the queries it got.
type fakeRIPE struct {
	mu      sync.Mutex
	queries []string
	// per data call: the "data" object, or an HTTP status to fail with
	data map[string]string
	fail map[string]int
}

func (f *fakeRIPE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/data/"), "/data.json")
	f.mu.Lock()
	f.queries = append(f.queries, name+"?"+r.URL.RawQuery)
	data, ok := f.data[name]
	code := f.fail[name]
	f.mu.Unlock()
	if code != 0 {
		http.Error(w, "boom", code)
		return
	}
	if !ok {
		w.WriteHeader(400)
		fmt.Fprint(w, `{"messages":[["error","unknown data call"]],"status":"error","status_code":400,"data":{}}`)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"messages":[],"see_also":[],"version":"1.1","data_call_name":%q,"cached":false,"status":"ok","status_code":200,"data":%s}`, name, data)
}

// set changes a data call's reply; "" makes it unknown.
func (f *fakeRIPE) set(name, data string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if data == "" {
		delete(f.data, name)
	} else {
		f.data[name] = data
	}
}

// setFail makes a data call fail with an HTTP status; 0 stops that.
func (f *fakeRIPE) setFail(name string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[name] = code
}

func (f *fakeRIPE) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

// The canned network is a documentation one: AS64500 (RFC 5398) announcing
// 198.51.100.0/24 (RFC 5737), with the AS block RIPEstat reports for it.
const (
	exampleNetworkInfo = `{"asns":["64500"],"prefix":"198.51.100.0/24"}`
	exampleOverview    = `{"type":"as","resource":"64500","block":{"resource":"64496-64511","desc":"Reserved for use in documentation and sample code","name":"IANA 16-bit Autonomous System (AS) Numbers Registry"},"holder":"EXAMPLEBB-AS Example Broadband","announced":true}`
	exampleGeo         = `{"located_resources":[{"resource":"198.51.100.2/32","locations":[{"country":"NL","city":"","resources":["198.51.100.0/24"],"latitude":52.38,"longitude":4.9,"covered_percentage":20.0},{"country":"NL","city":"Amsterdam","resources":["198.51.100.0/25"],"latitude":52.37,"longitude":4.89,"covered_percentage":80.0}],"unknown_percentage":0}],"unknown_percentage":{"v4":0.0},"parameters":{"resource":"198.51.100.2/32"}}`
)

func newFakeRIPE(t *testing.T) (*fakeRIPE, *RIPEstat) {
	t.Helper()
	f := &fakeRIPE{
		data: map[string]string{
			"network-info":     exampleNetworkInfo,
			"as-overview":      exampleOverview,
			"maxmind-geo-lite": exampleGeo,
			"whats-my-ip":      `{"ip":"198.51.100.2"}`,
		},
		fail: map[string]int{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, NewRIPEstat(RIPEstatOptions{BaseURL: srv.URL})
}

func TestRIPEstatLookupNetwork(t *testing.T) {
	f, r := newFakeRIPE(t)
	got, err := r.LookupNetwork(context.Background(), pub4)
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 64500 || got.ASName != "EXAMPLEBB-AS" || got.ISP != "Example Broadband" ||
		got.Prefix != netip.MustParsePrefix("198.51.100.0/24") || got.Country != "NL" || got.City != "Amsterdam" || got.Partial != "" {
		t.Fatalf("got %+v", got)
	}
	calls := f.calls()
	if len(calls) != 3 {
		t.Fatalf("calls %v", calls)
	}
	for _, c := range calls {
		if !strings.Contains(c, "sourceapp=lowpath") {
			t.Fatalf("query without sourceapp: %s", c)
		}
	}
	if !strings.Contains(strings.Join(calls, " "), "as-overview?resource=AS64500") {
		t.Fatalf("as-overview not asked for AS64500: %v", calls)
	}
}

func TestRIPEstatPartialFailures(t *testing.T) {
	f, r := newFakeRIPE(t)
	f.setFail("maxmind-geo-lite", 503)
	f.setFail("as-overview", 500)
	got, err := r.LookupNetwork(context.Background(), pub4)
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 64500 || got.ISP != "" || got.City != "" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.Partial, "ISP name unknown") || !strings.Contains(got.Partial, "city unknown") {
		t.Fatalf("partial %q", got.Partial)
	}
}

func TestRIPEstatErrors(t *testing.T) {
	f, r := newFakeRIPE(t)
	ctx := context.Background()

	f.set("network-info", `{"asns":[],"prefix":""}`) // what RIPEstat says for 192.168.1.1
	if _, err := r.LookupNetwork(ctx, pub4); err == nil || !strings.Contains(err.Error(), "not announced") {
		t.Fatalf("unannounced: %v", err)
	}
	f.setFail("network-info", 429)
	if _, err := r.LookupNetwork(ctx, pub4); err == nil || !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("429: %v", err)
	}
	f.setFail("network-info", 0)
	f.set("network-info", `not json`)
	if _, err := r.LookupNetwork(ctx, pub4); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("garbage: %v", err)
	}
	f.set("network-info", "")
	if _, err := r.LookupNetwork(ctx, pub4); err == nil {
		t.Fatal("error reply accepted")
	}
}

func TestRIPEstatWhatsMyIP(t *testing.T) {
	f, r := newFakeRIPE(t)
	pa, err := r.PublicAddr(context.Background(), IPv4)
	if err != nil || pa.Addr != pub4 || pa.Source == "" {
		t.Fatalf("got %+v, %v", pa, err)
	}
	f.set("whats-my-ip", `{"ip":"nonsense"}`)
	if _, err := r.PublicAddr(context.Background(), IPv4); err == nil {
		t.Fatal("bad address accepted")
	}
	// The fake listens on 127.0.0.1 only, so an IPv6-pinned request can't
	// reach it: the family pinning really holds.
	if _, err := r.PublicAddr(context.Background(), IPv6); err == nil {
		t.Fatal("IPv6 request reached an IPv4-only server")
	}
}

func TestRIPEstatHonorsContext(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(block)
	r := NewRIPEstat(RIPEstatOptions{BaseURL: srv.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.LookupNetwork(ctx, pub4); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
}

func TestSplitHolder(t *testing.T) {
	for _, tc := range []struct{ in, handle, name string }{
		{"EXAMPLEBB-AS Example Broadband (Holdings) B.V.", "EXAMPLEBB-AS", "Example Broadband (Holdings) B.V."},
		{"GOOGLE - Google LLC", "GOOGLE", "Google LLC"},
		{"COMCAST-7922 - Comcast Cable Communications, LLC", "COMCAST-7922", "Comcast Cable Communications, LLC"},
		{"M247 M247 Europe SRL", "M247", "M247 Europe SRL"},
		{"CLOUDFLARENET", "CLOUDFLARENET", "CLOUDFLARENET"},
		{"examplenet Example Broadband", "examplenet", "Example Broadband"},
		{"Some ISP without a handle", "", "Some ISP without a handle"},
		{"", "", ""},
	} {
		h, n := splitHolder(tc.in)
		if h != tc.handle || n != tc.name {
			t.Errorf("splitHolder(%q) = %q, %q; want %q, %q", tc.in, h, n, tc.handle, tc.name)
		}
	}
}

func TestRIPEstatCityCoordinates(t *testing.T) {
	f, r := newFakeRIPE(t)
	ctx := context.Background()
	f.set("maxmind-geo-lite", `{"located_resources":[{"resource":"203.0.113.9/32","locations":[{"country":"NL","city":"Amsterdam","resources":["203.0.113.0/24"],"latitude":52.37,"longitude":4.89,"covered_percentage":100.0}]}]}`)
	got, err := r.LookupNetwork(ctx, netip.MustParseAddr("203.0.113.9"))
	if err != nil {
		t.Fatal(err)
	}
	if got.City != "Amsterdam" || got.Lat == nil || got.Lon == nil || *got.Lat != 52.37 || *got.Lon != 4.89 {
		t.Fatalf("got %+v, want Amsterdam with its coordinates", got)
	}
	// Country only (anycast): its center is no place to measure from.
	f.set("maxmind-geo-lite", `{"located_resources":[{"resource":"203.0.113.9/32","locations":[{"country":"US","city":"","latitude":37.751,"longitude":-97.822,"covered_percentage":100.0}]}]}`)
	got, err = r.LookupNetwork(ctx, netip.MustParseAddr("203.0.113.9"))
	if err != nil || got.Country != "US" || got.Lat != nil || got.Lon != nil {
		t.Fatalf("country only: %+v, %v", got, err)
	}
	// Out-of-range coordinates are dropped, the city is kept.
	f.set("maxmind-geo-lite", `{"located_resources":[{"locations":[{"country":"NL","city":"Amsterdam","latitude":152.37,"longitude":4.89,"covered_percentage":100.0}]}]}`)
	got, _ = r.LookupNetwork(ctx, netip.MustParseAddr("203.0.113.9"))
	if got.City != "Amsterdam" || got.Lat != nil {
		t.Fatalf("bad coordinates: %+v", got)
	}
}
