package diag

import (
	"errors"
	"testing"
)

// The IP Helper calls must work without administrator rights.
func TestWindowsNetStatsLive(t *testing.T) {
	ns := NewNetStats()
	r, err := ns.DefaultRoute()
	if errors.Is(err, ErrNoRoute) {
		t.Skip("offline: no default route")
	}
	if err != nil {
		t.Fatal(err)
	}
	if r.Index <= 0 {
		t.Fatalf("route = %+v", r)
	}
	t.Logf("default route: %+v", r)
	a, err := ns.Counters(r.Index)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ns.Counters(r.Index)
	if err != nil {
		t.Fatal(err)
	}
	if b.RxBytes < a.RxBytes || b.TxBytes < a.TxBytes || !b.At.After(a.At) && !b.At.Equal(a.At) {
		t.Fatalf("counters went backwards: %+v then %+v", a, b)
	}
	if _, err := ns.Counters(1 << 30); err == nil {
		t.Fatal("a missing interface must fail")
	}
}
