package diag

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"lowpath/internal/icmp"
)

// --- statistics -------------------------------------------------------------

var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// series makes results 50 ms apart from t0; rtt(i) < 0 means lost.
func series(n int, rtt func(i int) time.Duration) []gwResult {
	rs := make([]gwResult, n)
	for i := range rs {
		rs[i].sent = t0.Add(time.Duration(i) * 50 * time.Millisecond)
		if d := rtt(i); d < 0 {
			rs[i].lost = true
		} else {
			rs[i].rtt = d
		}
	}
	return rs
}

func TestGatewayStats(t *testing.T) {
	// 200 pings: 2 ms, except a 120 ms spike, a run of 6 lost (a 300 ms
	// stall) and 2 single losses.
	rs := series(200, func(i int) time.Duration {
		switch {
		case i == 50:
			return 120 * time.Millisecond
		case i >= 100 && i < 106, i == 150, i == 170:
			return -1
		}
		return 2 * time.Millisecond
	})
	st := gatewayStats(rs, t0.Add(time.Hour))
	if st.Sent != 200 || st.Lost != 8 || st.MaxLossRun != 6 || st.Spikes != 1 {
		t.Fatalf("sent %d lost %d run %d spikes %d", st.Sent, st.Lost, st.MaxLossRun, st.Spikes)
	}
	if st.LossPct != 4 {
		t.Errorf("loss %.2f%%, want 4%%", st.LossPct)
	}
	if *st.P50MS != 2 || *st.MaxMS != 120 || *st.MinMS != 2 {
		t.Errorf("p50 %v max %v min %v", *st.P50MS, *st.MaxMS, *st.MinMS)
	}
	if st.P99MS == nil || *st.P99MS != 2 {
		t.Errorf("p99 %v: one spike in 192 replies is above the 99th percentile", st.P99MS)
	}
	// Jitter: the spike adds two jumps of 118 ms over 190 differences.
	if want := 236.0 / 190; st.JitterMS == nil || *st.JitterMS < want-0.01 || *st.JitterMS > want+0.01 {
		t.Errorf("jitter %v, want %.3f", st.JitterMS, want)
	}
	if st.WindowS < 9.9 || st.WindowS > 10 {
		t.Errorf("window %.2f s", st.WindowS)
	}
	if st.Limited {
		t.Error("bursty loss counted as a rate limit")
	}
}

func TestGatewayStatsFewRepliesAndSettled(t *testing.T) {
	rs := series(60, func(int) time.Duration { return time.Millisecond })
	// Only pings sent before settled count: later ones may still be lost.
	st := gatewayStats(rs, t0.Add(1*time.Second))
	if st.Sent != 20 {
		t.Fatalf("sent %d, want the 20 settled pings", st.Sent)
	}
	if st.P99MS != nil {
		t.Errorf("p99 %v from 20 replies; it needs %d", *st.P99MS, minP99Replies)
	}
	if st.P95MS == nil {
		t.Error("no p95")
	}
	none := gatewayStats(series(10, func(int) time.Duration { return -1 }), t0.Add(time.Hour))
	if none.P50MS != nil || none.JitterMS != nil || none.Lost != 10 || none.MaxLossRun != 10 {
		t.Errorf("all lost: %+v", none)
	}
	if empty := gatewayStats(nil, t0); empty.Sent != 0 || empty.LossPct != 0 {
		t.Errorf("empty: %+v", empty)
	}
}

func TestGatewaySeries(t *testing.T) {
	rs := series(60, func(i int) time.Duration {
		if i == 25 {
			return -1
		}
		return time.Duration(1+i%20) * time.Millisecond
	})
	got := gatewaySeries(rs, t0, t0.Add(time.Hour))
	if len(got) != 3 {
		t.Fatalf("%d seconds, want 3", len(got))
	}
	if got[0].T != t0.Unix() || got[0].Sent != 20 || got[1].Lost != 1 || *got[0].MaxMS != 20 {
		t.Errorf("first seconds: %+v %+v", got[0], got[1])
	}
	if got := gatewaySeries(rs, t0.Add(2*time.Second), t0.Add(time.Hour)); len(got) != 1 {
		t.Errorf("from the third second: %d entries", len(got))
	}
}

