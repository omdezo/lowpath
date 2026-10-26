package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"lowpath/internal/detect"
	"lowpath/internal/diag"
	"lowpath/internal/engine"
	"lowpath/internal/icmp"
)

// fakeLookup answers network lookups from a table.
type fakeLookup struct {
	mu    sync.Mutex
	nets  map[netip.Addr]diag.NetworkInfo
	calls int
}

func (f *fakeLookup) LookupNetwork(_ context.Context, a netip.Addr) (diag.NetworkInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if n, ok := f.nets[a]; ok {
		return n, nil
	}
	return diag.NetworkInfo{}, errors.New("RIPEstat: " + a.String() + " is not announced on the internet (no origin AS)")
}

func ptr[T any](v T) *T { return &v }

type currentResp struct {
	State   string `json:"state"`
	Source  string `json:"source"`
	Message string `json:"message"`
	Watch   struct {
		State   string `json:"state"`
		Message string `json:"message"`
	} `json:"watch"`
	Server *struct {
		Addr      string            `json:"addr"`
		Proto     string            `json:"proto"`
		PktsUp    *uint64           `json:"pkts_up"`
		PktsDown  *uint64           `json:"pkts_down"`
		Network   *diag.NetworkInfo `json:"network"`
		Note      string            `json:"network_note"`
		LookingUp bool              `json:"looking_up"`
		LAN       bool              `json:"lan"`
		Geo       struct {
			State      string   `json:"state"`
			DistanceKM *float64 `json:"distance_km"`
			MaxKM      *float64 `json:"max_km"`
			Note       string   `json:"note"`
		} `json:"geo"`
		PingMS   *float64 `json:"ping_ms"`
		JitterMS *float64 `json:"jitter_ms"`
		LossPct  *float64 `json:"loss_pct"`
		Received int      `json:"received"`
		PingNote string   `json:"ping_note"`
	} `json:"server"`
	Others []struct {
		Addr string `json:"addr"`
	} `json:"others"`
	You *struct {
		City string `json:"city"`
	} `json:"you"`
}

