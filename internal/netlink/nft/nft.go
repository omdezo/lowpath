// Package nft builds nftables requests: tables, base chains, sets, set
// elements and rules made of expressions, sent as one transaction (batch).
// It is the part of `nft` this program needs, with the layouts of
// linux/netfilter/nf_tables.h and linux/netfilter/nfnetlink.h. nftables
// puts its numbers in network byte order; register contents keep the byte
// order of what was loaded (packet bytes as on the wire, the packet mark in
// host order).
package nft

import (
	"encoding/binary"

	"lowpath/internal/netlink"
)

// Family is the nftables family of a table.
type Family uint8

// IPv4 is the "ip" family.
const IPv4 Family = 2

const (
	subsysNftables = 10
	batchBegin     = 0x10 // NFNL_MSG_BATCH_BEGIN
	batchEnd       = 0x11

	msgNewTable   = 0
	msgDelTable   = 2
	msgNewChain   = 3
	msgNewRule    = 6
	msgNewSet     = 9
	msgNewSetElem = 12
	msgDelSetElem = 14

	tableName  = 1
	tableFlags = 2

	chainTable  = 1
	chainName   = 3
	chainHook   = 4
	chainPolicy = 5
	chainType   = 7

	hookNum      = 1
	hookPriority = 2

	setTable   = 1
	setName    = 2
	setFlags   = 3
	setKeyType = 4
	setKeyLen  = 5
	setID      = 10

	elemListTable    = 1
	elemListSet      = 2
	elemListElements = 3
	elemListSetID    = 4
	elemKey          = 1

	ruleTable       = 1
	ruleChain       = 2
	ruleExpressions = 4

	listElem = 1
	exprName = 1
	exprData = 2

	dataValue   = 1
	dataVerdict = 2

	verdictCode = 1
)

// Table flags.
const (
	// TableOwner ties the table to the netlink socket that created it: the
	// kernel deletes it when the socket closes, including when the process
	// is killed (Linux 5.12+). Only that socket may change it.
	TableOwner = 0x2
)

// Hooks and policies for base chains.
const (
	HookLocalOut = 3 // NF_INET_LOCAL_OUT
	PolicyAccept = 1 // NF_ACCEPT
	// PriorityMangle is where `nft` puts "priority mangle".
	PriorityMangle = -150
)

func msg(t uint8, f Family, flags uint16, op string, a *netlink.Attrs) netlink.Message {
	// struct nfgenmsg: family, version (NFNETLINK_V0), resource id.
	data := append([]byte{byte(f), 0, 0, 0}, a.Bytes()...)
	return netlink.Message{Type: subsysNftables<<8 | uint16(t), Flags: flags, Op: op, Data: data}
}

// Batch wraps msgs in a transaction: the kernel applies all of them or, if
// one fails, none.
func Batch(msgs ...netlink.Message) []netlink.Message {
	// The begin and end messages carry the subsystem in the resource id,
	// big-endian.
	mark := func(t uint16) netlink.Message {
		// A refusal of the whole batch comes as an error for its header.
		return netlink.Message{Type: t, Data: []byte{0, 0, 0, subsysNftables}, Op: "nftables transaction"}
	}
	out := make([]netlink.Message, 0, len(msgs)+2)
	out = append(out, mark(batchBegin))
	out = append(out, msgs...)
	return append(out, mark(batchEnd))
}

// AddTable adds a table (no error when it exists).
func AddTable(f Family, name string, flags uint32) netlink.Message {
	var a netlink.Attrs
	a.String(tableName, name)
	if flags != 0 {
		a.BE32(tableFlags, flags)
	}
	return msg(msgNewTable, f, netlink.FlagAck|netlink.FlagCreate, "add table "+name, &a)
}

// DelTable deletes a table and everything in it; ENOENT when there is none.
func DelTable(f Family, name string) netlink.Message {
	var a netlink.Attrs
	a.String(tableName, name)
	return msg(msgDelTable, f, netlink.FlagAck, "delete table "+name, &a)
}

