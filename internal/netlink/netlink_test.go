package netlink

import (
	"bytes"
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
)

var ne = binary.NativeEndian

func TestAttrsPadding(t *testing.T) {
	if ne.Uint16([]byte{1, 0}) != 1 {
		t.Skip("fixtures are for little-endian hosts")
	}
	var a Attrs
	a.U8(1, 7)
	a.String(2, "lp")
	a.U32(3, 0x01020304)
	a.BE32(4, 0x01020304)
	a.Nested(5, func(in *Attrs) { in.U8(6, 9) })
	b := a.Bytes()
	if len(b)%4 != 0 {
		t.Fatalf("length %d not padded", len(b))
	}
	want := [][]byte{
		{5, 0, 1, 0, 7, 0, 0, 0},
		{7, 0, 2, 0, 'l', 'p', 0, 0},
		append([]byte{8, 0, 3, 0}, ne.AppendUint32(nil, 0x01020304)...),
		{8, 0, 4, 0, 1, 2, 3, 4},
		{12, 0, 5, 0x80, 5, 0, 6, 0, 9, 0, 0, 0},
	}
	if !bytes.Equal(b, bytes.Join(want, nil)) {
		t.Fatalf("got % x\nwant % x", b, bytes.Join(want, nil))
	}
	got := ParseAttrs(b)
	if len(got) != 5 || got[4].Type != 5 || !bytes.Equal(got[1].Data, []byte("lp\x00")) {
		t.Fatalf("ParseAttrs = %+v", got)
	}
	if in := ParseAttrs(got[4].Data); len(in) != 1 || in[0].Data[0] != 9 {
		t.Fatalf("nested = %+v", in)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	m := Message{Type: 24, Flags: FlagAck | FlagCreate, Data: []byte{1, 2, 3}}
	b := m.Append(nil, 7)
	b = Message{Type: 3, Data: nil}.Append(b, 8)
	if len(b) != 20+16 {
		t.Fatalf("encoded length %d", len(b))
	}
	if ne.Uint32(b) != 19 {
		t.Fatalf("header length %d, want 19 (unpadded)", ne.Uint32(b))
	}
	rs, err := ParseReplies(b)
	if err != nil || len(rs) != 2 {
		t.Fatalf("ParseReplies = %+v, %v", rs, err)
	}
	if rs[0].Type != 24 || rs[0].Flags != FlagAck|FlagCreate || rs[0].Seq != 7 || !bytes.Equal(rs[0].Data, []byte{1, 2, 3}) {
		t.Fatalf("first = %+v", rs[0])
	}
	if rs[1].Seq != 8 || rs[1].Type != 3 {
		t.Fatalf("second = %+v", rs[1])
	}
	for _, bad := range [][]byte{b[:10], append(ne.AppendUint32(nil, 99), b[4:16]...)} {
		if _, err := ParseReplies(bad); err == nil {
			t.Errorf("ParseReplies(% x) accepted a broken buffer", bad)
		}
	}
}

// errReply builds an NLMSG_ERROR answer to a request of n body bytes.
func errReply(code int32, flags uint16, n int, msg string) Reply {
	req := Message{Type: 6, Data: make([]byte, n)}.Append(nil, 5)
	d := ne.AppendUint32(nil, uint32(code))
	if flags&flagCapped != 0 {
		d = append(d, req[:HeaderLen]...)
	} else {
		d = append(d, req...)
	}
	if msg != "" {
		var a Attrs
		a.String(errAttrMsg, msg)
		d = append(d, a.Bytes()...)
	}
	return Reply{Type: typeError, Flags: flags, Seq: 5, Data: d}
}

func TestAck(t *testing.T) {
	cases := []struct {
		r     Reply
		errno syscall.Errno
		msg   string
	}{
		{errReply(0, 0, 8, ""), 0, ""},
		{errReply(-int32(syscall.ENOENT), 0, 8, ""), syscall.ENOENT, ""},
		{errReply(-int32(syscall.EINVAL), flagCapped|flagAckTLVs, 40, "Unknown chain type"), syscall.EINVAL, "Unknown chain type"},
		{errReply(-int32(syscall.EPERM), flagAckTLVs, 6, "not the owner"), syscall.EPERM, "not the owner"},
	}
	for _, c := range cases {
		errno, msg, ok := c.r.ack()
		if !ok || errno != c.errno || msg != c.msg {
			t.Errorf("ack = %v %q %v, want %v %q", errno, msg, ok, c.errno, c.msg)
		}
	}
	if _, _, ok := (Reply{Type: 16, Data: make([]byte, 40)}).ack(); ok {
		t.Error("a non-error message read as an ack")
	}
	e := &Error{Op: "add rule", Errno: syscall.ENOENT, Msg: "no such chain"}
	if !errors.Is(e, syscall.ENOENT) || e.Error() != "netlink: add rule: "+syscall.ENOENT.Error()+" (no such chain)" {
		t.Errorf("Error = %q", e)
	}
}
