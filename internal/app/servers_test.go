package app

import (
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"lowpath/internal/catalog"
)

// fakeServerLists records what the API asked for.
type fakeServerLists struct {
	mu     sync.Mutex
	lists  []int
	pings  []int
	closed bool
}

func (f *fakeServerLists) List(app int) catalog.ServerList {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, app)
	return catalog.ServerList{Source: "valve_sdr", AppID: app, State: "ready", POPs: []catalog.POPView{{ID: "dxb", City: "Dubai"}}}
}

func (f *fakeServerLists) Ping(app int) catalog.ServerList {
	f.mu.Lock()
	f.pings = append(f.pings, app)
	f.mu.Unlock()
	l := f.List(app)
	l.Pinging = true
	return l
}

func (f *fakeServerLists) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func newServersTestServer(t *testing.T, lists ServerLists) (*httptest.Server, *Server) {
	t.Helper()
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Config{Store: openTestStore(t), Games: games, Servers: lists, Capture: &fakeCapturer{}, Log: discard, Session: testSession})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); ts.Close() })
	return ts, srv
}

func TestValveGamesUseSDR(t *testing.T) {
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"cs2": 730, "dota2": 570, "deadlock": 1422450}
	for _, g := range games {
		sdr := slices.Contains(g.Discovery, DiscoverySDR)
		if app, ok := want[g.ID]; ok != sdr || (ok && g.SteamApp != app) {
			t.Errorf("%s: sdr %v, steam app %d", g.ID, sdr, g.SteamApp)
		}
	}
}

func TestServersAPI(t *testing.T) {
	f := &fakeServerLists{}
	ts, srv := newServersTestServer(t, f)

	var l catalog.ServerList
	if code := call(t, ts, "GET", "/api/games/cs2/servers", "", &l); code != 200 || l.State != "ready" || l.AppID != 730 || len(l.POPs) != 1 {
		t.Fatalf("cs2: %d %+v", code, l)
	}
	l = catalog.ServerList{}
	if code := call(t, ts, "POST", "/api/games/deadlock/servers/ping", "", &l); code != 200 || !l.Pinging || l.AppID != 1422450 {
		t.Fatalf("deadlock ping: %d %+v", code, l)
	}
	// A game with no public list says so, without asking the catalog.
	l = catalog.ServerList{}
	if code := call(t, ts, "GET", "/api/games/valorant/servers", "", &l); code != 200 || l.State != "unsupported" || l.Message == "" || l.POPs == nil {
		t.Fatalf("valorant: %d %+v", code, l)
	}
	if code := call(t, ts, "GET", "/api/games/nope/servers", "", nil); code != 404 {
		t.Fatalf("unknown game: %d", code)
	}
	f.mu.Lock()
	lists, pings := slices.Clone(f.lists), slices.Clone(f.pings)
	f.mu.Unlock()
	if !slices.Equal(lists, []int{730, 1422450}) || !slices.Equal(pings, []int{1422450}) {
		t.Fatalf("lists %v pings %v", lists, pings)
	}
	srv.Close()
	if !f.closed {
		t.Fatal("Close didn't stop the server lists")
	}
}

func TestServersAPIWithoutCatalog(t *testing.T) {
	ts, _ := newServersTestServer(t, nil)
	var l catalog.ServerList
	if code := call(t, ts, "GET", "/api/games/cs2/servers", "", &l); code != 200 || l.State != "unsupported" {
		t.Fatalf("cs2 without a catalog: %d %+v", code, l)
	}
}