func TestLimitedPattern(t *testing.T) {
	// A router that answers the first 10 pings of every second.
	limit := series(20*15, func(i int) time.Duration {
		if i%20 < 10 {
			return time.Millisecond
		}
		return -1
	})
	lim, per := limitedPattern(limit, t0.Add(time.Hour))
	if !lim || per != 10 {
		t.Fatalf("limited %v at %d/s, want 10/s", lim, per)
	}
	// The same, but too short to judge.
	if lim, _ := limitedPattern(limit[:20*8], t0.Add(time.Hour)); lim {
		t.Error("8 s judged")
	}
	// Wi-Fi: bursts of loss, most seconds clean.
	wifi := series(20*15, func(i int) time.Duration {
		if (i/20)%3 == 0 && i%20 < 8 {
			return -1
		}
		return 3 * time.Millisecond
	})
	if lim, _ := limitedPattern(wifi, t0.Add(time.Hour)); lim {
		t.Error("bursty Wi-Fi loss judged a rate limit")
	}
	// A dead second (a stall) is not a limiter's steady rate.
	dead := series(20*15, func(i int) time.Duration {
		if i/20 == 7 || i%20 >= 10 {
			return -1
		}
		return time.Millisecond
	})
	if lim, _ := limitedPattern(dead, t0.Add(time.Hour)); lim {
		t.Error("a second without replies judged a rate limit")
	}
	// Steady small loss isn't worth calling a limiter.
	small := series(20*15, func(i int) time.Duration {
		if i%20 == 0 {
			return -1
		}
		return time.Millisecond
	})
	if lim, _ := limitedPattern(small, t0.Add(time.Hour)); lim {
		t.Error("1 in 20 lost judged a rate limit")
	}
}

func TestGatewayAddr(t *testing.T) {
	for _, c := range []struct {
		r    Route
		want string
	}{
		{Route{Index: 7, Gateway: netip.MustParseAddr("192.0.2.1")}, "192.0.2.1"},
		{Route{Index: 7, Gateway: netip.MustParseAddr("::ffff:192.0.2.1")}, "192.0.2.1"},
		{Route{Index: 7, Gateway: netip.MustParseAddr("fe80::1")}, "fe80::1%7"},
		{Route{Index: 7, Gateway: netip.MustParseAddr("fe80::1%eth0")}, "fe80::1%eth0"},
		{Route{Index: 7, Gateway: netip.MustParseAddr("2001:db8::1")}, "2001:db8::1"},
		{Route{Index: 7, Gateway: netip.MustParseAddr("0.0.0.0")}, "invalid IP"},
		{Route{Index: 7}, "invalid IP"},
	} {
		if got := gatewayAddr(c.r).String(); got != c.want {
			t.Errorf("%v: %s, want %s", c.r.Gateway, got, c.want)
		}
	}
}

// --- the pinger, stepped by hand on a fake clock ------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// fakeDefaultRoute is a NetStats whose default route the test changes.
type fakeDefaultRoute struct {
	mu    sync.Mutex
	r     Route
	err   error
	reads int
}

func (f *fakeDefaultRoute) DefaultRoute() (Route, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	return f.r, f.err
}

func (f *fakeDefaultRoute) set(r Route, err error) {
	f.mu.Lock()
	f.r, f.err = r, err
	f.mu.Unlock()
}

func (f *fakeDefaultRoute) Counters(int) (Counters, error) { return Counters{}, ErrUnsupported }

var wifiRoute = Route{Index: 12, Name: "Wi-Fi", Kind: LinkWiFi, Gateway: netip.MustParseAddr("192.0.2.1")}

type stepper struct {
	t     *testing.T
	clk   *fakeClock
	net   *fakeDefaultRoute
	g     *GatewayPinger
	mu    sync.Mutex
	reply func(dst netip.Addr, now time.Time) (time.Duration, error)
	pings int
	dsts  []netip.Addr
}

