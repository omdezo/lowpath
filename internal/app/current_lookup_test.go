package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"lowpath/internal/diag"
)

// lookupReply is one answer of a scriptedLookup.
type lookupReply struct {
	info diag.NetworkInfo
	err  error
}

// scriptedLookup answers network lookups from a script, one reply per
// call; the last reply repeats. With gate set, each call waits for a value
// on it.
type scriptedLookup struct {
	mu      sync.Mutex
	replies []lookupReply
	calls   int
	gate    chan struct{}
}

func (s *scriptedLookup) LookupNetwork(ctx context.Context, _ netip.Addr) (diag.NetworkInfo, error) {
	s.mu.Lock()
	gate := s.gate
	s.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return diag.NetworkInfo{}, ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.replies[min(s.calls, len(s.replies)-1)]
	s.calls++
	return r.info, r.err
}

func (s *scriptedLookup) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// ripeAnswer looks docAddr up in a RIPEstat that answers as-overview and
// maxmind-geo-lite with the given HTTP status (200: the example data), so
// a partial's transient mark is the one the real client sets.
func ripeAnswer(t *testing.T, overview, geo int) diag.NetworkInfo {
	t.Helper()
	data := map[string]string{
		"network-info":     `{"asns":["64500"],"prefix":"203.0.113.0/24"}`,
		"as-overview":      `{"holder":"EXAMPLEBB-AS Example Broadband"}`,
		"maxmind-geo-lite": `{"located_resources":[{"locations":[{"country":"NL","city":"Amsterdam","latitude":52.37,"longitude":4.89,"covered_percentage":100.0}]}]}`,
	}
	status := map[string]int{"as-overview": overview, "maxmind-geo-lite": geo}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/data/"), "/data.json")
		if code := status[name]; code != 0 && code != 200 {
			http.Error(w, "<html>error</html>", code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"messages":[],"status":"ok","status_code":200,"data":%s}`, data[name])
	}))
	defer srv.Close()
	info, err := diag.NewRIPEstat(diag.RIPEstatOptions{BaseURL: srv.URL, HedgeAfter: -1}).LookupNetwork(context.Background(), docServer)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

var docServer = netip.MustParseAddr("203.0.113.50")

// testLookups is a netLookups on a clock the test moves.
type testLookups struct {
	*netLookups
	clock time.Time
}

func newTestLookups(l diag.NetworkLookup) *testLookups {
	tl := &testLookups{netLookups: newNetLookups(l, context.Background()), clock: time.Unix(1_800_000_000, 0)}
	tl.now = func() time.Time { return tl.clock }
	return tl
}

// settle waits for the lookup of a in flight, if any, to store its answer.
func (tl *testLookups) settle(t *testing.T, a netip.Addr) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		tl.mu.Lock()
		e := tl.m[a]
		running := e != nil && e.running
		tl.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("lookup did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

// timeoutErr is a net.Error that timed out, as a stalled dial gives.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

// The Current server card keeps an answer spoiled by a slow or busy
// registry only for a few seconds, like the cache under it, and a real one
// (a complete answer, a partial the registry gave, a lasting failure) for
// its usual time (#145).
func TestNetLookupsKeepTransientAnswersBriefly(t *testing.T) {
	transientPartial := ripeAnswer(t, http.StatusBadGateway, 200)
	realPartial := ripeAnswer(t, 200, http.StatusNotFound)
	complete := ripeAnswer(t, 200, 200)
	if !transientPartial.Transient() || transientPartial.ISP != "" || realPartial.Transient() || realPartial.City != "" || complete.Partial != "" {
		t.Fatalf("fixtures: %+v / %+v / %+v", transientPartial, realPartial, complete)
	}
	cases := []struct {
		name  string
		reply lookupReply
		keep  time.Duration
		note  string
	}{
		{"timeout", lookupReply{err: fmt.Errorf("RIPEstat network-info: %w", context.DeadlineExceeded)}, netLookupTransientTTL, "Network unknown: the lookup took too long."},
		{"stalled dial", lookupReply{err: fmt.Errorf("RIPEstat network-info: %w", timeoutErr{})}, netLookupTransientTTL, "Network unknown: RIPEstat network-info: i/o timeout"},
		{"not announced", lookupReply{err: errors.New("RIPEstat: 203.0.113.50 is not announced on the internet (no origin AS)")}, netLookupErrTTL, "Network unknown: RIPEstat: 203.0.113.50 is not announced on the internet (no origin AS)"},
		{"ISP name stalled", lookupReply{info: transientPartial}, netLookupTransientTTL, ""},
		{"no city in the registry", lookupReply{info: realPartial}, netLookupTTL, ""},
		{"complete", lookupReply{info: complete}, netLookupTTL, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedLookup{replies: []lookupReply{tc.reply}}
			n := newTestLookups(fake)
			if info, _, pending := n.get(docServer); info != nil || !pending {
				t.Fatalf("first get: %v, pending %v", info, pending)
			}
			n.settle(t, docServer)
			start := n.clock
			n.clock = start.Add(tc.keep - time.Millisecond)
			info, note, pending := n.get(docServer)
			if pending || fake.count() != 1 {
				t.Fatalf("not kept for %v: pending %v, %d lookups", tc.keep, pending, fake.count())
			}
			if note != tc.note || (tc.reply.err == nil) != (info != nil) {
				t.Fatalf("info %+v, note %q", info, note)
			}
			if info != nil && (info.ASN != 64500 || info.Partial != tc.reply.info.Partial) {
				t.Fatalf("info %+v", info)
			}
			n.clock = start.Add(tc.keep)
			if _, _, pending := n.get(docServer); !pending {
				t.Fatalf("kept longer than %v", tc.keep)
			}
			n.settle(t, docServer)
			if fake.count() != 2 {
				t.Fatalf("%d lookups after %v", fake.count(), tc.keep)
			}
		})
	}
}

// A registry that stays slow is asked again after 3 s, then 6, 12 … up to
// a minute, and once it answers fully the next slow spell starts at 3 s.
// What a lookup found stays on the card while it is looked up again, and
// a timeout doesn't replace it.
func TestNetLookupsBackOffAndKeepWhatTheyKnow(t *testing.T) {
	slow := lookupReply{err: fmt.Errorf("RIPEstat network-info: %w", context.DeadlineExceeded)}
	partial := lookupReply{info: ripeAnswer(t, http.StatusBadGateway, 200)}
	full := lookupReply{info: ripeAnswer(t, 200, 200)}
	fake := &scriptedLookup{replies: []lookupReply{slow, slow, slow, slow, slow, slow, slow, full, partial, slow, full}}
	n := newTestLookups(fake)
	lookup := func() (*diag.NetworkInfo, string) {
		t.Helper()
		if _, _, pending := n.get(docServer); !pending {
			t.Fatalf("no lookup at %d", fake.count())
		}
		n.settle(t, docServer)
		info, note, _ := n.get(docServer)
		return info, note
	}
	expect := func(waits ...time.Duration) {
		t.Helper()
		for _, w := range waits {
			n.clock = n.clock.Add(w - time.Millisecond)
			if _, _, pending := n.get(docServer); pending {
				t.Fatalf("asked again before %v (lookup %d)", w, fake.count())
			}
			n.clock = n.clock.Add(time.Millisecond)
			lookup()
		}
	}

	if info, note := lookup(); info != nil || note != "Network unknown: the lookup took too long." {
		t.Fatalf("after a timeout: %+v, %q", info, note)
	}
	expect(3*time.Second, 6*time.Second, 12*time.Second, 24*time.Second, 48*time.Second, time.Minute)
	if fake.count() != 7 {
		t.Fatalf("%d lookups", fake.count())
	}

	// The registry answers (kept 1 h), then once without the ISP name: the
	// complete answer stays, and the streak starts again at 3 s.
	expect(time.Minute)
	if info, _, _ := n.get(docServer); info == nil || info.ISP != "Example Broadband" {
		t.Fatalf("complete: %+v", info)
	}
	expect(time.Hour)
	if info, _, _ := n.get(docServer); info == nil || info.ISP != "Example Broadband" || info.Partial != "" {
		t.Fatalf("a part lost to a timeout replaced the complete answer: %+v", info)
	}
	// It stalls next time: the answer stays, and the next wait is 6 s.
	expect(3 * time.Second)
	if info, note, _ := n.get(docServer); info == nil || info.ASN != 64500 || note != "" {
		t.Fatalf("a timeout replaced what was known: %+v, %q", info, note)
	}

	// While the next lookup runs the card still shows the answer.
	fake.mu.Lock()
	fake.gate = make(chan struct{})
	fake.mu.Unlock()
	n.clock = n.clock.Add(6 * time.Second)
	if info, _, pending := n.get(docServer); !pending || info == nil || info.ASN != 64500 {
		t.Fatalf("while looking up again: %+v, pending %v", info, pending)
	}
	fake.gate <- struct{}{}
	n.settle(t, docServer)
	if info, _, pending := n.get(docServer); pending || info == nil || info.ISP != "Example Broadband" || fake.count() != 11 {
		t.Fatalf("complete: %+v, pending %v, %d lookups", info, pending, fake.count())
	}
	n.mu.Lock()
	streak := n.m[docServer].streak
	n.mu.Unlock()
	if streak != 0 {
		t.Fatalf("streak %d after a complete answer", streak)
	}
}

func TestTransientBackoff(t *testing.T) {
	for streak, want := range map[int]time.Duration{
		1: 3 * time.Second, 2: 6 * time.Second, 3: 12 * time.Second, 5: 48 * time.Second,
		6: time.Minute, 7: time.Minute, 1000: time.Minute,
	} {
		if got := transientBackoff(streak); got != want {
			t.Errorf("streak %d: %v, want %v", streak, got, want)
		}
	}
}

// A complete answer stays on the card when the next lookup of the same
// network loses a part to a timeout; an answer naming another network
// replaces it, partial or not.
func TestNetLookupsKeepCompleteOverTransientPartial(t *testing.T) {
	full := ripeAnswer(t, 200, 200)
	partial := ripeAnswer(t, http.StatusBadGateway, 200)
	moved := partial
	moved.ASN, moved.ASNs = 64501, []uint32{64501}
	fake := &scriptedLookup{replies: []lookupReply{{info: full}, {info: partial}, {info: moved}}}
	n := newTestLookups(fake)
	n.get(docServer)
	n.settle(t, docServer)

	n.clock = n.clock.Add(netLookupTTL)
	n.get(docServer)
	n.settle(t, docServer)
	if info, note, _ := n.get(docServer); info == nil || info.ISP != "Example Broadband" || info.Partial != "" || note != "" {
		t.Fatalf("a timeout dropped the ISP name: %+v, %q", info, note)
	}
	// Still asked again soon: the registry may answer fully then.
	n.clock = n.clock.Add(netLookupTransientTTL)
	if _, _, pending := n.get(docServer); !pending {
		t.Fatal("not asked again after a transient partial")
	}
	n.settle(t, docServer)
	if info, _, _ := n.get(docServer); info == nil || info.ASN != 64501 || info.ISP != "" {
		t.Fatalf("another network didn't replace the known one: %+v", info)
	}
}
