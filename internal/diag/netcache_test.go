package diag

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type countingLookup struct {
	mu    sync.Mutex
	calls map[netip.Addr]int
	info  NetworkInfo
	err   error
}

func (c *countingLookup) LookupNetwork(ctx context.Context, a netip.Addr) (NetworkInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[netip.Addr]int{}
	}
	c.calls[a]++
	if err := ctx.Err(); err != nil {
		return NetworkInfo{}, err
	}
	return c.info, c.err
}

func (c *countingLookup) count(a netip.Addr) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[a]
}

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestCachedLookupReusesAndExpires(t *testing.T) {
	next := &countingLookup{info: NetworkInfo{ASN: 64500}}
	clk := newClock()
	c := NewCachedLookup(next, CacheOptions{TTL: time.Hour, Now: clk.Now})
	a := netip.MustParseAddr("203.0.113.7")
	ctx := context.Background()
	for range 3 {
		if got, err := c.LookupNetwork(ctx, a); err != nil || got.ASN != 64500 {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
	if next.count(a) != 1 {
		t.Fatalf("looked up %d times, want 1", next.count(a))
	}
	// An IPv4-mapped form is the same address.
	c.LookupNetwork(ctx, netip.MustParseAddr("::ffff:203.0.113.7"))
	if next.count(a) != 1 {
		t.Fatal("mapped address missed the cache")
	}
	clk.Add(time.Hour + time.Second)
	c.LookupNetwork(ctx, a)
	if next.count(a) != 2 {
		t.Fatalf("expired entry not refreshed: %d", next.count(a))
	}
}

func TestCachedLookupErrorsAndPartials(t *testing.T) {
	next := &countingLookup{err: errors.New("down")}
	clk := newClock()
	c := NewCachedLookup(next, CacheOptions{TTL: time.Hour, ErrTTL: time.Minute, PartialTTL: 10 * time.Minute, Now: clk.Now})
	a := netip.MustParseAddr("203.0.113.7")
	ctx := context.Background()
	c.LookupNetwork(ctx, a)
	if _, err := c.LookupNetwork(ctx, a); err == nil || next.count(a) != 1 {
		t.Fatalf("error not cached briefly: %v, %d calls", err, next.count(a))
	}
	clk.Add(61 * time.Second)
	next.err, next.info = nil, NetworkInfo{ASN: 1, Partial: "city unknown"}
	if got, err := c.LookupNetwork(ctx, a); err != nil || got.ASN != 1 || next.count(a) != 2 {
		t.Fatalf("after error TTL: %+v, %v, %d calls", got, err, next.count(a))
	}
	clk.Add(11 * time.Minute)
	c.LookupNetwork(ctx, a)
	if next.count(a) != 3 {
		t.Fatalf("partial result kept too long: %d calls", next.count(a))
	}
}

func TestCachedLookupIgnoresCancelledCalls(t *testing.T) {
	next := &countingLookup{info: NetworkInfo{ASN: 7}}
	c := NewCachedLookup(next, CacheOptions{})
	a := netip.MustParseAddr("203.0.113.7")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.LookupNetwork(ctx, a); err == nil {
		t.Fatal("cancelled lookup succeeded")
	}
	if got, err := c.LookupNetwork(context.Background(), a); err != nil || got.ASN != 7 {
		t.Fatalf("a cancelled call poisoned the cache: %+v, %v", got, err)
	}
}

func TestCachedLookupBounded(t *testing.T) {
	next := &countingLookup{info: NetworkInfo{ASN: 7}}
	clk := newClock()
	c := NewCachedLookup(next, CacheOptions{Max: 2, Now: clk.Now})
	ctx := context.Background()
	a, b, d := netip.MustParseAddr("203.0.113.1"), netip.MustParseAddr("203.0.113.2"), netip.MustParseAddr("203.0.113.3")
	c.LookupNetwork(ctx, a)
	clk.Add(time.Second)
	c.LookupNetwork(ctx, b)
	clk.Add(time.Second)
	c.LookupNetwork(ctx, d) // evicts a, the oldest
	if len(c.entries) != 2 {
		t.Fatalf("%d entries", len(c.entries))
	}
	c.LookupNetwork(ctx, b)
	c.LookupNetwork(ctx, a)
	if next.count(b) != 1 || next.count(a) != 2 {
		t.Fatalf("wrong entry evicted: a=%d b=%d", next.count(a), next.count(b))
	}
}
