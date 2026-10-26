package owner

import (
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These run against the real socket table and process list; neither needs
// admin rights.

func ownSocket(t *testing.T, network string, laddr *net.UDPAddr) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP(network, laddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestUDPSocketsFindsOwnSockets(t *testing.T) {
	self := uint32(os.Getpid())
	v4 := ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	wild := ownSocket(t, "udp4", &net.UDPAddr{})
	dual := ownSocket(t, "udp", &net.UDPAddr{IP: net.IPv6unspecified}) // [::], dual-stack

	socks, err := NewWindows().UDPSockets()
	if err != nil {
		t.Fatal(err)
	}
	find := func(want netip.AddrPort) {
		t.Helper()
		for _, s := range socks {
			if s.Local.Port() == want.Port() && s.Local.Addr().Unmap() == want.Addr().Unmap() {
				if s.PID != self {
					t.Fatalf("%v owned by PID %d, want %d", want, s.PID, self)
				}
				return
			}
		}
		t.Fatalf("%v not in the socket table (%d sockets)", want, len(socks))
	}
	find(v4)
	find(netip.AddrPortFrom(netip.IPv4Unspecified(), wild.Port()))
	find(netip.AddrPortFrom(netip.IPv6Unspecified(), dual.Port()))
}

func TestProcessNamesFindsSelf(t *testing.T) {
	names, err := NewWindows().ProcessNames()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if got := names[uint32(os.Getpid())]; !strings.EqualFold(got, filepath.Base(exe)) {
		t.Fatalf("own image name = %q, want %q", got, filepath.Base(exe))
	}
}

func TestListProcessesExcludesSelf(t *testing.T) {
	ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	procs, err := ListProcesses(New(), uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range procs {
		if p.PID == uint32(os.Getpid()) || p.PID <= 4 {
			t.Fatalf("listed %+v", p)
		}
		if p.Name == "" || len(p.UDPPorts) == 0 {
			t.Fatalf("incomplete entry %+v", p)
		}
	}
	t.Logf("%d processes with UDP sockets", len(procs))
}

// A truncated table (the count says more rows than the buffer holds) yields
// the complete rows only.
func TestParseUDPRows(t *testing.T) {
	b := binary.LittleEndian.AppendUint32(nil, 3)
	row := func(a [4]byte, port uint16, pid uint32) {
		b = append(b, a[:]...)
		b = binary.BigEndian.AppendUint16(b, port)
		b = append(b, 0, 0)
		b = binary.LittleEndian.AppendUint32(b, pid)
	}
	row([4]byte{127, 0, 0, 1}, 7000, 42)
	row([4]byte{0, 0, 0, 0}, 53, 4)
	b = append(b, 1, 2, 3) // a cut-off third row
	got := parseUDPRows(nil, b, udpRowLen)
	want := []Socket{{netip.MustParseAddrPort("127.0.0.1:7000"), 42}, {netip.MustParseAddrPort("0.0.0.0:53"), 4}}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parseUDPRows = %v, want %v", got, want)
	}
	if got := parseUDPRows(nil, []byte{1}, udpRowLen); len(got) != 0 {
		t.Fatalf("short buffer: %v", got)
	}
}

func BenchmarkUDPSockets(b *testing.B) {
	w := NewWindows()
	for b.Loop() {
		if _, err := w.UDPSockets(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProcessNames(b *testing.B) {
	w := NewWindows()
	for b.Loop() {
		if _, err := w.ProcessNames(); err != nil {
			b.Fatal(err)
		}
	}
}
