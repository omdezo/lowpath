package stun

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"
)

// xorMapped encodes a XOR-MAPPED-ADDRESS value for ap under transaction id.
func xorMapped(ap netip.AddrPort, id TxID) []byte {
	var key [16]byte
	binary.BigEndian.PutUint32(key[0:4], MagicCookie)
	copy(key[4:], id[:])
	ip := ap.Addr().AsSlice()
	fam := byte(familyIPv4)
	if ap.Addr().Is6() {
		fam = familyIPv6
	}
	v := []byte{0, fam}
	v = binary.BigEndian.AppendUint16(v, ap.Port()^uint16(MagicCookie>>16))
	for i, b := range ip {
		v = append(v, b^key[i])
	}
	return v
}

// fakeServer is a local STUN server. reply decides, per request, what to
// send back (nil: nothing).
type fakeServer struct {
	conn  *net.UDPConn
	seen  atomic.Int32
	reply func(n int32, id TxID, from netip.AddrPort) [][]byte
}

func startServer(t *testing.T, network, addr string, reply func(n int32, id TxID, from netip.AddrPort) [][]byte) *fakeServer {
	t.Helper()
	ua, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		t.Skip(err)
	}
	conn, err := net.ListenUDP(network, ua)
	if err != nil {
		t.Skipf("can't listen on %s: %v", addr, err)
	}
	s := &fakeServer{conn: conn, reply: reply}
	t.Cleanup(func() { conn.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
			if n < HeaderLen || binary.BigEndian.Uint16(buf[0:2]) != typeBindingRequest {
				continue
			}
			var id TxID
			copy(id[:], buf[8:20])
			k := s.seen.Add(1)
			for _, out := range s.reply(k, id, from) {
				conn.WriteToUDPAddrPort(out, from)
			}
		}
	}()
	return s
}

func (s *fakeServer) addr() string { return s.conn.LocalAddr().String() }

// answer replies to every request with the sender's own address.
func answer(_ int32, id TxID, from netip.AddrPort) [][]byte {
	return [][]byte{response(id, typeBindingSuccess, attr(attrXorMappedAddress, xorMapped(from, id)))}
}

func fastClient() *Client {
	return NewClient(ClientOptions{RTO: 30 * time.Millisecond, MaxRTO: 60 * time.Millisecond, Attempts: 3})
}

func TestBindReturnsMappedAddress(t *testing.T) {
	for _, tc := range []struct{ network, addr string }{
		{"udp4", "127.0.0.1:0"},
		{"udp6", "[::1]:0"},
	} {
		s := startServer(t, tc.network, tc.addr, answer)
		b, err := fastClient().Bind(context.Background(), tc.network, s.addr())
		if err != nil {
			t.Fatalf("%s: %v", tc.network, err)
		}
		if !b.Mapped.Addr().IsLoopback() || b.Mapped.Port() == 0 {
			t.Fatalf("%s: mapped %v", tc.network, b.Mapped)
		}
		if b.Server.String() != s.addr() {
			t.Fatalf("%s: server %v, want %v", tc.network, b.Server, s.addr())
		}
		if !b.RTTKnown {
			t.Fatalf("%s: first-try answer should have an RTT, got %v", tc.network, b.RTT)
		}
	}
}

func TestBindResendsAndIgnoresStrays(t *testing.T) {
	s := startServer(t, "udp4", "127.0.0.1:0", func(n int32, id TxID, from netip.AddrPort) [][]byte {
		if n < 3 {
			return nil // lose the first two requests
		}
		other := id
		other[0] ^= 0xFF
		return [][]byte{
			[]byte("not stun at all"),
			response(other, typeBindingSuccess, attr(attrXorMappedAddress, xorMapped(netip.MustParseAddrPort("192.0.2.9:1"), other))),
			AppendBindingRequest(nil, id), // a request with our id is not an answer
			response(id, typeBindingSuccess, attr(attrXorMappedAddress, xorMapped(netip.MustParseAddrPort("203.0.113.5:4242"), id))),
		}
	})
	b, err := fastClient().Bind(context.Background(), "udp4", s.addr())
	if err != nil {
		t.Fatal(err)
	}
	if b.Mapped != netip.MustParseAddrPort("203.0.113.5:4242") {
		t.Fatalf("mapped %v", b.Mapped)
	}
	if b.RTTKnown {
		t.Fatalf("RTT after a resend is ambiguous and must be unknown, got %v", b.RTT)
	}
	if got := s.seen.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

func TestBindTimesOut(t *testing.T) {
	s := startServer(t, "udp4", "127.0.0.1:0", func(int32, TxID, netip.AddrPort) [][]byte { return nil })
	start := time.Now()
	_, err := fastClient().Bind(context.Background(), "udp4", s.addr())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v, want ErrTimeout", err)
	}
	// 30 + 60 + 60 ms of waiting.
	if el := time.Since(start); el < 140*time.Millisecond || el > 2*time.Second {
		t.Fatalf("gave up after %v", el)
	}
	if got := s.seen.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3", got)
	}
}

func TestBindHonorsContext(t *testing.T) {
	s := startServer(t, "udp4", "127.0.0.1:0", func(int32, TxID, netip.AddrPort) [][]byte { return nil })
	slow := NewClient(ClientOptions{RTO: 5 * time.Second, Attempts: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := slow.Bind(ctx, "udp4", s.addr())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if el := time.Since(start); el > time.Second {
		t.Fatalf("cancel took %v", el)
	}
}

func TestBindServerError(t *testing.T) {
	s := startServer(t, "udp4", "127.0.0.1:0", func(_ int32, id TxID, _ netip.AddrPort) [][]byte {
		return [][]byte{response(id, typeBindingError, attr(attrErrorCode, append([]byte{0, 0, 5, 0}, "Server Error"...)))}
	})
	_, err := fastClient().Bind(context.Background(), "udp4", s.addr())
	if err == nil || errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v, want the server's error", err)
	}
}

func TestBindClosedPortFailsFast(t *testing.T) {
	// Grab a free port and close it again: nothing listens there.
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	addr := c.LocalAddr().String()
	c.Close()
	start := time.Now()
	if _, err := NewClient(ClientOptions{RTO: 200 * time.Millisecond, Attempts: 3}).Bind(context.Background(), "udp4", addr); err == nil {
		t.Fatal("bind to a closed port succeeded")
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("closed port took %v", el)
	}
}

func TestBindRejectsOtherNetworks(t *testing.T) {
	if _, err := fastClient().Bind(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("tcp accepted")
	}
}
