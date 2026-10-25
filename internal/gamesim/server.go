// Package gamesim is a stand-in game server: it echoes every UDP datagram back
// to the sender, which is enough for a client to measure the latency, loss and
// jitter it actually experiences.
package gamesim

import (
	"context"
	"errors"
	"net"
	"net/netip"
)

type Server struct{ conn *net.UDPConn }

func Listen(addr string) (*Server, error) {
	ua, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", ua)
	if err != nil {
		return nil, err
	}
	return &Server{conn: c}, nil
}

func (s *Server) Addr() netip.AddrPort { return s.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.conn.Close()
	}()
	buf := make([]byte, 65535)
	for {
		n, src, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			continue // transient (e.g. ICMP unreachable on Windows)
		}
		s.conn.WriteToUDPAddrPort(buf[:n], src)
	}
}
