package catalog

import (
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	amsterdam = NewGeo(52.37, 4.90)
	frankfurt = NewGeo(50.11, 8.68)
	sydney    = NewGeo(-33.87, 151.21)
)

func TestCheckGeo(t *testing.T) {
	// Amsterdam to Frankfurt is about 360 km; 8 ms allows 816 km.
	c := CheckGeo(amsterdam, frankfurt, 8*time.Millisecond)
	if c.State != GeoCheckOK || c.DistanceKM == nil || *c.DistanceKM < 300 || *c.DistanceKM > 420 || c.MaxKM == nil || *c.MaxKM != 816 {
		t.Fatalf("near city: %+v", c)
	}
	// "Sydney" at 8 ms from Amsterdam is impossible: the database is wrong.
	c = CheckGeo(amsterdam, sydney, 8*time.Millisecond)
	if c.State != GeoCheckTooFar || !strings.Contains(c.Note, "within 820 km") || !strings.Contains(c.Note, "where the IP database puts you") {
		t.Fatalf("far city: %+v", c)
	}
	// Just beyond the light limit, inside the geolocation slack: allowed.
	c = CheckGeo(amsterdam, frankfurt, 3*time.Millisecond) // 306 km + slack
	if c.State != GeoCheckOK {
		t.Fatalf("within slack: %+v", c)
	}
	// No ping: nothing to check against. No reply yet, IPv6 and "can't
	// send" look the same here, so the note must not claim the server
	// ignores pings (the ping note says why).
	if c := CheckGeo(amsterdam, frankfurt, 0); c.State != GeoCheckUnknown || c.MaxKM != nil || c.DistanceKM == nil ||
		strings.Contains(c.Note, "doesn't answer") || c.Note == "" {
		t.Fatalf("no ping: %+v", c)
	}
	// No city for this PC: the ping still bounds the distance.
	if c := CheckGeo(Geo{}, frankfurt, 20*time.Millisecond); c.State != GeoCheckUnknown || !strings.Contains(c.Note, "2000 km") {
		t.Fatalf("no own city: %+v", c)
	}
	if NewGeo(91, 0).Known || NewGeo(0, 181).Known || !NewGeo(-33, 151).Known {
		t.Fatal("NewGeo range check")
	}
}

// scriptPinger answers each address from a list of results, then starts
// the list over again.
type scriptPinger struct {
	mu     sync.Mutex
	script map[netip.Addr][]error
	rtt    time.Duration
	calls  map[netip.Addr]int
}

func (p *scriptPinger) Ping(dst netip.Addr, _ time.Duration) (time.Duration, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.script[dst]
	i := p.calls[dst] % len(s)
	p.calls[dst]++
	if s[i] != nil {
		return 0, s[i]
	}
	return p.rtt + time.Duration(p.calls[dst]%2)*2*time.Millisecond, nil
}

func (p *scriptPinger) count(a netip.Addr) int { p.mu.Lock(); defer p.mu.Unlock(); return p.calls[a] }

func TestLivePinger(t *testing.T) {
	ok := netip.MustParseAddr("203.0.113.10")
	lossy := netip.MustParseAddr("203.0.113.11")
	silent := netip.MustParseAddr("203.0.113.12")
	v6 := netip.MustParseAddr("2001:db8::12")
	broken := netip.MustParseAddr("203.0.113.13")
	p := &scriptPinger{rtt: 20 * time.Millisecond, calls: map[netip.Addr]int{}, script: map[netip.Addr][]error{
		ok:     {nil},
		lossy:  {nil, ErrNoReply, nil, nil},
		silent: {ErrNoReply},
		v6:     {ErrIPv6},
		broken: {errors.New("access denied")},
	}}
	l := NewLivePinger(p, LiveOptions{Interval: 5 * time.Millisecond, Window: 4, Idle: 200 * time.Millisecond})
	defer l.Close()

	for _, a := range []netip.Addr{ok, lossy, silent, v6, broken} {
		if s := l.Stats(a); s.Sent != 0 || s.Note == "" {
			t.Fatalf("%v first call: %+v, want nothing measured yet", a, s)
		}
	}
	time.Sleep(100 * time.Millisecond)

	s := l.Stats(ok)
	if s.Sent != 4 || s.Received != 4 || s.Min != 20*time.Millisecond || s.Avg != 21*time.Millisecond || s.Jitter != time.Millisecond || s.LossPct() != 0 {
		t.Errorf("ok: %+v", s)
	}
	if s := l.Stats(lossy); s.Sent != 4 || s.Received != 3 || s.LossPct() != 25 {
		t.Errorf("lossy: %+v (the window keeps the last 4)", s)
	}
	if s := l.Stats(silent); s.Received != 0 || !strings.Contains(s.Note, "doesn't answer") {
		t.Errorf("silent: %+v", s)
	}
	if s := l.Stats(v6); s.Sent != 0 || !strings.Contains(s.Note, "IPv6") {
		t.Errorf("IPv6: %+v, want a note and no loss", s)
	}
	if s := l.Stats(broken); s.Sent != 0 || !strings.Contains(s.Note, "access denied") {
		t.Errorf("can't send: %+v, want a note and no loss", s)
	}

	// Nobody asks about ok any more: it stops being pinged.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := p.count(ok)
		time.Sleep(50 * time.Millisecond)
		if p.count(ok) == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("an address nobody asks about must stop being pinged")
		}
		l.Stats(lossy) // keep the others going; only ok goes idle
	}
}
