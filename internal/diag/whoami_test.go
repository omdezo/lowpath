package diag

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/stun"
)

// fakeFinder answers PublicAddr from a table, optionally after a delay.
type fakeFinder struct {
	addrs map[Family]netip.Addr
	errs  map[Family]error
	delay time.Duration
	calls atomic.Int32
}

func (f *fakeFinder) PublicAddr(ctx context.Context, fam Family) (PublicAddr, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return PublicAddr{}, ctx.Err()
		}
	}
	if err := f.errs[fam]; err != nil {
		return PublicAddr{}, err
	}
	a, ok := f.addrs[fam]
	if !ok {
		return PublicAddr{}, errNoAnswer
	}
	return PublicAddr{Addr: a, Source: "fake"}, nil
}

// tableLookup maps addresses to networks.
type tableLookup map[netip.Addr]NetworkInfo

func (t tableLookup) LookupNetwork(_ context.Context, a netip.Addr) (NetworkInfo, error) {
	if ni, ok := t[a]; ok {
		return ni, nil
	}
	return NetworkInfo{}, errors.New("RIPEstat network-info: HTTP 503")
}

var (
	pub4 = netip.MustParseAddr("5.37.1.2")
	pub6 = netip.MustParseAddr("2a02:2c8::1")
	omt  = NetworkInfo{ASN: 28885, ISP: "Oman Telecommunications Company (S.A.O.G)", Country: "OM", City: "Muscat"}
)

func TestLocateBothFamilies(t *testing.T) {
	l := NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4, IPv6: pub6}}},
		Lookup:  tableLookup{pub4: omt, pub6: omt},
	})
	loc, err := l.Locate(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if loc.State != StateOK || loc.IPv4 == nil || loc.IPv4.IP != pub4 || loc.IPv4.City != "Muscat" || loc.IPv6 == nil || loc.IPv6.ASN != 28885 || loc.IPv6Note != "" {
		t.Fatalf("got %+v", loc)
	}
}

func TestLocateNoIPv6IsFine(t *testing.T) {
	l := NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{
			addrs: map[Family]netip.Addr{IPv4: pub4},
			errs:  map[Family]error{IPv6: &net.OpError{Op: "dial", Net: "udp6", Err: errors.New("network is unreachable")}},
		}},
		Lookup: tableLookup{pub4: omt},
	})
	loc, _ := l.Locate(context.Background(), false)
	if loc.State != StateOK || loc.IPv6 != nil || !strings.Contains(loc.IPv6Note, "no public IPv6") {
		t.Fatalf("got %+v", loc)
	}
}

func TestLocateFallsBackToNextFinder(t *testing.T) {
	blocked := &fakeFinder{errs: map[Family]error{IPv4: stun.ErrTimeout, IPv6: stun.ErrTimeout}}
	https := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{blocked, https}, Lookup: tableLookup{pub4: omt}})
	loc, _ := l.Locate(context.Background(), false)
	if loc.State != StateOK || loc.IPv4 == nil || loc.IPv4.IP != pub4 {
		t.Fatalf("got %+v", loc)
	}
	if blocked.calls.Load() != 2 || https.calls.Load() != 2 {
		t.Fatalf("calls: stun %d, https %d", blocked.calls.Load(), https.calls.Load())
	}
}

func TestLocateIgnoresWrongFamilyAnswer(t *testing.T) {
	// An HTTPS fallback asked for IPv6 that came back over IPv4 must not
	// fill the IPv6 slot.
	f := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4, IPv6: pub4}}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: tableLookup{pub4: omt}})
	loc, _ := l.Locate(context.Background(), false)
	if loc.IPv6 != nil {
		t.Fatalf("IPv6 slot holds %v", loc.IPv6.IP)
	}
}

func TestLocateOffline(t *testing.T) {
	dnsErr := &net.DNSError{Err: "no such host", Name: "stun.l.google.com", IsNotFound: true}
	l := NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{errs: map[Family]error{IPv4: dnsErr, IPv6: dnsErr}}},
		Lookup:  tableLookup{},
	})
	loc, _ := l.Locate(context.Background(), false)
	if loc.State != StateOffline || loc.IPv4 != nil || loc.IPv6 != nil {
		t.Fatalf("got %+v", loc)
	}
	if !strings.Contains(loc.Message, "DNS") || !strings.Contains(loc.Message, "online") {
		t.Fatalf("message %q", loc.Message)
	}
}

