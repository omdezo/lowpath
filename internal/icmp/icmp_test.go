package icmp

import (
	"net/netip"
	"testing"
	"time"
)

func TestPingLoopback(t *testing.T) {
	rtt, err := Ping(netip.MustParseAddr("127.0.0.1"), time.Second)
	if err != nil {
		t.Skipf("ICMP not available here: %v", err)
	}
	if rtt < 0 || rtt > time.Second {
		t.Fatalf("implausible loopback RTT %v", rtt)
	}
}

func TestPingRejectsIPv6(t *testing.T) {
	if _, err := Ping(netip.MustParseAddr("::1"), time.Second); err != ErrUnsupported {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}