func newStepper(t *testing.T, tune func(*GatewayConfig)) *stepper {
	s := &stepper{t: t, clk: &fakeClock{t: t0}, net: &fakeDefaultRoute{r: wifiRoute}}
	s.reply = func(netip.Addr, time.Time) (time.Duration, error) { return 2 * time.Millisecond, nil }
	cfg := GatewayConfig{Net: s.net, Now: s.clk.now, Ping: s.ping}
	if tune != nil {
		tune(&cfg)
	}
	s.g = NewGatewayPinger(cfg)
	return s
}

func (s *stepper) ping(dst netip.Addr, _ time.Duration) (time.Duration, error) {
	s.mu.Lock()
	s.pings++
	s.dsts = append(s.dsts, dst)
	reply := s.reply
	s.mu.Unlock()
	return reply(dst, s.clk.now())
}

func (s *stepper) setReply(f func(dst netip.Addr, now time.Time) (time.Duration, error)) {
	s.mu.Lock()
	s.reply = f
	s.mu.Unlock()
}

func (s *stepper) pingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pings
}

// run steps the pinger for d of fake time, letting each ping finish before
// the next step, so the result doesn't depend on goroutine scheduling.
func (s *stepper) run(d time.Duration) {
	end := s.clk.now().Add(d)
	for {
		now := s.clk.now()
		if !now.Before(end) {
			return
		}
		wait := s.g.step(now)
		s.drain()
		s.clk.add(wait)
	}
}

func (s *stepper) drain() {
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.g.mu.Lock()
		n := s.g.inflight
		s.g.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			s.t.Fatal("pings never finished")
		}
		runtime.Gosched()
	}
}

func TestGatewayPingerClean(t *testing.T) {
	s := newStepper(t, nil)
	if v := s.g.View(); v.State != GatewayIdle || v.Running {
		t.Fatalf("before any look: %s", v.State)
	}
	s.g.Touch()
	if v := s.g.View(); v.State != GatewayMeasuring {
		t.Errorf("right after a look: %s, want measuring", v.State)
	}
	s.run(10 * time.Second)
	v := s.g.View()
	if v.State != GatewayOn || v.Stats == nil || v.RateHz != 20 {
		t.Fatalf("state %s rate %v stats %v (%s)", v.State, v.RateHz, v.Stats, v.Message)
	}
	if v.Stats.Lost != 0 || *v.Stats.P50MS != 2 || v.Stats.Sent < 170 {
		t.Errorf("stats %+v", *v.Stats)
	}
	if v.Gateway != "192.0.2.1" || v.Link != LinkWiFi || v.Interface != "Wi-Fi" {
		t.Errorf("view %+v", v)
	}
	if len(v.Series) < 8 {
		t.Errorf("%d seconds in the chart", len(v.Series))
	}
	sm := s.g.Sample()
	if sm == nil || sm.Lost != 0 || sm.Link != LinkWiFi || sm.TailPct != 99 || !strings.Contains(sm.Source, "20") {
		t.Fatalf("sample %+v", sm)
	}
	// Nobody looks any more: it stops after Idle (15 s), and the sample
	// stays for the verdict.
	s.run(20 * time.Second)
	n := s.pingCount()
	s.run(10 * time.Second)
	if s.pingCount() != n {
		t.Errorf("%d pings while idle", s.pingCount()-n)
	}
	if v := s.g.View(); v.State != GatewayIdle || v.Running {
		t.Errorf("after Idle: %s", v.State)
	}
	if s.g.Sample() == nil {
		t.Error("the sample went with the pinger")
	}
}

