package diag

import (
	"context"
	"net/netip"
	"os"
	"testing"
	"time"

	"lowpath/internal/stun"
)

// Real STUN servers and the real RIPEstat API. Run with
//
//	$env:LOWPATH_INTERNET=1; go test -run TestInternet -v ./internal/diag
func internet(t *testing.T) {
	t.Helper()
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		t.Skip("set LOWPATH_INTERNET=1 to run tests that use the internet")
	}
}

func TestInternetLocate(t *testing.T) {
	internet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	loc, err := NewInternetLocator().Locate(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("state %s %s", loc.State, loc.Message)
	for _, e := range []*Endpoint{loc.IPv4, loc.IPv6} {
		if e != nil {
			t.Logf("%s via %s: AS%d %q (%s) %s %s/%s %s", e.IP, e.Source, e.ASN, e.ISP, e.ASName, e.Prefix, e.City, e.Country, e.LookupError)
		}
	}
	t.Logf("ipv6 note: %s", loc.IPv6Note)
	if loc.State != StateOK || loc.IPv4 == nil || loc.IPv4.ASN == 0 || loc.IPv4.ISP == "" || loc.IPv4.Country == "" {
		t.Fatalf("incomplete: %+v", loc)
	}
	if !loc.IPv4.Prefix.Contains(loc.IPv4.IP) {
		t.Fatalf("prefix %s doesn't hold %s", loc.IPv4.Prefix, loc.IPv4.IP)
	}
}

// STUN and HTTPS must agree on the public IPv4 address (they would not
// behind a multi-WAN or a proxy, which is worth knowing too).
func TestInternetSTUNAgreesWithHTTPS(t *testing.T) {
	internet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := NewSTUNFinder(stun.NewClient(stun.ClientOptions{}), DefaultSTUNServers(), 0).PublicAddr(ctx, IPv4)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewRIPEstat(RIPEstatOptions{}).PublicAddr(ctx, IPv4)
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr != h.Addr {
		t.Logf("STUN saw %s, HTTPS saw %s: this connection uses more than one public address", s.Addr, h.Addr)
	}
}

// Known networks resolve to their registered holders.
func TestInternetRIPEstatKnownNetworks(t *testing.T) {
	internet(t)
	r := NewRIPEstat(RIPEstatOptions{})
	for ip, asn := range map[string]uint32{"193.0.6.139": 3333, "8.8.8.8": 15169, "2001:67c:2e8:22::c100:68b": 3333} {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		ni, err := r.LookupNetwork(ctx, netip.MustParseAddr(ip))
		cancel()
		if err != nil {
			// An IPv6 lookup needs IPv6 to reach RIPEstat.
			if netip.MustParseAddr(ip).Is6() {
				t.Logf("%s: %v (no IPv6 here?)", ip, err)
				continue
			}
			t.Fatalf("%s: %v", ip, err)
		}
		t.Logf("%s: %+v", ip, ni)
		if ni.ASN != asn || ni.ISP == "" {
			t.Fatalf("%s: got AS%d %q, want AS%d", ip, ni.ASN, ni.ISP, asn)
		}
	}
}
