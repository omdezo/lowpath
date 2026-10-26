package diag

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// stallingRIPE wraps fakeRIPE: the next stall[name] requests for a data
// call hang until the client gives up (RIPEstat's proxy holding a request,
// then answering 502), and the next busy[name] ones answer 502 at once.
type stallingRIPE struct {
	*fakeRIPE
	mu        sync.Mutex
	stall     map[string]int
	busy      map[string]int
	count     map[string]int
	abandoned int // stalled requests the client hung up on
}

func newStallingRIPE(t *testing.T, o RIPEstatOptions) (*stallingRIPE, *RIPEstat) {
	t.Helper()
	f, _ := newFakeRIPE(t)
	s := &stallingRIPE{fakeRIPE: f, stall: map[string]int{}, busy: map[string]int{}, count: map[string]int{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	o.BaseURL = srv.URL
	return s, NewRIPEstat(o)
}

func (s *stallingRIPE) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/data/"), "/data.json")
	s.mu.Lock()
	s.count[name]++
	stall, busy := s.stall[name] > 0, s.busy[name] > 0
	if stall {
		s.stall[name]--
	} else if busy {
		s.busy[name]--
	}
	s.mu.Unlock()
	switch {
	case stall:
		select {
		case <-r.Context().Done():
			s.mu.Lock()
			s.abandoned++
			s.mu.Unlock()
		case <-time.After(10 * time.Second):
			http.Error(w, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
		}
	case busy:
		http.Error(w, "<html>502 Bad Gateway</html>", http.StatusBadGateway)
	default:
		s.fakeRIPE.ServeHTTP(w, r)
	}
}

func (s *stallingRIPE) set(stall, busy map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range stall {
		s.stall[k] = v
	}
	for k, v := range busy {
		s.busy[k] = v
	}
}

func (s *stallingRIPE) calls(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count[name]
}

func (s *stallingRIPE) hungUp() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.abandoned
}

// eventually waits up to 2 s for cond (server-side bookkeeping trails the
// client by a moment).
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(what)
}

var docAddr = netip.MustParseAddr("198.51.100.7")

func TestRIPEstatHedgesASlowReply(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 50 * time.Millisecond})
	s.set(map[string]int{"network-info": 1}, nil)
	start := time.Now()
	got, err := r.LookupNetwork(context.Background(), docAddr)
	if err != nil || got.ASN != 64500 || got.ISP == "" || got.City == "" || got.Partial != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("took %v: the hedge didn't answer for the stalled request", el)
	}
	if n := s.calls("network-info"); n != 2 {
		t.Fatalf("network-info sent %d times, want 2", n)
	}
	// The stalled copy is cancelled once the hedge has answered.
	eventually(t, "stalled request left open", func() bool { return s.hungUp() == 1 })
}

func TestRIPEstatSendsAgainAfterBusyReply(t *testing.T) {
	// No timer hedge: only the 502 makes the second copy go out.
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: time.Hour})
	s.set(nil, map[string]int{"network-info": 1, "as-overview": 1})
	got, err := r.LookupNetwork(context.Background(), docAddr)
	if err != nil || got.ASN != 64500 || got.ISP == "" || got.Partial != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if s.calls("network-info") != 2 || s.calls("as-overview") != 2 || s.calls("maxmind-geo-lite") != 1 {
		t.Fatalf("calls: network-info %d, as-overview %d, geo %d", s.calls("network-info"), s.calls("as-overview"), s.calls("maxmind-geo-lite"))
	}
}

func TestRIPEstatSendsOnlyTwoCopies(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: time.Hour})
	s.set(nil, map[string]int{"network-info": 5})
	_, err := r.LookupNetwork(context.Background(), docAddr)
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || !transient(err) {
		t.Fatalf("err %v", err)
	}
	if n := s.calls("network-info"); n != 2 {
		t.Fatalf("network-info sent %d times, want 2", n)
	}
}

