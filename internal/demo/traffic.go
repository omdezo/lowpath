package demo

import (
	"context"
	"net"
	"net/netip"
	"time"
)

// Traffic plays the part of a running game: it sends tick-rate UDP packets to
// addr (the engine's local proxy port) and reads the server's echoes, so the
// engine carries real game-shaped traffic during the demo.
func Traffic(ctx context.Context, addr netip.AddrPort, tickRate int) error {
	c, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	go func() {
		buf := make([]byte, 2048)
		for {
			if _, err := c.Read(buf); err != nil && ctx.Err() != nil {
				return
			}
		}
	}()
	pkt := make([]byte, 96)
	t := time.NewTicker(time.Second / time.Duration(tickRate))
	defer t.Stop()
	for i := uint32(0); ; i++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			pkt[0], pkt[1], pkt[2], pkt[3] = byte(i>>24), byte(i>>16), byte(i>>8), byte(i)
			c.Write(pkt)
		}
	}
}
