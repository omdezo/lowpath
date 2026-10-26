package engine

import (
	"fmt"
	"net/netip"
	"testing"
	"time"
)

func addrN(i int) netip.AddrPort {
	return netip.MustParseAddrPort(fmt.Sprintf("203.0.113.%d:27015", i))
}

var local = netip.MustParseAddrPort("192.168.1.10:50000")

func TestFlowTableKeysByLocalAndRemote(t *testing.T) {
	ft := newFlowTable()
	now := time.Now()
	a := ft.get(local, addrN(1), now)
	if ft.get(local, addrN(1), now) != a {
		t.Fatal("same (local, remote) must map to the same flow")
	}
	b := ft.get(local, addrN(2), now)
	c := ft.get(netip.MustParseAddrPort("192.168.1.10:50001"), addrN(1), now)
	if a.id == b.id || a.id == c.id || b == c {
		t.Fatal("each (local, remote) pair needs its own flow id")
	}
	if a.srv != c.srv || a.srv == b.srv {
		t.Fatal("flows to the same remote must share one server entry")
	}
	if ft.byFlowID(b.id) != b {
		t.Fatal("flow not found by id")
	}
	if p := ft.primary.Load(); p != a.srv {
		t.Fatalf("primary %v, want the first server the game talked to", p)
	}
}

func TestPrimaryFollowsRecentUpstream(t *testing.T) {
	ft := newFlowTable()
	now := time.Now()
	send := func(i, n int) {
		f := ft.get(local, addrN(i), now)
		f.srv.up.Add(uint64(n))
	}
	send(1, 10)
	send(2, 50)
	if !ft.updatePrimary() || ft.primary.Load().addr != addrN(2) {
		t.Fatalf("primary %v, want the busier server", ft.primary.Load().addr)
	}
	// Equal traffic from now on: once the burst leaves the window it is a
	// tie, and a tie keeps the current primary.
	for range primaryWindow {
		send(1, 10)
		send(2, 10)
		if ft.updatePrimary() {
			t.Fatal("a tie must keep the current primary")
		}
	}
	// Everything quiet: keep it, the game is just paused.
	for range primaryWindow {
		if ft.updatePrimary() {
			t.Fatal("silence must keep the current primary")
		}
	}
	send(1, 5)
	if !ft.updatePrimary() || ft.primary.Load().addr != addrN(1) {
		t.Fatalf("primary %v, want the only server with recent traffic", ft.primary.Load().addr)
	}
}

func TestFlowTableGCAndList(t *testing.T) {
	ft := newFlowTable()
	start := time.Now()
	for i := 1; i <= 10; i++ {
		f := ft.get(local, addrN(i), start.Add(time.Duration(i)*time.Second))
		f.srv.up.Add(1)
	}
	// Server 1 was contacted first and stays primary although it is the
	// least recently active; the list must still include it.
	flows, list := ft.list()
	if flows != 10 || len(list) != maxServers {
		t.Fatalf("flows %d, %d servers listed; want 10 and %d", flows, len(list), maxServers)
	}
	if list[0].Addr != addrN(10) || list[maxServers-1].Addr != addrN(1) || !list[maxServers-1].Primary {
		t.Fatalf("list %+v: want most recent first and the primary included", list)
	}

	// 61 s after server 5's last packet: 1..5 are idle, the primary among them.
	if !ft.gc(start.Add(5*time.Second + flowIdle + time.Second)) {
		t.Fatal("gc should report the primary as gone")
	}
	flows, list = ft.list()
	if flows != 5 || len(list) != 5 {
		t.Fatalf("after gc: %d flows, %d servers; want 5 and 5", flows, len(list))
	}
	if p := ft.primary.Load(); p == nil || p.addr != addrN(10) {
		t.Fatalf("primary after gc %v, want the most recently active server", p)
	}
	if _, ok := ft.byID[ft.get(local, addrN(1), start).id]; !ok {
		t.Fatal("a collected flow must come back as a fresh flow")
	}
}

func TestFlowLearnerPicksBusiestUDPServer(t *testing.T) {
	l := NewFlowLearner()
	now := time.Now()
	login := netip.MustParseAddrPort("198.51.100.7:443")
	game := addrN(5)

	// A reply nobody asked for doesn't make a server.
	l.Observe(ProtoUDP, addrN(9), false, now)
	if len(l.Servers()) != 0 {
		t.Fatal("a packet sent to the game must not create a server")
	}
	// TCP first (the launcher logs in), then the match starts over UDP with
	// fewer packets per second: game traffic still wins.
	for range 20 {
		l.Observe(ProtoTCP, login, true, now)
	}
	l.Tick(now)
	if p := primaryOf(l.Servers()); p == nil || p.Addr != login {
		t.Fatalf("primary %+v, want the only server so far", p)
	}
	for range 5 {
		l.Observe(ProtoUDP, game, true, now)
		l.Observe(ProtoUDP, game, false, now)
	}
	for range 20 {
		l.Observe(ProtoTCP, login, true, now)
	}
	if !l.Tick(now.Add(time.Second)) {
		t.Fatal("the UDP server should take over")
	}
	list := l.Servers()
	p := primaryOf(list)
	if p == nil || p.Addr != game || p.Proto != ProtoUDP || p.PktsUp != 5 || p.PktsDown != 5 {
		t.Fatalf("primary %+v, want the UDP game server with 5 up, 5 down", p)
	}
	if !l.Knows(game) || l.Knows(addrN(9)) {
		t.Fatal("Knows must report learned servers only")
	}
	if len(list) != 2 {
		t.Fatalf("%d servers, want 2", len(list))
	}

	// The match ends: after a minute of silence both are forgotten.
	l.Tick(now.Add(flowIdle + 2*time.Second))
	if len(l.Servers()) != 0 {
		t.Fatalf("servers after idling: %+v", l.Servers())
	}
}

func primaryOf(list []ServerStatus) *ServerStatus {
	for i := range list {
		if list[i].Primary {
			return &list[i]
		}
	}
	return nil
}
