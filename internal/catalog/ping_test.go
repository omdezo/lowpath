package catalog

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// fakePinger answers from a table: an RTT, or an error, per address.
type fakePinger struct {
	mu    sync.Mutex
	rtt   map[netip.Addr]time.Duration
	err   map[netip.Addr]error
	calls map[netip.Addr]int
}

func newFakePinger() *fakePinger {
	return &fakePinger{rtt: map[netip.Addr]time.Duration{}, err: map[netip.Addr]error{}, calls: map[netip.Addr]int{}}
}

func (f *fakePinger) Ping(dst netip.Addr, _ time.Duration) (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[dst]++
	if err, ok := f.err[dst]; ok {
		return 0, err
	}
	if rtt, ok := f.rtt[dst]; ok {
		// Later replies are slower: the best one must win.
		return rtt + time.Duration(f.calls[dst]-1)*time.Millisecond, nil
	}
	return 0, ErrNoReply
}

func relaysOf(addrs ...string) []SDRRelay {
	var rs []SDRRelay
	for _, a := range addrs {
		rs = append(rs, SDRRelay{Addr: netip.MustParseAddr(a)})
	}
	return rs
}

func probeAll(t *testing.T, p *POPProber, pops []POP) map[string]POPPing {
	t.Helper()
	var mu sync.Mutex
	out := map[string]POPPing{}
	p.Probe(context.Background(), pops, func(id string, r POPPing) {
		mu.Lock()
		out[id] = r
		mu.Unlock()
	})
	return out
}

func TestProbePOPs(t *testing.T) {
	f := newFakePinger()
	f.rtt[netip.MustParseAddr("185.25.183.163")] = 17 * time.Millisecond
	f.rtt[netip.MustParseAddr("185.25.183.179")] = 19 * time.Millisecond
	f.rtt[netip.MustParseAddr("155.133.226.68")] = 112 * time.Millisecond // second fra relay never answers
	f.err[netip.MustParseAddr("203.0.113.9")] = errors.New("access denied")
	pops := []POP{
		{ID: "dxb", Relays: relaysOf("185.25.183.163", "185.25.183.179")},
		{ID: "fra", Relays: relaysOf("155.133.226.68", "155.133.226.70")},
		{ID: "dead", Relays: relaysOf("198.51.100.1")},
		{ID: "denied", Relays: relaysOf("203.0.113.9")},
		{ID: "v6", Relays: relaysOf("2606:4700::1111")},
		{ID: "partner"}, // no relays: not pinged, not reported
	}
	got := probeAll(t, NewPOPProber(f, nil), pops)

	if r := got["dxb"]; r.Received != 4 || r.Sent != 4 || r.RTT != 17*time.Millisecond || r.LossPct() != 0 {
		t.Errorf("dxb = %+v", r)
	}
	if r := got["fra"]; r.Received != 2 || r.Sent != 4 || r.RTT != 112*time.Millisecond || r.LossPct() != 50 {
		t.Errorf("fra = %+v", r)
	}
	if r := got["dead"]; r.Received != 0 || r.Sent != 2 || r.Note != "No reply to ping" || r.LossPct() != 100 {
		t.Errorf("dead = %+v", r)
	}
	// A ping that can't even be sent is not packet loss.
	if r := got["denied"]; r.Sent != 0 || r.Note == "" || f.calls[netip.MustParseAddr("203.0.113.9")] != 1 {
		t.Errorf("denied = %+v after %d tries", r, f.calls[netip.MustParseAddr("203.0.113.9")])
	}
	if r := got["v6"]; r.Sent != 0 || r.Note == "" {
		t.Errorf("v6 = %+v", r)
	}
	if _, ok := got["partner"]; ok {
		t.Error("a POP without relays was pinged")
	}
}

func TestPickRelaysSpreads(t *testing.T) {
	rs := relaysOf("1.1.1.1", "1.1.1.2", "1.1.1.3", "1.1.1.4", "2606:4700::1")
	got := pickRelays(rs, 2)
	if len(got) != 2 || got[0].String() != "1.1.1.1" || got[1].String() != "1.1.1.3" {
		t.Fatalf("picked %v", got)
	}
	if got := pickRelays(rs[:1], 2); len(got) != 1 {
		t.Fatalf("picked %v from one relay", got)
	}
}

func TestProbeStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := 0
	pops := make([]POP, 50)
	for i := range pops {
		pops[i] = POP{ID: "p", Relays: relaysOf("8.8.8.8")}
	}
	NewPOPProber(newFakePinger(), nil).Probe(ctx, pops, func(string, POPPing) { n++ })
	if n != 0 {
		t.Fatalf("%d results after cancel", n)
	}
}
