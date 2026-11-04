package diag

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"lowpath/internal/netlink"
)

// diagReply is one established socket as a SOCK_DIAG_BY_FAMILY dump
// answers it (struct inet_diag_msg and an INET_DIAG_INFO tcp_info with the
// Linux 4.1 byte counters), the layout sockdiag.Parse reads.
func diagReply(cookie uint64, local, remote string, acked, received uint64) netlink.Reply {
	l, r := netip.MustParseAddrPort(local), netip.MustParseAddrPort(remote)
	b := make([]byte, 72)
	b[0], b[1] = 2, 1 // AF_INET, ESTABLISHED
	if l.Addr().Is6() {
		b[0] = 10
	}
	binary.BigEndian.PutUint16(b[4:], l.Port())
	binary.BigEndian.PutUint16(b[6:], r.Port())
	copy(b[8:], l.Addr().AsSlice())
	copy(b[24:], r.Addr().AsSlice())
	binary.NativeEndian.PutUint32(b[44:], uint32(cookie))
	binary.NativeEndian.PutUint32(b[48:], uint32(cookie>>32))
	info := make([]byte, 136)
	binary.NativeEndian.PutUint64(info[120:], acked)
	binary.NativeEndian.PutUint64(info[128:], received)
	var a netlink.Attrs
	a.Add(2, info)
	return netlink.Reply{Type: 20, Data: append(b, a.Bytes()...)}
}

// fakeDump answers the IPv4 and IPv6 dumps with one connection each, or
// with the error given for that family.
func fakeDump(v4err, v6err error) func(netlink.Message) ([]netlink.Reply, error) {
	return func(m netlink.Message) ([]netlink.Reply, error) {
		if m.Data[0] == 10 { // AF_INET6
			if v6err != nil {
				return nil, v6err
			}
			return []netlink.Reply{diagReply(6, "[2001:db8:0:1::10]:50006", "[2001:db8:0:1::20]:445", 600, 6000)}, nil
		}
		if v4err != nil {
			return nil, v4err
		}
		return []netlink.Reply{diagReply(4, "192.0.2.10:50004", "203.0.113.5:443", 400, 4000)}, nil
	}
}

// refused is a dump the kernel refused, wrapped as netlink.Conn.Dump
// returns it.
func refused(family string, errno syscall.Errno) error {
	return &netlink.Error{Op: "dump " + family + " TCP sockets", Errno: errno}
}