func TestGatewayPingerWindowAndHold(t *testing.T) {
	boost := true
	var mu sync.Mutex
	s := newStepper(t, func(c *GatewayConfig) {
		c.Hold = func() bool { mu.Lock(); defer mu.Unlock(); return boost }
	})
	// A boost keeps it going without anyone looking.
	s.run(90 * time.Second)
	v := s.g.View()
	if v.State != GatewayOn || v.Stats.WindowS > 60 || v.Stats.Sent > 60*20 {
		t.Fatalf("state %s, %+v", v.State, v.Stats)
	}
	if len(v.Series) > 61 {
		t.Errorf("%d seconds kept", len(v.Series))
	}
	mu.Lock()
	boost = false
	mu.Unlock()
	s.run(2 * time.Second)
	if v := s.g.View(); v.State != GatewayIdle {
		t.Errorf("after the boost: %s", v.State)
	}
}

func TestGatewayPingerLossAndSpikes(t *testing.T) {
	s := newStepper(t, nil)
	var n int
	s.setReply(func(netip.Addr, time.Time) (time.Duration, error) {
		n++
		switch {
		case n%50 == 0:
			return 0, icmp.ErrTimeout
		case n%100 == 7:
			return 150 * time.Millisecond, nil
		}
		return 3 * time.Millisecond, nil
	})
	s.g.Touch()
	s.run(12 * time.Second)
	v := s.g.View()
	if v.State != GatewayOn || v.Stats.Lost == 0 || v.Stats.Spikes == 0 {
		t.Fatalf("%s %+v", v.State, v.Stats)
	}
	if v.Stats.LossPct < 1.5 || v.Stats.LossPct > 2.5 {
		t.Errorf("loss %.2f%%, want about 2%%", v.Stats.LossPct)
	}
	if sm := s.g.Sample(); sm.Lost != v.Stats.Lost || sm.Spikes != v.Stats.Spikes {
		t.Errorf("sample %+v vs stats %+v", sm, v.Stats)
	}
}

func TestGatewayPingerNoReply(t *testing.T) {
	s := newStepper(t, nil)
	s.setReply(func(netip.Addr, time.Time) (time.Duration, error) { return 0, icmp.ErrTimeout })
	s.g.Touch()
	s.run(5 * time.Second)
	v := s.g.View()
	if v.State != GatewayNoReply || v.Stats != nil || !strings.Contains(v.Message, "not loss") {
		t.Fatalf("%s: %s", v.State, v.Message)
	}
	// The verdict still gets it, and reads it as "ignores ping".
	if sm := s.g.Sample(); sm == nil || sm.Lost != sm.Sent {
		t.Fatalf("sample %+v", sm)
	}
}

func TestGatewayPingerCantSend(t *testing.T) {
	for _, c := range []struct {
		err   error
		state string
	}{
		{fmt.Errorf("listen: %w", os.ErrPermission), GatewayNoPermission},
		{icmp.ErrNoRoute, GatewayCantSend},
		{errors.New("socket: too many open files"), GatewayCantSend},
	} {
		s := newStepper(t, nil)
		s.setReply(func(netip.Addr, time.Time) (time.Duration, error) { return 0, c.err })
		s.g.Touch()
		s.run(3 * time.Second)
		v := s.g.View()
		if v.State != c.state || v.Message == "" {
			t.Errorf("%v: %s %q", c.err, v.State, v.Message)
		}
		if s.g.Sample() != nil {
			t.Errorf("%v: unsent pings became evidence", c.err)
		}
		// Once sending works again, so does the pinger.
		s.setReply(func(netip.Addr, time.Time) (time.Duration, error) { return time.Millisecond, nil })
		s.run(3 * time.Second)
		if v := s.g.View(); v.State != GatewayOn {
			t.Errorf("%v, then working: %s", c.err, v.State)
		}
	}
}

