package relay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/proto"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// echoServer is a stand-in game server on loopback that counts what it gets.
type echoServer struct {
	conn *net.UDPConn
	got  atomic.Int64
}

func newEcho(t *testing.T) *echoServer {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := &echoServer{conn: c}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, src, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			s.got.Add(1)
			c.WriteToUDPAddrPort(buf[:n], src)
		}
	}()
	return s
}

func (s *echoServer) addr() netip.AddrPort { return s.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

// startEgress serves a relay on loopback: the open ListenEgress when o is nil
// (the constructor the demo network and the app's test relay use), else
// ListenEgressOpts.
func startEgress(t *testing.T, o *Options) *Egress {
	t.Helper()
	var e *Egress
	var err error
	if o == nil {
		e, err = ListenEgress("127.0.0.1:0", quiet)
	} else {
		e, err = ListenEgressOpts("127.0.0.1:0", *o, quiet)
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.Serve(ctx)
	return e
}

func dialEgress(t *testing.T, e *Egress) *net.UDPConn {
	t.Helper()
	to := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), e.Addr().Port())
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(to))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func send(t *testing.T, c *net.UDPConn, h proto.Header, payload []byte) {
	t.Helper()
	if _, err := c.Write(proto.Append(nil, h, payload)); err != nil {
		t.Fatal(err)
	}
}

func recv(c *net.UDPConn, wait time.Duration) (proto.Header, []byte, error) {
	c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 2048)
	n, err := c.Read(buf)
	if err != nil {
		return proto.Header{}, nil, err
	}
	h, p, err := proto.Parse(buf[:n])
	return h, append([]byte(nil), p...), err
}

// probeSync sends a probe and waits for its reply. The egress handles packets
// one at a time in arrival order, so once the reply is back every packet this
// client sent earlier has been processed.
func probeSync(t *testing.T, c *net.UDPConn, token uint64) {
	t.Helper()
	send(t, c, proto.Header{Type: proto.TProbe, Token: token, Flow: 99, Seq: 1}, nil)
	h, _, err := recv(c, 2*time.Second)
	if err != nil || h.Type != proto.TProbeReply {
		t.Fatalf("probe reply: %+v %v", h, err)
	}
}

func okPing(rtt time.Duration) func(netip.Addr, time.Duration) (time.Duration, error) {
	return func(netip.Addr, time.Duration) (time.Duration, error) { return rtt, nil }
}

func TestOpenRelayDedupsAndRepliesOnEveryPath(t *testing.T) {
	game := newEcho(t)
	e := startEgress(t, nil)
	a, b := dialEgress(t, e), dialEgress(t, e)
	h := proto.Header{Type: proto.TData, Token: 12345, Flow: 7, Seq: 1, Addr: game.addr()}

	send(t, a, h, []byte("one"))
	got, p, err := recv(a, 2*time.Second)
	if err != nil || !bytes.Equal(p, []byte("one")) || got.Flow != 7 || got.Addr != game.addr() {
		t.Fatalf("reply on path a: %+v %q %v", got, p, err)
	}
	if got.Token != 12345 {
		t.Fatalf("reply token %d, want the client's 12345 echoed", got.Token)
	}

	// The duplicate over path b is dropped but makes b a live return path.
	send(t, b, h, []byte("one"))
	probeSync(t, b, 0)

	h.Seq = 2
	send(t, a, h, []byte("two"))
	for name, c := range map[string]*net.UDPConn{"a": a, "b": b} {
		if _, p, err := recv(c, 2*time.Second); err != nil || !bytes.Equal(p, []byte("two")) {
			t.Fatalf("path %s: %q %v", name, p, err)
		}
	}
	if n := game.got.Load(); n != 2 {
		t.Fatalf("game server got %d packets, want 2 (duplicate must be dropped)", n)
	}
	s := e.Stats()
	if s.Flows != 1 || s.PacketsUp != 2 || s.Duplicates != 1 || s.PacketsDown != 2 {
		t.Fatalf("stats %+v", s)
	}
}

