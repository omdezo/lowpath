package sockdiag

import (
	"encoding/binary"
	"net/netip"
	"testing"

	"lowpath/internal/netlink"
)

func TestDumpTCP(t *testing.T) {
	for _, v6 := range []bool{false, true} {
		m := DumpTCP(v6)
		if m.Type != 20 || len(m.Data) != reqLen {
			t.Fatalf("v6=%v: type %d, %d bytes", v6, m.Type, len(m.Data))
		}
		fam := byte(2)
		if v6 {
			fam = 10
		}
		if m.Data[0] != fam || m.Data[1] != 6 || m.Data[2] != 0x2 || m.Data[3] != 0 {
			t.Errorf("v6=%v: header % x", v6, m.Data[:4])
		}
		// Every state that moves data, never LISTEN, TIME_WAIT or CLOSE.
		states := binary.NativeEndian.Uint32(m.Data[4:])
		for st, want := range map[int]bool{1: true, 2: true, 3: true, 4: true, 5: true, 6: false, 7: false, 8: true, 9: true, 10: false, 11: true} {
			if got := states&(1<<st) != 0; got != want {
				t.Errorf("state %d asked = %v, want %v", st, got, want)
			}
		}
		for i, b := range m.Data[8:] {
			if b != 0 {
				t.Fatalf("socket id byte %d = %d, want a zero id (match all)", i, b)
			}
		}
	}
}

// msg builds a struct inet_diag_msg as the kernel sends it, followed by an
// INET_DIAG_INFO attribute holding a tcp_info of infoLen bytes (0: none).
func msg(family byte, local, remote netip.AddrPort, cookie uint64, uid, inode uint32, acked, received uint64, infoLen int) netlink.Reply {
	b := make([]byte, msgLen)
	b[0], b[1] = family, 1 // ESTABLISHED
	binary.BigEndian.PutUint16(b[4:], local.Port())
	binary.BigEndian.PutUint16(b[6:], remote.Port())
	put := func(at int, a netip.Addr) {
		if a.Is4() {
			v := a.As4()
			copy(b[at:], v[:])
		} else {
			v := a.As16()
			copy(b[at:], v[:])
		}
	}
	put(8, local.Addr())
	put(24, remote.Addr())
	binary.NativeEndian.PutUint32(b[44:], uint32(cookie))
	binary.NativeEndian.PutUint32(b[48:], uint32(cookie>>32))
	binary.NativeEndian.PutUint32(b[64:], uid)
	binary.NativeEndian.PutUint32(b[68:], inode)
	var a netlink.Attrs
	a.Add(5, []byte{0, 0, 0, 0}) // INET_DIAG_SHUTDOWN, ignored
	if infoLen > 0 {
		info := make([]byte, infoLen)
		if infoLen >= offBytesReceived+8 {
			binary.NativeEndian.PutUint64(info[offBytesAcked:], acked)
			binary.NativeEndian.PutUint64(info[offBytesReceived:], received)
		}
		a.Add(attrInfo, info)
	}
	return netlink.Reply{Type: typeByFamily, Data: append(b, a.Bytes()...)}
}

func TestParse(t *testing.T) {
	ap := netip.MustParseAddrPort
	cases := []struct {
		name  string
		reply netlink.Reply
		want  Socket
		ok    bool
	}{
		{
			name:  "IPv4 with counters (tcp_info of Linux 6.x, 280 bytes)",
			reply: msg(2, ap("192.0.2.10:50123"), ap("203.0.113.5:443"), 0x1_0000_0002, 1000, 4242, 1<<33, 5_000_000, 280),
			want: Socket{Local: ap("192.0.2.10:50123"), Remote: ap("203.0.113.5:443"), State: 1, UID: 1000, Inode: 4242,
				Cookie: 0x1_0000_0002, BytesAcked: 1 << 33, BytesReceived: 5_000_000, HasBytes: true},
			ok: true,
		},
		{
			name:  "IPv6, tcp_info exactly up to tcpi_bytes_received",
			reply: msg(10, ap("[2001:db8::10]:50124"), ap("[2001:db8:1::5]:443"), 7, 0, 0, 10, 20, offBytesReceived+8),
			want: Socket{Local: ap("[2001:db8::10]:50124"), Remote: ap("[2001:db8:1::5]:443"), State: 1, Cookie: 7,
				BytesAcked: 10, BytesReceived: 20, HasBytes: true},
			ok: true,
		},
		{
			name:  "v4-mapped peer of a dual-stack socket",
			reply: msg(10, ap("[::ffff:192.0.2.10]:50125"), ap("[::ffff:198.51.100.7]:80"), 8, 1000, 9, 1, 2, 232),
			want: Socket{Local: ap("[::ffff:192.0.2.10]:50125"), Remote: ap("[::ffff:198.51.100.7]:80"), State: 1, UID: 1000, Inode: 9,
				Cookie: 8, BytesAcked: 1, BytesReceived: 2, HasBytes: true},
			ok: true,
		},
		{
			name:  "Linux before 4.1: tcp_info stops before the byte counters",
			reply: msg(2, ap("192.0.2.10:1"), ap("203.0.113.5:2"), 9, 0, 1, 0, 0, 104),
			want:  Socket{Local: ap("192.0.2.10:1"), Remote: ap("203.0.113.5:2"), State: 1, Inode: 1, Cookie: 9},
			ok:    true,
		},
		{
			name:  "no tcp_info at all",
			reply: msg(2, ap("192.0.2.10:1"), ap("203.0.113.5:2"), 9, 0, 1, 0, 0, 0),
			want:  Socket{Local: ap("192.0.2.10:1"), Remote: ap("203.0.113.5:2"), State: 1, Inode: 1, Cookie: 9},
			ok:    true,
		},
		{name: "another message type", reply: netlink.Reply{Type: 3, Data: make([]byte, msgLen)}},
		{name: "truncated", reply: netlink.Reply{Type: typeByFamily, Data: make([]byte, msgLen-1)}},
		{name: "unknown family", reply: netlink.Reply{Type: typeByFamily, Data: append([]byte{1}, make([]byte, msgLen-1)...)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := Parse(c.reply)
			if ok != c.ok || got != c.want {
				t.Errorf("Parse = %+v, %v\nwant    %+v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}
