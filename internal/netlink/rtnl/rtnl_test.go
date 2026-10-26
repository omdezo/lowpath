package rtnl

import (
	"bytes"
	"net/netip"
	"testing"

	"lowpath/internal/netlink"
)

func attrs(t *testing.T, m netlink.Message, hdr int) map[uint16][]byte {
	t.Helper()
	if len(m.Data) < hdr {
		t.Fatalf("%s: body %d bytes, header %d", m.Op, len(m.Data), hdr)
	}
	out := map[uint16][]byte{}
	for _, a := range netlink.ParseAttrs(m.Data[hdr:]) {
		out[a.Type] = a.Data
	}
	return out
}

func TestRuleRoundTrip(t *testing.T) {
	r := Rule{Priority: 5100, Mark: 0x4c50, Mask: 0xffffffff, Table: 0x4c50}
	add := AddRule(r)
	if add.Type != typeNewRule || add.Flags&netlink.FlagExcl == 0 || add.Data[7] != frActToTbl || add.Data[4] != 0 {
		t.Fatalf("AddRule = %+v", add)
	}
	got, ok := ParseRule(netlink.Reply{Type: typeNewRule, Data: add.Data})
	if !ok || got != r {
		t.Fatalf("ParseRule = %+v %v, want %+v", got, ok, r)
	}
	if del := DelRule(r); del.Type != typeDelRule || !bytes.Equal(del.Data, add.Data) {
		t.Fatalf("DelRule = %+v", del)
	}
	// A small table id goes in the header too, as ip(8) sends it.
	if small := AddRule(Rule{Priority: 1, Table: 100}); small.Data[4] != 100 {
		t.Fatalf("table in header = %d", small.Data[4])
	}
	if _, ok := ParseRule(netlink.Reply{Type: typeNewRule, Data: append([]byte{10}, add.Data[1:]...)}); ok {
		t.Error("an IPv6 rule parsed as IPv4")
	}
	// The dump asks for IPv4 rules only, not every family's.
	if l := ListRules(); l.Type != typeGetRule || len(l.Data) != 12 || l.Data[0] != afInet {
		t.Errorf("ListRules = %+v", l)
	}
}

func TestRoute(t *testing.T) {
	m, err := AddRoute(Route{Table: 0x4c50, Dst: netip.MustParsePrefix("0.0.0.0/0"), OIF: 7})
	if err != nil {
		t.Fatal(err)
	}
	if m.Flags&netlink.FlagReplace == 0 || m.Data[1] != 0 || m.Data[6] != rtScopeLink || m.Data[4] != rtTableUnspec {
		t.Fatalf("default dev route header % x", m.Data[:12])
	}
	a := attrs(t, m, 12)
	if _, ok := a[rtaDst]; ok {
		t.Error("a default route carries RTA_DST")
	}
	if ne.Uint32(a[rtaTable]) != 0x4c50 || ne.Uint32(a[rtaOIF]) != 7 {
		t.Errorf("attrs %v", a)
	}
	m, err = AddRoute(Route{Table: 254, Dst: netip.MustParsePrefix("198.51.100.0/24"), Gateway: netip.MustParseAddr("203.0.113.1")})
	if err != nil {
		t.Fatal(err)
	}
	a = attrs(t, m, 12)
	if m.Data[1] != 24 || m.Data[6] != rtScopeUniverse || m.Data[4] != 254 ||
		!bytes.Equal(a[rtaDst], []byte{198, 51, 100, 0}) || !bytes.Equal(a[rtaGateway], []byte{203, 0, 113, 1}) {
		t.Errorf("gateway route % x, attrs %v", m.Data[:12], a)
	}
	if _, err := AddRoute(Route{Dst: netip.MustParsePrefix("2001:db8::/32")}); err == nil {
		t.Error("an IPv6 route was accepted")
	}
}

func TestLinkAndAddr(t *testing.T) {
	m := LinkUp(9, 65535)
	if ne.Uint32(m.Data[4:]) != 9 || ne.Uint32(m.Data[8:]) != iffUp || ne.Uint32(m.Data[12:]) != iffUp {
		t.Fatalf("LinkUp header % x", m.Data[:16])
	}
	if ne.Uint32(attrs(t, m, 16)[iflaMTU]) != 65535 {
		t.Error("no MTU")
	}
	if len(LinkUp(9, 0).Data) != 16 {
		t.Error("MTU 0 should leave it alone")
	}
	a, err := AddAddr(9, netip.MustParsePrefix("169.254.76.80/32"), ScopeLink)
	if err != nil {
		t.Fatal(err)
	}
	if a.Data[1] != 32 || a.Data[3] != rtScopeLink || ne.Uint32(a.Data[4:]) != 9 ||
		!bytes.Equal(attrs(t, a, 8)[ifaLocal], []byte{169, 254, 76, 80}) {
		t.Errorf("AddAddr % x", a.Data)
	}
	if _, err := AddAddr(9, netip.MustParsePrefix("2001:db8::1/64"), ScopeLink); err == nil {
		t.Error("an IPv6 address was accepted")
	}
	v := NewVeth("lpa", "lpb", 4242)
	li := attrs(t, v, 16)
	if string(li[iflaIfname]) != "lpa\x00" {
		t.Errorf("veth name %q", li[iflaIfname])
	}
	info := map[uint16][]byte{}
	for _, x := range netlink.ParseAttrs(li[iflaLinkinfo]) {
		info[x.Type] = x.Data
	}
	peer := netlink.ParseAttrs(netlink.ParseAttrs(info[iflaInfoData])[0].Data[16:])
	if string(info[iflaInfoKind]) != "veth\x00" || len(peer) != 2 || string(peer[0].Data) != "lpb\x00" || ne.Uint32(peer[1].Data) != 4242 {
		t.Errorf("veth linkinfo %v, peer %+v", info, peer)
	}
}
