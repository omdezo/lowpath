package capture

import (
	"errors"
	"net/netip"
	"runtime"
	"slices"
	"sync"
	"testing"

	"lowpath/internal/divert"
	"lowpath/internal/netlink"
	"lowpath/internal/netlink/nft"
)

// fakeTUNSystem stands in for the kernel side of the TUN backend.
type fakeTUNSystem struct {
	avail     error
	openErr   error
	recovered int
	route     tunRoute
	dev       *fakeTUNDev
}

func (f *fakeTUNSystem) check() error { return f.avail }
func (f *fakeTUNSystem) recover() error {
	f.recovered++
	return nil
}
func (f *fakeTUNSystem) open(r tunRoute) (tunDevice, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.route = r
	f.dev = &fakeTUNDev{fakeDev: newFakeDev()}
	return f.dev, nil
}

type fakeTUNDev struct {
	*fakeDev
	mu      sync.Mutex
	directs [][]uint16
}

func (d *fakeTUNDev) setDirect(ports []uint16) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.directs = append(d.directs, slices.Clone(ports))
	return nil
}

func (d *fakeTUNDev) lastDirect() []uint16 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.directs) == 0 {
		return nil
	}
	return d.directs[len(d.directs)-1]
}

func TestHasTUNRights(t *testing.T) {
	status := func(eff string) []byte {
		return []byte("Name:\tlowpath\nCapInh:\t0000000000000000\nCapPrm:\t000001ffffffffff\nCapEff:\t" + eff + "\nCapBnd:\t000001ffffffffff\n")
	}
	for eff, want := range map[string]bool{
		"000001ffffffffff": true,  // root
		"0000000000003000": true,  // setcap cap_net_admin,cap_net_raw+ep
		"0000000000001000": false, // CAP_NET_ADMIN only: no raw socket
		"0000000000002000": false,
		"0000000000000000": false, // a normal user
	} {
		if got, err := hasTUNRights(status(eff)); err != nil || got != want {
			t.Errorf("CapEff %s: %v, %v; want %v", eff, got, err, want)
		}
	}
	for _, bad := range []string{"Name:\tx\n", "CapEff:\tnot-hex\n"} {
		if _, err := hasTUNRights([]byte(bad)); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
	if !errors.Is(tunRights(nil), ErrNeedAdmin) || errors.Is(tunRights(nil), errors.ErrUnsupported) {
		t.Error("the TUN rights error must read as missing rights, and only that")
	}
}

func TestDirectPorts(t *testing.T) {
	f := newFakeOS() // game 50000, other program 50001, self 50002
	f.add(socket{Local: netip.MustParseAddrPort("192.168.1.10:6000"), PID: pidGame}, "")
	f.add(socket{Local: netip.MustParseAddrPort("[::]:6000"), PID: pidOther}, "") // shares the game's port
	f.add(socket{Local: netip.MustParseAddrPort("[::]:7000"), PID: pidOther}, "")
	f.add(socket{Local: netip.MustParseAddrPort("[::ffff:192.168.1.10]:7100"), PID: pidOther}, "")
	o := newTestOwners(t, f, "game.exe")
	got := o.cur.Load().directPorts(maxDirectPorts)
	if want := []uint16{7000, 7100, 50001, 50002}; !slices.Equal(got, want) {
		t.Fatalf("directPorts = %v, want %v (never the game's, never a shared one)", got, want)
	}
	if got := o.cur.Load().directPorts(2); !slices.Equal(got, []uint16{7000, 7100}) {
		t.Fatalf("limit 2: %v", got)
	}
}

func TestLinuxNamesMatchWindowsNames(t *testing.T) {
	f := newFakeOS()
	f.add(socket{Local: netip.MustParseAddrPort("0.0.0.0:6100"), PID: 400}, "lpcheck")
	f.add(socket{Local: netip.MustParseAddrPort("0.0.0.0:6101"), PID: 401}, "cs2")
	o := newTestOwners(t, f, "lpcheck", "cs2.exe")
	mustGame(t, o, "192.168.1.10:6100", true)
	mustGame(t, o, "192.168.1.10:6101", true)
	mustGame(t, o, "192.168.1.10:50001", false)
}

// rules lists the expression names of each rule in a batch, and the other
// messages by type.
func ruleShapes(msgs []netlink.Message) (rules [][]string, other []uint16) {
	for _, m := range msgs {
		if m.Type&0xff != 6 || m.Type>>8 != 10 { // NFT_MSG_NEWRULE
			other = append(other, m.Type)
			continue
		}
		var names []string
		for _, a := range netlink.ParseAttrs(m.Data[4:]) {
			if a.Type != 4 { // NFTA_RULE_EXPRESSIONS
				continue
			}
			for _, e := range netlink.ParseAttrs(a.Data) {
				for _, f := range netlink.ParseAttrs(e.Data) {
					if f.Type == 1 {
						names = append(names, string(f.Data[:len(f.Data)-1]))
					}
				}
			}
		}
		rules = append(rules, names)
	}
	return rules, other
}

func TestTUNRuleset(t *testing.T) {
	r := tunRoute{
		exclude: []netip.AddrPort{
			netip.MustParseAddrPort("198.51.100.20:7000"),
			netip.MustParseAddrPort("[::ffff:198.51.100.20]:7000"), // the same relay again
			netip.MustParseAddrPort("198.51.100.21:0"),             // the whole host
			netip.MustParseAddrPort("127.0.0.1:7878"),              // never marked anyway
			netip.MustParseAddrPort("[2001:db8::1]:7000"),          // IPv6 isn't captured
		},
		direct: []uint16{443, 5353},
	}
	msgs := tunRuleset(r, true)
	rules, other := ruleShapes(msgs)
	// begin, add/delete/add table, chain, set, elements, end
	if len(other) != 8 || other[0] != 0x10 || other[len(other)-1] != 0x11 {
		t.Fatalf("non-rule messages %x", other)
	}
	if a := netlink.ParseAttrs(msgs[3].Data[4:]); len(a) != 2 || a[1].Data[3] != nft.TableOwner {
		t.Fatalf("the new table isn't owned: %+v", a)
	}
	if a := netlink.ParseAttrs(tunRuleset(r, false)[3].Data[4:]); len(a) != 1 {
		t.Fatalf("unowned table has flags: %+v", a)
	}
	want := 3 + len(tunExcluded()) + 1 + 2 + 1 + 1
	if len(rules) != want {
		t.Fatalf("%d rules, want %d: %v", len(rules), want, rules)
	}
	relayRule := rules[3+len(tunExcluded())+1]
	hostRule := rules[3+len(tunExcluded())+2]
	if len(relayRule) != 5 || len(hostRule) != 3 {
		t.Errorf("relay rules %v and %v", relayRule, hostRule)
	}
	if last := rules[len(rules)-1]; !slices.Equal(last, []string{"meta", "immediate", "meta"}) {
		t.Errorf("marking rule %v", last)
	}
	if fast := rules[len(rules)-2]; !slices.Equal(fast, []string{"payload", "lookup", "immediate"}) {
		t.Errorf("fast-path rule %v", fast)
	}
	if _, other := ruleShapes(tunRuleset(tunRoute{}, true)); len(other) != 7 {
		t.Errorf("an empty direct set still adds elements: %x", other)
	}
}

func TestTUNExcludedPrefixes(t *testing.T) {
	ex := tunExcluded()
	for _, a := range []string{"10.1.2.3", "127.0.0.1", "169.254.76.80", "192.168.1.1", "100.64.0.1", "172.16.0.1", "224.0.0.251", "255.255.255.255", "0.0.0.0"} {
		if !slices.ContainsFunc(ex, func(p netip.Prefix) bool { return p.Contains(netip.MustParseAddr(a)) }) {
			t.Errorf("%s would be marked", a)
		}
	}
	for _, a := range []string{"203.0.113.7", "198.51.100.1", "192.0.2.1", "8.8.8.8"} {
		if slices.ContainsFunc(ex, func(p netip.Prefix) bool { return p.Contains(netip.MustParseAddr(a)) }) {
			t.Errorf("game server %s would never be captured", a)
		}
	}
	if m := prefixMask(netip.MustParsePrefix("100.64.0.0/10")); !slices.Equal(m, []byte{255, 192, 0, 0}) {
		t.Errorf("mask %v", m)
	}
}

func TestTUNDirectUpdate(t *testing.T) {
	if m := tunDirectUpdate(map[uint16]bool{1: true, 2: true}, []uint16{2, 1}); m != nil {
		t.Fatalf("no change made %d messages", len(m))
	}
	m := tunDirectUpdate(map[uint16]bool{1: true, 2: true}, []uint16{2, 3})
	_, other := ruleShapes(m)
	if len(other) != 4 || other[1] != 10<<8|14 || other[2] != 10<<8|12 {
		t.Fatalf("update messages %x, want begin, delete 1, add 3, end", other)
	}
	if m := tunDirectUpdate(map[uint16]bool{}, nil); m != nil {
		t.Fatal("empty to empty")
	}
}

func tunUnderTest(sys *fakeTUNSystem, f *fakeOS) *TUN {
	return &TUN{owners: f, sys: sys, self: pidSelf}
}

func TestTUNBackend(t *testing.T) {
	f := newFakeOS()
	sys := &fakeTUNSystem{}
	b := tunUnderTest(sys, f)
	if b.Name() != "tun" {
		t.Fatalf("Name = %q", b.Name())
	}
	var mu sync.Mutex
	var got []string
	s, err := b.Start(Config{
		Processes: []string{"game"},
		Exclude:   []netip.AddrPort{netip.MustParseAddrPort("198.51.100.20:7000")},
		OnPacket: func(_, _ netip.AddrPort, p []byte) bool {
			mu.Lock()
			got = append(got, string(p))
			mu.Unlock()
			return true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !slices.Equal(sys.route.direct, []uint16{50001, 50002}) || len(sys.route.exclude) != 1 {
		t.Fatalf("open got %+v", sys.route)
	}
	var out divert.Address
	out.SetOutbound(true)
	sys.dev.in <- recvd{udp(t, gameLocal, gameServer, "hi"), out}
	sys.dev.in <- recvd{udp(t, netip.MustParseAddrPort("192.168.1.10:50001"), gameServer, "web"), out}
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 && len(sys.dev.sentPackets()) == 1 })

	// A new program's socket joins the kernel's fast path at the next
	// refresh.
	f.add(socket{Local: netip.MustParseAddrPort("0.0.0.0:6200"), PID: 500}, "chat")
	waitFor(t, func() bool { return slices.Equal(sys.dev.lastDirect(), []uint16{6200, 50001, 50002}) })

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Recover(Marker{Backend: "tun", PID: 1}); err != nil || sys.recovered != 1 {
		t.Fatalf("Recover = %v (%d calls)", err, sys.recovered)
	}
}

func TestTUNStartFailures(t *testing.T) {
	on := func(_, _ netip.AddrPort, _ []byte) bool { return true }
	sys := &fakeTUNSystem{avail: tunRights(nil)}
	if _, err := tunUnderTest(sys, newFakeOS()).Start(Config{Processes: []string{"game"}, OnPacket: on}); !errors.Is(err, ErrNeedAdmin) {
		t.Fatalf("without rights: %v", err)
	}
	if sys.dev != nil {
		t.Fatal("opened without rights")
	}
	sys = &fakeTUNSystem{openErr: errors.New("nftables: operation not supported")}
	if _, err := tunUnderTest(sys, newFakeOS()).Start(Config{Processes: []string{"game"}, OnPacket: on}); err != sys.openErr {
		t.Fatalf("open failure: %v", err)
	}
	if _, err := tunUnderTest(&fakeTUNSystem{}, newFakeOS()).Start(Config{OnPacket: on}); err == nil {
		t.Fatal("no processes accepted")
	}
	f := newFakeOS()
	f.err = errors.New("/proc is gone")
	if _, err := tunUnderTest(&fakeTUNSystem{}, f).Start(Config{Processes: []string{"game"}, OnPacket: on}); err == nil {
		t.Fatal("an unreadable socket table accepted")
	}
}

func TestTUNOnOtherSystems(t *testing.T) {
	b := NewTUN(newFakeOS())
	err := b.Available()
	switch {
	case err == nil, errors.Is(err, ErrNeedAdmin), errors.Is(err, ErrNoTUN):
		// Linux: root, a normal user, or a kernel without TUN.
	case errors.Is(err, errors.ErrUnsupported):
		// Windows and macOS.
	default:
		t.Fatalf("Available = %v", err)
	}
	if runtime.GOOS != "linux" {
		if err := b.Recover(Marker{Backend: "tun"}); err != nil {
			t.Fatalf("Recover where there is nothing to undo: %v", err)
		}
	}
}
