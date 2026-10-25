package engine_test

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"lowpath/internal/engine"
	"lowpath/internal/icmp"
	"lowpath/internal/relay"
)

const stunServer = "stun.l.google.com:19302"

// TestInternetSTUNThroughCapture is route mode end to end minus the driver,
// so it runs without admin rights: the engine in capture mode (Send/Deliver
// called directly, as the capture layer would) tunnels real STUN binding
// requests through an in-process egress, the same loopback test relay the app
// offers, to Google, and the replies come back addressed to the game's socket.
// It needs internet access, so it only runs with LOWPATH_INTERNET=1, and it
// skips when the STUN server can't be reached directly either.
func TestInternetSTUNThroughCapture(t *testing.T) {
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		t.Skip("set LOWPATH_INTERNET=1 to tunnel STUN to " + stunServer)
	}
	server := stunReachableOrSkip(t)

	eg, err := relay.ListenEgress("127.0.0.1:0", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eg.Serve(ctx)
	t.Cleanup(func() { eg.Close() })

	e, s := startCapture(t, engine.Config{
		Paths: []engine.PathSpec{{ID: "loopback", Label: "Loopback test relay", Addr: eg.Addr()}},
	})

	// The game's socket as the capture layer reports it. Nothing binds it: the
	// reply only has to come back through Deliver with this address on it.
	local := netip.MustParseAddrPort("192.168.77.10:50123")

	// 20 requests/s like lpcheck, until a reply arrives.
	sent := map[[12]byte]bool{}
	var reply []byte
	for end := time.Now().Add(5 * time.Second); reply == nil && time.Now().Before(end); {
		req, txid := stunRequest()
		sent[txid] = true
		if !e.Send(local, server, req) {
			t.Fatal("Send refused a packet while the loopback relay is up")
		}
		time.Sleep(50 * time.Millisecond)
		for _, d := range s.all() {
			if d.local != local || d.remote != server {
				t.Fatalf("reply delivered as %v <- %v, want %v <- %v", d.local, d.remote, local, server)
			}
			if txid, ok := stunSuccess(d.payload); ok && sent[txid] {
				reply = d.payload
				break
			}
		}
	}
	if reply == nil {
		t.Fatalf("no STUN reply came back through the tunnel (%d delivered, relay %+v)", len(s.all()), eg.Stats())
	}
	if pub, ok := stunMapped(reply); ok {
		t.Logf("STUN reply via the loopback relay: public address %v", pub)
	} else {
		t.Errorf("STUN success response without XOR-MAPPED-ADDRESS: % x", reply)
	}

	// Before = ICMP from this PC to the primary server; after = probe RTT to
	// the relay plus the relay's own ping to the server. Both need the server
	// to answer ping, so check that first instead of failing on it.
	if _, err := icmp.Ping(server.Addr(), time.Second); err != nil {
		t.Logf("%v ignores ICMP here (%v); not checking before/after", server.Addr(), err)
		return
	}
	var snap engine.Snapshot
	ok := waitFor(t, 5*time.Second, func() bool {
		req, _ := stunRequest()
		e.Send(local, server, req) // keep the server primary and in use
		snap = e.Snapshot()
		return snap.BeforeKnown && snap.ServerLegKnown && snap.After.Server > 0
	})
	t.Logf("before %.1fms (%d samples), after %.1fms of which server leg %.1fms, servers %+v",
		snap.Before.RTT, snap.Before.Samples, snap.After.RTT, snap.ServerLeg, snap.Servers)
	if !ok {
		t.Fatalf("before known=%v, server leg known=%v, after %+v: want ICMP numbers for a public server",
			snap.BeforeKnown, snap.ServerLegKnown, snap.After)
	}
	if len(snap.Servers) != 1 || !snap.Servers[0].Primary || snap.Servers[0].Addr != server {
		t.Errorf("servers %+v, want %v as the only (primary) server", snap.Servers, server)
	}
}

// stunReachableOrSkip resolves the STUN server and does one exchange with it
// directly, skipping the test when this machine is offline.
func stunReachableOrSkip(t *testing.T) netip.AddrPort {
	t.Helper()
	ua, err := net.ResolveUDPAddr("udp4", stunServer)
	if err != nil {
		t.Skipf("offline? %v", err)
	}
	server := netip.AddrPortFrom(ua.AddrPort().Addr().Unmap(), ua.AddrPort().Port())
	c, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(server))
	if err != nil {
		t.Skipf("offline? %v", err)
	}
	defer c.Close()
	buf := make([]byte, 1500)
	for range 3 {
		req, txid := stunRequest()
		c.Write(req)
		c.SetReadDeadline(time.Now().Add(time.Second))
		n, err := c.Read(buf)
		if err != nil {
			continue
		}
		if got, ok := stunSuccess(buf[:n]); ok && got == txid {
			return server
		}
	}
	t.Skipf("%s does not answer directly; offline or UDP blocked", stunServer)
	return netip.AddrPort{}
}

const stunCookie = 0x2112A442

// stunRequest builds an RFC 5389 Binding request with a random transaction id.
func stunRequest() ([]byte, [12]byte) {
	var txid [12]byte
	rand.Read(txid[:])
	b := binary.BigEndian.AppendUint16(nil, 0x0001) // Binding request
	b = binary.BigEndian.AppendUint16(b, 0)         // no attributes
	b = binary.BigEndian.AppendUint32(b, stunCookie)
	return append(b, txid[:]...), txid
}

// stunSuccess reports whether b is a Binding success response and its txid.
func stunSuccess(b []byte) (txid [12]byte, ok bool) {
	if len(b) < 20 || binary.BigEndian.Uint16(b) != 0x0101 || binary.BigEndian.Uint32(b[4:]) != stunCookie ||
		int(binary.BigEndian.Uint16(b[2:]))+20 != len(b) {
		return txid, false
	}
	copy(txid[:], b[8:20])
	return txid, true
}

// stunMapped decodes an IPv4 XOR-MAPPED-ADDRESS: the address the server saw.
func stunMapped(b []byte) (netip.AddrPort, bool) {
	for a := b[20:]; len(a) >= 4; {
		typ, n := binary.BigEndian.Uint16(a), int(binary.BigEndian.Uint16(a[2:]))
		if len(a) < 4+n {
			break
		}
		v := a[4 : 4+n]
		if typ == 0x0020 && n == 8 && v[1] == 0x01 {
			var ip [4]byte
			binary.BigEndian.PutUint32(ip[:], binary.BigEndian.Uint32(v[4:])^stunCookie)
			return netip.AddrPortFrom(netip.AddrFrom4(ip), binary.BigEndian.Uint16(v[2:])^stunCookie>>16), true
		}
		a = a[4+(n+3)&^3:]
	}
	return netip.AddrPort{}, false
}
