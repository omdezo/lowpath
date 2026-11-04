//go:build !windows

package icmp

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// TTL-limited echo on Linux, macOS and the BSDs: a raw ICMP socket when the
// process may open one (root, or CAP_NET_RAW on Linux), else the system's
// unprivileged ICMP socket (ttl_linux.go, ttl_darwin.go). Each probe opens
// its own socket, so concurrent probes (one per hop) never share state.

var ttl4, ttl6 = newTTLChain(ttlNeeds, ttlSenders(false)...), newTTLChain(ttlNeeds, ttlSenders(true)...)

func ttlSenders(v6 bool) []ttlSender {
	senders := []ttlSender{rawSocket{v6: v6}}
	if s := unprivilegedTTL(v6); s != nil {
		senders = append(senders, s)
	}
	return senders
}

func pingTTL4(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	return ttl4.sendTTL(dst, ttl, timeout)
}

func pingTTL6(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	return ttl6.sendTTL(dst, ttl, timeout)
}

// rawSocket sends through a raw ICMP socket.
type rawSocket struct{ v6 bool }

func (r rawSocket) sendTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	network, laddr := "ip4:icmp", "0.0.0.0"
	if r.v6 {
		network, laddr = "ip6:ipv6-icmp", "::"
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		return Reply{}, socketError(err)
	}
	defer c.Close()
	return echoTTL(c, &net.IPAddr{IP: dst.AsSlice(), Zone: dst.Zone()}, dst, ttl, timeout, r.v6)
}

// socketError maps the errors of opening a socket: an address family the
// PC doesn't have (IPv6 turned off) is ErrNoRoute, a socket type the
// system lacks (Linux before 3.11 has no IPv6 ping socket) matches
// errors.ErrUnsupported. Permission errors already match os.ErrPermission.
func socketError(err error) error {
	switch {
	case errors.Is(err, syscall.EAFNOSUPPORT):
		return ErrNoRoute
	case errors.Is(err, syscall.EPROTONOSUPPORT), errors.Is(err, syscall.ESOCKTNOSUPPORT):
		return fmt.Errorf("%w: %w", err, errors.ErrUnsupported)
	}
	return err
}

// sendError maps a send that failed because the PC can't reach dst to
// ErrNoRoute. A send the system refuses (EPERM from a firewall rule that
// drops the probe, EACCES from a "prohibit" route or a broadcast address)
// is this probe's answer, not a socket the process may not open: it no
// longer matches os.ErrPermission, so the chain doesn't give up on the
// socket for every later probe and the trace doesn't ask for root.
func sendError(err error) error {
	switch {
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.EADDRNOTAVAIL):
		return ErrNoRoute
	case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES):
		return fmt.Errorf("icmp: the system refused to send the probe (a firewall rule or a route forbids it): %v", err)
	}
	return err
}

// echoTTL sends one echo with the given TTL through c, a socket that
// receives every ICMP message the PC gets (a raw socket, or a macOS
// datagram one), and waits for the destination's echo reply or a router's
// error quoting the probe.
func echoTTL(c *icmp.PacketConn, to net.Addr, dst netip.Addr, ttl int, timeout time.Duration, v6 bool) (Reply, error) {
	p := ttlProbe{dst: dst.WithZone(""), id: uint16(rand.IntN(0x10000)), seq: uint16(rand.IntN(0x10000)), v6: v6}
	r := newReplyReader(c, time.Now().Add(timeout))
	start, err := p.send(c, to, ttl)
	if err != nil {
		return Reply{}, err
	}
	return p.await(r, start)
}

// ttlProbe is one TTL-limited echo request. Matching answers on the
// probe's own ID and sequence keeps concurrent probes apart.
type ttlProbe struct {
	dst     netip.Addr // without a zone, as answers report it
	id, seq uint16
	v6      bool
}

// send sets c's TTL and sends the echo request, and returns when it did.
func (p ttlProbe) send(c *icmp.PacketConn, to net.Addr, ttl int) (time.Time, error) {
	var reqType icmp.Type = ipv4.ICMPTypeEcho
	var err error
	if p.v6 {
		reqType = ipv6.ICMPTypeEchoRequest
		err = c.IPv6PacketConn().SetHopLimit(ttl)
	} else {
		err = c.IPv4PacketConn().SetTTL(ttl)
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("icmp: set TTL: %w", err)
	}
	// The kernel fills in the ICMPv6 checksum on these sockets.
	msg := icmp.Message{Type: reqType, Body: &icmp.Echo{ID: int(p.id), Seq: int(p.seq), Data: payloadTTL[:]}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return time.Time{}, err
	}
	start := time.Now()
	if _, err := c.WriteTo(b, to); err != nil {
		return time.Time{}, sendError(err)
	}
	return start, nil
}

// await reads until the destination's echo reply or a router's error
// quoting the probe arrives, timed from start to reading it (which
// includes a stall the read outlasted: see replyReader), or ErrTimeout.
func (p ttlProbe) await(r *replyReader, start time.Time) (Reply, error) {
	proto := 1
	var replyType icmp.Type = ipv4.ICMPTypeEchoReply
	if p.v6 {
		proto, replyType = 58, ipv6.ICMPTypeEchoReply
	}
	buf := make([]byte, 1500)
	for {
		n, from, err := r.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return Reply{}, ErrTimeout
			}
			return Reply{}, err
		}
		rtt := time.Since(start)
		peer, ok := peerAddr(from)
		if !ok {
			continue
		}
		m, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil {
			continue
		}
		switch body := m.Body.(type) {
		case *icmp.Echo:
			if m.Type == replyType && uint16(body.ID) == p.id && uint16(body.Seq) == p.seq && peer == p.dst {
				return Reply{From: peer, RTT: rtt, Kind: Echo}, nil
			}
		case *icmp.TimeExceeded:
			if q, qid, qseq, ok := quotedEcho(body.Data, p.v6); ok && q == p.dst && qid == p.id && qseq == p.seq {
				return Reply{From: peer, RTT: rtt, Kind: TimeExceeded}, nil
			}
		case *icmp.DstUnreach:
			if q, qid, qseq, ok := quotedEcho(body.Data, p.v6); ok && q == p.dst && qid == p.id && qseq == p.seq {
				return Reply{From: peer, RTT: rtt, Kind: Unreachable}, nil
			}
		}
	}
}

// peerAddr is the sender of a message: a raw socket reports a *net.IPAddr,
// a datagram one a *net.UDPAddr.
func peerAddr(a net.Addr) (netip.Addr, bool) {
	p, ok := netip.AddrFromSlice(addrIP(a))
	return p.Unmap(), ok
}