func TestLocateLookupFailureIsPartial(t *testing.T) {
	l := NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}}},
		Lookup:  tableLookup{},
	})
	loc, _ := l.Locate(context.Background(), false)
	if loc.State != StatePartial || loc.IPv4 == nil || loc.IPv4.IP != pub4 || loc.IPv4.ASN != 0 || !strings.Contains(loc.Message, "503") {
		t.Fatalf("got %+v", loc)
	}
}

func TestLocateNonPublicAddress(t *testing.T) {
	for _, a := range []string{"192.168.1.20", "100.72.1.1", "127.0.0.1"} {
		ip := netip.MustParseAddr(a)
		lookup := &countingLookup{info: omt}
		l := NewLocator(LocatorConfig{Finders: []AddrFinder{&fakeFinder{addrs: map[Family]netip.Addr{IPv4: ip}}}, Lookup: lookup})
		loc, _ := l.Locate(context.Background(), false)
		if loc.State != StatePartial || loc.IPv4 == nil || !strings.Contains(loc.IPv4.LookupError, "not a public") {
			t.Fatalf("%s: got %+v", a, loc)
		}
		if lookup.count(ip) != 0 {
			t.Fatalf("%s was sent to the registry", a)
		}
	}
}

func TestLocateCachesAndRefreshes(t *testing.T) {
	clk := newClock()
	f := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: tableLookup{pub4: omt}, MaxAge: time.Minute, Now: clk.Now})
	ctx := context.Background()
	if _, ok := l.Last(); ok {
		t.Fatal("result before any lookup")
	}
	l.Locate(ctx, false)
	l.Locate(ctx, false)
	if n := f.calls.Load(); n != 2 { // one per family
		t.Fatalf("fresh result not reused: %d finder calls", n)
	}
	l.Locate(ctx, true)
	if n := f.calls.Load(); n != 4 {
		t.Fatalf("refresh ignored: %d finder calls", n)
	}
	clk.Add(2 * time.Minute)
	l.Locate(ctx, false)
	if n := f.calls.Load(); n != 6 {
		t.Fatalf("stale result reused: %d finder calls", n)
	}
	if last, ok := l.Last(); !ok || last.IPv4.IP != pub4 {
		t.Fatalf("last %+v", last)
	}
}

func TestLocateSharesOneLookup(t *testing.T) {
	f := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}, delay: 50 * time.Millisecond}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: tableLookup{pub4: omt}})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if loc, err := l.Locate(context.Background(), true); err != nil || loc.State != StateOK {
				t.Errorf("got %+v, %v", loc, err)
			}
		})
	}
	wg.Wait()
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("%d finder calls for 8 concurrent callers, want 2 (one lookup)", n)
	}
}

func TestLocateCallerCancelKeepsLookup(t *testing.T) {
	f := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}, delay: 80 * time.Millisecond}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: tableLookup{pub4: omt}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := l.Locate(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	loc, err := l.Locate(context.Background(), false) // joins the lookup still running
	if err != nil || loc.State != StateOK || f.calls.Load() != 2 {
		t.Fatalf("got %+v, %v, %d calls", loc, err, f.calls.Load())
	}
}

func TestLocateTimeout(t *testing.T) {
	f := &fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4}, delay: time.Hour}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Timeout: 50 * time.Millisecond})
	start := time.Now()
	loc, err := l.Locate(context.Background(), false)
	if err != nil || loc.State != StateOffline || !strings.Contains(loc.Message, "no reply in time") {
		t.Fatalf("got %+v, %v", loc, err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v", el)
	}
}

func TestRedacted(t *testing.T) {
	loc := Location{IPv4: &Endpoint{IP: netip.MustParseAddr("5.37.1.2"), NetworkInfo: omt}, IPv6: &Endpoint{IP: netip.MustParseAddr("2a02:2c8:1:2:3:4:5:6")}}
	r := loc.Redacted()
	if r.IPv4.IP.String() != "5.37.1.0" || r.IPv6.IP.String() != "2a02:2c8:1::" || r.IPv4.ASN != 28885 {
		t.Fatalf("redacted %+v %+v", r.IPv4, r.IPv6)
	}
	if loc.IPv4.IP != pub4 {
		t.Fatal("Redacted changed the original")
	}

	// GeoLite's point can be finer than the city: the report keeps only the city.
	lat, lon := 52.37, 4.89
	loc.IPv4.City, loc.IPv4.Lat, loc.IPv4.Lon = "Amsterdam", &lat, &lon
	r = loc.Redacted()
	if r.IPv4.Lat != nil || r.IPv4.Lon != nil || r.IPv4.City != "Amsterdam" || loc.IPv4.Lat == nil {
		t.Fatalf("coordinates in the report: %+v", r.IPv4.NetworkInfo)
	}
}

