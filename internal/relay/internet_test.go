package relay

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"lowpath/internal/proto"
)

// TestInternetSTUN relays a real STUN binding request to Google through a
// keyed, policy-enforcing egress using the real ICMP pinger. It needs internet
// access, so it only runs with LOWPATH_INTERNET=1.
func TestInternetSTUN(t *testing.T) {
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		t.Skip("set LOWPATH_INTERNET=1 to run against stun.l.google.com")
	}
	ua, err := net.ResolveUDPAddr("udp4", "stun.l.google.com:19302")
	if err != nil {
		t.Fatal(err)
	}
	server := ua.AddrPort()
	server = netip.AddrPortFrom(server.Addr().Unmap(), server.Port())
	key := "integration-key"
	e := startEgress(t, &Options{Key: key}) // default Ping = icmp.Ping
	c := dialEgress(t, e)
	token := proto.TokenFromKey(key)

	// RFC 5389 Binding request: type 0x0001, length 0, magic cookie, txid.
	req := []byte{0x00, 0x01, 0x00, 0x00, 0x21, 0x12, 0xA4, 0x42}
	txid := make([]byte, 12)
	rand.Read(txid)
	req = append(req, txid...)
	send(t, c, proto.Header{Type: proto.TData, Token: token, Flow: 1, Seq: 1, Addr: server}, req)
	h, p, err := recv(c, 3*time.Second)
	if err != nil {
		t.Fatalf("no STUN reply through the relay: %v", err)
	}
	if h.Token != token || h.Addr != server || len(p) < 20 ||
		binary.BigEndian.Uint16(p) != 0x0101 || !bytes.Equal(p[8:20], txid) {
		t.Fatalf("unexpected reply %+v % x", h, p)
	}

	// Google answers ping, so the server leg should become known.
	deadline := time.Now().Add(5 * time.Second)
	for seq := uint32(1); ; seq++ {
		send(t, c, proto.Header{Type: proto.TProbe, Token: token, Flow: 2, Seq: seq, Addr: server}, nil)
		_, p, err := recv(c, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := proto.ParseServerRTT(p); ok {
			t.Logf("relay -> %s ICMP RTT %v", server.Addr(), d)
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server leg never became known")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Logf("stats %+v", e.Stats())
}