func TestGatewayPingerRouteStates(t *testing.T) {
	for _, c := range []struct {
		r     Route
		err   error
		state string
	}{
		{Route{}, ErrNoRoute, GatewayOffline},
		{Route{}, ErrUnsupported, GatewayUnsupported},
		{Route{}, errors.New("GetIpForwardTable2: access denied"), GatewayCantSend},
		{Route{Index: 3, Name: "VPN", Kind: LinkOther}, nil, GatewayNoGateway},
	} {
		s := newStepper(t, nil)
		s.net.set(c.r, c.err)
		s.g.Touch()
		s.run(3 * time.Second)
		if v := s.g.View(); v.State != c.state || v.Message == "" {
			t.Errorf("%v %v: %s %q", c.r, c.err, v.State, v.Message)
		}
		if s.pingCount() != 0 {
			t.Errorf("%v %v: %d pings sent", c.r, c.err, s.pingCount())
		}
		// The route comes back (Wi-Fi reconnects): pinging starts.
		s.net.set(wifiRoute, nil)
		s.run(3 * time.Second)
		if v := s.g.View(); v.State != GatewayOn {
			t.Errorf("%v %v, then Wi-Fi: %s", c.r, c.err, v.State)
		}
	}
}

func TestGatewayPingerConnectionChange(t *testing.T) {
	s := newStepper(t, nil)
	s.g.Touch()
	s.run(5 * time.Second)
	cable := Route{Index: 4, Name: "Ethernet", Kind: LinkEthernet, Gateway: netip.MustParseAddr("192.0.2.1")}
	s.net.set(cable, nil)
	s.g.Touch()
	s.run(5 * time.Second)
	v := s.g.View()
	if !strings.Contains(v.Note, "Wi-Fi to Ethernet") || v.Link != LinkEthernet {
		t.Fatalf("note %q link %s", v.Note, v.Link)
	}
	if v.Stats == nil || v.Stats.Sent > 5*20 {
		t.Errorf("the Wi-Fi pings weren't dropped: %+v", v.Stats)
	}
	if sm := s.g.Sample(); sm.Link != LinkEthernet {
		t.Errorf("sample from %s", sm.Link)
	}
}

func TestGatewayPingerPause(t *testing.T) {
	var mu sync.Mutex
	reason := ""
	s := newStepper(t, func(c *GatewayConfig) {
		c.Pause = func() string { mu.Lock(); defer mu.Unlock(); return reason }
	})
	s.g.Touch()
	s.run(3 * time.Second)
	mu.Lock()
	reason = "The bufferbloat test is running."
	mu.Unlock()
	n := s.pingCount()
	s.g.Touch()
	s.run(3 * time.Second)
	if s.pingCount() != n {
		t.Errorf("%d pings during the pause", s.pingCount()-n)
	}
	if v := s.g.View(); v.State != GatewayPaused || v.Message != reason {
		t.Errorf("%s %q", v.State, v.Message)
	}
	mu.Lock()
	reason = ""
	mu.Unlock()
	s.g.Touch()
	s.run(3 * time.Second)
	if v := s.g.View(); v.State != GatewayOn {
		t.Errorf("after the pause: %s", v.State)
	}
}

func TestGatewayPingerSleepGap(t *testing.T) {
	s := newStepper(t, func(c *GatewayConfig) { c.Idle = time.Hour })
	s.g.Touch()
	s.run(5 * time.Second)
	s.clk.add(40 * time.Second) // the PC slept
	s.run(3 * time.Second)
	v := s.g.View()
	if !strings.Contains(v.Note, "asleep") {
		t.Fatalf("note %q", v.Note)
	}
	if v.Stats == nil || v.Stats.Sent > 3*20 {
		t.Errorf("pings from before the sleep kept: %+v", v.Stats)
	}
}

func TestGatewayPingerRateLimitedRouter(t *testing.T) {
	s := newStepper(t, func(c *GatewayConfig) { c.Idle = time.Hour })
	// The router answers 8 pings a second and drops the rest.
	var cur int64
	answered := 0
	s.setReply(func(_ netip.Addr, now time.Time) (time.Duration, error) {
		if now.Unix() != cur {
			cur, answered = now.Unix(), 0
		}
		if answered >= 8 {
			return 0, icmp.ErrTimeout
		}
		answered++
		return time.Millisecond, nil
	})
	s.g.Touch()
	s.run(20 * time.Second)
	v := s.g.View()
	if v.RateHz != 2 || !strings.Contains(v.Note, "about 8 pings a second") {
		t.Fatalf("rate %v note %q", v.RateHz, v.Note)
	}
	// At twice a second every ping is answered.
	s.run(20 * time.Second)
	v = s.g.View()
	if v.State != GatewayOn || v.Stats.Lost != 0 || v.Stats.Limited {
		t.Errorf("slow rate: %s %+v", v.State, v.Stats)
	}
	if sm := s.g.Sample(); !strings.Contains(sm.Source, "twice") {
		t.Errorf("sample source %q", sm.Source)
	}
}