func TestRIPEstatDoesNotRepeatFinalAnswers(t *testing.T) {
	// A 429 (fair use) or an error status is RIPEstat's answer; sending
	// it again would only add load.
	for _, tc := range []struct {
		name  string
		setup func(*stallingRIPE)
		want  string
	}{
		{"429", func(s *stallingRIPE) { s.setFail("network-info", 429) }, "HTTP 429"},
		{"error status", func(s *stallingRIPE) { s.fakeRIPE.set("network-info", "") }, "RIPEstat network-info"},
	} {
		s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: time.Hour})
		tc.setup(s)
		_, err := r.LookupNetwork(context.Background(), docAddr)
		if err == nil || !strings.Contains(err.Error(), tc.want) || transient(err) {
			t.Fatalf("%s: err %v (transient %v)", tc.name, err, transient(err))
		}
		if n := s.calls("network-info"); n != 1 {
			t.Fatalf("%s: network-info sent %d times, want 1", tc.name, n)
		}
	}
}

func TestRIPEstatBothCopiesStall(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 30 * time.Millisecond, Timeout: 200 * time.Millisecond})
	s.set(map[string]int{"network-info": 2}, nil)
	start := time.Now()
	_, err := r.LookupNetwork(context.Background(), docAddr)
	if err == nil || !transient(err) || describe(err) != "no reply in time" {
		t.Fatalf("err %v (%q)", err, describe(err))
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v", el)
	}
	if n := s.calls("network-info"); n != 2 {
		t.Fatalf("network-info sent %d times, want 2", n)
	}
	// The error never repeats the URL, whose query holds the address.
	if strings.Contains(err.Error(), docAddr.String()) {
		t.Fatalf("error repeats the address: %v", err)
	}
}

func TestRIPEstatHedgeOff(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: -1, Timeout: 100 * time.Millisecond})
	s.set(map[string]int{"network-info": 1}, nil)
	if _, err := r.LookupNetwork(context.Background(), docAddr); err == nil || !transient(err) {
		t.Fatalf("err %v", err)
	}
	if n := s.calls("network-info"); n != 1 {
		t.Fatalf("network-info sent %d times with hedging off", n)
	}
}

func TestRIPEstatHedgeNeedsAFreeSlot(t *testing.T) {
	// With one slot the first copy holds it: no hedge, and the follow-up
	// calls queue for the slot instead of running beside each other.
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 20 * time.Millisecond, Timeout: 150 * time.Millisecond, MaxInFlight: 1})
	s.set(map[string]int{"network-info": 1}, nil)
	if _, err := r.LookupNetwork(context.Background(), docAddr); err == nil {
		t.Fatal("stalled lookup succeeded")
	}
	if n := s.calls("network-info"); n != 1 {
		t.Fatalf("hedged without a free slot: %d requests", n)
	}
	got, err := r.LookupNetwork(context.Background(), docAddr)
	if err != nil || got.ISP == "" || got.City == "" {
		t.Fatalf("one slot: %+v, %v", got, err)
	}
}

func TestRIPEstatCallerCancelIsNotTransient(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: time.Hour})
	s.set(map[string]int{"network-info": 1}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := r.LookupNetwork(ctx, docAddr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	var te transientError
	if errors.As(err, &te) {
		t.Fatalf("the caller's own deadline was marked transient: %v", err)
	}
	if n := s.calls("network-info"); n != 1 {
		t.Fatalf("sent again after the caller gave up: %d", n)
	}
}

func TestRIPEstatSlowFollowUpIsTransientPartial(t *testing.T) {
	s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 20 * time.Millisecond, Timeout: 150 * time.Millisecond})
	s.set(map[string]int{"as-overview": 2}, nil)
	got, err := r.LookupNetwork(context.Background(), docAddr)
	if err != nil || got.ASN != 64500 || got.ISP != "" || got.City == "" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got.Partial != "ISP name unknown (no reply in time)" || !got.transient {
		t.Fatalf("partial %q, transient %v", got.Partial, got.transient)
	}
	// A failure that is RIPEstat's answer is a partial too, but not one
	// to ask again about soon.
	s.setFail("maxmind-geo-lite", 404)
	got, _ = r.LookupNetwork(context.Background(), docAddr)
	if got.Partial != "city unknown (RIPEstat maxmind-geo-lite: HTTP 404)" || got.transient {
		t.Fatalf("partial %q, transient %v", got.Partial, got.transient)
	}
}