// Only a kernel without IPv6 sock_diag may skip the IPv6 dump quietly; any
// other IPv6 failure is reported with the IPv4 connections, and an IPv4
// failure fails the reading.
func TestSockDiagDumpFailures(t *testing.T) {
	timeout := errors.New("netlink: no answer from the kernel within 3s")
	cases := []struct {
		name         string
		v4err, v6err error
		ids          []uint64 // the connections returned
		partial      bool     // a connsPartial error
		unsupported  bool
		errLike      error // what the error must match; nil: no error unless unsupported
	}{
		{name: "both read", ids: []uint64{4, 6}},
		// CONFIG_IPV6=n: no AF_INET6 handler, so no IPv6 sockets to miss.
		{name: "no IPv6 sock_diag in the kernel", v6err: refused("IPv6", syscall.ENOENT), ids: []uint64{4}},
		{name: "IPv6 dump denied (EACCES)", v6err: refused("IPv6", syscall.EACCES), ids: []uint64{4}, partial: true, errLike: syscall.EACCES},
		{name: "IPv6 dump denied (EPERM)", v6err: refused("IPv6", syscall.EPERM), ids: []uint64{4}, partial: true, errLike: syscall.EPERM},
		{name: "IPv6 dump timed out", v6err: timeout, ids: []uint64{4}, partial: true, errLike: timeout},
		{name: "IPv4 dump denied", v4err: refused("IPv4", syscall.EACCES), errLike: syscall.EACCES},
		{name: "IPv4 dump timed out", v4err: timeout, errLike: timeout},
		{name: "IPv4 and IPv6 dumps denied", v4err: refused("IPv4", syscall.EACCES), v6err: refused("IPv6", syscall.EACCES), errLike: syscall.EACCES},
		{name: "no inet_diag in the kernel", v4err: refused("IPv4", syscall.ENOENT), unsupported: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &sockDiag{owners: map[uint64]uint32{}}
			conns, err := s.read(fakeDump(c.v4err, c.v6err))
			var ids []uint64
			for _, x := range conns {
				ids = append(ids, x.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(c.ids) {
				t.Errorf("connections %v, want %v", ids, c.ids)
			}
			var partial connsPartial
			if got := errors.As(err, &partial); got != c.partial {
				t.Errorf("partial = %v, want %v (err: %v)", got, c.partial, err)
			}
			if got := isUnsupported(err); got != c.unsupported {
				t.Errorf("unsupported = %v, want %v (err: %v)", got, c.unsupported, err)
			}
			if c.errLike != nil && !errors.Is(err, c.errLike) {
				t.Errorf("err = %v, want one matching %v", err, c.errLike)
			}
			if c.errLike == nil && !c.unsupported && err != nil {
				t.Errorf("err = %v, want none", err)
			}
		})
	}
}

// TestSockDiagCountsRealConnections opens real TCP connections over IPv4
// and IPv6 loopback, moves a known amount each way, and finds them in the
// kernel's dump with their byte counters and this process as the owner.
// No rights are needed (run it as nobody too). A partial reading fails it
// too: on a kernel booted with ipv6.disable=1, which has no IPv6 loopback
// to test, the IPv6 dump must still answer, empty.
func TestSockDiagCountsRealConnections(t *testing.T) {
	cs := newPlatformConnStats()
	if _, err := cs.TCPConns(); errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("no per-connection counters on this kernel: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			host := "127.0.0.1:0"
			if network == "tcp6" {
				host = "[::1]:0"
			}
			ln, err := net.Listen(network, host)
			if err != nil {
				t.Skipf("no %s loopback: %v", network, err)
			}
			defer ln.Close()
			const up, down = 3 << 20, 5 << 20
			// The server keeps its end open until the dump: a closed socket
			// becomes a FIN_WAIT2 or TIME_WAIT mini-socket without tcp_info.
			served, done := make(chan error, 1), make(chan struct{})
			defer close(done)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					served <- err
					return
				}
				defer c.Close()
				if _, err := io.CopyN(io.Discard, c, up); err != nil {
					served <- err
					return
				}
				_, err = c.Write(make([]byte, down))
				served <- err
				<-done
			}()
			c, err := net.Dial(network, ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			if _, err := c.Write(make([]byte, up)); err != nil {
				t.Fatal(err)
			}
			if _, err := io.CopyN(io.Discard, c, down); err != nil {
				t.Fatal(err)
			}
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			local := netip.MustParseAddrPort(c.LocalAddr().String())
			// Both ends finished, but the last ACKs may still be on their
			// way through loopback: wait for them briefly.
			var client, server TCPConn
			deadline := time.Now().Add(2 * time.Second)
			for {
				conns, err := cs.TCPConns()
				if err != nil {
					t.Fatal(err)
				}
				client, server = TCPConn{}, TCPConn{}
				for _, x := range conns {
					switch {
					case x.Local == local:
						client = x
					case x.Remote == local:
						server = x
					}
				}
				if client.Sent >= up && client.Received >= down && server.Received >= up && server.Sent >= down || time.Now().After(deadline) {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if client.ID == 0 || server.ID == 0 || client.ID == server.ID {
				t.Fatalf("connections not found or not told apart: client %+v, server %+v", client, server)
			}
			// tcp_info counts payload, plus one for the SYN (and the FIN):
			// a sequence number each.
			near := func(got, want uint64) bool { return got >= want && got <= want+2 }
			if !near(client.Sent, up) || !near(client.Received, down) || !near(server.Received, up) || !near(server.Sent, down) {
				t.Errorf("client sent %d, received %d; server received %d, sent %d; want %d and %d",
					client.Sent, client.Received, server.Received, server.Sent, up, down)
			}
			owners := cs.Owners([]TCPConn{client, server})
			if pid := uint32(os.Getpid()); owners[client.ID] != pid || owners[server.ID] != pid {
				t.Errorf("owners = %v, want this process (%d) for both", owners, pid)
			}
			t.Logf("%s: client %v sent %d, received %d (cookie %#x)", network, client.Local, client.Sent, client.Received, client.ID)
		})
	}
}
