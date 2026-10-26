package owner

import (
	"encoding/binary"
	"net/netip"
	"testing"
)

// socketFDInfo builds a PROC_PIDFDSOCKETINFO reply the way xnu lays it out.
func socketFDInfo(kind, proto, family uint32, vflag byte, laddr netip.Addr, port uint16) []byte {
	b := make([]byte, socketFDInfoLen)
	le := binary.LittleEndian
	le.PutUint32(b[soiKindOff:], kind)
	le.PutUint32(b[soiProtocolOff:], proto)
	le.PutUint32(b[soiFamilyOff:], family)
	b[insiVflagOff] = vflag
	binary.BigEndian.PutUint16(b[insiLportOff:], port) // htons(port) stored in an int
	if laddr.Is4() {
		a := laddr.As4()
		copy(b[insiLaddr4Off:], a[:])
	} else if laddr.IsValid() {
		a := laddr.As16()
		copy(b[insiLaddrOff:], a[:])
	}
	return b
}

func TestParseSocketFDInfo(t *testing.T) {
	ip := netip.MustParseAddr
	for _, c := range []struct {
		name string
		b    []byte
		want string // "" = not a UDP socket
	}{
		{"IPv4", socketFDInfo(sockinfoIn, ipprotoUDP, afInetDarwin, iniIPv4, ip("127.0.0.1"), 7000), "127.0.0.1:7000"},
		{"IPv4 wildcard", socketFDInfo(sockinfoIn, ipprotoUDP, afInetDarwin, iniIPv4, ip("0.0.0.0"), 27015), "0.0.0.0:27015"},
		{"IPv6", socketFDInfo(sockinfoIn, ipprotoUDP, afInet6Darwin, iniIPv6, ip("2001:db8::7"), 3478), "[2001:db8::7]:3478"},
		{"dual-stack wildcard", socketFDInfo(sockinfoIn, ipprotoUDP, afInet6Darwin, iniIPv6|iniIPv4, ip("::"), 65535), "[::]:65535"},
		{"IPv6 socket carrying IPv4", socketFDInfo(sockinfoIn, ipprotoUDP, afInet6Darwin, iniIPv4, ip("198.51.100.4"), 9000), "198.51.100.4:9000"},
		{"TCP", socketFDInfo(2, 6, afInetDarwin, iniIPv4, ip("127.0.0.1"), 80), ""},
		{"raw ICMP", socketFDInfo(sockinfoIn, 1, afInetDarwin, iniIPv4, ip("0.0.0.0"), 0), ""},
		{"no address family flag", socketFDInfo(sockinfoIn, ipprotoUDP, afInet6Darwin, 0, ip("::"), 1), ""},
		{"Unix socket", socketFDInfo(3, 0, 1, 0, netip.Addr{}, 0), ""},
		{"short reply", make([]byte, 100), ""},
	} {
		got, ok := parseSocketFDInfo(c.b)
		if c.want == "" {
			if ok {
				t.Errorf("%s: parsed %v", c.name, got)
			}
			continue
		}
		if !ok || got != netip.MustParseAddrPort(c.want) {
			t.Errorf("%s: %v, %v; want %s", c.name, got, ok, c.want)
		}
	}
}

// The offsets follow from xnu's proc_info.h; pin the ones derived by hand
// so a slip shows up here and not as wrong ports on a Mac.
func TestSocketFDInfoLayout(t *testing.T) {
	for name, c := range map[string][2]int{
		"soi_protocol": {soiProtocolOff, 180}, "soi_family": {soiFamilyOff, 184}, "soi_kind": {soiKindOff, 256},
		"insi_lport": {insiLportOff, 268}, "insi_vflag": {insiVflagOff, 288}, "insi_laddr": {insiLaddrOff, 312},
	} {
		if c[0] != c[1] {
			t.Errorf("%s at %d, want %d", name, c[0], c[1])
		}
	}
}

func TestParseProcFDs(t *testing.T) {
	var b []byte
	add := func(fd int32, typ uint32) {
		b = binary.LittleEndian.AppendUint32(b, uint32(fd))
		b = binary.LittleEndian.AppendUint32(b, typ)
	}
	add(0, 1) // vnode
	add(3, proxFDTypeSocket)
	add(4, 6) // pipe
	add(9, proxFDTypeSocket)
	b = append(b, 1, 2, 3) // a cut-off entry
	if got := parseProcFDs(b); len(got) != 2 || got[0] != 3 || got[1] != 9 {
		t.Errorf("parseProcFDs = %v", got)
	}
	if got := parseProcFDs(nil); len(got) != 0 {
		t.Errorf("empty: %v", got)
	}
}

func TestParseProcArgs2(t *testing.T) {
	build := func(argc int32, parts ...string) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(argc))
		for _, p := range parts {
			b = append(append(b, p...), 0)
		}
		return b
	}
	argv0, env := parseProcArgs2(build(2, "/Applications/Game.app/Contents/MacOS/wine", "\x00\x00", `C:\Games\game.exe`, "-windowed",
		"HOME=/Users/player", "WINEPREFIX=/Users/player/Bottles/x"))
	if argv0 != `C:\Games\game.exe` || len(env) != 2 || envLookup(env)("WINEPREFIX") != "/Users/player/Bottles/x" {
		t.Errorf("argv0 %q, env %q", argv0, env)
	}
	if a, e := parseProcArgs2(build(1, "/bin/x", "x")); a != "x" || e != nil {
		t.Errorf("no environment: %q %q", a, e)
	}
	for _, bad := range [][]byte{nil, {1, 0}, binary.LittleEndian.AppendUint32(nil, 3), append(binary.LittleEndian.AppendUint32(nil, 1), "/bin/x"...)} {
		if a, _ := parseProcArgs2(bad); a != "" {
			t.Errorf("parseProcArgs2(%q) = %q", bad, a)
		}
	}
	if got := cString([]byte("abc\x00def")); got != "abc" {
		t.Errorf("cString = %q", got)
	}
	if got := cString([]byte("abc")); got != "abc" {
		t.Errorf("cString without NUL = %q", got)
	}
}
