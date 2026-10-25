package capture

import (
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

	socks, err := udpSockets()
	if err != nil {
		t.Fatal(err)
	}
	find := func(want netip.AddrPort) {
		t.Helper()
		for _, s := range socks {
			if s.local.Port() == want.Port() && s.local.Addr().Unmap() == want.Addr().Unmap() {
				if s.pid != self {
					t.Fatalf("%v owned by PID %d, want %d", want, s.pid, self)
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
	names, err := processNames()
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if got := names[uint32(os.Getpid())]; !strings.EqualFold(got, filepath.Base(exe)) {
		t.Fatalf("own image name = %q, want %q", got, filepath.Base(exe))
	}
}

// End to end against the real OS: this test binary plays the game.
func TestOwnersWithRealOS(t *testing.T) {
	exe, _ := os.Executable()
	local := ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	dual := ownSocket(t, "udp", &net.UDPAddr{IP: net.IPv6unspecified})
	viaDual := netip.AddrPortFrom(netip.MustParseAddr("192.0.2.10"), dual.Port())

	o, err := newOwners([]string{strings.ToUpper(filepath.Base(exe))}, nil, 0, udpSockets, processNames)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []netip.AddrPort{local, viaDual} {
		if g, err := o.isGame(a); err != nil || !g {
			t.Fatalf("isGame(%v) = %v, %v; want true", a, g, err)
		}
	}
	// A socket opened after the table was built is found on its first packet.
	late := ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if g, err := o.isGame(late); err != nil || !g {
		t.Fatalf("new socket: isGame = %v, %v", g, err)
	}

	// The same process as ourselves is never a game.
	o, err = newOwners([]string{filepath.Base(exe)}, nil, uint32(os.Getpid()), udpSockets, processNames)
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := o.isGame(local); g {
		t.Fatal("own process classified as a game")
	}
}

func TestProcessesExcludesSelf(t *testing.T) {
	ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	procs, err := Processes()
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

func TestAvailableWithoutAdmin(t *testing.T) {
	if IsAdmin() {
		t.Skip("elevated")
	}
	if err := Available(); err != ErrNeedAdmin {
		t.Fatalf("Available() = %v, want ErrNeedAdmin", err)
	}
}