// TestCurrentServerFromFlowEvents plays a game that Lowpath doesn't route:
// the OS's flow events name its server, which the card shows with its
// network, a city checked against the ping, and live ping figures; when the
// game exits the watch closes and the card falls back to "last time".
func TestCurrentServerFromFlowEvents(t *testing.T) {
	fp := newFake(t)
	game := netip.MustParseAddrPort("203.0.113.50:27015")
	other := netip.MustParseAddrPort("198.51.100.9:3478")
	loc := exampleLocation()
	loc.IPv4.Lat, loc.IPv4.Lon = ptr(52.37), ptr(4.90) // Amsterdam
	lookup := &fakeLookup{nets: map[netip.Addr]diag.NetworkInfo{
		game.Addr(): {ASN: 64501, ASName: "EXAMPLE-GAMES", ISP: "Example Games Hosting", Country: "DE", City: "Frankfurt am Main",
			Lat: ptr(50.11), Lon: ptr(8.68), Prefix: netip.MustParsePrefix("203.0.113.0/24")},
	}}
	ping := func(dst netip.Addr, _ time.Duration) (time.Duration, error) {
		if dst == game.Addr() {
			return 9 * time.Millisecond, nil
		}
		return 0, icmp.ErrTimeout
	}
	ts, srv := newRealServer(t, fp, func(c *Config) {
		c.Locator, c.ServerLookup, c.Ping = &fakeLocator{loc: loc, have: true}, lookup, ping
	})
	clock := time.Now()
	srv.current.now = func() time.Time { return clock }
	tick := func(d time.Duration) {
		clock = clock.Add(d)
		srv.scanRunning()
		srv.current.tick(srv.runningGames())
	}
	get := func() currentResp {
		var r currentResp
		if code := call(t, ts, "GET", "/api/games/cs2/current-server", "", &r); code != 200 {
			t.Fatalf("current-server: %d", code)
		}
		return r
	}

	// Nothing known, no game running.
	r := get()
	if r.State != "none" || r.Server != nil || r.Message == "" || r.Watch.State != "idle" {
		t.Fatalf("before the game: %+v", r)
	}
	if code := call(t, ts, "GET", "/api/games/nope/current-server", "", nil); code != 404 {
		t.Fatalf("unknown game: %d", code)
	}

	// The game starts: the watch opens and learns its PID.
	pids := fakePIDs(2)
	cs2PID, steamPID := pids[0], pids[1]
	fp.Games.SetRunning("cs2.exe", "steam.exe")
	fp.Owners.Set(nil, map[uint32]string{cs2PID: "cs2.exe", steamPID: "steam.exe"})
	tick(time.Second)
	if !fp.Flows.Watching() {
		t.Fatal("a running game must open the flow watch")
	}
	local := netip.MustParseAddrPort("192.168.1.20:50123")
	emit := func(ev detect.FlowEvent) {
		if !fp.Flows.Emit(ev) {
			t.Fatal("no watch open")
		}
	}
	for range 12 {
		emit(detect.FlowEvent{PID: cs2PID, Proto: "udp", Local: local, Remote: game, Bytes: 120, Outbound: true})
		// A receive with the ends the other way round still counts for the game server.
		emit(detect.FlowEvent{PID: cs2PID, Proto: "udp", Local: game, Remote: local, Bytes: 300})
	}
	emit(detect.FlowEvent{PID: cs2PID, Proto: "udp", Local: local, Remote: other, Outbound: true})
	// Not game servers: the game's web traffic, DNS, and another program.
	emit(detect.FlowEvent{PID: cs2PID, Proto: "tcp", Local: local, Remote: netip.MustParseAddrPort("198.51.100.80:443"), Outbound: true})
	emit(detect.FlowEvent{PID: cs2PID, Proto: "udp", Local: local, Remote: netip.MustParseAddrPort("192.168.1.1:53"), Outbound: true})
	emit(detect.FlowEvent{PID: steamPID, Proto: "udp", Local: local, Remote: netip.MustParseAddrPort("198.51.100.200:27017"), Outbound: true})
	tick(time.Second)

	r = get()
	if r.State != "live" || r.Source != "traffic" || r.Server == nil || r.Server.Addr != game.String() || r.Server.Proto != "udp" ||
		*r.Server.PktsUp != 12 || *r.Server.PktsDown != 12 || r.Watch.State != "on" {
		t.Fatalf("while playing: %+v", r)
	}
	if len(r.Others) != 1 || r.Others[0].Addr != other.String() {
		t.Fatalf("others %+v, want only the STUN server", r.Others)
	}
	// The lookup and the ping run in the background; the card shows them
	// on a later poll.
	deadline := time.Now().Add(5 * time.Second)
	for r.Server.Network == nil || r.Server.Received == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no network or ping after 5 s: %+v", r.Server)
		}
		time.Sleep(20 * time.Millisecond)
		r = get()
	}
	s := r.Server
	if s.Network.ASN != 64501 || s.Network.ISP != "Example Games Hosting" || s.Network.City != "Frankfurt am Main" || s.LookingUp {
		t.Fatalf("network %+v", s.Network)
	}
	if s.PingMS == nil || *s.PingMS != 9 || s.JitterMS == nil || *s.JitterMS != 0 || s.LossPct == nil || *s.LossPct != 0 {
		t.Fatalf("ping %+v", s)
	}
	// Frankfurt is ~360 km from Amsterdam; 9 ms allows ~920 km.
	if s.Geo.State != "ok" || s.Geo.DistanceKM == nil || s.Geo.MaxKM == nil || s.Geo.Note == "" || r.You == nil || r.You.City != "Amsterdam" {
		t.Fatalf("geo %+v, you %+v", s.Geo, r.You)
	}

	// Five seconds of play save the server, as a boost would.
	for range 5 {
		emit(detect.FlowEvent{PID: cs2PID, Proto: "udp", Local: local, Remote: game, Outbound: true})
		tick(time.Second)
	}
	if saved := srv.store.Servers("cs2"); len(saved) == 0 || saved[0] != game {
		t.Fatalf("saved servers %v", saved)
	}

	// The game exits: the watch stays a little (a relaunch keeps it), then
	// closes; the learned servers age out and the card shows last time's.
	fp.Games.SetRunning()
	tick(time.Second)
	if !fp.Flows.Watching() {
		t.Fatal("the watch should linger after the game exits")
	}
	tick(watchLinger)
	if fp.Flows.Watching() {
		t.Fatal("the watch must close after the game is gone")
	}
	tick(time.Minute)
	r = get()
	if r.State != "last" || r.Source != "history" || r.Server == nil || r.Server.Addr != game.String() || r.Server.PktsUp != nil {
		t.Fatalf("after the game: %+v", r)
	}
}

