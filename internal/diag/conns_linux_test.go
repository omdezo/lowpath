package diag

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"
)

// TestSockDiagCountsRealConnections opens real TCP connections over IPv4
// and IPv6 loopback, moves a known amount each way, and finds them in the
// kernel's dump with their byte counters and this process as the owner.
// No rights are needed (run it as nobody too).
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
