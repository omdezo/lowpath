package diag

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIsPublic(t *testing.T) {
	for a, want := range map[string]bool{
		// Public addresses can't come from the documentation ranges
		// (isPublic rejects those, below), so these are public resolvers.
		"1.1.1.1":              true,
		"8.8.8.8":              true,
		"::ffff:1.1.1.1":       true,
		"2606:4700:4700::1111": true,
		"192.168.1.20":         false,
		"100.72.1.1":           false, // CGNAT
		"198.18.0.5":           false, // benchmarking; "fake IP" VPN/proxy tools
		"192.0.2.1":            false, // documentation
		"203.0.113.9":          false,
		"240.0.0.1":            false, // reserved
		"0.1.2.3":              false,
		"2001:db8::1":          false,
		"fd00::1":              false,
		"fe80::1":              false,
		"::ffff:10.0.0.1":      false,
		"::1":                  false,
		"255.255.255.255":      false,
		"64:ff9b:1::1:2:3":     false,
	} {
		if got := isPublic(netip.MustParseAddr(a)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", a, got, want)
		}
	}
}

func TestRedactedScrubsAddressesInText(t *testing.T) {
	loc := Location{
		State:    StatePartial,
		Message:  "Your ISP couldn't be looked up: RIPEstat: 198.51.100.2 is not announced on the internet (no origin AS)",
		IPv6Note: "No public IPv6 address found: read udp6 [2001:db8:1:2::5]:50123->[2001:4860:4860::8888]:19302: wsarecv: refused.",
		IPv4: &Endpoint{
			IP:          pub4,
			LookupError: "RIPEstat: 198.51.100.2 is not announced",
			NetworkInfo: NetworkInfo{Partial: "city unknown (dial tcp 198.51.100.2:443: i/o timeout)"},
		},
	}
	r := loc.Redacted()
	all := strings.Join([]string{r.Message, r.IPv6Note, r.IPv4.LookupError, r.IPv4.Partial}, "\n")
	for _, leak := range []string{"198.51.100.2", "2001:db8:1:2::5"} {
		if strings.Contains(all, leak) {
			t.Fatalf("redacted text still holds %s:\n%s", leak, all)
		}
	}
	for _, want := range []string{"RIPEstat: 198.51.100.0 is not", "[2001:db8:1::]:50123", "198.51.100.0:443", "refused."} {
		if !strings.Contains(all, want) {
			t.Fatalf("redacted text lacks %q:\n%s", want, all)
		}
	}
	if !strings.Contains(loc.Message, "198.51.100.2") {
		t.Fatal("Redacted changed the original")
	}
}

func TestRIPEstatErrorsDoNotRepeatTheQuery(t *testing.T) {
	// A refused connection comes back as a *url.Error, whose text holds the
	// URL and so the address being looked up.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ln.Close()
	r := NewRIPEstat(RIPEstatOptions{BaseURL: base, Timeout: 2 * time.Second})
	_, err = r.LookupNetwork(context.Background(), pub4)
	if err == nil {
		t.Fatal("lookup against a closed port succeeded")
	}
	if strings.Contains(err.Error(), "resource=") || strings.Contains(err.Error(), pub4.String()) {
		t.Fatalf("error repeats the query: %v", err)
	}
	if !strings.HasPrefix(err.Error(), "RIPEstat network-info: ") {
		t.Fatalf("error %q", err)
	}
}

func TestRIPEstatWhatsMyIPGoesDirect(t *testing.T) {
	// Through an HTTP(S)_PROXY whats-my-ip would report the proxy's
	// address; only the registry lookups may use one.
	r := NewRIPEstat(RIPEstatOptions{})
	for f, c := range r.clients {
		tr := c.Transport.(*http.Transport)
		if direct := tr.Proxy == nil; direct != (f != anyFamily) {
			t.Errorf("family %q: direct=%v", f, direct)
		}
	}
}

func TestClip(t *testing.T) {
	long := strings.Repeat("é", 5000)
	if got := clip(long, 200); len([]rune(got)) != 201 || !strings.HasSuffix(got, "…") {
		t.Fatalf("clip gave %d runes", len([]rune(got)))
	}
	if got := clip("  short  ", 200); got != "short" {
		t.Fatalf("clip(short) = %q", got)
	}
}

func TestLocateRetriesFailuresSooner(t *testing.T) {
	clk := newClock()
	f := &fakeFinder{errs: map[Family]error{IPv4: errors.New("offline"), IPv6: errors.New("offline")}}
	l := newTestLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: tableLookup{pub4: example}, MaxAge: 2 * time.Minute, Now: clk.Now})
	ctx := context.Background()
	if loc, _ := l.Locate(ctx, false); loc.State != StateOffline {
		t.Fatalf("state %s", loc.State)
	}
	clk.Add(5 * time.Second)
	l.Locate(ctx, false)
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("offline result not reused for a few seconds: %d calls", n)
	}
	// Back online: the next look after FailMaxAge (15 s) finds it, well
	// before MaxAge.
	// (No lookup is running: Locate returned, so this doesn't race.)
	f.errs, f.addrs = nil, map[Family]netip.Addr{IPv4: pub4}
	clk.Add(15 * time.Second)
	if loc, _ := l.Locate(ctx, false); loc.State != StateOK {
		t.Fatalf("not back after 20 s: %+v", loc)
	}
	// A good result is kept for MaxAge.
	clk.Add(time.Minute)
	l.Locate(ctx, false)
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("ok result not reused: %d calls", n)
	}
}

func TestDescribeNetTimeout(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}
	if got := describe(err); got != "no reply in time" {
		t.Fatalf("describe = %q", got)
	}
	dns := &net.DNSError{Err: "timeout", Name: "stat.ripe.net", IsTimeout: true}
	if got := describe(dns); !strings.Contains(got, "DNS") {
		t.Fatalf("describe(dns timeout) = %q", got)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