func TestCurrentServerWatchUnavailable(t *testing.T) {
	fp := newFake(t)
	fp.Flows.SetErr(detect.ErrNeedAdmin)
	lookup := &fakeLookup{}
	ts, srv := newRealServer(t, fp, func(c *Config) {
		c.Locator, c.ServerLookup = &fakeLocator{loc: exampleLocation(), have: true}, lookup
		c.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout }
	})
	fp.Games.SetRunning("cs2.exe")
	srv.scanRunning()
	srv.current.tick(srv.runningGames())
	var r currentResp
	call(t, ts, "GET", "/api/games/cs2/current-server", "", &r)
	if r.Watch.State != "needs_admin" || r.Watch.Message == "" || r.State != "none" {
		t.Fatalf("needs admin: %+v", r)
	}
	// It doesn't retry every second.
	srv.current.tick(srv.runningGames())
	if fp.Flows.Starts() != 0 {
		t.Fatal("a failed watch must not count as started")
	}

	// A saved server from an earlier boost still shows, with honest gaps: a
	// private address has no registry entry, and it doesn't answer pings.
	lan := netip.MustParseAddrPort("192.168.1.30:7777")
	srv.store.RecordServer("cs2", lan)
	deadline := time.Now().Add(5 * time.Second)
	for {
		call(t, ts, "GET", "/api/games/cs2/current-server", "", &r)
		if r.Server != nil && r.Server.PingNote != "" && r.Server.PingNote != "Measuring…" && r.Server.PingNote != "No reply yet." {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ping verdict: %+v", r.Server)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r.State != "last" || !r.Server.LAN || r.Server.Network != nil || r.Server.PingMS != nil || r.Server.LossPct != nil || r.Server.Geo.State != "unknown" {
		t.Fatalf("saved LAN server: %+v", r.Server)
	}
	if lookup.calls != 0 {
		t.Fatal("a private address must not be sent to the registry")
	}
}

func TestCurrentServerPrefersTheBoost(t *testing.T) {
	_, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.Locator, c.ServerLookup = &fakeLocator{}, &fakeLookup{}
		c.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout }
	})
	g, _ := srv.game("cs2")
	a := netip.MustParseAddrPort("203.0.113.60:27015")
	snap := &engine.Snapshot{Servers: []engine.ServerStatus{{Addr: a, Proto: "udp", PktsUp: 40, PktsDown: 38, LastSeen: time.Now(), Primary: true}}}
	v := srv.current.view(g, snap)
	if v.State != currentLive || v.Source != sourceBoost || v.Server == nil || v.Server.Addr != a || !v.Server.LookingUp {
		t.Fatalf("view %+v", v)
	}
	// A session whose primary went quiet isn't "now".
	snap.Servers[0].LastSeen = time.Now().Add(-time.Minute)
	if v := srv.current.view(g, snap); v.State != currentNone {
		t.Fatalf("stale session server: %+v", v)
	}
}

func TestGameRemoteFilter(t *testing.T) {
	for _, c := range []struct {
		proto, addr string
		want        bool
	}{
		{"udp", "203.0.113.5:27015", true},
		{"tcp", "203.0.113.5:3724", true},
		{"udp", "[2001:db8::5]:7777", true},
		{"udp", "192.168.1.40:7777", true}, // a LAN server is a server
		{"udp", "203.0.113.5:443", false},  // QUIC
		{"tcp", "203.0.113.5:80", false},
		{"udp", "192.168.1.1:53", false},
		{"udp", "127.0.0.1:7777", false},
		{"udp", "224.0.0.251:5353", false},
		{"udp", "255.255.255.255:7777", false},
		{"udp", "[fe80::1]:7777", false},
		{"udp", "0.0.0.0:7777", false},
		{"icmp", "203.0.113.5:1", false},
	} {
		if got := gameRemote(c.proto, netip.MustParseAddrPort(c.addr)); got != c.want {
			t.Errorf("%s %s: %v, want %v", c.proto, c.addr, got, c.want)
		}
	}
}

