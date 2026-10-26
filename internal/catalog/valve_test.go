package catalog

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"
)

// fakeSource serves a fixed body or error and counts downloads. With block
// set it hangs until the context ends, like a dead link.
type fakeSource struct {
	mu    sync.Mutex
	body  []byte
	err   error
	block bool
	calls int
}

func (f *fakeSource) Fetch(ctx context.Context, appID int) ([]byte, error) {
	f.mu.Lock()
	f.calls++
	body, err, block := f.body, f.err, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, &FetchError{Err: ctx.Err()}
	}
	return body, err
}

func (f *fakeSource) set(body []byte, err error) {
	f.mu.Lock()
	f.body, f.err = body, err
	f.mu.Unlock()
}

func (f *fakeSource) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeProber gives every POP a ping from a table; others get no reply.
// With gate set, the first round waits for it to close.
type fakeProber struct {
	mu     sync.Mutex
	rtt    map[string]time.Duration
	rounds int
	gate   chan struct{}
}

func (f *fakeProber) Probe(ctx context.Context, pops []POP, done func(string, POPPing)) {
	f.mu.Lock()
	f.rounds++
	first, gate := f.rounds == 1, f.gate
	f.mu.Unlock()
	if first && gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return
		}
	}
	for _, p := range pops {
		if len(p.Relays) == 0 {
			continue
		}
		if rtt, ok := f.rtt[p.ID]; ok {
			done(p.ID, POPPing{RTT: rtt, Sent: 4, Received: 4, At: time.Now()})
		} else {
			done(p.ID, POPPing{Sent: 4, Note: "No reply to ping", At: time.Now()})
		}
	}
}

func (f *fakeProber) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rounds
}

// fakeClock is a settable time.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// memCache is an in-memory Cache.
type memCache struct {
	mu sync.Mutex
	b  map[string][]byte
	at map[string]time.Time
}

func newMemCache() *memCache { return &memCache{b: map[string][]byte{}, at: map[string]time.Time{}} }

func (m *memCache) Load(k string) ([]byte, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.b[k]
	if !ok {
		return nil, time.Time{}, os.ErrNotExist
	}
	return b, m.at[k], nil
}

func (m *memCache) Save(k string, b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.b[k], m.at[k] = b, time.Now()
	return nil
}

type rig struct {
	src   *fakeSource
	probe *fakeProber
	cache *memCache
	clock *fakeClock
	v     *ValveServers
}

