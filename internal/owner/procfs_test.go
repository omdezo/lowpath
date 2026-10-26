package owner

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// The /proc fixtures are in the little-endian form every machine Lowpath
// builds for prints.
func skipBigEndian(t *testing.T) {
	t.Helper()
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("fixtures are little-endian")
	}
}

const procUDPHeader = "   sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode ref pointer drops\n"

// procFixture is a small /proc: a native game with IPv4 and IPv6 sockets,
// a Proton game whose loader is wine64-preloader, another user's process
// whose fd folder can't be read, and a process that rewrote its command
// line.
func procFixture() fstest.MapFS {
	link := func(target string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(target), Mode: fs.ModeSymlink | 0o777}
	}
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	return fstest.MapFS{
		"proc/net/udp": file(procUDPHeader +
			"  1: 0100007F:1B58 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 101 2 0000000000000000 0\n" +
			"  2: 00000000:6987 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 102 2 0000000000000000 0\n" +
			"  3: 0200000A:0035 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 103 2 0000000000000000 0\n" +
			"  4: 00000000:0044 00000000:0000 07 00000000:00000000 00:00000000 00000000     0        0 0 2 0000000000000000 0\n" +
			"  5: garbage\n"),
		"proc/net/udp6": file(procUDPHeader +
			"  1: 00000000000000000000000000000000:1B59 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 104 2 0000000000000000 0\n" +
			"  2: B80D0120000000000000000001000000:1B5A 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 105 2 0000000000000000 0\n" +
			"  3: 0000000000000000FFFF00000100007F:1B5B 00000000000000000000000000000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 106 2 0000000000000000 0\n"),

		// Native Linux game.
		"proc/1200/comm":    file("cs2\n"),
		"proc/1200/cmdline": file("/home/player/.steam/steam/steamapps/common/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/cs2\x00-steam\x00"),
		"proc/1200/exe":     link("/home/player/.local/share/Steam/steamapps/common/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/cs2"),
		"proc/1200/fd/0":    link("/dev/null"),
		"proc/1200/fd/5":    link("socket:[101]"),
		"proc/1200/fd/6":    link("socket:[104]"),
		"proc/1200/fd/7":    link("socket:[999]"), // a TCP socket: not in the UDP tables
		"proc/1200/fd/8":    link("anon_inode:[eventfd]"),

		// Proton game: comm cut to 15 bytes, the loader as program file.
		"proc/1300/comm":    file("RocketLeague.ex\n"),
		"proc/1300/cmdline": file(`C:\Program Files\Epic Games\rocketleague\Binaries\Win64\RocketLeague.exe` + "\x00"),
		"proc/1300/exe":     link("/home/player/.steam/steam/steamapps/common/Proton 9.0 (Beta)/files/bin/wine64-preloader"),
		"proc/1300/environ": file("HOME=/home/player\x00WINEPREFIX=/home/player/Games/epic\x00"),
		"proc/1300/fd/3":    link("socket:[102]"),
		"proc/1300/fd/4":    link("socket:[105]"),
		"proc/1300/fd/9":    link("socket:[106]"),

		// Another user's DNS cache: names visible, fd folder not.
		"proc/400/comm":    file("systemd-resolve\n"),
		"proc/400/cmdline": file("/usr/lib/systemd/systemd-resolved\x00"),

		// A program that rewrote its command line, with an exe link that
		// points at a replaced file.
		"proc/1500/comm":    file("helper\n"),
		"proc/1500/cmdline": file("/opt/app/helper --type=utility --dir=/var/x\x00"),
		"proc/1500/exe":     link("/opt/app/helper (deleted)"),

		"proc/self":    link("1200"),
		"proc/meminfo": file("MemTotal: 1 kB\n"),
	}
}

func TestProcfsUDPSockets(t *testing.T) {
	skipBigEndian(t)
	got, err := NewProcfs(procFixture()).UDPSockets()
	if err != nil {
		t.Fatal(err)
	}
	ap := netip.MustParseAddrPort
	want := []Socket{
		{ap("127.0.0.1:7000"), 1200},
		{ap("[::]:7001"), 1200},
		{ap("0.0.0.0:27015"), 1300},
		{ap("[2001:db8::1]:7002"), 1300},
		{ap("[::ffff:127.0.0.1]:7003"), 1300},
	}
	slices.SortFunc(got, func(a, b Socket) int { return int(a.Local.Port()) - int(b.Local.Port()) })
	slices.SortFunc(want, func(a, b Socket) int { return int(a.Local.Port()) - int(b.Local.Port()) })
	if !reflect.DeepEqual(got, want) {
		t.Errorf("UDPSockets =\n%v\nwant\n%v", got, want)
	}
}

