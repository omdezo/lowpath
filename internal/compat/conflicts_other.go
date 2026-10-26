//go:build !windows

package compat

import (
	"errors"
	"fmt"
	"net"
)

// unixSystem is the Linux and macOS System. The boosters and packet filter
// drivers Lowpath looks for are Windows programs, so only processes (a
// booster run under Wine still shows up by name), network adapters and the
// internet route are read; services and installed programs report
// errors.ErrUnsupported and are skipped.
type unixSystem struct {
	procs ProcessLister
	route RouteFinder
}

// NewSystem returns this OS's System: running processes from procs, and the
// internet route from the kernel's choice of source address toward the
// internet (EgressRoute), with route as the fallback. Reading the 0/0 route
// alone misses OpenVPN's def1 routes and wg-quick's policy routing.
func NewSystem(procs ProcessLister, route RouteFinder) System {
	return unixSystem{procs: procs, route: NewEgressRoute(route)}
}

func (u unixSystem) Processes() ([]string, error) { return processNames(u.procs), nil }

func (u unixSystem) DefaultRoute() (int, error) { return routeIndex(u.route) }

func (unixSystem) Services() ([]Service, error) {
	return nil, fmt.Errorf("compat: no Windows services here: %w", errors.ErrUnsupported)
}

func (unixSystem) Programs() ([]string, error) {
	return nil, fmt.Errorf("compat: no Windows program list here: %w", errors.ErrUnsupported)
}

// Adapters lists the interfaces by name (tun0, wg0, utun3, ...); the kernel
// gives no driver description or IANA type through this call.
func (unixSystem) Adapters() ([]Adapter, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list network adapters: %w", err)
	}
	out := make([]Adapter, 0, len(ifs))
	for _, i := range ifs {
		out = append(out, Adapter{Index: i.Index, Name: i.Name, Up: i.Flags&net.FlagUp != 0})
	}
	return out, nil
}