// An anycast address is known only by country; the card must not pretend
// the country's center is the server's place, and says what the ping shows.
func TestCurrentServerCountryOnly(t *testing.T) {
	a := netip.MustParseAddrPort("198.51.100.77:19302")
	lookup := &fakeLookup{nets: map[netip.Addr]diag.NetworkInfo{a.Addr(): {ASN: 64502, ISP: "Example Anycast", Country: "US"}}}
	_, srv := newRealServer(t, newFake(t), func(c *Config) {
		c.Locator, c.ServerLookup = &fakeLocator{loc: exampleLocation(), have: true}, lookup
		c.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 7 * time.Millisecond, nil }
	})
	g, _ := srv.game("cs2")
	snap := &engine.Snapshot{Servers: []engine.ServerStatus{{Addr: a, Proto: "udp", PktsUp: 5, LastSeen: time.Now(), Primary: true}}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		v := srv.current.view(g, snap)
		if s := v.Server; s.Network != nil && s.Received > 0 {
			if s.Geo.State != "unknown" || s.Geo.DistanceKM != nil || s.Geo.MaxKM == nil || *s.Geo.MaxKM != 714 ||
				s.Geo.Note != "The IP database knows only the country. The ping puts it within 710 km of you." {
				t.Fatalf("geo %+v", s.Geo)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no lookup or ping: %+v", v.Server)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A game in its lobby talks only TCP (chat, matchmaking): the card may
// show that server, but it must not be saved, because the saved server is
// proxy mode's UDP endpoint and Analyze's baseline.
func TestCurrentServerDoesNotSaveTCP(t *testing.T) {
	fp := newFake(t)
	ts, srv := newRealServer(t, fp, func(c *Config) {
		c.Locator, c.ServerLookup = &fakeLocator{}, &fakeLookup{}
		c.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout }
	})
	clock := time.Now()
	srv.current.now = func() time.Time { return clock }
	tick := func() {
		clock = clock.Add(time.Second)
		srv.scanRunning()
		srv.current.tick(srv.runningGames())
	}
	cs2PID := fakePIDs(1)[0]
	fp.Games.SetRunning("cs2.exe")
	fp.Owners.Set(nil, map[uint32]string{cs2PID: "cs2.exe"})
	tick()
	local := netip.MustParseAddrPort("192.168.1.20:50124")
	chat := netip.MustParseAddrPort("203.0.113.70:5223")
	for range 8 {
		for range 5 {
			fp.Flows.Emit(detect.FlowEvent{PID: cs2PID, Proto: "tcp", Local: local, Remote: chat, Outbound: true})
		}
		tick()
	}
	var r currentResp
	call(t, ts, "GET", "/api/games/cs2/current-server", "", &r)
	if r.State != "live" || r.Server == nil || r.Server.Addr != chat.String() || r.Server.Proto != "tcp" {
		t.Fatalf("lobby: %+v", r)
	}
	if saved := srv.store.Servers("cs2"); len(saved) != 0 {
		t.Fatalf("a TCP server was saved as the game's server: %v", saved)
	}
}

// The OS can end the flow watch by itself (an ETW session stopped from
// outside). The card must stop saying "on", and the watch comes back.
func TestCurrentServerWatchEndedByTheOS(t *testing.T) {
	fp := newFake(t)
	ts, srv := newRealServer(t, fp, func(c *Config) {
		c.Locator, c.ServerLookup = &fakeLocator{}, &fakeLookup{}
		c.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout }
	})
	clock := time.Now()
	srv.current.now = func() time.Time { return clock }
	tick := func(d time.Duration) {
		clock = clock.Add(d)
		srv.scanRunning()
		srv.current.tick(srv.runningGames())
	}
	watch := func() (r currentResp) {
		call(t, ts, "GET", "/api/games/cs2/current-server", "", &r)
		return r
	}
	fp.Games.SetRunning("cs2.exe")
	tick(time.Second)
	if r := watch(); r.Watch.State != "on" || fp.Flows.Starts() != 1 {
		t.Fatalf("watch %+v, %d starts", r.Watch, fp.Flows.Starts())
	}

	fp.Flows.End()
	tick(time.Second)
	if r := watch(); r.Watch.State != "error" || r.Watch.Message == "" {
		t.Fatalf("after the OS ended the watch: %+v", r.Watch)
	}
	tick(time.Second)
	if fp.Flows.Starts() != 1 {
		t.Fatal("the watch must not restart every second")
	}
	tick(watchRetryError)
	if r := watch(); r.Watch.State != "on" || fp.Flows.Starts() != 2 || !fp.Flows.Watching() {
		t.Fatalf("retry: %+v, %d starts", r.Watch, fp.Flows.Starts())
	}
}

// lockedLog collects log lines written from any goroutine.
type lockedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.buf.String() }