// Chain is a base chain: one attached to a netfilter hook.
type Chain struct {
	Table, Name string
	Type        string // "filter", "route", "nat"
	Hook        uint32
	Priority    int32
	Policy      uint32
}

// AddChain adds a base chain.
func AddChain(f Family, c Chain) netlink.Message {
	var a netlink.Attrs
	a.String(chainTable, c.Table)
	a.String(chainName, c.Name)
	a.Nested(chainHook, func(h *netlink.Attrs) {
		h.BE32(hookNum, c.Hook)
		h.BE32(hookPriority, uint32(c.Priority))
	})
	a.BE32(chainPolicy, c.Policy)
	a.String(chainType, c.Type)
	return msg(msgNewChain, f, netlink.FlagAck|netlink.FlagCreate, "add chain "+c.Name, &a)
}

// Set is a named set of fixed-size keys.
type Set struct {
	Table, Name string
	// ID names the set within one batch, so a rule added in the same batch
	// can refer to it.
	ID uint32
	// KeyType is nft's data type number, for `nft list` to print the keys
	// (TypeInetService); the kernel only uses KeyLen.
	KeyType uint32
	KeyLen  uint32
}

// TypeInetService is nft's "inet_service" (a port, 2 bytes, big-endian).
const TypeInetService = 13

// AddSet adds a set.
func AddSet(f Family, s Set) netlink.Message {
	var a netlink.Attrs
	a.String(setTable, s.Table)
	a.String(setName, s.Name)
	a.BE32(setFlags, 0)
	a.BE32(setKeyType, s.KeyType)
	a.BE32(setKeyLen, s.KeyLen)
	a.BE32(setID, s.ID)
	return msg(msgNewSet, f, netlink.FlagAck|netlink.FlagCreate, "add set "+s.Name, &a)
}

// AddElems adds keys to a set (setID: when the set is new in this batch).
func AddElems(f Family, table, set string, setID uint32, keys [][]byte) netlink.Message {
	return elems(msgNewSetElem, netlink.FlagAck|netlink.FlagCreate, "add to set "+set, f, table, set, setID, keys)
}

// DelElems removes keys from a set; ENOENT when one isn't in it.
func DelElems(f Family, table, set string, keys [][]byte) netlink.Message {
	return elems(msgDelSetElem, netlink.FlagAck, "remove from set "+set, f, table, set, 0, keys)
}

func elems(t uint8, flags uint16, op string, f Family, table, set string, setID uint32, keys [][]byte) netlink.Message {
	var a netlink.Attrs
	a.String(elemListTable, table)
	a.String(elemListSet, set)
	if setID != 0 {
		a.BE32(elemListSetID, setID)
	}
	a.Nested(elemListElements, func(l *netlink.Attrs) {
		for _, k := range keys {
			l.Nested(listElem, func(e *netlink.Attrs) {
				e.Nested(elemKey, func(d *netlink.Attrs) { d.Add(dataValue, k) })
			})
		}
	})
	return msg(t, f, flags, op, &a)
}

// AddRule appends a rule to a chain.
func AddRule(f Family, table, chain string, exprs ...Expr) netlink.Message {
	var a netlink.Attrs
	a.String(ruleTable, table)
	a.String(ruleChain, chain)
	a.Nested(ruleExpressions, func(l *netlink.Attrs) {
		for _, e := range exprs {
			l.Nested(listElem, func(el *netlink.Attrs) {
				el.String(exprName, e.name())
				el.Nested(exprData, e.encode)
			})
		}
	})
	return msg(msgNewRule, f, netlink.FlagAck|netlink.FlagCreate|netlink.FlagAppend, "add rule to "+chain, &a)
}

// Port is a port as a set key or comparison value.
func Port(p uint16) []byte { return binary.BigEndian.AppendUint16(nil, p) }

// HostU32 is a 32-bit value as the kernel keeps it in a register when it is
// not packet data (the packet mark, a socket's uid).
func HostU32(v uint32) []byte { return binary.NativeEndian.AppendUint32(nil, v) }
