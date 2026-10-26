package detect

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// kernelNetworkData builds the user data of a Kernel-Network flow event the
// way the manifest lays it out (PID, size, daddr, saddr, dport, sport,
// then fields the parser doesn't read).
func kernelNetworkData(pid, size uint32, d, s netip.AddrPort, tail int) []byte {
	b := binary.LittleEndian.AppendUint32(nil, pid)
	b = binary.LittleEndian.AppendUint32(b, size)
	b = append(b, d.Addr().AsSlice()...)
	b = append(b, s.Addr().AsSlice()...)
	b = binary.BigEndian.AppendUint16(b, d.Port())
	b = binary.BigEndian.AppendUint16(b, s.Port())
	return append(b, make([]byte, tail)...)
}

func TestParseKernelNetwork(t *testing.T) {
	srv4 := netip.MustParseAddrPort("203.0.113.40:7777")
	loc4 := netip.MustParseAddrPort("192.168.1.20:50123")
	srv6 := netip.MustParseAddrPort("[2001:db8::40]:7777")
	loc6 := netip.MustParseAddrPort("[2001:db8:1::20]:50123")
	cases := []struct {
		id       uint16
		d, s     netip.AddrPort
		tail     int
		proto    string
		outbound bool
	}{
		{knUDPSent4, srv4, loc4, 8, "udp", true},
		{knUDPRecv4, srv4, loc4, 8, "udp", false},
		{knUDPSent6, srv6, loc6, 8, "udp", true},
		{knUDPRecv6, srv6, loc6, 8, "udp", false},
		{knTCPSent4, srv4, loc4, 16, "tcp", true}, // startime and endtime come before seqnum
		{knTCPRecv4, srv4, loc4, 8, "tcp", false},
		{knTCPSent6, srv6, loc6, 16, "tcp", true},
		{knTCPRecv6, srv6, loc6, 8, "tcp", false},
	}
	for _, c := range cases {
		ev, ok := parseKernelNetwork(c.id, kernelNetworkData(4242, 120, c.d, c.s, c.tail))
		if !ok {
			t.Fatalf("event %d not parsed", c.id)
		}
		want := FlowEvent{PID: 4242, Proto: c.proto, Local: c.s, Remote: c.d, Bytes: 120, Outbound: c.outbound}
		if ev != want {
			t.Errorf("event %d: got %+v, want %+v", c.id, ev, want)
		}
	}
}

func TestParseKernelNetworkRejects(t *testing.T) {
	d := netip.MustParseAddrPort("203.0.113.40:7777")
	s := netip.MustParseAddrPort("192.168.1.20:50123")
	full := kernelNetworkData(1, 1, d, s, 8)
	if _, ok := parseKernelNetwork(12, full); ok {
		t.Error("a TCP connect attempt is not a flow event")
	}
	if _, ok := parseKernelNetwork(knUDPSent4, full[:19]); ok {
		t.Error("truncated data must be rejected")
	}
	// IPv4 data under an IPv6 event id is too short for two 16-byte addresses.
	if _, ok := parseKernelNetwork(knUDPSent6, full); ok {
		t.Error("IPv4-sized data must not parse as IPv6")
	}
	// An IPv4-mapped IPv6 address comes out as plain IPv4.
	m := netip.AddrPortFrom(netip.AddrFrom16(d.Addr().As16()), d.Port())
	ev, ok := parseKernelNetwork(knUDPSent6, kernelNetworkData(1, 1, m, netip.MustParseAddrPort("[2001:db8::1]:1"), 0))
	if !ok || ev.Remote != d {
		t.Errorf("mapped address: %+v, %v", ev.Remote, ok)
	}
}
