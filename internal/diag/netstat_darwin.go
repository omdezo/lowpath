package diag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// bsdNetStats reads the routing sysctl: NET_RT_DUMP for the default route,
// NET_RT_IFLIST2 for the 64-bit interface counters (if_data64). macOS may
// hand apps that are not signed by Apple 32-bit-wrapping values in those
// fields; Rate copes with a 32-bit wrap.
type bsdNetStats struct{}

func newPlatformNetStats() NetStats { return bsdNetStats{} }

func (bsdNetStats) DefaultRoute() (Route, error) {
	for _, af := range []int{syscall.AF_INET, syscall.AF_INET6} {
		b, err := route.FetchRIB(af, route.RIBTypeRoute, 0)
		if err != nil {
			continue
		}
		msgs, err := route.ParseRIB(route.RIBTypeRoute, b)
		if err != nil {
			continue
		}
		for _, m := range msgs {
			rm, ok := m.(*route.RouteMessage)
			if !ok || rm.Flags&unix.RTF_UP == 0 || rm.Flags&unix.RTF_IFSCOPE != 0 || !isDefault(rm.Addrs) {
				continue
			}
			r := Route{Index: rm.Index, Kind: LinkUnknown}
			if ifc, err := net.InterfaceByIndex(rm.Index); err == nil {
				r.Name = ifc.Name
			}
			if len(rm.Addrs) > unix.RTAX_GATEWAY && rm.Flags&unix.RTF_GATEWAY != 0 {
				r.Gateway = routeAddr(rm.Addrs[unix.RTAX_GATEWAY])
			}
			return r, nil
		}
	}
	return Route{}, ErrNoRoute
}

func isDefault(addrs []route.Addr) bool {
	if len(addrs) <= unix.RTAX_DST {
		return false
	}
	if a := routeAddr(addrs[unix.RTAX_DST]); !a.IsValid() || !a.IsUnspecified() {
		return false
	}
	if len(addrs) > unix.RTAX_NETMASK && addrs[unix.RTAX_NETMASK] != nil {
		if m := routeAddr(addrs[unix.RTAX_NETMASK]); m.IsValid() && !m.IsUnspecified() {
			return false
		}
	}
	return true
}

func routeAddr(a route.Addr) netip.Addr {
	switch v := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(v.IP)
	case *route.Inet6Addr:
		return netip.AddrFrom16(v.IP)
	}
	return netip.Addr{}
}

func (bsdNetStats) Counters(index int) (Counters, error) {
	b, err := route.FetchRIB(syscall.AF_UNSPEC, route.RIBType(unix.NET_RT_IFLIST2), 0)
	if err != nil {
		return Counters{}, fmt.Errorf("diag: sysctl NET_RT_IFLIST2: %w", err)
	}
	var h unix.IfMsghdr2
	size := int(unsafe.Sizeof(h))
	for off := 0; off+4 <= len(b); {
		n := int(binary.NativeEndian.Uint16(b[off:]))
		if n < 4 || off+n > len(b) {
			break
		}
		if b[off+3] == unix.RTM_IFINFO2 && n >= size {
			// Copy into an aligned struct before reading the 64-bit fields.
			copy(unsafe.Slice((*byte)(unsafe.Pointer(&h)), size), b[off:off+size])
			if int(h.Index) == index {
				return Counters{
					At:        time.Now(),
					RxBytes:   h.Data.Ibytes,
					TxBytes:   h.Data.Obytes,
					RxLinkBps: h.Data.Baudrate,
					TxLinkBps: h.Data.Baudrate,
				}, nil
			}
		}
		off += n
	}
	return Counters{}, errors.New("diag: interface not found")
}
