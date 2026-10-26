package diag

import (
	"errors"
	"fmt"
	"net/netip"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// winNetStats uses the IP Helper API, which needs no administrator rights:
// GetBestInterfaceEx for the interface Windows would use to reach the
// internet, GetIpForwardTable2 for its gateway, GetIfEntry2Ex for the 64-bit
// byte counters.
type winNetStats struct{}

func newPlatformNetStats() NetStats { return winNetStats{} }

// ndisPhysicalMediumNative80211 is NDIS_PHYSICAL_MEDIUM's Wi-Fi value.
const ndisPhysicalMediumNative80211 = 9

func (winNetStats) DefaultRoute() (Route, error) {
	// Well-known public addresses (1.1.1.1, 2606:4700:4700::1111) only ask
	// the routing table which interface would carry internet traffic;
	// nothing is sent to them.
	v4 := [4]byte{1, 1, 1, 1}
	v6 := [16]byte{0x26, 0x06, 0x47, 0x00, 0x47, 0x00, 14: 0x11, 15: 0x11}
	var idx uint32
	if err := windows.GetBestInterfaceEx(&windows.SockaddrInet4{Addr: v4}, &idx); err != nil {
		if err6 := windows.GetBestInterfaceEx(&windows.SockaddrInet6{Addr: v6}, &idx); err6 != nil {
			return Route{}, ErrNoRoute
		}
	}
	r := Route{Index: int(idx), Kind: LinkUnknown}
	row := windows.MibIfRow2{InterfaceIndex: idx}
	if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormalWithoutStatistics, &row); err == nil {
		r.Name = windows.UTF16ToString(row.Alias[:])
		r.Kind = linkKind(row)
	}
	r.Gateway = defaultGateway(idx)
	return r, nil
}

func linkKind(row windows.MibIfRow2) string {
	switch {
	case row.Type == windows.IF_TYPE_IEEE80211 || row.PhysicalMediumType == ndisPhysicalMediumNative80211:
		return LinkWiFi
	case row.Type == windows.IF_TYPE_ETHERNET_CSMACD:
		return LinkEthernet
	default:
		return LinkOther
	}
}

// defaultGateway is the next hop of the lowest-metric default route (0.0.0.0/0
// first, then ::/0) on interface idx, or the zero Addr.
func defaultGateway(idx uint32) netip.Addr {
	var tab *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(windows.AF_UNSPEC, &tab); err != nil {
		return netip.Addr{}
	}
	defer windows.FreeMibTable(unsafe.Pointer(tab))
	var best netip.Addr
	bestMetric := ^uint32(0)
	for _, row := range tab.Rows() {
		if row.InterfaceIndex != idx || row.DestinationPrefix.PrefixLength != 0 {
			continue
		}
		hop, ok := sockaddrInetAddr(&row.NextHop)
		if !ok || hop.IsUnspecified() {
			continue
		}
		// IPv4 wins over IPv6: ICMP to the gateway is IPv4 only for now.
		if best.IsValid() && best.Is4() && !hop.Is4() {
			continue
		}
		if !best.IsValid() || (hop.Is4() && !best.Is4()) || row.Metric < bestMetric {
			best, bestMetric = hop, row.Metric
		}
	}
	return best
}

func sockaddrInetAddr(sa *windows.RawSockaddrInet) (netip.Addr, bool) {
	switch sa.Family {
	case windows.AF_INET:
		v4 := (*windows.RawSockaddrInet4)(unsafe.Pointer(sa))
		return netip.AddrFrom4(v4.Addr), true
	case windows.AF_INET6:
		v6 := (*windows.RawSockaddrInet6)(unsafe.Pointer(sa))
		return netip.AddrFrom16(v6.Addr), true
	}
	return netip.Addr{}, false
}

func (winNetStats) Counters(index int) (Counters, error) {
	if index <= 0 {
		return Counters{}, errors.New("diag: bad interface index")
	}
	row := windows.MibIfRow2{InterfaceIndex: uint32(index)}
	if err := windows.GetIfEntry2Ex(windows.MibIfEntryNormal, &row); err != nil {
		return Counters{}, fmt.Errorf("diag: GetIfEntry2Ex(%d): %w", index, err)
	}
	return Counters{
		At:        time.Now(),
		RxBytes:   row.InOctets,
		TxBytes:   row.OutOctets,
		RxLinkBps: row.ReceiveLinkSpeed,
		TxLinkBps: row.TransmitLinkSpeed,
	}, nil
}