func TestCachedLookupForgetsTransientFailures(t *testing.T) {
	clk := newClock()
	next := &countingLookup{err: transientError{fmt.Errorf("RIPEstat network-info: %w", context.DeadlineExceeded)}}
	c := NewCachedLookup(next, CacheOptions{ErrTTL: time.Minute, PartialTTL: time.Hour, TransientTTL: 3 * time.Second, Now: clk.Now})
	a := netip.MustParseAddr("203.0.113.7")
	ctx := context.Background()
	c.LookupNetwork(ctx, a)
	c.LookupNetwork(ctx, a)
	if next.count(a) != 1 {
		t.Fatalf("transient failure not kept for a moment: %d calls", next.count(a))
	}
	clk.Add(4 * time.Second)
	next.err, next.info = nil, NetworkInfo{ASN: 64500, Partial: "ISP name unknown (no reply in time)", transient: true}
	if got, err := c.LookupNetwork(ctx, a); err != nil || got.ASN != 64500 || next.count(a) != 2 {
		t.Fatalf("transient failure kept too long: %+v, %v, %d calls", got, err, next.count(a))
	}
	clk.Add(4 * time.Second)
	next.info = NetworkInfo{ASN: 64500, ISP: "Example Broadband"}
	if got, _ := c.LookupNetwork(ctx, a); got.ISP != "Example Broadband" || next.count(a) != 3 {
		t.Fatalf("transient partial kept like a real one: %+v, %d calls", got, next.count(a))
	}
}

// The case in #112: the first network-info reply is slow. With hedging
// the very first lookup is complete; without it (or when both copies
// stall) the result says to ask again shortly, and asking again without
// refresh then gets the ISP.
func TestLocateSurvivesSlowFirstLookup(t *testing.T) {
	finder := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}}
	t.Run("hedged", func(t *testing.T) {
		s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 50 * time.Millisecond})
		s.set(map[string]int{"network-info": 1}, nil)
		l := newTestLocator(LocatorConfig{Finders: []AddrFinder{finder}, Lookup: NewCachedLookup(r, CacheOptions{})})
		loc, err := l.Locate(context.Background(), false)
		if err != nil || loc.State != StateOK || loc.IPv4.ISP == "" || loc.RetryAfterS != 0 {
			t.Fatalf("got %+v, %v", loc, err)
		}
	})
	t.Run("ask again", func(t *testing.T) {
		s, r := newStallingRIPE(t, RIPEstatOptions{HedgeAfter: 20 * time.Millisecond, Timeout: 150 * time.Millisecond})
		s.set(map[string]int{"network-info": 2}, nil)
		clk := newClock()
		l := newTestLocator(LocatorConfig{
			Finders: []AddrFinder{finder},
			Lookup:  NewCachedLookup(r, CacheOptions{Now: clk.Now}),
			Now:     clk.Now,
		})
		ctx := context.Background()
		loc, _ := l.Locate(ctx, false)
		if loc.State != StatePartial || loc.IPv4.ISP != "" || loc.Message != "Your ISP couldn't be looked up: no reply in time" {
			t.Fatalf("first: %+v", loc)
		}
		if loc.RetryAfterS != 4 {
			t.Fatalf("retry_after_s %d, want 4", loc.RetryAfterS)
		}
		// Asked again too soon: the same result, no new request.
		clk.Add(time.Second)
		if again, _ := l.Locate(ctx, false); again.State != StatePartial || s.calls("network-info") != 2 {
			t.Fatalf("too soon: %+v, %d requests", again, s.calls("network-info"))
		}
		clk.Add(time.Duration(loc.RetryAfterS) * time.Second)
		loc, _ = l.Locate(ctx, false)
		if loc.State != StateOK || loc.IPv4.ISP == "" || loc.IPv4.ASN != 64500 || loc.RetryAfterS != 0 || loc.Message != "" {
			t.Fatalf("after the retry: %+v", loc)
		}
	})
	t.Run("real failure", func(t *testing.T) {
		// An answer that won't change soon gives no retry hint.
		s, r := newStallingRIPE(t, RIPEstatOptions{})
		s.setFail("network-info", 429)
		l := newTestLocator(LocatorConfig{Finders: []AddrFinder{finder}, Lookup: NewCachedLookup(r, CacheOptions{})})
		loc, _ := l.Locate(context.Background(), false)
		if loc.State != StatePartial || loc.RetryAfterS != 0 || !strings.Contains(loc.Message, "HTTP 429") {
			t.Fatalf("got %+v", loc)
		}
	})
}