// SocketOwners finds any socket by inode, TCP ones included, and leaves out
// what it can't see: another user's process, an inode no process holds.
func TestProcfsSocketOwners(t *testing.T) {
	p := NewProcfs(procFixture())
	got, err := p.SocketOwners(map[uint64]bool{999: true, 105: true, 4242: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[uint64]uint32{999: 1200, 105: 1300}; !reflect.DeepEqual(got, want) {
		t.Errorf("SocketOwners = %v, want %v", got, want)
	}
	if got, err := p.SocketOwners(nil); err != nil || len(got) != 0 {
		t.Errorf("nothing asked: %v, %v", got, err)
	}
	if _, err := NewProcfs(fstest.MapFS{}).SocketOwners(map[uint64]bool{1: true}); err == nil {
		t.Error("no /proc: want an error, not \"no owners\"")
	}
}

func TestProcfsPrograms(t *testing.T) {
	progs, err := NewProcfs(procFixture()).Programs()
	if err != nil {
		t.Fatal(err)
	}
	byPID := map[uint32]Program{}
	for _, p := range progs {
		byPID[p.PID] = p
	}
	want := map[uint32]Program{
		1200: {PID: 1200, Name: "cs2", Names: []string{"cs2"},
			Exe: "/home/player/.local/share/Steam/steamapps/common/Counter-Strike Global Offensive/game/bin/linuxsteamrt64/cs2"},
		1300: {PID: 1300, Name: "RocketLeague.exe", Names: []string{"RocketLeague.exe", "RocketLeague.ex", "wine64-preloader"},
			Exe: "/home/player/Games/epic/dosdevices/c:/Program Files/Epic Games/rocketleague/Binaries/Win64/RocketLeague.exe"},
		400:  {PID: 400, Name: "systemd-resolved", Names: []string{"systemd-resolved", "systemd-resolve"}},
		1500: {PID: 1500, Name: "helper", Names: []string{"helper"}, Exe: "/opt/app/helper"},
	}
	if !reflect.DeepEqual(byPID, want) {
		t.Errorf("Programs =\n%+v\nwant\n%+v", byPID, want)
	}
	names, err := NewProcfs(procFixture()).ProcessNames()
	if err != nil || names[1300] != "RocketLeague.exe" || names[1200] != "cs2" || len(names) != 4 {
		t.Errorf("ProcessNames = %v, %v", names, err)
	}
}

// ListProcesses on top of Procfs: what "Add game" shows on Linux.
func TestProcfsListProcesses(t *testing.T) {
	skipBigEndian(t)
	got, err := ListProcesses(NewProcfs(procFixture()), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: 1200, Name: "cs2", UDPPorts: []uint16{7000, 7001}},
		{PID: 1300, Name: "RocketLeague.exe", UDPPorts: []uint16{7002, 7003, 27015}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListProcesses =\n%+v\nwant\n%+v", got, want)
	}
}

// No /proc (a container without it) is an error, never an empty table that
// reads as "nothing is open".
func TestProcfsWithoutProc(t *testing.T) {
	empty := NewProcfs(fstest.MapFS{})
	if _, err := empty.UDPSockets(); err == nil {
		t.Error("UDPSockets without /proc/net/udp: no error")
	}
	if _, err := empty.Programs(); err == nil {
		t.Error("Programs without /proc: no error")
	}
	onlyNet := NewProcfs(fstest.MapFS{"proc/net/udp": {Data: []byte(procUDPHeader)}})
	if socks, err := onlyNet.UDPSockets(); err != nil || len(socks) != 0 {
		t.Errorf("empty table: %v, %v", socks, err)
	}
	noPIDs := NewProcfs(fstest.MapFS{
		"proc/net/udp": {Data: []byte(procUDPHeader + "  1: 0100007F:1B58 00000000:0000 07 00000000:00000000 00:00000000 00000000  1000        0 101 2 0 0\n")},
	})
	if _, err := noPIDs.UDPSockets(); err == nil || !strings.Contains(err.Error(), "/proc") {
		t.Errorf("sockets but no processes: %v", err)
	}
	if _, err := noPIDs.ProcessNames(); err == nil {
		t.Error("ProcessNames without processes: no error")
	}
}

func TestParseProcAddr(t *testing.T) {
	skipBigEndian(t)
	for in, want := range map[string]string{
		"0100007F:0035":                         "127.0.0.1:53",
		"00000000:FFFF":                         "0.0.0.0:65535",
		"0271000A:1F90":                         "10.0.113.2:8080",
		"00000000000000000000000001000000:0001": "[::1]:1",
		"B80D0120000000000000000002000000:0BB8": "[2001:db8::2]:3000",
	} {
		got, ok := parseProcAddr(in)
		if !ok || got != netip.MustParseAddrPort(want) {
			t.Errorf("parseProcAddr(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "0100007F", "0100007F:35", "0100007:0035", "XX00007F:0035", "0100007F:GGGG", "01000000000000:0001"} {
		if got, ok := parseProcAddr(bad); ok {
			t.Errorf("parseProcAddr(%q) = %v, want failure", bad, got)
		}
	}
}

func TestSocketInode(t *testing.T) {
	for in, want := range map[string]uint64{"socket:[12345]": 12345, "socket:[0]": 0, "socket:[": 0, "pipe:[5]": 0, "socket:[x]": 0, "/dev/null": 0} {
		got, ok := socketInode(in)
		if got != want || ok != (want != 0) {
			t.Errorf("socketInode(%q) = %d, %v", in, got, ok)
		}
	}
}

// A read error in the middle of the table is reported, not taken as the
// end of the table.
func TestParseProcNetUDPError(t *testing.T) {
	boom := errors.New("boom")
	err := parseProcNetUDP(errReader{boom}, map[uint64]netip.AddrPort{})
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }
