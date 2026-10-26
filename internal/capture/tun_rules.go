package capture

import (
	"net/netip"

	"lowpath/internal/netlink"
	"lowpath/internal/netlink/nft"
	"lowpath/internal/netlink/rtnl"
)

// tunSetID names the direct-ports set inside the batch that creates it.
const tunSetID = 1

// tunRule is the policy-routing rule that sends marked packets to the TUN.
var tunRule = rtnl.Rule{Priority: tunRulePrio, Mark: tunMark, Mask: 0xffffffff, Table: tunTable}

// tunRuleset is the nftables transaction that (re)creates the table: it
// adds and deletes any table of that name first (a killed run's, which on
// kernels before 5.12 outlives it), so the result is always exactly this.
// With owned, the table goes away when the netlink socket that made it
// closes. The chain, in order:
//
//	meta mark != 0 return              already marked: a VPN's packets, our own resends
//	meta l4proto != udp return
//	ip frag-off & 0x3fff != 0 return   pieces of a datagram
//	ip daddr <prefix> return           per tunExcluded prefix
//	udp dport 53 return
//	ip daddr <relay> [udp dport <port>] return
//	udp sport @direct return           other programs' sockets
//	meta skuid <any> meta mark set <tunMark>
//
// The last rule needs a socket owned by a user: the skuid load stops the
// rule for kernel sockets (in-kernel tunnels), which then keep their route.
func tunRuleset(r tunRoute, owned bool) []netlink.Message {
	f := nft.IPv4
	var flags uint32
	if owned {
		flags = nft.TableOwner
	}
	msgs := []netlink.Message{
		nft.AddTable(f, tunNftTable, 0),
		nft.DelTable(f, tunNftTable),
		nft.AddTable(f, tunNftTable, flags),
		nft.AddChain(f, nft.Chain{
			Table: tunNftTable, Name: tunNftChain, Type: "route",
			Hook: nft.HookLocalOut, Priority: nft.PriorityMangle, Policy: nft.PolicyAccept,
		}),
		nft.AddSet(f, nft.Set{Table: tunNftTable, Name: tunNftSet, ID: tunSetID, KeyType: nft.TypeInetService, KeyLen: 2}),
	}
	if len(r.direct) > 0 {
		msgs = append(msgs, nft.AddElems(f, tunNftTable, tunNftSet, tunSetID, portKeys(r.direct)))
	}
	ret := nft.Verdict{Code: nft.VerdictReturn}
	rule := func(e ...nft.Expr) { msgs = append(msgs, nft.AddRule(f, tunNftTable, tunNftChain, e...)) }
	daddr := nft.Payload{Base: nft.BaseNetwork, Offset: 16, Len: 4, Reg: nft.Reg1}
	dport := nft.Payload{Base: nft.BaseTransport, Offset: 2, Len: 2, Reg: nft.Reg1}

	rule(nft.Meta{Key: nft.MetaMark, Reg: nft.Reg1}, nft.Cmp{Op: nft.CmpNeq, Reg: nft.Reg1, Data: nft.HostU32(0)}, ret)
	rule(nft.Meta{Key: nft.MetaL4Proto, Reg: nft.Reg1}, nft.Cmp{Op: nft.CmpNeq, Reg: nft.Reg1, Data: []byte{protoUDP}}, ret)
	rule(nft.Payload{Base: nft.BaseNetwork, Offset: 6, Len: 2, Reg: nft.Reg1},
		nft.Bitwise{Reg: nft.Reg1, Mask: []byte{0x3f, 0xff}, Xor: []byte{0, 0}},
		nft.Cmp{Op: nft.CmpNeq, Reg: nft.Reg1, Data: []byte{0, 0}}, ret)
	for _, p := range tunExcluded() {
		rule(daddr, nft.Bitwise{Reg: nft.Reg1, Mask: prefixMask(p), Xor: make([]byte, 4)},
			nft.Cmp{Op: nft.CmpEq, Reg: nft.Reg1, Data: p.Masked().Addr().AsSlice()}, ret)
	}
	rule(dport, nft.Cmp{Op: nft.CmpEq, Reg: nft.Reg1, Data: nft.Port(53)}, ret)
	for _, e := range tunExcludes(r.exclude) {
		ex := []nft.Expr{daddr, nft.Cmp{Op: nft.CmpEq, Reg: nft.Reg1, Data: e.Addr().AsSlice()}}
		if e.Port() != 0 {
			ex = append(ex, dport, nft.Cmp{Op: nft.CmpEq, Reg: nft.Reg1, Data: nft.Port(e.Port())})
		}
		rule(append(ex, ret)...)
	}
	rule(nft.Payload{Base: nft.BaseTransport, Offset: 0, Len: 2, Reg: nft.Reg1},
		nft.Lookup{Set: tunNftSet, SetID: tunSetID, Reg: nft.Reg1}, ret)
	rule(nft.Meta{Key: nft.MetaSkUID, Reg: nft.Reg1},
		nft.Immediate{Reg: nft.Reg2, Data: nft.HostU32(tunMark)},
		nft.Meta{Key: nft.MetaMark, Reg: nft.Reg2, Set: true})
	return nft.Batch(msgs...)
}

// tunDirectUpdate is the transaction that turns the direct set from have
// into want; nil when they are the same.
func tunDirectUpdate(have map[uint16]bool, want []uint16) []netlink.Message {
	var add, del []uint16
	in := map[uint16]bool{}
	for _, p := range want {
		in[p] = true
		if !have[p] {
			add = append(add, p)
		}
	}
	for p := range have {
		if !in[p] {
			del = append(del, p)
		}
	}
	var msgs []netlink.Message
	if len(del) > 0 {
		msgs = append(msgs, nft.DelElems(nft.IPv4, tunNftTable, tunNftSet, portKeys(del)))
	}
	if len(add) > 0 {
		msgs = append(msgs, nft.AddElems(nft.IPv4, tunNftTable, tunNftSet, 0, portKeys(add)))
	}
	if msgs == nil {
		return nil
	}
	return nft.Batch(msgs...)
}

func portKeys(ports []uint16) [][]byte {
	out := make([][]byte, len(ports))
	for i, p := range ports {
		out[i] = nft.Port(p)
	}
	return out
}

// prefixMask is the netmask of an IPv4 prefix, as 4 bytes.
func prefixMask(p netip.Prefix) []byte {
	m := make([]byte, 4)
	for i := range p.Bits() {
		m[i/8] |= 0x80 >> (i % 8)
	}
	return m
}
