package icmp

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
	"golang.org/x/sys/unix"
)

// ttlNeeds is what Linux wants when neither socket below can be opened.
const ttlNeeds = "root, CAP_NET_RAW, or net.ipv4.ping_group_range covering one of your groups"

// pingSocket sends through a Linux ping socket (SOCK_DGRAM with
// IPPROTO_ICMP or IPPROTO_ICMPV6), which needs no privileges when
// net.ipv4.ping_group_range covers one of the process's groups (the same
// setting governs IPv6; systemd opens it to every group, Docker in its
// containers). Unlike a raw socket it sees only its own traffic: the kernel
// sets the echo ID to the socket's own and delivers the destination's echo
// reply as a datagram, but a router's "time exceeded" or "unreachable"
// never as one. With IP_RECVERR (IPV6_RECVERR) on, those wait on the
// socket's error queue: recvmsg with MSG_ERRQUEUE returns the echo they
// quote as data, the destination as its address, and a control message
// with struct sock_extended_err followed by the router's address
// (SO_EE_OFFENDER). See IP_RECVERR(2const), icmp(7) and ping_err in
// net/ipv4/ping.c; iputils' ping -t reads routers' errors the same way.
type pingSocket struct{ v6 bool }

func unprivilegedTTL(v6 bool) ttlSender { return pingSocket{v6: v6} }

func (p pingSocket) sendTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	family, proto := unix.AF_INET, unix.IPPROTO_ICMP
	level, recvErr, ttlOpt := unix.IPPROTO_IP, unix.IP_RECVERR, unix.IP_TTL
	var reqType icmp.Type = ipv4.ICMPTypeEcho
	if p.v6 {
		family, proto = unix.AF_INET6, unix.IPPROTO_ICMPV6
		level, recvErr, ttlOpt = unix.IPPROTO_IPV6, unix.IPV6_RECVERR, unix.IPV6_UNICAST_HOPS
		reqType = ipv6.ICMPTypeEchoRequest
	}
	fd, err := unix.Socket(family, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return Reply{}, socketError(os.NewSyscallError("socket", err))
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptInt(fd, level, recvErr, 1); err != nil {
		return Reply{}, fmt.Errorf("icmp: ask for ICMP errors: %w", os.NewSyscallError("setsockopt", err))
	}
	if err := unix.SetsockoptInt(fd, level, ttlOpt, ttl); err != nil {
		return Reply{}, fmt.Errorf("icmp: set TTL: %w", os.NewSyscallError("setsockopt", err))
	}
	to, err := sockaddr(dst)
	if err != nil {
		return Reply{}, err
	}

	// The kernel replaces the ID and fills in the checksum.
	seq := uint16(rand.IntN(0x10000))
	msg := icmp.Message{Type: reqType, Body: &icmp.Echo{Seq: int(seq), Data: payloadTTL[:]}}
	b, err := msg.Marshal(nil)
	if err != nil {
		return Reply{}, err
	}
	start := time.Now()
	if err := unix.Sendto(fd, b, 0, to); err != nil {
		return Reply{}, sendError(os.NewSyscallError("sendto", err))
	}
	q := pingQueue{fd: fd, v6: p.v6, level: level, recvErr: recvErr, dst: dst.WithZone(""), seq: seq}
	return q.wait(start, start.Add(timeout))
}

// pingQueue reads one probe's answer from its ping socket.
type pingQueue struct {
	fd             int
	v6             bool
	level, recvErr int // the control message that carries a queued error
	dst            netip.Addr
	seq            uint16
}

// wait polls the socket until the probe's answer arrives or the deadline
// passes. It blocks in poll(2) rather than in Go's network poller: the
// poller reports a wakeup with only EPOLLERR set, which is how a queued
// error announces itself, as an error of the read (ErrNotPollable).
func (q pingQueue) wait(start, deadline time.Time) (Reply, error) {
	buf := make([]byte, 1500)
	oob := make([]byte, 512)
	fds := []unix.PollFd{{Fd: int32(q.fd), Events: unix.POLLIN}}
	for {
		if r, ok := q.queued(buf, oob); ok {
			r.RTT = time.Since(start)
			return r, nil
		}
		if r, ok := q.echo(buf); ok {
			r.RTT = time.Since(start)
			return r, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return Reply{}, ErrTimeout
		}
		// POLLERR is reported whether asked for or not.
		ms := int((left + time.Millisecond - 1) / time.Millisecond)
		if _, err := unix.Poll(fds, ms); err != nil && !errors.Is(err, unix.EINTR) {
			return Reply{}, os.NewSyscallError("poll", err)
		}
	}
}

// queued reads the socket's error queue for a router's answer to the probe.
func (q pingQueue) queued(buf, oob []byte) (Reply, bool) {
	for {
		n, oobn, _, from, err := unix.Recvmsg(q.fd, buf, oob, unix.MSG_ERRQUEUE|unix.MSG_DONTWAIT)
		if err != nil {
			return Reply{}, false // EAGAIN: the queue is empty
		}
		// The data is the echo the error quotes, from its ICMP header on;
		// the address, when the kernel gives one, the destination it was
		// sent to. The queue holds this socket's errors only, so both are
		// a check, not a search.
		if !q.quotesProbe(buf[:n]) {
			continue
		}
		if a := sockaddrAddr(from); a.IsValid() && a != q.dst {
			continue
		}
		msgs, err := unix.ParseSocketControlMessage(oob[:oobn])
		if err != nil {
			continue
		}
		for _, m := range msgs {
			if int(m.Header.Level) != q.level || int(m.Header.Type) != q.recvErr {
				continue
			}
			e, ok := parseExtendedErr(m.Data)
			if !ok {
				continue
			}
			if kind, ok := e.kind(); ok {
				return Reply{From: e.Offender.Unmap(), Kind: kind}, true
			}
		}
	}
}

// echo reads the destination's echo reply. A read can also return the
// error the kernel set on the socket along with the queued one; that is
// read once and gone.
func (q pingQueue) echo(buf []byte) (Reply, bool) {
	proto := 1
	var replyType icmp.Type = ipv4.ICMPTypeEchoReply
	if q.v6 {
		proto, replyType = 58, ipv6.ICMPTypeEchoReply
	}
	for range 16 {
		n, from, err := unix.Recvfrom(q.fd, buf, unix.MSG_DONTWAIT)
		if errors.Is(err, unix.EAGAIN) {
			return Reply{}, false
		}
		if err != nil {
			continue
		}
		m, err := icmp.ParseMessage(proto, buf[:n])
		if err != nil || m.Type != replyType {
			continue
		}
		e, ok := m.Body.(*icmp.Echo)
		if !ok || uint16(e.Seq) != q.seq {
			continue
		}
		if peer := sockaddrAddr(from); peer == q.dst {
			return Reply{From: peer, Kind: Echo}, true
		}
	}
	return Reply{}, false
}

// quotesProbe reports whether b, an echo quoted by an ICMP error, is this
// probe's echo request.
func (q pingQueue) quotesProbe(b []byte) bool {
	req := byte(8)
	if q.v6 {
		req = 128
	}
	return len(b) >= 8 && b[0] == req && uint16(b[6])<<8|uint16(b[7]) == q.seq
}

func sockaddr(a netip.Addr) (unix.Sockaddr, error) {
	if a.Is4() {
		return &unix.SockaddrInet4{Addr: a.As4()}, nil
	}
	sa := &unix.SockaddrInet6{Addr: a.As16()}
	if z := a.Zone(); z != "" {
		if n, err := strconv.Atoi(z); err == nil {
			sa.ZoneId = uint32(n)
		} else if ifi, err := net.InterfaceByName(z); err == nil {
			sa.ZoneId = uint32(ifi.Index)
		} else {
			return nil, fmt.Errorf("icmp: zone %q: %w", z, err)
		}
	}
	return sa, nil
}

// sockaddrAddr is the address of a socket address, without a zone.
func sockaddrAddr(sa unix.Sockaddr) netip.Addr {
	switch v := sa.(type) {
	case *unix.SockaddrInet4:
		return netip.AddrFrom4(v.Addr)
	case *unix.SockaddrInet6:
		return netip.AddrFrom16(v.Addr).Unmap()
	}
	return netip.Addr{}
}
