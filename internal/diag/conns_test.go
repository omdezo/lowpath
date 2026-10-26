package diag

import (
	"errors"
	"net"
	"net/netip"
	"reflect"
	"testing"
	"time"
)

// home is a PC on a dual-stack home network (192.0.2.10/24 and a global
// /64), with a container bridge and a VPN tunnel next to it.
var home = LinkAddrs{
	Iface: []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24"), netip.MustParsePrefix("2001:db8:0:1::10/64"), netip.MustParsePrefix("fe80::10/64")},
	Local: []netip.Addr{
		netip.MustParseAddr("192.0.2.10"), netip.MustParseAddr("2001:db8:0:1::10"), netip.MustParseAddr("fe80::10"),
		netip.MustParseAddr("127.0.0.1"), netip.MustParseAddr("::1"),
		netip.MustParseAddr("172.17.0.1"), netip.MustParseAddr("100.64.0.2"),
	},
}

func TestLinkAddrsKind(t *testing.T) {
	cases := []struct {
		name          string
		local, remote string
		want          peerKind
	}{
		{"internet over IPv4", "192.0.2.10:50000", "203.0.113.5:443", peerInternet},
		{"internet over IPv6", "[2001:db8:0:1::10]:50000", "[2001:db8:ffff::5]:443", peerInternet},
		{"v4-mapped peer of a dual-stack socket", "[::ffff:192.0.2.10]:50000", "[::ffff:203.0.113.5]:443", peerInternet},
		{"NAS on the home subnet (RFC 1918 range)", "192.0.2.10:50000", "10.0.0.20:445", peerLAN},
		{"NAS in the interface's own prefix", "192.0.2.10:50000", "192.0.2.20:445", peerLAN},
		{"NAS by global IPv6 in the on-link /64", "[2001:db8:0:1::10]:50000", "[2001:db8:0:1::20]:445", peerLAN},
		{"ULA peer", "[2001:db8:0:1::10]:50000", "[fd00::20]:8009", peerLAN},
		{"link-local peer, zone given", "[fe80::10%eth0]:50000", "[fe80::20%eth0]:8009", peerLAN},
		{"IPv4 link-local peer", "192.0.2.10:50000", "169.254.1.2:80", peerLAN},
		{"v4-mapped LAN peer", "[::ffff:192.0.2.10]:50000", "[::ffff:192.0.2.20]:445", peerLAN},
		{"loopback", "127.0.0.1:50000", "127.0.0.1:7878", peerElsewhere},
		{"to this PC's own address", "192.0.2.10:50000", "192.0.2.10:7878", peerElsewhere},
		{"container bridge", "172.17.0.1:50000", "172.17.0.2:5432", peerElsewhere},
		{"VPN tunnel", "100.64.0.2:50000", "203.0.113.5:443", peerElsewhere},
		{"no peer", "192.0.2.10:50000", "0.0.0.0:0", peerElsewhere},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn := TCPConn{Local: netip.MustParseAddrPort(c.local), Remote: netip.MustParseAddrPort(c.remote)}
			if got := home.kind(conn); got != c.want {
				t.Errorf("kind = %d, want %d", got, c.want)
			}
		})
	}
}

func TestPrefixes(t *testing.T) {
	got := prefixes([]net.Addr{
		&net.IPNet{IP: net.IPv4(192, 0, 2, 10), Mask: net.CIDRMask(24, 32)},
		&net.IPNet{IP: net.IPv4(192, 0, 2, 11), Mask: net.CIDRMask(120, 128)}, // IPv4 with a 16-byte mask
		&net.IPNet{IP: net.ParseIP("2001:db8::10"), Mask: net.CIDRMask(64, 128)},
		&net.IPNet{IP: net.ParseIP("2001:db8::11"), Mask: net.IPMask{0xff, 0, 0xff, 0}}, // not a prefix mask
		&net.IPAddr{IP: net.IPv4(192, 0, 2, 12)},
	})
	want := []netip.Prefix{netip.MustParsePrefix("192.0.2.10/24"), netip.MustParsePrefix("192.0.2.11/24"), netip.MustParsePrefix("2001:db8::10/64")}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prefixes = %v, want %v", got, want)
	}
}

// fakeConns is a connection table a test changes between samples.
type fakeConns struct {
	conns   []TCPConn
	owners  map[uint64]uint32
	err     error
	reads   int
	lookups int
}

