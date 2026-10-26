package compat

import (
	"errors"
	"net/netip"
	"strings"
	"testing"

	"lowpath/internal/diag"
)

type fixedRoute struct {
	r   diag.Route
	err error
}

func (f fixedRoute) DefaultRoute() (diag.Route, error) { return f.r, f.err }

// A VPN that adds 0/1 + 128/1 (OpenVPN def1) or takes the traffic with a
// policy rule (wg-quick on Linux) leaves the 0/0 route on the physical
// link; the source address the kernel picks names the tunnel.
func TestEgressRouteFollowsTheSourceAddress(t *testing.T) {
	ifs := []netInterface{
		{Index: 2, Name: "eth0", Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.2"), netip.MustParseAddr("2001:db8::2")}},
		{Index: 5, Name: "tun0", Addrs: []netip.Addr{netip.MustParseAddr("198.51.100.2")}},
	}
	fallback := fixedRoute{r: diag.Route{Index: 2, Name: "eth0"}}
	for _, tc := range []struct {
		name string
		src  map[string]string // target → source address; missing: unreachable
		want int
		err  error
	}{
		{"full tunnel", map[string]string{"v4": "198.51.100.2", "v6": "2001:db8::2"}, 5, nil},
		{"no tunnel", map[string]string{"v4": "192.0.2.2"}, 2, nil},
		{"IPv6 only", map[string]string{"v6": "2001:db8::2"}, 2, nil},
		{"mapped IPv4", map[string]string{"v4": "::ffff:198.51.100.2"}, 5, nil},
		{"unknown source: fallback", map[string]string{"v4": "203.0.113.7"}, 2, nil},
		{"offline: fallback says so", nil, 0, diag.ErrNoRoute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fb := fallback
			if tc.err != nil {
				fb = fixedRoute{err: tc.err}
			}
			e := &EgressRoute{
				targets: []string{"v4", "v6"},
				local: func(target string) (netip.Addr, error) {
					s, ok := tc.src[target]
					if !ok {
						return netip.Addr{}, errors.New("network is unreachable")
					}
					return netip.MustParseAddr(s), nil
				},
				ifaces:   func() ([]netInterface, error) { return ifs, nil },
				fallback: fb,
			}
			r, err := e.DefaultRoute()
			if !errors.Is(err, tc.err) || r.Index != tc.want {
				t.Fatalf("DefaultRoute = %+v, %v; want index %d, %v", r, err, tc.want, tc.err)
			}
		})
	}
}

func TestEgressRouteInterfacesFail(t *testing.T) {
	e := &EgressRoute{
		targets:  []string{"v4"},
		local:    func(string) (netip.Addr, error) { return netip.MustParseAddr("198.51.100.2"), nil },
		ifaces:   func() ([]netInterface, error) { return nil, errors.New("permission denied") },
		fallback: fixedRoute{r: diag.Route{Index: 2}},
	}
	if r, err := e.DefaultRoute(); err != nil || r.Index != 2 {
		t.Fatalf("DefaultRoute = %+v, %v; want the fallback", r, err)
	}
}

// The real lookup, toward loopback so it needs no internet: the source is
// 127.0.0.1 and it belongs to the loopback interface.
func TestEgressRouteRealLoopback(t *testing.T) {
	e := NewEgressRoute(fixedRoute{err: diag.ErrNoRoute})
	e.targets = []string{"127.0.0.1:9"}
	r, err := e.DefaultRoute()
	if err != nil {
		t.Fatalf("DefaultRoute: %v", err)
	}
	if r.Index <= 0 || !strings.Contains(strings.ToLower(r.Name), "lo") {
		t.Errorf("route to 127.0.0.1 = %+v, want the loopback interface", r)
	}
}
