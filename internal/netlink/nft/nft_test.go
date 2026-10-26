package nft

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"lowpath/internal/netlink"
)

func attrs(b []byte) map[uint16][]byte {
	out := map[uint16][]byte{}
	for _, a := range netlink.ParseAttrs(b) {
		out[a.Type] = a.Data
	}
	return out
}

func be(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func TestBatch(t *testing.T) {
	b := Batch(AddTable(IPv4, "lowpath", TableOwner), DelTable(IPv4, "lowpath"))
	if len(b) != 4 || b[0].Type != batchBegin || b[3].Type != batchEnd {
		t.Fatalf("batch %+v", b)
	}
	// struct nfgenmsg with the subsystem as a big-endian resource id.
	if !bytes.Equal(b[0].Data, []byte{0, 0, 0, subsysNftables}) || b[0].Flags&netlink.FlagAck != 0 {
		t.Fatalf("begin %+v", b[0])
	}
	add := b[1]
	if add.Type != subsysNftables<<8|msgNewTable || add.Data[0] != byte(IPv4) || add.Flags&netlink.FlagAck == 0 {
		t.Fatalf("add table %+v", add)
	}
	a := attrs(add.Data[4:])
	if string(a[tableName]) != "lowpath\x00" || !bytes.Equal(a[tableFlags], []byte{0, 0, 0, TableOwner}) {
		t.Fatalf("table attrs %v", a)
	}
	if _, ok := attrs(AddTable(IPv4, "x", 0).Data[4:])[tableFlags]; ok {
		t.Error("flags 0 should be left out")
	}
	if b[2].Type != subsysNftables<<8|msgDelTable {
		t.Error("delete table type")
	}
}

func TestListTables(t *testing.T) {
	l := ListTables(IPv4)
	if l.Type != subsysNftables<<8|msgGetTable || !bytes.Equal(l.Data, []byte{byte(IPv4), 0, 0, 0}) {
		t.Fatalf("list tables %+v", l)
	}
	// An answer as the kernel sends it: name, flags, use, handle.
	var a netlink.Attrs
	a.String(tableName, "lowpath")
	a.BE32(tableFlags, TableOwner)
	a.BE32(3, 2)
	a.Add(tableHandle, binary.BigEndian.AppendUint64(nil, 0x1234))
	r := netlink.Reply{Type: subsysNftables<<8 | msgNewTable, Data: append([]byte{byte(IPv4), 0, 0, 0}, a.Bytes()...)}
	if got, ok := ParseTable(r); !ok || got != (Table{Name: "lowpath", Handle: 0x1234, Flags: TableOwner}) {
		t.Fatalf("ParseTable = %+v %v", got, ok)
	}
	// Kernels before 4.16 send no handle.
	var old netlink.Attrs
	old.String(tableName, "filter")
	if got, ok := ParseTable(netlink.Reply{Type: r.Type, Data: append([]byte{byte(IPv4), 0, 0, 0}, old.Bytes()...)}); !ok || got != (Table{Name: "filter"}) {
		t.Fatalf("ParseTable without a handle = %+v %v", got, ok)
	}
	for _, bad := range []netlink.Reply{{Type: subsysNftables<<8 | msgNewChain, Data: r.Data}, {Type: r.Type, Data: []byte{2}}} {
		if _, ok := ParseTable(bad); ok {
			t.Errorf("%+v parsed as a table", bad)
		}
	}
}

func TestChainAndSet(t *testing.T) {
	c := AddChain(IPv4, Chain{Table: "t", Name: "out", Type: "route", Hook: HookLocalOut, Priority: PriorityMangle, Policy: PolicyAccept})
	a := attrs(c.Data[4:])
	h := attrs(a[chainHook])
	if be(h[hookNum]) != 3 || int32(be(h[hookPriority])) != -150 || string(a[chainType]) != "route\x00" || be(a[chainPolicy]) != 1 {
		t.Fatalf("chain %v hook %v", a, h)
	}
	s := attrs(AddSet(IPv4, Set{Table: "t", Name: "direct", ID: 1, KeyType: TypeInetService, KeyLen: 2}).Data[4:])
	if be(s[setKeyLen]) != 2 || be(s[setID]) != 1 || be(s[setKeyType]) != 13 {
		t.Fatalf("set %v", s)
	}
	e := attrs(AddElems(IPv4, "t", "direct", 1, [][]byte{Port(53), Port(3478)}).Data[4:])
	list := netlink.ParseAttrs(e[elemListElements])
	if len(list) != 2 || be(e[elemListSetID]) != 1 {
		t.Fatalf("elements %v", e)
	}
	if key := attrs(attrs(list[1].Data)[elemKey]); !bytes.Equal(key[dataValue], []byte{0x0d, 0x96}) {
		t.Fatalf("second key % x", key[dataValue])
	}
	if _, ok := attrs(DelElems(IPv4, "t", "direct", [][]byte{Port(1)}).Data[4:])[elemListSetID]; ok {
		t.Error("deleting from an existing set needs no set id")
	}
}

func TestRuleExpressions(t *testing.T) {
	r := AddRule(IPv4, "t", "out",
		Meta{Key: MetaMark, Reg: Reg1},
		Cmp{Op: CmpNeq, Reg: Reg1, Data: HostU32(0)},
		Verdict{Code: VerdictReturn},
		Payload{Base: BaseTransport, Offset: 2, Len: 2, Reg: Reg1},
		Bitwise{Reg: Reg1, Mask: []byte{0x3f, 0xff}, Xor: []byte{0, 0}},
		Lookup{Set: "direct", SetID: 1, Reg: Reg1, Invert: true},
		Immediate{Reg: Reg2, Data: HostU32(0x4c50)},
		Meta{Key: MetaMark, Reg: Reg2, Set: true},
	)
	if r.Flags&netlink.FlagAppend == 0 {
		t.Error("rules are appended")
	}
	var names []string
	var data []map[uint16][]byte
	for _, e := range netlink.ParseAttrs(attrs(r.Data[4:])[ruleExpressions]) {
		ea := attrs(e.Data)
		names = append(names, string(bytes.TrimRight(ea[exprName], "\x00")))
		data = append(data, attrs(ea[exprData]))
	}
	want := []string{"meta", "cmp", "immediate", "payload", "bitwise", "lookup", "immediate", "meta"}
	if !slices.Equal(names, want) {
		t.Fatalf("expressions %v, want %v", names, want)
	}
	if be(data[0][metaDreg]) != Reg1 || be(data[0][metaKey]) != MetaMark {
		t.Errorf("meta load %v", data[0])
	}
	if c := attrs(data[1][cmpData]); !bytes.Equal(c[dataValue], HostU32(0)) || be(data[1][cmpOp]) != CmpNeq {
		t.Errorf("cmp %v", data[1])
	}
	if v := attrs(attrs(data[2][immData])[dataVerdict]); be(data[2][immDreg]) != RegVerdict || int32(be(v[verdictCode])) != VerdictReturn {
		t.Errorf("verdict %v", data[2])
	}
	if be(data[3][payloadBase]) != BaseTransport || be(data[3][payloadOffset]) != 2 || be(data[3][payloadLen]) != 2 {
		t.Errorf("payload %v", data[3])
	}
	if be(data[4][bitwiseLen]) != 2 || !bytes.Equal(attrs(data[4][bitwiseMask])[dataValue], []byte{0x3f, 0xff}) {
		t.Errorf("bitwise %v", data[4])
	}
	if string(data[5][lookupSet]) != "direct\x00" || be(data[5][lookupSetID]) != 1 || be(data[5][lookupFlags]) != lookupInv {
		t.Errorf("lookup %v", data[5])
	}
	if !bytes.Equal(attrs(data[6][immData])[dataValue], HostU32(0x4c50)) || be(data[6][immDreg]) != Reg2 {
		t.Errorf("immediate %v", data[6])
	}
	if _, ok := data[7][metaDreg]; ok || be(data[7][metaSreg]) != Reg2 {
		t.Errorf("meta set %v", data[7])
	}
}