func (f *fakeConns) TCPConns() ([]TCPConn, error) {
	f.reads++
	return append([]TCPConn(nil), f.conns...), f.err
}

func (f *fakeConns) Owners(conns []TCPConn) map[uint64]uint32 {
	f.lookups++
	out := map[uint64]uint32{}
	for _, c := range conns {
		if pid, ok := f.owners[c.ID]; ok {
			out[c.ID] = pid
		}
	}
	return out
}

func conn(id uint64, local, remote string, sent, recv uint64) TCPConn {
	return TCPConn{ID: id, Local: netip.MustParseAddrPort(local), Remote: netip.MustParseAddrPort(remote), Sent: sent, Received: recv}
}

func newTestConnWatch(f *fakeConns, names map[uint32]string) (*ConnWatch, *int) {
	nameReads := 0
	w := NewConnWatch(f, func() (map[uint32]string, error) { nameReads++; return names, nil }, func(int) (LinkAddrs, error) { return home, nil })
	return w, &nameReads
}

func TestConnWatchSplitsLANFromInternet(t *testing.T) {
	f := &fakeConns{owners: map[uint64]uint32{1: 300, 2: 300, 3: 400, 9: 500, 11: 300}}
	w, nameReads := newTestConnWatch(f, map[uint32]string{300: "steam", 400: "backup", 500: "nas-sync"})
	at := time.Unix(1000, 0)
	f.conns = []TCPConn{
		conn(1, "192.0.2.10:50001", "203.0.113.5:443", 1000, 1_000_000),
		conn(2, "192.0.2.10:50002", "203.0.113.6:443", 0, 2_000_000),
		conn(3, "[2001:db8:0:1::10]:50003", "[2001:db8:ffff::7]:443", 5_000, 0),
		conn(9, "192.0.2.10:50009", "192.0.2.20:445", 0, 50_000_000),
		conn(10, "127.0.0.1:50010", "127.0.0.1:7878", 9e9, 9e9),
	}
	if _, ok := w.Sample(at, 7); ok {
		t.Fatal("the first reading has nothing to compare with")
	}
	// One second later: Steam downloads 10 MB/s over two connections, a
	// backup uploads 2 MB/s over IPv6, the NAS copy moves 100 MB/s on the
	// LAN, loopback traffic is ignored, and connections opened during the
	// second count in full.
	f.conns = []TCPConn{
		conn(1, "192.0.2.10:50001", "203.0.113.5:443", 1000, 5_000_000),
		conn(2, "192.0.2.10:50002", "203.0.113.6:443", 0, 8_000_000),
		conn(3, "[2001:db8:0:1::10]:50003", "[2001:db8:ffff::7]:443", 2_005_000, 0),
		conn(9, "192.0.2.10:50009", "192.0.2.20:445", 0, 150_000_000),
		conn(10, "127.0.0.1:50010", "127.0.0.1:7878", 2e10, 2e10),
		conn(11, "192.0.2.10:50011", "203.0.113.5:443", 0, 1_000_000),
		conn(12, "192.0.2.10:50012", "203.0.113.8:443", 0, 3_000_000),
	}
	s, ok := w.Sample(at.Add(time.Second), 7)
	if !ok {
		t.Fatal("second reading: no sample")
	}
	if s.LANRx != 100e6 || s.LANTx != 0 {
		t.Errorf("LAN = %v down, %v up; want 100 MB/s down", s.LANRx, s.LANTx)
	}
	want := []Talker{
		{PID: 300, Name: "steam", Rx: 11e6}, // conn 12 has no owner: no one's
		{PID: 400, Name: "backup", Tx: 2e6},
	}
	if !reflect.DeepEqual(s.Talkers, want) {
		t.Errorf("talkers =\n%+v\nwant\n%+v", s.Talkers, want)
	}
	if *nameReads != 1 {
		t.Errorf("process list read %d times, want once", *nameReads)
	}
	// Another second with the same programs: names come from the cache.
	for i := range f.conns {
		f.conns[i].Received += 1_000_000
	}
	if _, ok := w.Sample(at.Add(2*time.Second), 7); !ok || *nameReads != 1 {
		t.Errorf("third reading: ok %v, process list read %d times", ok, *nameReads)
	}
}

