package nft

import "lowpath/internal/netlink"

// Expr is one step of a rule. A rule runs its expressions in order and stops
// at the first one that doesn't match (or can't run, such as a socket
// lookup for a packet without a socket); the rest then don't happen.
type Expr interface {
	name() string
	encode(*netlink.Attrs)
}

// Registers. Reg1 and Reg2 are the 16-byte registers; a 4-byte or shorter
// value fits in one.
const (
	RegVerdict = 0
	Reg1       = 1
	Reg2       = 2
)

// Meta keys.
const (
	MetaMark    = 3
	MetaSkUID   = 10
	MetaL4Proto = 16
)

// Comparison operators.
const (
	CmpEq  = 0
	CmpNeq = 1
)

// Payload bases.
const (
	BaseNetwork   = 1
	BaseTransport = 2
)

// Verdicts.
const (
	VerdictReturn = -5
	VerdictAccept = 1
)

const (
	metaDreg = 1
	metaKey  = 2
	metaSreg = 3

	cmpSreg = 1
	cmpOp   = 2
	cmpData = 3

	payloadDreg   = 1
	payloadBase   = 2
	payloadOffset = 3
	payloadLen    = 4

	bitwiseSreg = 1
	bitwiseDreg = 2
	bitwiseLen  = 3
	bitwiseMask = 4
	bitwiseXor  = 5

	lookupSet   = 1
	lookupSreg  = 2
	lookupSetID = 4
	lookupFlags = 5
	lookupInv   = 1

	immDreg = 1
	immData = 2
)

// Meta loads packet metadata (Key) into Reg, or with Set, sets it from Reg.
type Meta struct {
	Key uint32
	Reg uint32
	Set bool
}

func (Meta) name() string { return "meta" }
func (m Meta) encode(a *netlink.Attrs) {
	if m.Set {
		a.BE32(metaSreg, m.Reg)
	} else {
		a.BE32(metaDreg, m.Reg)
	}
	a.BE32(metaKey, m.Key)
}

// Cmp compares Reg with Data; the rule stops when the comparison is false.
type Cmp struct {
	Op   uint32
	Reg  uint32
	Data []byte
}

func (Cmp) name() string { return "cmp" }
func (c Cmp) encode(a *netlink.Attrs) {
	a.BE32(cmpSreg, c.Reg)
	a.BE32(cmpOp, c.Op)
	a.Nested(cmpData, func(d *netlink.Attrs) { d.Add(dataValue, c.Data) })
}

// Payload loads Len packet bytes at Offset from the start of the network or
// transport header into Reg.
type Payload struct {
	Base, Offset, Len uint32
	Reg               uint32
}

func (Payload) name() string { return "payload" }
func (p Payload) encode(a *netlink.Attrs) {
	a.BE32(payloadDreg, p.Reg)
	a.BE32(payloadBase, p.Base)
	a.BE32(payloadOffset, p.Offset)
	a.BE32(payloadLen, p.Len)
}

// Bitwise sets Reg to (Reg & Mask) ^ Xor; Mask and Xor have the same length.
type Bitwise struct {
	Reg       uint32
	Mask, Xor []byte
}

func (Bitwise) name() string { return "bitwise" }
func (b Bitwise) encode(a *netlink.Attrs) {
	a.BE32(bitwiseSreg, b.Reg)
	a.BE32(bitwiseDreg, b.Reg)
	a.BE32(bitwiseLen, uint32(len(b.Mask)))
	a.Nested(bitwiseMask, func(d *netlink.Attrs) { d.Add(dataValue, b.Mask) })
	a.Nested(bitwiseXor, func(d *netlink.Attrs) { d.Add(dataValue, b.Xor) })
}

// Lookup matches when Reg is in the set (or, Invert, when it isn't).
type Lookup struct {
	Set    string
	SetID  uint32 // when the set is new in the same batch
	Reg    uint32
	Invert bool
}

func (Lookup) name() string { return "lookup" }
func (l Lookup) encode(a *netlink.Attrs) {
	a.String(lookupSet, l.Set)
	if l.SetID != 0 {
		a.BE32(lookupSetID, l.SetID)
	}
	a.BE32(lookupSreg, l.Reg)
	if l.Invert {
		a.BE32(lookupFlags, lookupInv)
	}
}

// Immediate loads Data into Reg.
type Immediate struct {
	Reg  uint32
	Data []byte
}

func (Immediate) name() string { return "immediate" }
func (i Immediate) encode(a *netlink.Attrs) {
	a.BE32(immDreg, i.Reg)
	a.Nested(immData, func(d *netlink.Attrs) { d.Add(dataValue, i.Data) })
}

// Verdict ends the rule with a verdict (VerdictReturn: leave this chain).
type Verdict struct{ Code int32 }

func (Verdict) name() string { return "immediate" }
func (v Verdict) encode(a *netlink.Attrs) {
	a.BE32(immDreg, RegVerdict)
	a.Nested(immData, func(d *netlink.Attrs) {
		d.Nested(dataVerdict, func(vd *netlink.Attrs) { vd.BE32(verdictCode, uint32(v.Code)) })
	})
}
