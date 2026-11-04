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
	c, raw, err := listen4()
	if err != nil {
		return 0, err
	}
	defer c.Close()

	p := echo4{dst: dst, raw: raw, id: rand.IntN(0xffff), seq: rand.IntN(0xffff)}
	r := newReplyReader(c, time.Now().Add(timeout))
	start, err := p.send(c)
	if err != nil {
		return 0, err
	}
	return p.await(r, start)
}

// listen4 opens a raw socket when privileged (relays run as root under
// systemd), else an unprivileged "ping socket" (needs
// net.ipv4.ping_group_range). raw says which.
func listen4() (c *icmp.PacketConn, raw bool, err error) {
	if c, err = icmp.ListenPacket("ip4:icmp", "0.0.0.0"); err == nil {
		return c, true, nil
	}
	c, err = icmp.ListenPacket("udp4", "0.0.0.0")
	return c, false, err
}

// echo4 is one IPv4 echo request and the reply that answers it.
type echo4 struct {
	dst     netip.Addr
	raw     bool // sent through a raw socket, which keeps our ID
	id, seq int
}

// send sends the echo request through c and returns when it did.
func (p echo4) send(c net.PacketConn) (time.Time, error) {
	msg := icmp.Message{Type: ipv4.ICMPTypeEcho, Body: &icmp.Echo{ID: p.id, Seq: p.seq, Data: []byte("lowpath")}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return time.Time{}, err
	}
	var to net.Addr = &net.IPAddr{IP: p.dst.AsSlice()}
	if !p.raw {
		to = &net.UDPAddr{IP: p.dst.AsSlice()}
	}
	start := time.Now()
	if _, err := c.WriteTo(b, to); err != nil {
		return time.Time{}, err
	}
	return start, nil
}

// await reads until the echo reply arrives and returns the time from start
// to reading it (which includes a stall the read outlasted: see
// replyReader), or ErrTimeout.
func (p echo4) await(r *replyReader, start time.Time) (time.Duration, error) {
	buf := make([]byte, 1500)
	for {
		n, from, err := r.ReadFrom(buf)
		if err != nil {
			return 0, ErrTimeout
		}
		rtt := time.Since(start)
		m, err := icmp.ParseMessage(1, buf[:n])
		if err != nil || m.Type != ipv4.ICMPTypeEchoReply {
			continue
		}
		e, ok := m.Body.(*icmp.Echo)
		if !ok || e.Seq != p.seq || (p.raw && e.ID != p.id) {
			continue // the kernel rewrites the ID on ping sockets
		}
		if fa, ok := netip.AddrFromSlice(addrIP(from)); !ok || fa.Unmap() != p.dst {
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
