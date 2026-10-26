package diag

import (
	"errors"
	"math"
	"net/netip"
	"time"
)

var (
	// ErrUnsupported: this platform has no implementation (the caller shows
	// "—" instead of a number).
	ErrUnsupported = errors.New("diag: not supported on this platform")
	// ErrNoRoute: the PC has no default route, i.e. it is offline.
	ErrNoRoute = errors.New("diag: no default route (offline?)")
)

// Link kinds reported by NetStats.
const (
	LinkUnknown  = ""
	LinkEthernet = "ethernet"
	LinkWiFi     = "wifi"
	LinkOther    = "other"
)

// Route is the interface that carries traffic to the internet and the
// gateway (the home router) on it.
type Route struct {
	Index   int        `json:"index"`
	Name    string     `json:"name"`
	Gateway netip.Addr `json:"gateway"` // zero: none (point-to-point link, IPv6 without a router yet, ...)
	Kind    string     `json:"kind"`    // LinkEthernet, LinkWiFi, LinkOther or LinkUnknown
}

// Counters are an interface's cumulative byte counters at one moment.
type Counters struct {
	At      time.Time
	RxBytes uint64
	TxBytes uint64
	// Link speeds the adapter negotiated, in bits/s (0 = unknown). This is
	// Wi-Fi or Ethernet speed, not what the internet line can carry.
	RxLinkBps uint64
	TxLinkBps uint64
}

// NetStats reads the default route and per-interface byte counters. The
// implementation is per OS (GetIfEntry2Ex on Windows, sysfs on Linux, the
// routing sysctl on macOS); everything else uses it through this interface.
type NetStats interface {
	DefaultRoute() (Route, error)
	Counters(index int) (Counters, error)
}

// NewNetStats returns the platform's NetStats.
func NewNetStats() NetStats { return newPlatformNetStats() }

// Throughput is a measured rate, in bytes per second, over Window.
type Throughput struct {
	RxBps  float64
	TxBps  float64
	Window time.Duration
}

// Rate is the throughput between two counter readings of the same
// interface. ok is false when the readings can't be compared (same instant,
// out of order, or a counter went backwards in a way that isn't a 32-bit
// wrap, e.g. the adapter was reset).
func Rate(prev, cur Counters) (Throughput, bool) {
	dt := cur.At.Sub(prev.At)
	if dt <= 0 {
		return Throughput{}, false
	}
	rx, ok1 := counterDelta(prev.RxBytes, cur.RxBytes)
	tx, ok2 := counterDelta(prev.TxBytes, cur.TxBytes)
	if !ok1 || !ok2 {
		return Throughput{}, false
	}
	s := dt.Seconds()
	return Throughput{RxBps: float64(rx) / s, TxBps: float64(tx) / s, Window: dt}, true
}

// counterDelta is cur-prev for a counter that may be 32 bits wide (macOS
// hands unsigned apps 32-bit values in 64-bit fields, and so do some old
// drivers). A counter that went backwards is taken as a 32-bit wrap only
// when it was in the upper half of the 32-bit range and the wrapped delta
// is under 2 GiB (far more than one reading apart at any real line rate).
// Anything else is an adapter reset: counting it as a wrap would invent up
// to 4 GiB of traffic, which the passive detector would learn as its line
// rate.
func counterDelta(prev, cur uint64) (uint64, bool) {
	if cur >= prev {
		return cur - prev, true
	}
	const half = 1 << 31
	if prev <= math.MaxUint32 && prev >= half {
		if d := cur + (1 << 32) - prev; d < half {
			return d, true
		}
	}
	return 0, false
}

// Meter turns successive counter readings into rates, starting over when
// the default interface changes (Wi-Fi to Ethernet, VPN up, ...).
type Meter struct {
	index int
	prev  Counters
	have  bool
}

// NewMeter returns an empty meter.
func NewMeter() *Meter { return &Meter{} }

// Add records a reading of interface index and returns the rate since the
// previous reading of the same interface.
func (m *Meter) Add(index int, c Counters) (Throughput, bool) {
	if !m.have || index != m.index {
		m.index, m.prev, m.have = index, c, true
		return Throughput{}, false
	}
	t, ok := Rate(m.prev, c)
	m.prev = c
	return t, ok
}

// Reset forgets the last reading (e.g. after a read error).
func (m *Meter) Reset() { m.have = false }
