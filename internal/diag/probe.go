package diag

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Prober measures one round trip to a fixed target. Probe must honor ctx
// (its deadline is the probe timeout) and be safe to call concurrently: the
// bufferbloat test sends the next probe before a slow one has answered.
type Prober interface {
	// Name says what is measured, for the UI ("Gateway 192.168.1.1 (ICMP)").
	Name() string
	Probe(ctx context.Context) (time.Duration, error)
}

// PingFunc sends one ICMP echo (icmp.Ping in production).
type PingFunc func(dst netip.Addr, timeout time.Duration) (time.Duration, error)

// ICMPProber pings an address.
type ICMPProber struct {
	label string
	addr  netip.Addr
	ping  PingFunc
}

// NewICMPProber pings addr with ping; label names the target ("Gateway").
func NewICMPProber(label string, addr netip.Addr, ping PingFunc) *ICMPProber {
	return &ICMPProber{label: label, addr: addr, ping: ping}
}

func (p *ICMPProber) Name() string { return fmt.Sprintf("%s %s (ICMP)", p.label, p.addr) }

func (p *ICMPProber) Probe(ctx context.Context) (time.Duration, error) {
	timeout := time.Second
	if dl, ok := ctx.Deadline(); ok {
		timeout = time.Until(dl)
	}
	if timeout <= 0 {
		return 0, context.DeadlineExceeded
	}
	return p.ping(p.addr, timeout)
}

// TCPProber times a TCP handshake (connect, then close at once). It works
// where ICMP is blocked or needs privileges, at the cost of a SYN/ACK
// exchange per sample, so it is the last resort.
type TCPProber struct {
	label string
	addr  netip.AddrPort
	d     net.Dialer
}

// NewTCPProber connects to addr; label names the target.
func NewTCPProber(label string, addr netip.AddrPort) *TCPProber {
	return &TCPProber{label: label, addr: addr}
}

func (p *TCPProber) Name() string { return fmt.Sprintf("%s %s (TCP connect)", p.label, p.addr) }

func (p *TCPProber) Probe(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	c, err := p.d.DialContext(ctx, "tcp", p.addr.String())
	if err != nil {
		return 0, err
	}
	rtt := time.Since(start)
	c.Close()
	return rtt, nil
}

// FuncProber adapts a function (the app's relay probe, test fakes).
type FuncProber struct {
	label string
	fn    func(ctx context.Context) (time.Duration, error)
}

// NewFuncProber wraps fn.
func NewFuncProber(label string, fn func(ctx context.Context) (time.Duration, error)) *FuncProber {
	return &FuncProber{label: label, fn: fn}
}

func (p *FuncProber) Name() string                                     { return p.label }
func (p *FuncProber) Probe(ctx context.Context) (time.Duration, error) { return p.fn(ctx) }
