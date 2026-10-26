package capture

import (
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lowpath/internal/divert"
	"lowpath/internal/owner"
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

// End to end against the real OS: this test binary plays the game.
func TestOwnersWithRealOS(t *testing.T) {
	exe, _ := os.Executable()
	local := ownSocket(t, "udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	dual := ownSocket(t, "udp", &net.UDPAddr{IP: net.IPv6unspecified})
	viaDual := netip.AddrPortFrom(netip.MustParseAddr("192.0.2.10"), dual.Port())

	o, err := newOwners([]string{strings.ToUpper(filepath.Base(exe))}, nil, 0, owner.New())
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
	o, err = newOwners([]string{filepath.Base(exe)}, nil, uint32(os.Getpid()), owner.New())
	if err != nil {
		t.Fatal(err)
	}
	if g, _ := o.isGame(local); g {
		t.Fatal("own process classified as a game")
	}
}

func TestAvailableWithoutAdmin(t *testing.T) {
	if divert.IsAdmin() {
		t.Skip("elevated")
	}
	if err := available(false); err != ErrNeedAdmin {
		t.Fatalf("available(false) = %v, want ErrNeedAdmin", err)
	}
}
