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

func TestTokenFromKey(t *testing.T) {
	if TokenFromKey("") != 0 {
		t.Fatal("empty key must give the open-relay token")
	}
	if a, b := TokenFromKey("alpha"), TokenFromKey("beta"); a == 0 || a == b || a != TokenFromKey("alpha") {
		t.Fatalf("tokens: %x %x", a, b)
	}
}