func TestConnWatchQuietLineLooksUpNoOwners(t *testing.T) {
	f := &fakeConns{owners: map[uint64]uint32{1: 300}}
	w, _ := newTestConnWatch(f, nil)
	at := time.Unix(0, 0)
	f.conns = []TCPConn{conn(1, "192.0.2.10:50001", "203.0.113.5:443", 0, 0)}
	w.Sample(at, 7)
	f.conns[0].Received = 50_000 // 400 kbit/s: a chat client, a web page
	s, ok := w.Sample(at.Add(time.Second), 7)
	if !ok || len(s.Talkers) != 0 || f.lookups != 0 {
		t.Errorf("quiet line: ok %v, talkers %v, %d owner lookups", ok, s.Talkers, f.lookups)
	}
}

func TestConnWatchUnknownNamesAndReusedIDs(t *testing.T) {
	f := &fakeConns{owners: map[uint64]uint32{1: 77}}
	w, nameReads := newTestConnWatch(f, map[uint32]string{})
	at := time.Unix(0, 0)
	f.conns = []TCPConn{conn(1, "192.0.2.10:50001", "203.0.113.5:443", 0, 9_000_000)}
	w.Sample(at, 7)
	f.conns[0].Received = 19_000_000
	s, _ := w.Sample(at.Add(time.Second), 7)
	if len(s.Talkers) != 1 || s.Talkers[0].Name != "PID 77" {
		t.Errorf("unnamed process: %+v", s.Talkers)
	}
	if *nameReads != 1 {
		t.Errorf("process list read %d times", *nameReads)
	}
	// Counters that go backwards under the same ID add nothing.
	f.conns[0].Received = 1_000
	if s, ok := w.Sample(at.Add(2*time.Second), 7); !ok || len(s.Talkers) != 0 || s.LANRx != 0 {
		t.Errorf("counter went backwards: %+v %v", s, ok)
	}
}

func TestConnWatchStartsOver(t *testing.T) {
	f := &fakeConns{}
	w, _ := newTestConnWatch(f, nil)
	at := time.Unix(0, 0)
	f.conns = []TCPConn{conn(9, "192.0.2.10:50009", "192.0.2.20:445", 0, 0)}
	w.Sample(at, 7)
	// Another adapter: its counters aren't comparable with the last ones.
	f.conns[0].Received = 10e6
	if _, ok := w.Sample(at.Add(time.Second), 8); ok {
		t.Error("sample across an interface change")
	}
	f.conns[0].Received = 20e6
	if s, ok := w.Sample(at.Add(2*time.Second), 8); !ok || s.LANRx != 10e6 {
		t.Errorf("after the change: %+v %v", s, ok)
	}
	// Reset (the session ended) starts over too, and so does a reading
	// with no time between.
	w.Reset()
	if _, ok := w.Sample(at.Add(3*time.Second), 8); ok {
		t.Error("sample right after Reset")
	}
	if _, ok := w.Sample(at.Add(3*time.Second), 8); ok {
		t.Error("sample with no time between readings")
	}
	// Unreadable interface addresses: no split rather than a wrong one.
	w.addrs = func(int) (LinkAddrs, error) { return LinkAddrs{}, errors.New("no such interface") }
	w.Sample(at.Add(4*time.Second), 9)
	if _, ok := w.Sample(at.Add(5*time.Second), 9); ok {
		t.Error("sample without the interface's addresses")
	}
}

func TestConnWatchWithoutCounters(t *testing.T) {
	// Windows and macOS: unsupported, asked once and never again.
	f := &fakeConns{err: connsUnsupported{"test"}}
	w, _ := newTestConnWatch(f, nil)
	for i := range 3 {
		if _, ok := w.Sample(time.Unix(int64(i), 0), 7); ok {
			t.Fatal("sample without counters")
		}
	}
	if f.reads != 1 {
		t.Errorf("unsupported counters read %d times, want once", f.reads)
	}
	if !errors.Is(f.err, ErrUnsupported) || !errors.Is(f.err, errors.ErrUnsupported) {
		t.Error("connsUnsupported must match both unsupported errors")
	}
	// Any other error is retried the next second.
	f = &fakeConns{err: errors.New("netlink: no answer")}
	w, _ = newTestConnWatch(f, nil)
	w.Sample(time.Unix(0, 0), 7)
	w.Sample(time.Unix(1, 0), 7)
	if f.reads != 2 {
		t.Errorf("failing counters read %d times, want 2", f.reads)
	}
	// No ConnStats at all.
	if _, ok := NewConnWatch(nil, nil, nil).Sample(time.Unix(0, 0), 7); ok {
		t.Error("sample from a nil ConnStats")
	}
}
