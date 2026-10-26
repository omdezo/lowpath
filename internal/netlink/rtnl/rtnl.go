// Package rtnl builds rtnetlink requests: bring a link up, add addresses and
// routes, add and remove policy-routing rules (what `ip link`, `ip addr`,
// `ip route` and `ip rule` send). The layouts are linux/rtnetlink.h,
// linux/if_link.h and linux/fib_rules.h; numbers are in host byte order.
package rtnl

import (
	"encoding/binary"
	"fmt"
	"net/netip"

	"lowpath/internal/netlink"
)

// Message types.
const (
	typeNewLink  = 16
	typeDelLink  = 17
	typeNewAddr  = 20
	typeNewRoute = 24
	typeDelRoute = 25
	typeNewRule  = 32
	typeDelRule  = 33
	typeGetRule  = 34
)

const (
	afUnspec = 0
	afInet   = 2

	iffUp = 0x1

	iflaIfname   = 3
	iflaMTU      = 4
	iflaLinkinfo = 18
	iflaNetNSPid = 19
	iflaInfoKind = 1
	iflaInfoData = 2
	vethInfoPeer = 1

	ifaAddress = 1
	ifaLocal   = 2

	rtaDst     = 1
	rtaOIF     = 4
	rtaGateway = 5
	rtaTable   = 15

	rtProtStatic    = 4
	rtScopeUniverse = 0
	rtScopeLink     = 253
	rtnUnicast      = 1
	rtTableUnspec   = 0

	fraPriority = 6
	fraFwmark   = 10
	fraTable    = 15
	fraFwmask   = 16
	frActToTbl  = 1
)

var ne = binary.NativeEndian

// ifinfomsg is struct ifinfomsg: family, type, index, flags, change.
func ifinfomsg(index int32, flags, change uint32) []byte {
	b := []byte{afUnspec, 0, 0, 0}
	b = ne.AppendUint32(b, uint32(index))
	b = ne.AppendUint32(b, flags)
	return ne.AppendUint32(b, change)
}

// LinkUp brings the link with this index up and sets its MTU (0 = leave it).
func LinkUp(index int, mtu uint32) netlink.Message {
	var a netlink.Attrs
	if mtu > 0 {
		a.U32(iflaMTU, mtu)
	}
	return netlink.Message{
		Type: typeNewLink, Flags: netlink.FlagAck, Op: "bring the link up",
		Data: append(ifinfomsg(int32(index), iffUp, iffUp), a.Bytes()...),
	}
}

// DelLink removes the link with this index.
func DelLink(index int) netlink.Message {
	return netlink.Message{Type: typeDelLink, Flags: netlink.FlagAck, Op: "delete a link", Data: ifinfomsg(int32(index), 0, 0)}
}

// NewVeth creates a veth pair: name here, peer in the network namespace of
// the process peerNSPid (0 = here too).
func NewVeth(name, peer string, peerNSPid int) netlink.Message {
	var a netlink.Attrs
	a.String(iflaIfname, name)
	a.Nested(iflaLinkinfo, func(li *netlink.Attrs) {
		li.String(iflaInfoKind, "veth")
		li.Nested(iflaInfoData, func(d *netlink.Attrs) {
			var p netlink.Attrs
			p.String(iflaIfname, peer)
			if peerNSPid > 0 {
				p.U32(iflaNetNSPid, uint32(peerNSPid))
			}
			d.Add(vethInfoPeer, append(ifinfomsg(0, 0, 0), p.Bytes()...))
		})
	})
	return netlink.Message{
		Type: typeNewLink, Flags: netlink.FlagAck | netlink.FlagCreate | netlink.FlagExcl, Op: "create a veth pair",
		Data: append(ifinfomsg(0, 0, 0), a.Bytes()...),
	}
}

// Address scopes.
const (
	ScopeUniverse = rtScopeUniverse
	ScopeLink     = rtScopeLink
)

// AddAddr assigns an IPv4 address to a link.
func AddAddr(index int, p netip.Prefix, scope uint8) (netlink.Message, error) {
	if !p.Addr().Is4() {
		return netlink.Message{}, fmt.Errorf("rtnl: %v is not IPv4", p)
	}
	b := []byte{afInet, byte(p.Bits()), 0, scope}
	b = ne.AppendUint32(b, uint32(index))
	ip := p.Addr().AsSlice()
	var a netlink.Attrs
	a.Add(ifaLocal, ip)
	a.Add(ifaAddress, ip)
	return netlink.Message{
		Type: typeNewAddr, Flags: netlink.FlagAck | netlink.FlagCreate | netlink.FlagExcl, Op: "add an address",
		Data: append(b, a.Bytes()...),
	}, nil
}

