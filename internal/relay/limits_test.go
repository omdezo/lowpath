package relay

import (
	"bytes"
	"net"
	"net/netip"
	"testing"
	"time"

	"lowpath/internal/proto"
)

// silentServer is a game server that answers only when the test says so.
func silentServer(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func udpAddr(c *net.UDPConn) netip.AddrPort { return c.LocalAddr().(*net.UDPAddr).AddrPort() }

// barrier sends a probe and waits for its reply, skipping data packets: the
// egress handles packets in arrival order, so once the reply is back it has
// handled everything c sent before.
func barrier(t *testing.T, c *net.UDPConn) {
	t.Helper()
	send(t, c, proto.Header{Type: proto.TProbe, Flow: 99, Seq: 1}, nil)
	end := time.Now().Add(2 * time.Second)
	for {
		h, _, err := recv(c, time.Until(end))
		if err != nil {
			t.Fatalf("probe reply: %v", err)
		}
		if h.Type == proto.TProbeReply {
			return
		}
	}
}

// recvPayload waits for a data packet carrying want, skipping others.
func recvPayload(c *net.UDPConn, want []byte, wait time.Duration) bool {
	end := time.Now().Add(wait)
	for time.Now().Before(end) {
		_, p, err := recv(c, time.Until(end))
		if err != nil {
			return false
		}
		if bytes.Equal(p, want) {
			return true
		}
	}
	return false
}

// A flow's replies go to at most maxPaths client addresses, however many
// sources send for it: each reply is copied to every one of them.
func TestReplyAddressesCapped(t *testing.T) {
	game := newEcho(t)
	e := startEgress(t, nil)
	var conns []*net.UDPConn
	for i := range maxPaths + 2 {
		c := dialEgress(t, e)
		conns = append(conns, c)
		send(t, c, proto.Header{Type: proto.TData, Flow: 3, Seq: uint32(i + 1), Addr: game.addr()}, []byte("hi"))
		barrier(t, c) // keeps the order of first packets, which decides who is in
	}
	send(t, conns[0], proto.Header{Type: proto.TData, Flow: 3, Seq: 100, Addr: game.addr()}, []byte("last"))
	got := 0
	for i, c := range conns {
		if recvPayload(c, []byte("last"), 300*time.Millisecond) {
			got++
			if i >= maxPaths {
				t.Errorf("source %d got the reply though %d sources came before it", i, maxPaths)
			}
		}
	}
	if got != maxPaths {
		t.Fatalf("reply copied to %d sources, want %d", got, maxPaths)
	}
}

// When the game goes quiet for longer than pathTTL (a map load) while the
// server keeps sending, the replies must still reach the paths it used last.
func TestRepliesContinueWhileClientIsQuiet(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out pathTTL")
	}
	srv := silentServer(t)
	e := startEgress(t, nil)
	a, b := dialEgress(t, e), dialEgress(t, e)
	for _, c := range []*net.UDPConn{a, b} {
		send(t, c, proto.Header{Type: proto.TData, Flow: 4, Seq: 1, Addr: udpAddr(srv)}, []byte("join"))
	}
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	_, relayAddr, err := srv.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	barrier(t, b) // b's copy has been seen too

	time.Sleep(pathTTL + 500*time.Millisecond)
	srv.WriteToUDPAddrPort([]byte("state"), relayAddr)
	for name, c := range map[string]*net.UDPConn{"a": a, "b": b} {
		if !recvPayload(c, []byte("state"), 2*time.Second) {
			t.Errorf("path %s: server packet dropped after %v of client silence", name, pathTTL)
		}
	}
}

func TestOversizedServerPacketCounted(t *testing.T) {
	srv := silentServer(t)
	e := startEgress(t, nil)
	c := dialEgress(t, e)
	send(t, c, proto.Header{Type: proto.TData, Flow: 5, Seq: 1, Addr: udpAddr(srv)}, []byte("hi"))
	srv.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	_, relayAddr, err := srv.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	srv.WriteToUDPAddrPort(make([]byte, maxDatagram+1), relayAddr)
	srv.WriteToUDPAddrPort(make([]byte, maxDatagram), relayAddr)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	big := make([]byte, 65535)
	n, err := c.Read(big)
	if err != nil {
		t.Fatal(err)
	}
	if h, p, err := proto.Parse(big[:n]); err != nil || h.Type != proto.TData || len(p) != maxDatagram {
		t.Fatalf("want the %d-byte packet relayed whole, got %d bytes %v", maxDatagram, len(p), err)
	}
	if s := e.Stats(); s.TooBig != 1 || s.PacketsDown != 1 {
		t.Fatalf("stats %+v, want one too-big drop and one packet down", s)
	}
}

// Flows the server never answered go after quietTTL; answered ones stay
// until flowTTL, except that a full table reclaims every quiet flow.
func TestQuietFlowsExpireEarly(t *testing.T) {
	game := newEcho(t)
	silent := silentServer(t)
	e := startEgress(t, nil)
	c := dialEgress(t, e)
	send(t, c, proto.Header{Type: proto.TData, Flow: 1, Seq: 1, Addr: game.addr()}, []byte("hi"))
	if _, _, err := recv(c, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	send(t, c, proto.Header{Type: proto.TData, Flow: 2, Seq: 1, Addr: udpAddr(silent)}, []byte("anyone?"))
	barrier(t, c)
	if n := e.Stats().Flows; n != 2 {
		t.Fatalf("%d flows, want 2", n)
	}

	later := time.Now().Add(quietTTL + time.Second)
	e.expireFlows(later)
	e.mu.Lock()
	_, answered := e.flows[1]
	_, unanswered := e.flows[2]
	e.mu.Unlock()
	if !answered || unanswered {
		t.Fatalf("after quietTTL: answered flow kept=%v, unanswered kept=%v; want true, false", answered, unanswered)
	}

	e.mu.Lock()
	e.reclaimLocked(later)
	n := len(e.flows)
	e.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d flows left after reclaiming quiet ones", n)
	}
}
