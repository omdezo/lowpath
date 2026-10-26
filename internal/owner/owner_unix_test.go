//go:build linux || darwin

package owner

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These run against the real socket table and process list as an ordinary
// user; the same tests as on Windows.

func ownSocket(t *testing.T, network string, laddr *net.UDPAddr) netip.AddrPort {
	t.Helper()
	c, err := net.ListenUDP(network, laddr)
	if err != nil {
		t.Skipf("%s socket: %v", network, err) // no IPv6 on this machine
	}
	t.Cleanup(func() { c.Close() })
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

func TestUDPSocketsFindsOwnSockets(t *testing.T) {
	self := uint32(os.Getpid())
	v4 := ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	wild := ownSocket(t, "udp4", &net.UDPAddr{})
	socks, err := NewTable().UDPSockets()
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

	dual := ownSocket(t, "udp", &net.UDPAddr{IP: net.IPv6unspecified}) // [::], dual-stack
	if socks, err = NewTable().UDPSockets(); err != nil {
		t.Fatal(err)
	}
	find(netip.AddrPortFrom(netip.IPv6Unspecified(), dual.Port()))
}

func TestProgramsFindsSelf(t *testing.T) {
	progs, err := NewTable().Programs()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	i := slices.IndexFunc(progs, func(p Program) bool { return p.PID == uint32(os.Getpid()) })
	if i < 0 {
		t.Fatalf("own process not among %d programs", len(progs))
	}
	p := progs[i]
	if !strings.EqualFold(p.Name, filepath.Base(exe)) || !slices.Contains(p.Names, p.Name) {
		t.Errorf("own entry %+v, want name %q", p, filepath.Base(exe))
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil && p.Exe != exe && p.Exe != real {
		t.Errorf("own path %q, want %q", p.Exe, exe)
	}
	names, err := NewTable().ProcessNames()
	if err != nil || names[uint32(os.Getpid())] != p.Name {
		t.Errorf("ProcessNames: %q, %v", names[uint32(os.Getpid())], err)
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

func BenchmarkUDPSockets(b *testing.B) {
	r := NewTable()
	for b.Loop() {
		if _, err := r.UDPSockets(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrograms(b *testing.B) {
	r := NewTable()
	for b.Loop() {
		if _, err := r.Programs(); err != nil {
			b.Fatal(err)
		}
	}
}