func newRig(t *testing.T) *rig {
	r := &rig{
		src:   &fakeSource{body: loadFixture(t)},
		probe: &fakeProber{rtt: map[string]time.Duration{"dxb": 17 * time.Millisecond, "fra": 113 * time.Millisecond, "bom2": 44 * time.Millisecond}},
		cache: newMemCache(),
		clock: &fakeClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)},
	}
	r.v = NewValveServers(ValveOptions{Source: r.src, Prober: r.probe, Cache: r.cache, Now: r.clock.Now, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(r.v.Close)
	return r
}

// waitFor polls List until ok says the background work is done.
func waitFor(t *testing.T, v *ValveServers, ok func(ServerList) bool) ServerList {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		l := v.List(730)
		if ok(l) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out; last list: %+v", l)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func settled(l ServerList) bool { return l.State != "loading" && !l.Pinging && l.PingedAt != nil }

func TestValveListFetchesAndPings(t *testing.T) {
	r := newRig(t)
	r.src.block = true // first answer must not wait for the download
	start := time.Now()
	if l := r.v.List(730); l.State != "loading" || len(l.POPs) != 0 || l.Source != "valve_sdr" {
		t.Fatalf("first list = %+v", l)
	}
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("List blocked on the download")
	}
	r.v.Close() // ends the hung download
	if l := r.v.List(730); l.State != "error" {
		t.Fatalf("after a failed download: %+v", l)
	}

	r = newRig(t)
	l := waitFor(t, r.v, settled)
	if l.State != "ready" || l.Stale || l.Revision == 0 || l.FetchedAt == nil {
		t.Fatalf("list = %+v", l)
	}
	// 11 of the 14 fixture POPs have relays; ctu, dsto and mdb1 have none.
	if len(l.POPs) != 11 || l.NoRelay != 3 {
		t.Fatalf("%d POPs, %d without relays", len(l.POPs), l.NoRelay)
	}
	want := []string{"dxb", "bom2", "fra"}
	for i, id := range want {
		p := l.POPs[i]
		if p.ID != id || p.PingMS == nil || p.LossPct == nil || *p.LossPct != 0 {
			t.Fatalf("row %d = %+v, want %s with a ping", i, p, id)
		}
	}
	if *l.POPs[0].PingMS != 17 {
		t.Fatalf("dxb ping = %v", *l.POPs[0].PingMS)
	}
	last := l.POPs[len(l.POPs)-1]
	if last.PingMS != nil || last.Note == "" || last.LossPct == nil || *last.LossPct != 100 {
		t.Fatalf("unanswered POP = %+v: no number, a reason", last)
	}
	for _, p := range l.POPs {
		if p.Lat == nil || p.Lon == nil {
			t.Fatalf("%s has no coordinates", p.ID)
		}
		if p.ID == "mad" && (p.GeoFix != GeoCorrected || *p.Lon > 0) {
			t.Fatalf("mad = %+v", p)
		}
	}
	if _, _, err := r.cache.Load("sdr-730"); err != nil {
		t.Fatal("download not saved for offline use")
	}
	if r.src.count() != 1 || r.probe.count() != 1 {
		t.Fatalf("%d downloads, %d ping rounds", r.src.count(), r.probe.count())
	}
}

func TestValveRefreshAndRepings(t *testing.T) {
	r := newRig(t)
	waitFor(t, r.v, settled)

	// Within the TTLs nothing is fetched or pinged again.
	r.clock.Add(2 * time.Minute)
	r.v.List(730)
	if r.src.count() != 1 || r.probe.count() != 1 {
		t.Fatalf("%d downloads, %d ping rounds", r.src.count(), r.probe.count())
	}
	// Pings go stale first.
	r.clock.Add(4 * time.Minute)
	waitFor(t, r.v, func(l ServerList) bool { return r.probe.count() == 2 && !l.Pinging })
	// Ping asks for a new round right away.
	r.v.Ping(730)
	waitFor(t, r.v, func(l ServerList) bool { return r.probe.count() == 3 && !l.Pinging })
	// After an hour the list is downloaded again (and pinged again).
	r.clock.Add(time.Hour)
	waitFor(t, r.v, func(l ServerList) bool { return r.src.count() == 2 && settled(l) })
}

func TestValveOfflineWithoutCache(t *testing.T) {
	r := newRig(t)
	r.src.set(nil, &FetchError{Err: errors.New("network is unreachable")})
	l := waitFor(t, r.v, func(l ServerList) bool { return l.State == "error" })
	if l.Message == "" || len(l.POPs) != 0 || r.probe.count() != 0 {
		t.Fatalf("offline list = %+v", l)
	}
	// No hammering: the next try waits for the backoff.
	for range 5 {
		r.v.List(730)
	}
	if r.src.count() != 1 {
		t.Fatalf("%d downloads during the backoff", r.src.count())
	}
	// Back online after the backoff: the list arrives.
	r.src.set(loadFixture(t), nil)
	r.clock.Add(retryMin)
	l = waitFor(t, r.v, settled)
	if l.State != "ready" || l.Message != "" {
		t.Fatalf("after reconnecting: %+v", l)
	}
}

func TestValveOfflineUsesCache(t *testing.T) {
	r := newRig(t)
	r.cache.b["sdr-730"] = loadFixture(t)
	r.cache.at["sdr-730"] = r.clock.Now().Add(-3 * time.Hour) // old: a refresh is due
	r.src.set(nil, &FetchError{Status: 503})
	l := r.v.List(730)
	if l.State != "ready" || len(l.POPs) == 0 {
		t.Fatalf("cached list not shown at once: %+v", l)
	}
	l = waitFor(t, r.v, func(l ServerList) bool { return settled(l) && l.Message != "" })
	if !l.Stale || l.POPs[0].ID != "dxb" {
		t.Fatalf("stale list = %+v", l)
	}
}

func TestValveFreshCacheSkipsDownload(t *testing.T) {
	r := newRig(t)
	r.cache.b["sdr-730"] = loadFixture(t)
	r.cache.at["sdr-730"] = r.clock.Now().Add(-10 * time.Minute)
	l := waitFor(t, r.v, settled)
	if l.State != "ready" || l.Stale || r.src.count() != 0 {
		t.Fatalf("list %+v after %d downloads", l, r.src.count())
	}
}

func TestValveCorruptCacheIgnored(t *testing.T) {
	r := newRig(t)
	r.cache.b["sdr-730"] = []byte("garbage")
	r.cache.at["sdr-730"] = r.clock.Now()
	l := waitFor(t, r.v, settled)
	if l.State != "ready" || r.src.count() != 1 {
		t.Fatalf("list %+v after %d downloads", l, r.src.count())
	}
}

func TestRetryIn(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "a moment", 30 * time.Second: "30 s", 2 * time.Minute: "2 min", 14*time.Minute + 50*time.Second: "15 min"} {
		if got := retryIn(d); got != want {
			t.Errorf("retryIn(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestValveCacheFromTheFutureIsRefreshed(t *testing.T) {
	// Saved while the PC's clock was a year ahead: without a check the list
	// would count as current until that date.
	r := newRig(t)
	r.cache.b["sdr-730"] = loadFixture(t)
	r.cache.at["sdr-730"] = r.clock.Now().Add(365 * 24 * time.Hour)
	if l := r.v.List(730); l.State != "ready" || !l.Stale {
		t.Fatalf("future-dated cache = %+v, want shown and stale", l)
	}
	l := waitFor(t, r.v, func(l ServerList) bool { return r.src.count() == 1 && settled(l) && !l.Stale })
	if l.FetchedAt == nil || !l.FetchedAt.Equal(r.clock.Now()) {
		t.Fatalf("fetched_at = %v after the refresh", l.FetchedAt)
	}
}

func TestValvePingRetriesDownloadNow(t *testing.T) {
	r := newRig(t)
	r.src.set(nil, &FetchError{Err: errors.New("network is unreachable")})
	waitFor(t, r.v, func(l ServerList) bool { return l.State == "error" })
	// Back online, still inside the backoff: List waits, the button doesn't.
	r.src.set(loadFixture(t), nil)
	r.v.List(730)
	if r.src.count() != 1 {
		t.Fatalf("%d downloads during the backoff", r.src.count())
	}
	r.v.Ping(730)
	l := waitFor(t, r.v, settled)
	if l.State != "ready" || r.src.count() != 2 {
		t.Fatalf("after try again: %+v, %d downloads", l, r.src.count())
	}
}

func TestValveNewListDuringPingIsPinged(t *testing.T) {
	// An old saved list is pinged at once; the download lands while that
	// round still runs. Its POPs must get their own round.
	r := newRig(t)
	r.probe.gate = make(chan struct{})
	r.cache.b["sdr-730"] = loadFixture(t)
	r.cache.at["sdr-730"] = r.clock.Now().Add(-3 * time.Hour)
	r.v.List(730)
	waitFor(t, r.v, func(l ServerList) bool { return r.src.count() == 1 && !l.Stale && l.Pinging })
	close(r.probe.gate)
	waitFor(t, r.v, func(l ServerList) bool { return r.probe.count() == 2 && settled(l) })
}

func TestValveCloseStopsNewWork(t *testing.T) {
	r := newRig(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				r.v.List(730)
				r.v.Ping(730)
			}
		}()
	}
	r.v.Close()
	n := r.src.count()
	wg.Wait()
	r.v.List(730)
	r.v.Ping(730)
	if r.src.count() != n {
		t.Fatalf("downloads started after Close: %d, then %d", n, r.src.count())
	}
}