// countingDial fails every dial with err and counts the attempts: a second
// copy of a request opens a second connection.
type countingDial struct {
	mu  sync.Mutex
	n   int
	err error
}

func (d *countingDial) dial(context.Context, string, string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.n++
	return nil, d.err
}

func (d *countingDial) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func TestRIPEstatDoesNotRepeatPermanentTransportFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		transient bool
		dials     int
	}{
		{"no such host", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "stat.ripe.net", IsNotFound: true}}, false, 1},
		{"DNS timeout", &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "stat.ripe.net", IsTimeout: true}}, true, 2},
		{"reset", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection reset by peer")}, true, 2},
	} {
		d := &countingDial{err: tc.err}
		r := NewRIPEstat(RIPEstatOptions{BaseURL: "http://stat.ripe.invalid", HedgeAfter: time.Hour, Dial: d.dial})
		_, err := r.LookupNetwork(context.Background(), docAddr)
		if err == nil || transient(err) != tc.transient {
			t.Fatalf("%s: err %v, transient %v", tc.name, err, transient(err))
		}
		if n := d.count(); n != tc.dials {
			t.Fatalf("%s: %d connections, want %d", tc.name, n, tc.dials)
		}
	}
}

func TestRIPEstatDoesNotRepeatCertificateFailures(t *testing.T) {
	// A certificate the client doesn't trust (here httptest's own; in the
	// field an intercepting proxy or a wrong clock) won't verify on a
	// second try either.
	f, _ := newFakeRIPE(t)
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewUnstartedServer(f)
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			mu.Lock()
			conns++
			mu.Unlock()
		}
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	r := NewRIPEstat(RIPEstatOptions{BaseURL: srv.URL, HedgeAfter: time.Hour})
	_, err := r.LookupNetwork(context.Background(), docAddr)
	if err == nil || transient(err) {
		t.Fatalf("err %v, transient %v", err, transient(err))
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Fatalf("%d connections, want 1", conns)
	}
}

// One RIPEstat serves the Network card and the Current server card; its
// cap on requests in flight holds across both.
func TestRIPEstatCapCoversEveryCaller(t *testing.T) {
	f, _ := newFakeRIPE(t)
	var mu sync.Mutex
	now, peak := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		now++
		peak = max(peak, now)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		f.ServeHTTP(w, req)
		mu.Lock()
		now--
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	r := NewRIPEstat(RIPEstatOptions{BaseURL: srv.URL, MaxInFlight: 3})
	card, servers := NewCachedLookup(r, CacheOptions{}), NewCachedLookup(r, CacheOptions{Max: 256})
	var wg sync.WaitGroup
	for i := range 8 {
		a := netip.AddrFrom4([4]byte{203, 0, 113, byte(i + 1)})
		lookup := NetworkLookup(card)
		if i%2 == 1 {
			lookup = servers
		}
		wg.Go(func() {
			if _, err := lookup.LookupNetwork(context.Background(), a); err != nil {
				t.Errorf("%s: %v", a, err)
			}
		})
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if peak > 3 || peak == 0 {
		t.Fatalf("%d requests in flight at once, cap 3", peak)
	}
}