func TestGatewayPingerIPv6Router(t *testing.T) {
	v6 := Route{Index: 9, Name: "Wi-Fi", Kind: LinkWiFi, Gateway: netip.MustParseAddr("fe80::1")}
	var mu sync.Mutex
	var got []netip.Addr
	kind := icmp.Echo
	s := newStepper(t, func(c *GatewayConfig) {
		c.PingTTL = func(dst netip.Addr, ttl int, _ time.Duration) (icmp.Reply, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, dst)
			return icmp.Reply{From: dst, RTT: time.Millisecond, Kind: kind}, nil
		}
	})
	s.net.set(v6, nil)
	s.g.Touch()
	s.run(3 * time.Second)
	if v := s.g.View(); v.State != GatewayOn || v.Gateway != "fe80::1" {
		t.Fatalf("%s %s", v.State, v.Gateway)
	}
	mu.Lock()
	if len(got) == 0 || got[0].Zone() != "9" {
		t.Errorf("pinged %v without the interface zone", got)
	}
	kind = icmp.Unreachable
	mu.Unlock()
	s.run(3 * time.Second)
	if v := s.g.View(); v.Stats == nil || v.Stats.Lost == 0 {
		t.Errorf("unreachable replies not counted as lost: %+v", v.Stats)
	}
	if s.pingCount() != 0 {
		t.Error("the IPv4 ping was used for an IPv6 router")
	}

	// Without an IPv6 ping the pinger says so instead of counting loss.
	s2 := newStepper(t, nil)
	s2.net.set(v6, nil)
	s2.g.Touch()
	s2.run(2 * time.Second)
	if v := s2.g.View(); v.State != GatewayUnsupported || !strings.Contains(v.Message, "IPv6") {
		t.Errorf("no IPv6 ping: %s %q", v.State, v.Message)
	}
}

func TestGatewayPingerSlowRouterDoesNotBlock(t *testing.T) {
	// A router that never answers within the step: pings pile up to
	// MaxInFlight and the steps go on without waiting.
	release := make(chan struct{})
	s := newStepper(t, func(c *GatewayConfig) { c.MaxInFlight = 5 })
	s.setReply(func(netip.Addr, time.Time) (time.Duration, error) {
		<-release
		return 0, icmp.ErrTimeout
	})
	s.g.Touch()
	start := time.Now()
	for range 50 {
		s.clk.add(s.g.step(s.clk.now()))
	}
	if time.Since(start) > 2*time.Second {
		t.Error("steps waited for pings")
	}
	for deadline := time.Now().Add(5 * time.Second); s.pingCount() < 5 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	if n := s.pingCount(); n != 5 {
		t.Errorf("%d pings in flight, cap 5", n)
	}
	close(release)
	s.drain()
}

func TestGatewayViewRedacted(t *testing.T) {
	v := GatewayView{Gateway: "203.0.113.77", Message: "Pings to your router can't be sent: write 203.0.113.77: no buffer", Series: []GatewaySecond{{}}}
	r := v.Redacted()
	if r.Gateway != "203.0.113.0" || strings.Contains(r.Message, ".77") || r.Series != nil {
		t.Errorf("%+v", r)
	}
	if lan := (GatewayView{Gateway: "10.0.0.1"}).Redacted(); lan.Gateway != "10.0.0.1" {
		t.Errorf("a LAN router was cut: %s", lan.Gateway)
	}
}
