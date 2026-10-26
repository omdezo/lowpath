package diag

import (
	"context"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/icmp"
)

// These tests use the real internet (Cloudflare's speed-test endpoints):
//
//	$env:LOWPATH_INTERNET=1; go test -run TestInternet ./internal/diag
func needInternet(t *testing.T) {
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		t.Skip("set LOWPATH_INTERNET=1 to run tests that use the internet")
	}
}

func TestInternetLoaderReachesCloudflare(t *testing.T) {
	needInternet(t)
	l, err := NewHTTPLoader(HTTPLoaderConfig{MaxDownBytes: 50e6, MaxUpBytes: 20e6})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, dir := range []string{"download", "upload"} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var moved atomic.Int64
		fn := l.Download
		if dir == "upload" {
			fn = l.Upload
		}
		err := fn(ctx, func(n int64) { moved.Add(n) })
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		t.Logf("%s: %.1f MB in 3 s", dir, float64(moved.Load())/1e6)
		if moved.Load() == 0 {
			t.Fatalf("%s moved nothing", dir)
		}
	}
}

func TestInternetShortBloatTest(t *testing.T) {
	needInternet(t)
	l, err := NewHTTPLoader(HTTPLoaderConfig{MaxDownBytes: 100e6, MaxUpBytes: 40e6})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ns := NewNetStats()
	route, _ := ns.DefaultRoute()
	cfg := BloatConfig{
		Internet: []Candidate{
			{Tier: 1, Prober: NewICMPProber("Cloudflare", netip.MustParseAddr("1.1.1.1"), icmp.Ping)},
			{Tier: 2, Prober: NewTCPProber("Cloudflare", netip.MustParseAddrPort("1.1.1.1:443"))},
		},
		Loader: l, Net: ns, Route: route,
		Phase: 2 * time.Second, Ramp: 500 * time.Millisecond,
	}
	if route.Gateway.Is4() {
		cfg.Gateway = NewICMPProber("Gateway", route.Gateway, icmp.Ping)
	}
	res, err := NewBloatTest(cfg).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("grade %q target %q idle %v down +%v up +%v down %v Mbit/s up %v Mbit/s, %.0f MB; note %q",
		res.Grade, res.Target, deref(res.IdleMS), deref(res.DownIncreaseMS), deref(res.UpIncreaseMS), deref(res.DownMbps), deref(res.UpMbps), res.DataMB, res.Note)
	if res.Grade == "" {
		t.Fatalf("no grade: %s", res.Note)
	}
}