// startSTUN runs a minimal local STUN server that answers with the
// sender's address.
func startSTUN(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			if n < stun.HeaderLen {
				continue
			}
			ip := from.Addr().As4()
			v := []byte{0, 1}
			v = binary.BigEndian.AppendUint16(v, from.Port()^uint16(stun.MagicCookie>>16))
			for i := range ip {
				v = append(v, ip[i]^byte(uint32(stun.MagicCookie)>>(24-8*i)))
			}
			out := binary.BigEndian.AppendUint16(nil, 0x0101)
			out = binary.BigEndian.AppendUint16(out, uint16(4+len(v)))
			out = append(out, buf[4:20]...) // cookie + transaction id
			out = binary.BigEndian.AppendUint16(out, 0x0020)
			out = binary.BigEndian.AppendUint16(out, uint16(len(v)))
			out = append(out, v...)
			conn.WriteToUDPAddrPort(out, from)
		}
	}()
	return conn.LocalAddr().String()
}

func TestSTUNFinderFirstAnswerWins(t *testing.T) {
	// A dead server next to a live one: the live one answers and the dead
	// one costs nothing.
	dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	live := startSTUN(t)
	client := stun.NewClient(stun.ClientOptions{RTO: time.Second, Attempts: 2})
	f := NewSTUNFinder(client, []string{dead.LocalAddr().String(), live}, 0)
	start := time.Now()
	pa, err := f.PublicAddr(context.Background(), IPv4)
	if err != nil || !pa.Addr.IsLoopback() || pa.Source != "stun "+live {
		t.Fatalf("got %+v, %v", pa, err)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Fatalf("waited %v for the dead server", el)
	}

	// The loopback answer is not looked up anywhere.
	lookup := &countingLookup{info: omt}
	l := NewLocator(LocatorConfig{Finders: []AddrFinder{f}, Lookup: lookup})
	loc, _ := l.Locate(context.Background(), false)
	if loc.IPv4 == nil || loc.IPv4.LookupError == "" || lookup.count(loc.IPv4.IP) != 0 {
		t.Fatalf("got %+v", loc.IPv4)
	}
}

func TestSTUNFinderAllFail(t *testing.T) {
	dead, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Close()
	// A name that never resolves; slow resolvers can take many seconds to say
	// so, which the finder's own timeout cuts short.
	f := NewSTUNFinder(stun.NewClient(stun.ClientOptions{RTO: 20 * time.Millisecond, Attempts: 2}), []string{dead.LocalAddr().String(), "nonexistent.invalid:3478"}, 300*time.Millisecond)
	start := time.Now()
	if _, err := f.PublicAddr(context.Background(), IPv4); err == nil || !strings.Contains(err.Error(), "nonexistent.invalid") {
		t.Fatalf("err %v", err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v", el)
	}
	if _, err := NewSTUNFinder(stun.NewClient(stun.ClientOptions{}), nil, 0).PublicAddr(context.Background(), IPv4); err == nil {
		t.Fatal("no servers: no error")
	}
}

func TestLocateStateFollowsIPv4(t *testing.T) {
	// The IPv6 network lookup fails but IPv4's works: the line is fine,
	// and the IPv6 entry says what went wrong.
	l := NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{addrs: map[Family]netip.Addr{IPv4: pub4, IPv6: pub6}}},
		Lookup:  tableLookup{pub4: omt},
	})
	loc, _ := l.Locate(context.Background(), false)
	if loc.State != StateOK || loc.Message != "" || loc.IPv6 == nil || loc.IPv6.LookupError == "" {
		t.Fatalf("got %+v / %+v", loc, loc.IPv6)
	}
	// IPv6-only line: IPv6 decides.
	l = NewLocator(LocatorConfig{
		Finders: []AddrFinder{&fakeFinder{addrs: map[Family]netip.Addr{IPv6: pub6}}},
		Lookup:  tableLookup{pub6: omt},
	})
	loc, _ = l.Locate(context.Background(), false)
	if loc.State != StateOK || loc.IPv4 != nil || loc.IPv6.ASN != 28885 {
		t.Fatalf("IPv6 only: %+v", loc)
	}
}
