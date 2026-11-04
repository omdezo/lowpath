package relay

import (
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/proto"
)

var errNoReply = errors.New("no reply")

func observeRTT(m *monitor, a netip.Addr) (time.Duration, bool) {
	l := m.observe(a)
	return l.RTT, l.HasRTT
}

// The monitor reports the leg's loss and jitter from its latest pings, and
// only alongside a known RTT.
func TestMonitorReportsLossAndJitter(t *testing.T) {
	m := newMonitor(nil, time.Hour)
	a := netip.MustParseAddr("198.51.100.30")
	s := &server{lastUsed: time.Now()}
	m.servers[a] = s

	// Every fifth ping lost (20%), replies alternating 10 and 14 ms.
	replies := 0
	ping := func(i int) {
		t.Helper()
		var err error
		rtt := 10 * time.Millisecond
		if i%5 == 4 {
			err, rtt = errNoReply, 0
		} else {
			if replies%2 == 1 {
				rtt = 14 * time.Millisecond
			}
			replies++
		}
		if !m.record(a, s, rtt, err) {
			t.Fatal("server forgotten while in use")
		}
	}
	for i := range legMinPings - 1 {
		ping(i)
	}
	if l := m.observe(a); !l.HasRTT || l.HasLoss || l.HasJitter {
		t.Fatalf("after %d pings: %+v, want the RTT alone", legMinPings-1, l)
	}
	for i := legMinPings - 1; i < 200; i++ {
		ping(i)
	}
	l := m.observe(a)
	if !l.HasLoss || l.LossPct != 20 {
		t.Errorf("loss %v/%v, want 20%%", l.LossPct, l.HasLoss)
	}
	if !l.HasJitter || l.Jitter < 3900*time.Microsecond || l.Jitter > 4*time.Millisecond {
		t.Errorf("jitter %v/%v, want close to the 4 ms steps", l.Jitter, l.HasJitter)
	}

	// Three misses in a row: the RTT is stale, and the misses aren't passed
	// on as a loss figure either.
	for range maxMisses {
		m.record(a, s, 0, errNoReply)
	}
	if l := m.observe(a); l != (proto.ServerLeg{}) {
		t.Fatalf("after %d misses in a row: %+v, want nothing known", maxMisses, l)
	}
}

func TestMonitorSmoothingAndMisses(t *testing.T) {
	m := newMonitor(nil, time.Hour) // record is driven by hand; no pinger runs
	a := netip.MustParseAddr("203.0.113.1")
	s := &server{lastUsed: time.Now()}
	m.servers[a] = s

	step := func(rtt time.Duration, err error, want time.Duration, known bool) {
		t.Helper()
		if !m.record(a, s, rtt, err) {
			t.Fatal("server forgotten while in use")
		}
		got, ok := observeRTT(m, a)
		if ok != known || (known && got != want) {
			t.Fatalf("after (%v, %v): got %v/%v, want %v/%v", rtt, err, got, ok, want, known)
		}
	}
	step(20*time.Millisecond, nil, 20*time.Millisecond, true) // first sample taken as is
	step(40*time.Millisecond, nil, 25*time.Millisecond, true) // EWMA: 20 + 0.25*(40-20)
	step(0, errNoReply, 25*time.Millisecond, true)            // one miss keeps the estimate
	step(0, errNoReply, 25*time.Millisecond, true)
	step(0, errNoReply, 0, false)                             // third miss in a row: unknown
	step(10*time.Millisecond, nil, 10*time.Millisecond, true) // restart from the fresh sample

	s.lastUsed = time.Now().Add(-serverIdle - time.Second)
	if m.record(a, s, time.Millisecond, nil) {
		t.Fatal("idle server kept")
	}
	if m.count() != 0 {
		t.Fatal("idle server still in the table")
	}
}

func TestMonitorPingsRightAwayThenPeriodically(t *testing.T) {
	var mu sync.Mutex
	calls := map[netip.Addr]int{}
	m := newMonitor(func(a netip.Addr, _ time.Duration) (time.Duration, error) {
		mu.Lock()
		calls[a]++
		mu.Unlock()
		return 5 * time.Millisecond, nil
	}, 10*time.Millisecond)
	defer m.stop()

	a := netip.MustParseAddr("198.51.100.1")
	m.touch(a)
	m.touch(a) // same server: still one pinger
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := calls[a]
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d pings in 2s at a 10ms interval", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if d, ok := observeRTT(m, a); !ok || d != 5*time.Millisecond {
		t.Fatalf("observe = %v, %v", d, ok)
	}
	if m.count() != 1 {
		t.Fatalf("%d servers, want 1", m.count())
	}
}

func TestMonitorLimit(t *testing.T) {
	var running atomic.Int64
	m := newMonitor(func(netip.Addr, time.Duration) (time.Duration, error) {
		running.Add(1)
		return time.Millisecond, nil
	}, time.Hour)
	defer m.stop()

	base := netip.MustParseAddr("2001:db8::").As16()
	for i := 0; i < maxServers+10; i++ {
		b := base
		b[14], b[15] = byte(i>>8), byte(i)
		m.touch(netip.AddrFrom16(b))
	}
	if n := m.count(); n != maxServers {
		t.Fatalf("%d servers monitored, want the cap %d", n, maxServers)
	}
	if _, ok := observeRTT(m, netip.MustParseAddr("192.0.2.200")); ok {
		t.Fatal("server over the cap reported a RTT")
	}
	m.stop()
	if _, ok := observeRTT(m, netip.MustParseAddr("192.0.2.201")); ok || m.count() != maxServers {
		t.Fatal("stopped monitor accepted a new server")
	}
}
