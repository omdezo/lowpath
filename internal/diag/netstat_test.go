package diag

import (
	"math"
	"net/netip"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestRateHandlesWrapAndReset(t *testing.T) {
	t0 := time.Unix(100, 0)
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }

	tp, ok := Rate(Counters{At: t0, RxBytes: 1000, TxBytes: 50}, Counters{At: at(2), RxBytes: 5000, TxBytes: 250})
	if !ok || tp.RxBps != 2000 || tp.TxBps != 100 || tp.Window != 2*time.Second {
		t.Fatalf("plain rate = %+v %v", tp, ok)
	}
	// A 32-bit counter that wrapped.
	tp, ok = Rate(Counters{At: t0, RxBytes: math.MaxUint32 - 99}, Counters{At: at(1), RxBytes: 100})
	if !ok || tp.RxBps != 200 {
		t.Fatalf("32-bit wrap = %+v %v", tp, ok)
	}
	// A 64-bit counter that went backwards: the adapter was reset.
	if _, ok := Rate(Counters{At: t0, RxBytes: 1 << 40}, Counters{At: at(1), RxBytes: 5}); ok {
		t.Fatal("a reset counter must not give a rate")
	}
	if _, ok := Rate(Counters{At: t0}, Counters{At: t0}); ok {
		t.Fatal("same instant must not give a rate")
	}
}

func TestMeterStartsOverOnInterfaceChange(t *testing.T) {
	m := NewMeter()
	t0 := time.Unix(0, 0)
	if _, ok := m.Add(3, Counters{At: t0, RxBytes: 0}); ok {
		t.Fatal("first reading has no rate")
	}
	if tp, ok := m.Add(3, Counters{At: t0.Add(time.Second), RxBytes: 800}); !ok || tp.RxBps != 800 {
		t.Fatalf("rate = %+v %v", tp, ok)
	}
	if _, ok := m.Add(7, Counters{At: t0.Add(2 * time.Second), RxBytes: 5}); ok {
		t.Fatal("a new interface must start over")
	}
	if tp, ok := m.Add(7, Counters{At: t0.Add(3 * time.Second), RxBytes: 105}); !ok || tp.RxBps != 100 {
		t.Fatalf("rate after switch = %+v %v", tp, ok)
	}
}

const procRouteFixture = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
wlan0	00000000	FE01A8C0	0003	0	0	600	00000000	0	0	0
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
docker0	00000000	010011AC	0002	0	0	0	00000000	0	0	0
`

func TestParseProcRoute(t *testing.T) {
	rs := parseProcRoute(strings.NewReader(procRouteFixture))
	if len(rs) != 2 {
		t.Fatalf("routes = %+v (the down docker0 default and the /24 must be skipped)", rs)
	}
	best, _ := bestRoute(rs)
	if best.iface != "eth0" || best.gateway != netip.MustParseAddr("192.168.1.1") || best.metric != 100 {
		t.Fatalf("best = %+v", best)
	}
}

const ipv6RouteFixture = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00000003 wlan0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001 wlan0
`

func TestParseIPv6Route(t *testing.T) {
	rs := parseIPv6Route(strings.NewReader(ipv6RouteFixture))
	if len(rs) != 1 || rs[0].iface != "wlan0" || rs[0].gateway != netip.MustParseAddr("fe80::1") || rs[0].metric != 0x400 {
		t.Fatalf("routes = %+v", rs)
	}
}

func sysfsFixture() fstest.MapFS {
	return fstest.MapFS{
		"proc/net/route":                          {Data: []byte(procRouteFixture)},
		"sys/class/net/eth0/ifindex":              {Data: []byte("2\n")},
		"sys/class/net/eth0/type":                 {Data: []byte("1\n")},
		"sys/class/net/eth0/speed":                {Data: []byte("1000\n")},
		"sys/class/net/eth0/statistics/rx_bytes":  {Data: []byte("123456789\n")},
		"sys/class/net/eth0/statistics/tx_bytes":  {Data: []byte("4242\n")},
		"sys/class/net/wlan0/ifindex":             {Data: []byte("3\n")},
		"sys/class/net/wlan0/type":                {Data: []byte("1\n")},
		"sys/class/net/wlan0/wireless/link":       {Data: []byte("x")},
		"sys/class/net/wlan0/statistics/rx_bytes": {Data: []byte("1\n")},
		"sys/class/net/wlan0/statistics/tx_bytes": {Data: []byte("2\n")},
	}
}

func TestSysfsNetStats(t *testing.T) {
	now := time.Unix(42, 0)
	ns := NewSysfsNetStats(sysfsFixture(), func() time.Time { return now })
	r, err := ns.DefaultRoute()
	if err != nil {
		t.Fatal(err)
	}
	want := Route{Index: 2, Name: "eth0", Gateway: netip.MustParseAddr("192.168.1.1"), Kind: LinkEthernet}
	if r != want {
		t.Fatalf("route = %+v, want %+v", r, want)
	}
	c, err := ns.Counters(2)
	if err != nil {
		t.Fatal(err)
	}
	if c.RxBytes != 123456789 || c.TxBytes != 4242 || c.RxLinkBps != 1e9 || !c.At.Equal(now) {
		t.Fatalf("counters = %+v", c)
	}
	// Wi-Fi has no speed file.
	c, err = ns.Counters(3)
	if err != nil || c.RxLinkBps != 0 || c.TxBytes != 2 {
		t.Fatalf("wlan0 counters = %+v %v", c, err)
	}
	if _, err := ns.Counters(9); err == nil {
		t.Fatal("unknown index must fail")
	}
}

func TestSysfsNetStatsWiFiAndOffline(t *testing.T) {
	fs := sysfsFixture()
	fs["proc/net/route"] = &fstest.MapFile{Data: []byte(strings.Split(procRouteFixture, "\n")[0] + "\n" + strings.Split(procRouteFixture, "\n")[1] + "\n")}
	r, err := NewSysfsNetStats(fs, nil).DefaultRoute()
	if err != nil || r.Kind != LinkWiFi || r.Name != "wlan0" || r.Gateway != netip.MustParseAddr("192.168.1.254") {
		t.Fatalf("wifi route = %+v %v", r, err)
	}
	delete(fs, "proc/net/route")
	if _, err := NewSysfsNetStats(fs, nil).DefaultRoute(); err != ErrNoRoute {
		t.Fatalf("no routes: err = %v, want ErrNoRoute", err)
	}
}

func TestGradeScale(t *testing.T) {
	for _, c := range []struct {
		ms   float64
		want string
	}{{0, "A+"}, {4.9, "A+"}, {5, "A"}, {29.9, "A"}, {30, "B"}, {59, "B"}, {60, "C"}, {199, "C"}, {200, "D"}, {399, "D"}, {400, "F"}, {2000, "F"}, {math.NaN(), ""}} {
		if got := Grade(c.ms); got != c.want {
			t.Errorf("Grade(%v) = %q, want %q", c.ms, got, c.want)
		}
	}
}
