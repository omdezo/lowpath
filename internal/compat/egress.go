package compat

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"lowpath/internal/diag"
)

// egressTargets are the public addresses EgressRoute asks the kernel about
// (the same ones diag asks GetBestInterfaceEx about on Windows). Nothing is
// sent to them: connecting a UDP socket only picks the route and the source
// address.
var egressTargets = []string{"1.1.1.1:53", "[2606:4700:4700::1111]:53"}

// netInterface is one interface and its addresses, as EgressRoute matches
// them.
type netInterface struct {
	Index int
	Name  string
	Addrs []netip.Addr
}

// EgressRoute finds the interface that really carries internet traffic by
// asking the kernel which source address it would use toward a public
// address, then which interface owns that address. Unlike reading the
// 0.0.0.0/0 route, this follows what a VPN does to take the traffic over:
// OpenVPN's redirect-gateway def1 and wg-quick on macOS add 0.0.0.0/1 and
// 128.0.0.0/1 and leave the default route alone; wg-quick on Linux puts its
// default route in its own table behind an ip rule, which /proc/net/route
// never shows. When the source address can't be matched to an interface,
// the fallback (the plain default route) answers.
type EgressRoute struct {
	targets  []string
	local    func(target string) (netip.Addr, error)
	ifaces   func() ([]netInterface, error)
	fallback RouteFinder
}

// NewEgressRoute returns an EgressRoute that falls back to fallback.
func NewEgressRoute(fallback RouteFinder) *EgressRoute {
	return &EgressRoute{targets: egressTargets, local: udpSource, ifaces: systemInterfaces, fallback: fallback}
}

// DefaultRoute returns the interface the kernel would send internet traffic
// out of, or an error matching diag.ErrNoRoute when no target is reachable
// (offline).
func (e *EgressRoute) DefaultRoute() (diag.Route, error) {
	var srcs []netip.Addr
	for _, t := range e.targets {
		if a, err := e.local(t); err == nil && a.IsValid() && !a.IsUnspecified() {
			srcs = append(srcs, a.Unmap())
		}
	}
	if len(srcs) == 0 {
		// No route to any target: offline, or the kernel refused. The
		// default route reader tells the two apart.
		return e.fallback.DefaultRoute()
	}
	ifs, err := e.ifaces()
	if err != nil {
		return e.fallback.DefaultRoute()
	}
	for _, src := range srcs {
		for _, i := range ifs {
			for _, a := range i.Addrs {
				if a.Unmap() == src {
					return diag.Route{Index: i.Index, Name: i.Name, Kind: diag.LinkUnknown}, nil
				}
			}
		}
	}
	return e.fallback.DefaultRoute()
}

// udpSource is the source address the kernel picks for target. Connecting a
// UDP socket sends no packet.
func udpSource(target string) (netip.Addr, error) {
	c, err := net.Dial("udp", target)
	if err != nil {
		return netip.Addr{}, err
	}
	defer c.Close()
	ua, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok {
		return netip.Addr{}, fmt.Errorf("compat: local address %v", c.LocalAddr())
	}
	a, ok := netip.AddrFromSlice(ua.IP)
	if !ok {
		return netip.Addr{}, errors.New("compat: no local address")
	}
	return a, nil
}

// systemInterfaces lists the interfaces that are up with their addresses.
func systemInterfaces() ([]netInterface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]netInterface, 0, len(ifs))
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		ni := netInterface{Index: i.Index, Name: i.Name}
		for _, a := range addrs {
			if p, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(p.IP); ok {
					ni.Addrs = append(ni.Addrs, ip.Unmap())
				}
			}
		}
		out = append(out, ni)
	}
	return out, nil
}