func TestTokenRequired(t *testing.T) {
	game := newEcho(t)
	e := startEgress(t, &Options{Key: "secret", AllowPrivate: true, Ping: okPing(time.Millisecond)})
	c := dialEgress(t, e)
	good := proto.TokenFromKey("secret")
	h := proto.Header{Type: proto.TData, Flow: 1, Seq: 1, Addr: game.addr()}

	send(t, c, h, []byte("no token"))
	h.Seq, h.Token = 2, proto.TokenFromKey("wrong")
	send(t, c, h, []byte("wrong token"))
	send(t, c, proto.Header{Type: proto.TProbe, Flow: 2, Seq: 1}, nil)

	h.Seq, h.Token = 3, good
	send(t, c, h, []byte("right token"))
	got, p, err := recv(c, 2*time.Second)
	if err != nil || got.Type != proto.TData || !bytes.Equal(p, []byte("right token")) {
		t.Fatalf("want only the keyed packet echoed, got %+v %q %v", got, p, err)
	}
	if got.Token != good {
		t.Fatalf("reply token %x, want %x", got.Token, good)
	}
	probeSync(t, c, good)
	if n := game.got.Load(); n != 1 {
		t.Fatalf("game server got %d packets, want 1", n)
	}
	if s := e.Stats(); s.TokenDrops != 3 || s.PacketsUp != 1 || s.Probes != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestPrivateDestinationRefused(t *testing.T) {
	game := newEcho(t)
	var pinged atomic.Int64
	ping := func(netip.Addr, time.Duration) (time.Duration, error) {
		pinged.Add(1)
		return time.Millisecond, nil
	}
	e := startEgress(t, &Options{Ping: ping, PingInterval: 10 * time.Millisecond})
	c := dialEgress(t, e)

	send(t, c, proto.Header{Type: proto.TData, Flow: 1, Seq: 1, Addr: game.addr()}, []byte("hi"))
	send(t, c, proto.Header{Type: proto.TProbe, Flow: 2, Seq: 1, Addr: game.addr()}, nil)
	h, p, err := recv(c, 2*time.Second)
	if err != nil || h.Type != proto.TProbeReply {
		t.Fatalf("want the probe reply first (data must be refused), got %+v %v", h, err)
	}
	if _, ok := proto.ParseServerRTT(p); ok {
		t.Fatal("refused destination must report an unknown server RTT")
	}
	time.Sleep(50 * time.Millisecond)
	if n := game.got.Load(); n != 0 {
		t.Fatalf("game server on loopback got %d packets", n)
	}
	if n := pinged.Load(); n != 0 {
		t.Fatalf("refused destination was pinged %d times", n)
	}
	if s := e.Stats(); s.PolicyDrops != 1 || s.Flows != 0 || s.Servers != 0 {
		t.Fatalf("stats %+v", s)
	}

	// The same packet is relayed when private destinations are allowed.
	open := startEgress(t, &Options{AllowPrivate: true, Ping: ping})
	oc := dialEgress(t, open)
	send(t, oc, proto.Header{Type: proto.TData, Flow: 1, Seq: 1, Addr: game.addr()}, []byte("hi"))
	if _, p, err := recv(oc, 2*time.Second); err != nil || string(p) != "hi" {
		t.Fatalf("AllowPrivate relay: %q %v", p, err)
	}
}

func TestOwnAddressRefused(t *testing.T) {
	e, err := ListenEgressOpts("127.0.0.1:0", Options{Ping: okPing(0)}, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	self := addrSet{netip.MustParseAddr("203.0.113.9"): {}}
	e.self.Store(&self)
	if e.allowed(netip.MustParseAddr("203.0.113.9")) {
		t.Fatal("relay's own address allowed")
	}
	if e.allowed(netip.MustParseAddr("::ffff:203.0.113.9")) {
		t.Fatal("relay's own address allowed in v4-mapped form")
	}
	if !e.allowed(netip.MustParseAddr("203.0.113.10")) {
		t.Fatal("public address refused")
	}
}

func TestProbeReplyCarriesServerRTT(t *testing.T) {
	server := netip.MustParseAddrPort("203.0.113.7:7777")
	ping := func(a netip.Addr, _ time.Duration) (time.Duration, error) {
		if a != server.Addr() {
			return 0, errors.New("unexpected address")
		}
		return 23 * time.Millisecond, nil
	}
	e := startEgress(t, &Options{Ping: ping, PingInterval: 10 * time.Millisecond})
	c := dialEgress(t, e)

	// The first probe starts the pinger; poll until its result shows up.
	deadline := time.Now().Add(2 * time.Second)
	for seq := uint32(1); ; seq++ {
		send(t, c, proto.Header{Type: proto.TProbe, Flow: 5, Seq: seq, TS: 77, Addr: server}, nil)
		h, p, err := recv(c, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if h.Type != proto.TProbeReply || h.Flow != 5 || h.Seq != seq || h.TS != 77 || h.Addr != server {
			t.Fatalf("probe header not echoed: %+v", h)
		}
		if d, ok := proto.ParseServerRTT(p); ok {
			if d != 23*time.Millisecond {
				t.Fatalf("server RTT %v, want 23ms", d)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server RTT never became known")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// A probe without a server address still answers, with "unknown".
	send(t, c, proto.Header{Type: proto.TProbe, Flow: 5, Seq: 1000}, nil)
	_, p, err := recv(c, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 4 {
		t.Fatalf("payload %d bytes, want a 4-byte RTT block", len(p))
	}
	if _, ok := proto.ParseServerRTT(p); ok {
		t.Fatal("probe without Addr must report unknown")
	}
}

func TestProbeReplyUnknownWhenPingFails(t *testing.T) {
	var calls atomic.Int64
	ping := func(netip.Addr, time.Duration) (time.Duration, error) {
		calls.Add(1)
		return 0, errors.New("no reply")
	}
	e := startEgress(t, &Options{Ping: ping, PingInterval: 5 * time.Millisecond})
	c := dialEgress(t, e)
	server := netip.MustParseAddrPort("198.51.100.20:27015")
	for seq := uint32(1); seq <= 10; seq++ {
		send(t, c, proto.Header{Type: proto.TProbe, Flow: 1, Seq: seq, Addr: server}, nil)
		_, p, err := recv(c, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if d, ok := proto.ParseServerRTT(p); ok {
			t.Fatalf("got RTT %v from a server that never answers", d)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("server was never pinged")
	}
}
