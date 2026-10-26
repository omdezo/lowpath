package icmp

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSender answers every probe the same way and counts them.
type fakeSender struct {
	from  string
	err   error
	calls int
}

func (f *fakeSender) sendTTL(netip.Addr, int, time.Duration) (Reply, error) {
	f.calls++
	if f.err != nil {
		return Reply{}, f.err
	}
	return Reply{From: netip.MustParseAddr(f.from), Kind: TimeExceeded, RTT: time.Millisecond}, nil
}

func TestTTLChain(t *testing.T) {
	denied := fmt.Errorf("socket: %w", os.ErrPermission)
	missing := fmt.Errorf("socket: protocol not supported: %w", errors.ErrUnsupported)
	cases := []struct {
		name          string
		raw, ping     error
		wantFrom      string // "" when the probe fails
		wantErr       error
		rawN, pingN   int // calls after two probes
		wantNeedsText bool
	}{
		{name: "raw socket works", wantFrom: "192.0.2.1", rawN: 2, pingN: 0},
		{name: "raw refused, ping socket works", raw: denied, wantFrom: "198.51.100.1", rawN: 1, pingN: 2},
		{name: "raw missing, ping socket works", raw: missing, wantFrom: "198.51.100.1", rawN: 2, pingN: 2},
		{name: "both refused", raw: denied, ping: denied, wantErr: os.ErrPermission, rawN: 1, pingN: 2, wantNeedsText: true},
		{name: "raw refused, ping socket missing", raw: denied, ping: missing, wantErr: os.ErrPermission, rawN: 1, pingN: 2, wantNeedsText: true},
		{name: "both missing", raw: missing, ping: missing, wantErr: errors.ErrUnsupported, rawN: 2, pingN: 2},
		{name: "no route is an answer", raw: ErrNoRoute, wantErr: ErrNoRoute, rawN: 2, pingN: 0},
		{name: "silence is an answer", raw: ErrTimeout, wantErr: ErrTimeout, rawN: 2, pingN: 0},
		{name: "other errors are answers", raw: errors.New("sendto: message too long"), wantErr: nil, rawN: 2, pingN: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := &fakeSender{from: "192.0.2.1", err: c.raw}
			ping := &fakeSender{from: "198.51.100.1", err: c.ping}
			chain := newTTLChain("root or a ping socket", raw, ping)
			for range 2 {
				r, err := chain.sendTTL(netip.MustParseAddr("203.0.113.9"), 3, time.Second)
				switch {
				case c.wantFrom != "":
					if err != nil || r.From != netip.MustParseAddr(c.wantFrom) {
						t.Fatalf("got %+v, %v; want a reply from %s", r, err, c.wantFrom)
					}
				case c.wantErr != nil:
					if !errors.Is(err, c.wantErr) {
						t.Fatalf("got %v, want %v", err, c.wantErr)
					}
				default:
					if err == nil || errors.Is(err, os.ErrPermission) || errors.Is(err, errors.ErrUnsupported) {
						t.Fatalf("got %v, want the sender's own error", err)
					}
				}
				if got := err != nil && strings.Contains(err.Error(), "needs root or a ping socket"); got != c.wantNeedsText {
					t.Fatalf("error %q: names what the OS needs = %v, want %v", err, got, c.wantNeedsText)
				}
			}
			if raw.calls != c.rawN || ping.calls != c.pingN {
				t.Fatalf("calls: raw %d, ping %d; want %d, %d", raw.calls, ping.calls, c.rawN, c.pingN)
			}
		})
	}
}

func TestTTLChainEmpty(t *testing.T) {
	if _, err := newTTLChain("root").sendTTL(netip.MustParseAddr("192.0.2.1"), 1, time.Second); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("got %v, want errors.ErrUnsupported", err)
	}
}
