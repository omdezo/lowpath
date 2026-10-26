package relay

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"lowpath/internal/proto"
)

func startForwarder(t *testing.T, next netip.AddrPort, limit int) *Forwarder {
	t.Helper()
	f, err := ListenForwarder("127.0.0.1:0", next, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if limit > 0 {
		f.limit = limit
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.Serve(ctx)
	return f
}

func dialForwarder(t *testing.T, f *Forwarder) *net.UDPConn {
	t.Helper()
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(f.Addr()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func loopback(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), a.Port())
}

// waitUp polls until the hop has passed n packets on to the next hop.
func waitUp(t *testing.T, f *Forwarder, n uint64) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if f.Stats().PacketsUp >= n {
			return
		}
	}
	t.Fatalf("hop passed on %d packets, want %d", f.Stats().PacketsUp, n)
}

func TestForwarderRelaysBothWays(t *testing.T) {
	game := newEcho(t)
	e := startEgress(t, nil)
	f := startForwarder(t, loopback(e.Addr()), 0)
	a, b := dialForwarder(t, f), dialForwarder(t, f)

	h := proto.Header{Type: proto.TData, Flow: 9, Seq: 1, Addr: game.addr()}
	send(t, a, h, []byte("via hop"))
	if got, p, err := recv(a, 2*time.Second); err != nil || got.Flow != 9 || !bytes.Equal(p, []byte("via hop")) {
		t.Fatalf("reply through the hop: %+v %q %v", got, p, err)
	}
	// Another client address gets its own socket, so the egress sees a
	// separate return path and replies reach both.
	h.Seq = 2
	send(t, b, h, []byte("second"))
	for name, c := range map[string]*net.UDPConn{"a": a, "b": b} {
		if !recvPayload(c, []byte("second"), 2*time.Second) {
			t.Errorf("client %s missed the reply", name)
		}
	}
	if s := f.Stats(); s.Peers != 2 || s.PacketsUp != 2 || s.PacketsDown < 3 {
		t.Fatalf("stats %+v", s)
	}
}

func TestForwarderIgnoresJunk(t *testing.T) {
	next := silentServer(t)
	f := startForwarder(t, udpAddr(next), 0)
	c := dialForwarder(t, f)
	c.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	send(t, c, proto.Header{Type: proto.TProbe, Flow: 1, Seq: 1}, nil)

	next.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2048)
	n, err := next.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if h, _, err := proto.Parse(buf[:n]); err != nil || h.Type != proto.TProbe {
		t.Fatalf("next hop got %q, want only the overlay packet", buf[:n])
	}
	// The forwarder counts a packet just after sending it on, so the next
	// hop can see it a moment before the counter moves.
	s := f.Stats()
	for end := time.Now().Add(time.Second); s.PacketsUp == 0 && time.Now().Before(end); s = f.Stats() {
		time.Sleep(5 * time.Millisecond)
	}
	if s.Malformed != 1 || s.Peers != 1 || s.PacketsUp != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// Client addresses the next hop never answers (spoofed sources, wrong keys)
// lose their sockets quickly, and make room for new clients when the hop is
// full; clients that get answers are never pushed out.
func TestForwarderPeerLimits(t *testing.T) {
	next := silentServer(t)
	f := startForwarder(t, udpAddr(next), 2)
	probe := proto.Header{Type: proto.TProbe, Flow: 1, Seq: 1}
	var conns []*net.UDPConn
	for range 3 {
		c := dialForwarder(t, f)
		conns = append(conns, c)
		send(t, c, probe, nil)
	}
	waitUp(t, f, 3)
	if s := f.Stats(); s.Peers != 2 || s.Evicted != 1 || s.LimitDrops != 0 {
		t.Fatalf("stats %+v, want the third client to evict an unanswered one", s)
	}
	f.expire(time.Now().Add(unansweredPeerTTL + time.Second))
	if n := f.Stats().Peers; n != 0 {
		t.Fatalf("%d unanswered client sockets kept past %v", n, unansweredPeerTTL)
	}

	game := newEcho(t)
	e := startEgress(t, nil)
	g := startForwarder(t, loopback(e.Addr()), 2)
	data := proto.Header{Type: proto.TData, Flow: 2, Seq: 1, Addr: game.addr()}
	for i := range 2 {
		c := dialForwarder(t, g)
		data.Seq = uint32(i + 1)
		send(t, c, data, []byte("hi"))
		if _, _, err := recv(c, 2*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	late := dialForwarder(t, g)
	send(t, late, data, []byte("hi"))
	if _, _, err := recv(late, 300*time.Millisecond); err == nil {
		t.Fatal("a client got through a hop full of answered clients")
	}
	if s := g.Stats(); s.Peers != 2 || s.LimitDrops != 1 || s.Evicted != 0 {
		t.Fatalf("stats %+v, want the newcomer refused and nobody evicted", s)
	}
	g.expire(time.Now().Add(unansweredPeerTTL + time.Second))
	if n := g.Stats().Peers; n != 2 {
		t.Fatalf("answered clients dropped after %v; they should last %v", unansweredPeerTTL, peerTTL)
	}
	g.expire(time.Now().Add(peerTTL + time.Second))
	if n := g.Stats().Peers; n != 0 {
		t.Fatalf("%d client sockets kept past %v of silence", n, peerTTL)
	}
}