// A crashed Lowpath leaves its flow watch running (an ETW session outlives
// its process). Start-up stops it once, before the first watch, whatever
// Reset says, and the watch works the same afterwards.
func TestCurrentServerResetsLeftoverWatchAtStartUp(t *testing.T) {
	for _, c := range []struct {
		name     string
		leftover bool
		err      error
		log      string // what the log must say ("" = nothing about it)
	}{
		{name: "left behind", leftover: true, log: "level=INFO msg=\"stopped the flow watch an earlier run left behind\""},
		{name: "nothing left behind"},
		{name: "no rights", leftover: true, err: detect.ErrNeedAdmin, log: "level=DEBUG"},
		{name: "other error", err: errors.New("ETW: stop the leftover session: example failure"), log: "level=WARN msg=\"stop a left-behind flow watch\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			fp := newFake(t)
			fp.Flows.SetLeftover(c.leftover)
			fp.Flows.SetResetErr(c.err)
			var log lockedLog
			_, srv := newRealServer(t, fp, func(cfg *Config) {
				cfg.Locator, cfg.ServerLookup = &fakeLocator{}, &fakeLookup{}
				cfg.Ping = func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout }
				cfg.Log = slog.New(slog.NewTextHandler(&log, &slog.HandlerOptions{Level: slog.LevelDebug}))
			})
			if fp.Flows.Resets() != 1 {
				t.Fatalf("%d resets at start-up, want 1", fp.Flows.Resets())
			}
			if fp.Flows.Leftover() != (c.leftover && c.err != nil) {
				t.Fatalf("leftover still there: %v", fp.Flows.Leftover())
			}
			got := ""
			for _, line := range strings.Split(log.String(), "\n") {
				if strings.Contains(line, "flow watch") && strings.Contains(line, "component=current-server") && !strings.Contains(line, "watching game flows") {
					got += line
				}
			}
			if (c.log == "") != (got == "") || !strings.Contains(got, c.log) {
				t.Fatalf("log %q, want %q", got, c.log)
			}

			// Games come and go: the watch opens and closes as before, and
			// nothing resets again.
			clock := time.Now()
			srv.current.now = func() time.Time { return clock }
			tick := func(d time.Duration) {
				clock = clock.Add(d)
				srv.scanRunning()
				srv.current.tick(srv.runningGames())
			}
			for round := 1; round <= 2; round++ {
				fp.Games.SetRunning("cs2.exe")
				tick(time.Second)
				if !fp.Flows.Watching() || fp.Flows.Starts() != round {
					t.Fatalf("round %d: watching %v, %d starts", round, fp.Flows.Watching(), fp.Flows.Starts())
				}
				fp.Games.SetRunning()
				tick(time.Second)
				tick(watchLinger)
				if fp.Flows.Watching() {
					t.Fatalf("round %d: the watch outlived the game", round)
				}
			}
			if fp.Flows.Resets() != 1 {
				t.Fatalf("%d resets, want 1", fp.Flows.Resets())
			}
		})
	}
}

// Reset may wait on the OS. Start-up doesn't wait for it, and no watch
// opens until it has returned, so it can't meet one of ours.
func TestCurrentServerResetDoesNotHoldUpStartUp(t *testing.T) {
	fp := newFake(t)
	fp.Flows.SetLeftover(true)
	release := fp.Flows.HoldReset()
	t.Cleanup(release)
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Store: openTestStore(t), Games: games, Platform: fp.Platform(), Log: discard, Session: testSession,
		Locator: &fakeLocator{}, ServerLookup: &fakeLookup{},
		Ping: func(netip.Addr, time.Duration) (time.Duration, error) { return 0, icmp.ErrTimeout },
	}
	// Built on its own goroutine: a NewServer that waited for the reset
	// would otherwise hang the test instead of failing it.
	type built struct {
		srv *Server
		err error
	}
	ch := make(chan built, 1)
	go func() {
		srv, err := NewServer(cfg)
		ch <- built{srv, err}
	}()
	var srv *Server
	select {
	case b := <-ch:
		if b.err != nil {
			t.Fatal(b.err)
		}
		srv = b.srv
	case <-time.After(5 * time.Second):
		t.Fatal("NewServer waited for the reset")
	}
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	var r currentResp
	if code := call(t, ts, "GET", "/api/games/cs2/current-server", "", &r); code != 200 {
		t.Fatalf("API while resetting: %d", code)
	}

	fp.Games.SetRunning("cs2.exe")
	srv.scanRunning()
	srv.current.tick(srv.runningGames())
	if fp.Flows.Watching() || fp.Flows.Starts() != 0 {
		t.Fatal("a watch opened before the reset returned")
	}

	release()
	waitReset(t, srv)
	if fp.Flows.Leftover() {
		t.Fatal("the leftover is still there")
	}
	srv.current.tick(srv.runningGames())
	if !fp.Flows.Watching() || fp.Flows.Starts() != 1 || fp.Flows.Resets() != 1 {
		t.Fatalf("after the reset: watching %v, %d starts, %d resets", fp.Flows.Watching(), fp.Flows.Starts(), fp.Flows.Resets())
	}
}