// Route is an IPv4 route.
type Route struct {
	Table   uint32
	Dst     netip.Prefix // 0.0.0.0/0 for a default route
	Gateway netip.Addr   // zero: the destination is on the link
	OIF     int          // output interface index
}

func (r Route) message(typ uint16, flags uint16, op string) (netlink.Message, error) {
	if !r.Dst.Addr().Is4() || (r.Gateway.IsValid() && !r.Gateway.Is4()) {
		return netlink.Message{}, fmt.Errorf("rtnl: route %v via %v is not IPv4", r.Dst, r.Gateway)
	}
	scope := byte(rtScopeLink)
	if r.Gateway.IsValid() {
		scope = rtScopeUniverse
	}
	tbl := byte(rtTableUnspec)
	if r.Table < 256 {
		tbl = byte(r.Table)
	}
	b := []byte{afInet, byte(r.Dst.Bits()), 0, 0, tbl, rtProtStatic, scope, rtnUnicast}
	b = ne.AppendUint32(b, 0)
	var a netlink.Attrs
	if r.Dst.Bits() > 0 {
		a.Add(rtaDst, r.Dst.Masked().Addr().AsSlice())
	}
	if r.Gateway.IsValid() {
		a.Add(rtaGateway, r.Gateway.AsSlice())
	}
	if r.OIF > 0 {
		a.U32(rtaOIF, uint32(r.OIF))
	}
	a.U32(rtaTable, r.Table)
	return netlink.Message{Type: typ, Flags: flags, Op: op, Data: append(b, a.Bytes()...)}, nil
}

// AddRoute adds r, replacing a route to the same destination in that table.
func AddRoute(r Route) (netlink.Message, error) {
	return r.message(typeNewRoute, netlink.FlagAck|netlink.FlagCreate|netlink.FlagReplace, "add a route")
}

// DelRoute removes r.
func DelRoute(r Route) (netlink.Message, error) {
	return r.message(typeDelRoute, netlink.FlagAck, "delete a route")
}

// Rule is an IPv4 policy-routing rule "fwmark Mark/Mask lookup Table" at
// Priority, the only kind this program uses.
type Rule struct {
	Priority uint32
	Mark     uint32
	Mask     uint32
	Table    uint32
}

func (r Rule) message(typ uint16, flags uint16, op string) netlink.Message {
	tbl := byte(rtTableUnspec)
	if r.Table < 256 {
		tbl = byte(r.Table)
	}
	b := []byte{afInet, 0, 0, 0, tbl, 0, 0, frActToTbl}
	b = ne.AppendUint32(b, 0)
	var a netlink.Attrs
	a.U32(fraPriority, r.Priority)
	a.U32(fraFwmark, r.Mark)
	a.U32(fraFwmask, r.Mask)
	a.U32(fraTable, r.Table)
	return netlink.Message{Type: typ, Flags: flags, Op: op, Data: append(b, a.Bytes()...)}
}

// AddRule adds r; it fails with EEXIST when the same rule is there.
func AddRule(r Rule) netlink.Message {
	return r.message(typeNewRule, netlink.FlagAck|netlink.FlagCreate|netlink.FlagExcl, "add a routing rule")
}

// DelRule removes one rule matching r; ENOENT when there is none.
func DelRule(r Rule) netlink.Message {
	return r.message(typeDelRule, netlink.FlagAck, "delete a routing rule")
}

// ListRules is the dump request for every IPv4 rule; ParseRule reads the
// answers.
func ListRules() netlink.Message {
	return netlink.Message{Type: typeGetRule, Op: "list routing rules", Data: make([]byte, 12)}
}

// ParseRule decodes one rule of a ListRules answer. ok is false for a
// message that isn't an IPv4 rule.
func ParseRule(r netlink.Reply) (rule Rule, ok bool) {
	if r.Type != typeNewRule || len(r.Data) < 12 || r.Data[0] != afInet {
		return Rule{}, false
	}
	rule.Table = uint32(r.Data[4])
	for _, a := range netlink.ParseAttrs(r.Data[12:]) {
		if len(a.Data) < 4 {
			continue
		}
		v := ne.Uint32(a.Data)
		switch a.Type {
		case fraPriority:
			rule.Priority = v
		case fraFwmark:
			rule.Mark = v
		case fraFwmask:
			rule.Mask = v
		case fraTable:
			rule.Table = v
		}
	}
	return rule, true
}
