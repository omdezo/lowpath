package diag

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"lowpath/internal/icmp"
)

// TestInternetTraceToCloudflare runs a short real route trace (20 rounds,
// half a second apart) to 1.1.1.1, which answers ping:
//
//	$env:LOWPATH_INTERNET=1; go test -run TestInternetTrace -v ./internal/diag
var cloudflareDNS = netip.MustParseAddr("1.1.1.1")

func TestInternetTraceToCloudflare(t *testing.T) {
	needInternet(t)
	tr := NewTrace(TraceConfig{
		Target: cloudflareDNS, Host: "Cloudflare DNS", Ping: icmp.PingTTL,
		Count: 20, Interval: 500 * time.Millisecond,
	})
	res, err := tr.Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log("\n" + res.Report())
	if !res.Reached || len(res.Hops) < 2 {
		t.Fatalf("want the destination reached through at least one router, got reached=%v with %d hops: %s", res.Reached, len(res.Hops), res.Verdict)
	}
	if last := res.Hops[len(res.Hops)-1]; last.Addr != cloudflareDNS.String() || last.Recv == 0 {
		t.Fatalf("last hop %+v", last)
	}
	if res.Hops[0].Recv == 0 {
		t.Logf("the first router never answered: %+v", res.Hops[0])
	}
}
