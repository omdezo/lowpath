package relay

import (
	"math"
	"testing"
	"time"
)

const ms = time.Millisecond

func TestLegWindowLoss(t *testing.T) {
	var w legWindow
	for i := range legMinPings - 1 {
		w.add(10*ms, i%4 != 0)
	}
	if _, ok := w.loss(); ok {
		t.Fatalf("loss known after %d pings, want %d first", legMinPings-1, legMinPings)
	}
	w.add(10*ms, true)
	// Pings 0, 4, 8, 12, 16 lost: 5 of 20.
	if pct, ok := w.loss(); !ok || pct != 25 {
		t.Fatalf("loss %v/%v, want 25%%", pct, ok)
	}

	// Fill the window with replies: the early losses age out one by one.
	for range legWindowSize - legMinPings {
		w.add(10*ms, true)
	}
	if pct, _ := w.loss(); math.Abs(pct-100*5.0/legWindowSize) > 1e-9 {
		t.Fatalf("full window: loss %v, want 5 of %d", pct, legWindowSize)
	}
	for range legMinPings {
		w.add(10*ms, true)
	}
	if pct, _ := w.loss(); pct != 0 {
		t.Fatalf("loss %v after the lost pings left the window", pct)
	}
	// And a full window of misses is 100%.
	for range legWindowSize {
		w.add(0, false)
	}
	if pct, _ := w.loss(); pct != 100 || w.nLost != legWindowSize {
		t.Fatalf("all lost: loss %v (%d lost)", pct, w.nLost)
	}
}

// RFC 3550 section 6.4.1 on a sequence worked by hand: J += (|D| - J)/16,
// D the change between consecutive replies' round trips, lost pings skipped.
func TestLegWindowJitterRFC3550(t *testing.T) {
	var w legWindow
	w.add(10*ms, true) // no previous reply: J stays 0
	w.add(12*ms, true) // |D| = 2: J = 2/16 = 0.125
	w.add(9*ms, true)  // |D| = 3: J = 0.125 + (3 - 0.125)/16 = 0.3046875
	w.add(0, false)    // lost: skipped
	w.add(15*ms, true) // |D| = 15 - 9 = 6: J = 0.3046875 + (6 - 0.3046875)/16
	want := 0.3046875 + (6-0.3046875)/16
	if got := w.jitter / float64(ms); math.Abs(got-want) > 1e-9 {
		t.Fatalf("jitter %.9f ms, want %.9f", got, want)
	}
	if _, ok := w.jitterEstimate(); ok {
		t.Fatal("jitter reported after 3 comparisons")
	}

	// A steady round trip has none, however long it is.
	var steady legWindow
	for range 100 {
		steady.add(80*ms, true)
	}
	if j, ok := steady.jitterEstimate(); !ok || j != 0 {
		t.Fatalf("steady leg: jitter %v/%v, want a known 0", j, ok)
	}
}

func TestLegWindowJitterNeedsComparisons(t *testing.T) {
	var w legWindow
	// Alternate reply and loss: plenty of outcomes, but every reply still
	// counts against the reply before it.
	for i := range 2 * legMinPings {
		w.add(time.Duration(10+i%4)*ms, i%2 == 0)
	}
	if _, ok := w.loss(); !ok {
		t.Fatal("loss unknown after 40 pings")
	}
	if _, ok := w.jitterEstimate(); ok {
		t.Fatalf("jitter known after %d comparisons, want %d", w.pairs, legMinPings)
	}
	w.add(10*ms, true)
	if _, ok := w.jitterEstimate(); !ok {
		t.Fatalf("jitter unknown after %d comparisons", w.pairs)
	}
}
