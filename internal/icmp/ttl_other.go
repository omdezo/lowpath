//go:build !windows

package icmp

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"runtime"
	"syscall"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// TTL-limited echo through a raw ICMP socket, which needs root (or
// CAP_NET_RAW on Linux). Each probe opens its own socket: a raw socket
// sees every ICMP message the PC receives, and matching on the probe's own
// ID and sequence keeps concurrent probes (one per hop) apart.

func pingTTL4(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	return rawTTL(dst, ttl, timeout, false)
}

func pingTTL6(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	return rawTTL(dst, ttl, timeout, true)
}

func rawTTL(dst netip.Addr, ttl int, timeout time.Duration, v6 bool) (Reply, error) {
	network, laddr, proto := "ip4:icmp", "0.0.0.0", 1
	var reqType, replyType icmp.Type = ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply
	if v6 {
		network, laddr, proto = "ip6:ipv6-icmp", "::", 58
		reqType, replyType = ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return Reply{}, fmt.Errorf("icmp: a route trace on %s needs root or CAP_NET_RAW: %w", runtime.GOOS, err)
		}
		return Reply{}, err
	}
	defer c.Close()
	if v6 {
		err = c.IPv6PacketConn().SetHopLimit(ttl)
	} else {
		err = c.IPv4PacketConn().SetTTL(ttl)
	}
	if err != nil {
		return Reply{}, fmt.Errorf("icmp: set TTL: %w", err)
	}

	id, seq := uint16(rand.IntN(0x10000)), uint16(rand.IntN(0x10000))
	// The kernel fills in the ICMPv6 checksum on raw sockets.
	msg := icmp.Message{Type: reqType, Body: &icmp.Echo{ID: int(id), Seq: int(seq), Data: payloadTTL[:]}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return Reply{}, err
	}
	c.SetReadDeadline(time.Now().Add(timeout))
	start := time.Now()
	if _, err := c.WriteTo(b, &net.IPAddr{IP: dst.AsSlice(), Zone: dst.Zone()}); err != nil {
		if errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.EADDRNOTAVAIL) {
			return Reply{}, ErrNoRoute
		}
		return Reply{}, err
	}
	want := dst.WithZone("")
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
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
			if m.Type == replyType && uint16(body.ID) == id && uint16(body.Seq) == seq && peer == want {
				return Reply{From: peer, RTT: rtt, Kind: Echo}, nil
			}
		case *icmp.TimeExceeded:
			if q, qid, qseq, ok := quotedEcho(body.Data, v6); ok && q == want && qid == id && qseq == seq {
				return Reply{From: peer, RTT: rtt, Kind: TimeExceeded}, nil
			}
		case *icmp.DstUnreach:
			if q, qid, qseq, ok := quotedEcho(body.Data, v6); ok && q == want && qid == id && qseq == seq {
				return Reply{From: peer, RTT: rtt, Kind: Unreachable}, nil
			}
		}
	}
}

func peerAddr(a net.Addr) (netip.Addr, bool) {
	ia, ok := a.(*net.IPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	p, ok := netip.AddrFromSlice(ia.IP)
	return p.Unmap(), ok
}
