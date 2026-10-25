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
