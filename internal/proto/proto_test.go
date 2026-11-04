package proto

import (
	"bytes"
	"net/netip"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	cases := []Header{
		{Type: TData, Token: TokenFromKey("k"), Flow: 0xDEADBEEFCAFEF00D, Seq: 42, TS: 1234, Addr: netip.MustParseAddrPort("203.0.113.7:7777")},
		{Type: TData, Flow: 1, Seq: 0xFFFFFFFF, TS: 9, Addr: netip.MustParseAddrPort("[2001:db8::1]:443")},
		{Type: TProbe, Flow: 7, Seq: 3, TS: 99},
	}
	payload := []byte("hello game server")
	for _, h := range cases {
		b := Append(nil, h, payload)
		got, p, err := Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		if got != h {
			t.Fatalf("header mismatch: got %+v want %+v", got, h)
		}
		if !bytes.Equal(p, payload) {
			t.Fatalf("payload mismatch")
		}
	}
}

func TestMappedV4IsUnmapped(t *testing.T) {
	h := Header{Type: TData, Addr: netip.MustParseAddrPort("[::ffff:10.0.0.1]:80")}
	got, _, err := Parse(Append(nil, h, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got.Addr.String() != "10.0.0.1:80" {
		t.Fatalf("got %v", got.Addr)
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, _, err := Parse([]byte{1, 2, 3}); err != ErrShort {
		t.Fatalf("want ErrShort, got %v", err)
	}
	b := Append(nil, Header{Type: TProbe}, nil)
	b[0] = 0
	if _, _, err := Parse(b); err != ErrMagic {
		t.Fatalf("want ErrMagic, got %v", err)
	}
	b = Append(nil, Header{Type: TProbe}, nil)
	b[26] = 5
	if _, _, err := Parse(b); err != ErrAddr {
		t.Fatalf("want ErrAddr, got %v", err)
	}
}

func TestServerRTT(t *testing.T) {
	if d, ok := ParseServerRTT(AppendServerRTT(nil, 23456*time.Microsecond)); !ok || d != 23456*time.Microsecond {
		t.Fatalf("got %v %v", d, ok)
	}
	if _, ok := ParseServerRTT(AppendServerRTT(nil, -1)); ok {
		t.Fatal("unknown RTT decoded as known")
	}
	if _, ok := ParseServerRTT(nil); ok {
		t.Fatal("empty payload decoded as known")
	}
}

func TestServerLegRoundTrip(t *testing.T) {
	cases := []ServerLeg{
		{RTT: 23456 * time.Microsecond, LossPct: 12.34, Jitter: 1500 * time.Microsecond, HasRTT: true, HasLoss: true, HasJitter: true},
		{RTT: 9 * time.Millisecond, LossPct: 0, Jitter: 0, HasRTT: true, HasLoss: true, HasJitter: true}, // a clean leg: known zeros
		{LossPct: 100, HasLoss: true},
		{RTT: time.Millisecond, HasRTT: true}, // loss and jitter not measured yet
		{},                                    // nothing known
	}
	for _, l := range cases {
		b := AppendServerLeg(nil, l)
		if len(b) != ServerLegLen {
			t.Fatalf("%+v: %d bytes, want %d", l, len(b), ServerLegLen)
		}
		if got := ParseServerLeg(b); got != l {
			t.Errorf("round trip: got %+v, want %+v", got, l)
		}
	}
	// Loss is carried in hundredths of a percent and clamped to 0..100.
	for in, want := range map[float64]float64{1.234: 1.23, 1.236: 1.24, -3: 0, 250: 100} {
		if got := ParseServerLeg(AppendServerLeg(nil, ServerLeg{LossPct: in, HasLoss: true})); !got.HasLoss || got.LossPct != want {
			t.Errorf("loss %v: got %v/%v, want %v", in, got.LossPct, got.HasLoss, want)
		}
	}
	// Values the unit can't carry never read as known: the jitter saturates
	// below the "unknown" mark, a loss over 100% is refused.
	if got := ParseServerLeg(AppendServerLeg(nil, ServerLeg{Jitter: 2 * time.Hour, HasJitter: true})); !got.HasJitter || got.Jitter >= 2*time.Hour {
		t.Errorf("huge jitter: %+v", got)
	}
	b := AppendServerLeg(nil, ServerLeg{})
	b[4], b[5] = 0x27, 0x11 // 10001 hundredths: 100.01%
	if got := ParseServerLeg(b); got.HasLoss {
		t.Errorf("loss over 100%% decoded as %v", got.LossPct)
	}
}

// A new relay's reply read by a client that predates loss and jitter: it
// parses with ParseServerRTT, which looks at the first four bytes only.
func TestServerLegNewReplyOldClient(t *testing.T) {
	pkt := Append(nil, Header{Type: TProbeReply, Flow: 9, Seq: 4, Addr: netip.MustParseAddrPort("203.0.113.7:7777")},
		AppendServerLeg(nil, ServerLeg{RTT: 31 * time.Millisecond, LossPct: 5, Jitter: 3 * time.Millisecond, HasRTT: true, HasLoss: true, HasJitter: true}))
	h, p, err := Parse(pkt)
	if err != nil || h.Type != TProbeReply {
		t.Fatalf("header %+v %v", h, err)
	}
	if d, ok := ParseServerRTT(p); !ok || d != 31*time.Millisecond {
		t.Fatalf("old parser read %v/%v, want 31ms", d, ok)
	}
	if _, ok := ParseServerRTT(AppendServerLeg(nil, ServerLeg{LossPct: 5, HasLoss: true})); ok {
		t.Fatal("old parser took an unknown RTT with a known loss as known")
	}
}

// An older relay's reply read by a new client: the RTT, and loss and jitter
// unknown rather than 0.
func TestServerLegOldReplyNewClient(t *testing.T) {
	l := ParseServerLeg(AppendServerRTT(nil, 18*time.Millisecond))
	if !l.HasRTT || l.RTT != 18*time.Millisecond || l.HasLoss || l.HasJitter {
		t.Fatalf("4-byte reply: %+v", l)
	}
	if l := ParseServerLeg(AppendServerRTT(nil, -1)); l != (ServerLeg{}) {
		t.Fatalf("unknown RTT: %+v", l)
	}
	if l := ParseServerLeg(nil); l != (ServerLeg{}) {
		t.Fatalf("empty payload: %+v", l)
	}
	// A truncated block keeps what it has: loss without the jitter field.
	full := AppendServerLeg(nil, ServerLeg{RTT: time.Millisecond, LossPct: 2.5, Jitter: time.Millisecond, HasRTT: true, HasLoss: true, HasJitter: true})
	if l := ParseServerLeg(full[:6]); !l.HasRTT || !l.HasLoss || l.LossPct != 2.5 || l.HasJitter {
		t.Fatalf("6-byte reply: %+v", l)
	}
	// A future relay may append more: the known fields still decode.
	if l := ParseServerLeg(append(full, 0xAB, 0xCD)); !l.HasRTT || !l.HasLoss || !l.HasJitter || l.Jitter != time.Millisecond {
		t.Fatalf("longer reply: %+v", l)
	}
}

func TestTokenFromKey(t *testing.T) {
	if TokenFromKey("") != 0 {
		t.Fatal("empty key must give the open-relay token")
	}
	if a, b := TokenFromKey("alpha"), TokenFromKey("beta"); a == 0 || a == b || a != TokenFromKey("alpha") {
		t.Fatalf("tokens: %x %x", a, b)
	}
}
