package diag

import (
	"context"
	"net/netip"
	"sync"
	"time"
)

// CachedLookup remembers what a NetworkLookup said about each address. An
// address's AS and city change rarely, while the public address itself is
// checked every time (switching from Wi-Fi to Ethernet or to a phone
// hotspot changes it), so this keeps repeat lookups off the network and
// within RIPEstat's fair-use guidance.
type CachedLookup struct {
	next       NetworkLookup
	ttl        time.Duration
	errTTL     time.Duration
	partialTTL time.Duration
	max        int
	now        func() time.Time
	mu         sync.Mutex
	entries    map[netip.Addr]cacheEntry
}

type cacheEntry struct {
	info    NetworkInfo
	err     error
	expires time.Time
	added   time.Time
}

// CacheOptions tune a CachedLookup; zero fields take the defaults.
type CacheOptions struct {
	TTL time.Duration // a found network, default 24 h
	// ErrTTL keeps a failed lookup from being retried at once (default
	// 30 s): short, so coming back online shows the ISP soon.
	ErrTTL time.Duration
	// PartialTTL is how long a result with a missing part is kept, so the
	// missing part is tried again later (default 10 min).
	PartialTTL time.Duration
	Max        int // entries, default 64; the oldest goes first
	Now        func() time.Time
}

func NewCachedLookup(next NetworkLookup, o CacheOptions) *CachedLookup {
	if o.TTL <= 0 {
		o.TTL = 24 * time.Hour
	}
	if o.ErrTTL <= 0 {
		o.ErrTTL = 30 * time.Second
	}
	if o.Max <= 0 {
		o.Max = 64
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.PartialTTL <= 0 {
		o.PartialTTL = 10 * time.Minute
	}
	return &CachedLookup{next: next, ttl: o.TTL, errTTL: o.ErrTTL, partialTTL: o.PartialTTL, max: o.Max, now: o.Now, entries: map[netip.Addr]cacheEntry{}}
}

func (c *CachedLookup) LookupNetwork(ctx context.Context, a netip.Addr) (NetworkInfo, error) {
	a = a.Unmap()
	now := c.now()
	c.mu.Lock()
	e, ok := c.entries[a]
	c.mu.Unlock()
	if ok && now.Before(e.expires) {
		return e.info, e.err
	}
	info, err := c.next.LookupNetwork(ctx, a)
	if err != nil && ctx.Err() != nil {
		return info, err // the caller gave up; that says nothing about the address
	}
	ttl := c.ttl
	switch {
	case err != nil:
		ttl = c.errTTL
	case info.Partial != "":
		ttl = c.partialTTL
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[a]; !ok && len(c.entries) >= c.max {
		var oldest netip.Addr
		var at time.Time
		for k, v := range c.entries {
			if at.IsZero() || v.added.Before(at) {
				oldest, at = k, v.added
			}
		}
		delete(c.entries, oldest)
	}
	c.entries[a] = cacheEntry{info: info, err: err, expires: now.Add(ttl), added: now}
	return info, err
}
