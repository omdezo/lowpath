package capture

import (
	"errors"
	"maps"
	"net/netip"
	"slices"
	"sync"
	"testing"
)

const (
	pidGame  = 100
	pidOther = 200
	pidSelf  = 300
)

// fakeOS stands in for the socket table and the process list.
type fakeOS struct {
	mu        sync.Mutex
	socks     []socket
	procs     map[uint32]string
	sockCalls int
	procCalls int
	err       error
}

func newFakeOS() *fakeOS {
	return &fakeOS{
		socks: []socket{
			{Local: gameLocal, PID: pidGame},
			{Local: netip.MustParseAddrPort("192.168.1.10:50001"), PID: pidOther},
			{Local: netip.MustParseAddrPort("0.0.0.0:50002"), PID: pidSelf},
		},
		procs: map[uint32]string{pidGame: "Game.exe", pidOther: "chrome.exe", pidSelf: "game.exe"},
	}
}

func (f *fakeOS) UDPSockets() ([]socket, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sockCalls++
	return slices.Clone(f.socks), f.err
}

func (f *fakeOS) ProcessNames() (map[uint32]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procCalls++
	return maps.Clone(f.procs), f.err
}

func (f *fakeOS) add(s socket, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.socks = append(f.socks, s)
	if name != "" {
		f.procs[s.PID] = name
	}
}

func (f *fakeOS) calls() (socks, procs int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sockCalls, f.procCalls
}

func newTestOwners(t *testing.T, f *fakeOS, processes ...string) *owners {
	t.Helper()
	o, err := newOwners(processes, nil, pidSelf, f)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func mustGame(t *testing.T, o *owners, local string, want bool) {
	t.Helper()
	got, err := o.isGame(netip.MustParseAddrPort(local))
	if err != nil || got != want {
		t.Fatalf("isGame(%s) = %v, %v; want %v", local, got, err, want)
	}
}

func TestOwnersVerdicts(t *testing.T) {
	f := newFakeOS()
	f.add(socket{Local: netip.MustParseAddrPort("0.0.0.0:6000"), PID: pidGame}, "")
	f.add(socket{Local: netip.MustParseAddrPort("[::]:6001"), PID: pidGame}, "")                  // dual-stack
	f.add(socket{Local: netip.MustParseAddrPort("[::ffff:192.168.1.10]:6002"), PID: pidGame}, "") // v4-mapped
	f.add(socket{Local: netip.MustParseAddrPort("[2001:db8::1]:6003"), PID: pidGame}, "")         // pure IPv6
	f.add(socket{Local: netip.MustParseAddrPort("192.168.1.10:6000"), PID: pidOther}, "")         // exact beats wildcard
	f.add(socket{Local: netip.MustParseAddrPort("[::]:6004"), PID: pidGame}, "")
	f.add(socket{Local: netip.MustParseAddrPort("0.0.0.0:6004"), PID: pidOther}, "") // v4 wildcard beats [::]
	o := newTestOwners(t, f, " GAME ")                                               // normalized to game.exe

	mustGame(t, o, gameLocal.String(), true)
	mustGame(t, o, "192.168.1.10:50001", false) // chrome
	mustGame(t, o, "192.168.1.10:50002", false) // our own process, despite its name
	mustGame(t, o, "10.0.0.5:6000", true)       // via 0.0.0.0
	mustGame(t, o, "192.168.1.10:6000", false)  // exact owner is chrome
	mustGame(t, o, "10.0.0.5:6001", true)       // via [::]
	mustGame(t, o, "192.168.1.10:6002", true)
	mustGame(t, o, "192.168.1.10:6004", false)
	mustGame(t, o, "192.168.1.10:6003", false) // the IPv6 socket can't send IPv4

	if got := o.gamePIDs(); !slices.Equal(got, []uint32{pidGame}) {
		t.Fatalf("gamePIDs = %v", got)
	}
}

func TestOwnersExactPIDs(t *testing.T) {
	f := newFakeOS()
	o, err := newOwners(nil, []uint32{pidOther, pidSelf}, pidSelf, f)
	if err != nil {
		t.Fatal(err)
	}
	mustGame(t, o, "192.168.1.10:50001", true)
	mustGame(t, o, gameLocal.String(), false)
	mustGame(t, o, "192.168.1.10:50002", false) // self, even when listed
}

// The very first packet of a socket the table hasn't seen yet must be
// classified from a fresh table, not let out as unknown.
func TestOwnersFirstPacketOfNewGame(t *testing.T) {
	f := newFakeOS()
	o := newTestOwners(t, f, "late.exe")
	s0, p0 := f.calls()

	f.add(socket{Local: netip.MustParseAddrPort("192.168.1.10:7000"), PID: 400}, "late.exe")
	mustGame(t, o, "192.168.1.10:7000", true)
	if s, p := f.calls(); s != s0+1 || p != p0+1 {
		t.Fatalf("lookups: sockets %d->%d, processes %d->%d; want one each", s0, s, p0, p)
	}
	if got := o.gamePIDs(); !slices.Equal(got, []uint32{400}) {
		t.Fatalf("gamePIDs = %v", got)
	}

	// A new socket of a known process needs no process list.
	f.add(socket{Local: netip.MustParseAddrPort("192.168.1.10:7001"), PID: 400}, "")
	mustGame(t, o, "192.168.1.10:7001", true)
	if _, p := f.calls(); p != p0+1 {
		t.Fatal("process list re-read for a known PID")
	}
}

func TestOwnersUnownedCached(t *testing.T) {
	f := newFakeOS()
	o := newTestOwners(t, f, "game.exe")
	mustGame(t, o, "192.168.1.10:9999", false)
	s1, _ := f.calls()
	mustGame(t, o, "192.168.1.10:9999", false)
	if s, _ := f.calls(); s != s1 {
		t.Fatal("unowned endpoint re-queried on every packet")
	}
	// The next background refresh drops the negative entry, so a game that
	// binds the port later is found.
	f.add(socket{Local: netip.MustParseAddrPort("192.168.1.10:9999"), PID: pidGame}, "")
	if err := o.refresh(); err != nil {
		t.Fatal(err)
	}
	mustGame(t, o, "192.168.1.10:9999", true)
}

func TestOwnersPIDReuse(t *testing.T) {
	f := newFakeOS()
	o := newTestOwners(t, f, "game.exe")
	mustGame(t, o, gameLocal.String(), true)
	f.mu.Lock()
	f.procs[pidGame] = "notepad.exe" // the game exited and its PID was reused
	f.mu.Unlock()
	if err := o.refresh(); err != nil {
		t.Fatal(err)
	}
	mustGame(t, o, gameLocal.String(), false)
	if len(o.gamePIDs()) != 0 {
		t.Fatalf("gamePIDs = %v", o.gamePIDs())
	}
}

func TestOwnersErrors(t *testing.T) {
	f := newFakeOS()
	f.err = errors.New("boom")
	if _, err := newOwners([]string{"game.exe"}, nil, pidSelf, f); err == nil {
		t.Fatal("newOwners ignored a listing error")
	}
	f.err = nil
	o := newTestOwners(t, f, "game.exe")
	f.err = errors.New("boom")
	if _, err := o.isGame(netip.MustParseAddrPort("192.168.1.10:9999")); err == nil {
		t.Fatal("lookup error swallowed")
	}
	mustGame(t, o, gameLocal.String(), true) // the cached table still answers
}
