//go:build !windows

package icmp

import (
	"math/rand/v2"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
)

func ping4(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	// Raw socket when privileged (relays run as root under systemd), else an
	// unprivileged "ping socket" (needs net.ipv4.ping_group_range).
	raw := true
	c, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		raw = false
		if c, err = icmp.ListenPacket("udp4", "0.0.0.0"); err != nil {
			return 0, err
		}
	}
	defer c.Close()

	id, seq := rand.IntN(0xffff), rand.IntN(0xffff)
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: id, Seq: seq, Data: []byte("lowpath")}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return 0, err
	}
	var to net.Addr = &net.IPAddr{IP: dst.AsSlice()}
	if !raw {
		to = &net.UDPAddr{IP: dst.AsSlice()}
	}
	deadline := time.Now().Add(timeout)
	c.SetReadDeadline(deadline)
	start := time.Now()
	if _, err := c.WriteTo(b, to); err != nil {
		return 0, err
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return 0, ErrTimeout
		}
		rtt := time.Since(start)
		m, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		e, ok := m.Body.(*icmp.Echo)
		if !ok || e.Seq != seq || (raw && e.ID != id) {
			continue // the kernel rewrites the ID on ping sockets
		}
		if fa, ok := netip.AddrFromSlice(addrIP(from)); !ok || fa.Unmap() != dst {
			continue
		}
		return rtt, nil
	}
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPAddr:
		return v.IP
	case *net.UDPAddr:
		return v.IP
	}
	return nil
}
