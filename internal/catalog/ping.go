package catalog

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"lowpath/internal/icmp"
)

// Pinger sends one echo request and returns the round trip time. A missing
// reply is ErrNoReply; other errors mean the PC can't send pings at all.
type Pinger interface {
	Ping(dst netip.Addr, timeout time.Duration) (time.Duration, error)
}

var (
	ErrNoReply = errors.New("no reply")
	ErrIPv6    = errors.New("IPv6 ping is not supported")
)

// ICMPPinger pings with internal/icmp: IcmpSendEcho on Windows (no admin
// rights needed), a raw or unprivileged ping socket elsewhere.
// Send replaces icmp.Ping (tests); nil = icmp.Ping.
type ICMPPinger struct {
	Send func(dst netip.Addr, timeout time.Duration) (time.Duration, error)
}

func (p ICMPPinger) Ping(dst netip.Addr, timeout time.Duration) (time.Duration, error) {
	send := p.Send
	if send == nil {
		send = icmp.Ping
	}
	rtt, err := send(dst, timeout)
	switch {
	case errors.Is(err, icmp.ErrTimeout):
		return 0, ErrNoReply
	case errors.Is(err, icmp.ErrUnsupported):
		return 0, ErrIPv6
	}
	return rtt, err
}

// POPPing is the result of pinging one POP's relays.
type POPPing struct {
	RTT      time.Duration // best reply; meaningful only when Received > 0
	Sent     int
	Received int
	Note     string // why there is no number, when Received == 0
	At       time.Time
}

// LossPct is the share of echo requests that got no reply.
func (p POPPing) LossPct() float64 {
	if p.Sent == 0 {
		return 0
	}
	return 100 * float64(p.Sent-p.Received) / float64(p.Sent)
}

// POPProber measures the direct ping from this PC to every POP: a few echo
// requests to up to two relays per POP, the best reply counting (the
// minimum is the path's latency; the rest is queueing on the way).
type POPProber struct {
	pinger   Pinger
	now      func() time.Time
	timeout  time.Duration // per echo request
	relays   int           // relays pinged per POP
	perRelay int           // echo requests per relay
	parallel int           // POPs pinged at the same time
}

// NewPOPProber pings with p; nil now: time.Now.
func NewPOPProber(p Pinger, now func() time.Time) *POPProber {
	if now == nil {
		now = time.Now
	}
	return &POPProber{pinger: p, now: now, timeout: 1500 * time.Millisecond, relays: 2, perRelay: 2, parallel: 8}
}

// Probe pings every POP that has relays and calls done (from several
// goroutines) as each one finishes, so results show up as they come in.
// It returns when all are done or ctx ends.
func (p *POPProber) Probe(ctx context.Context, pops []POP, done func(id string, r POPPing)) {
	sem := make(chan struct{}, p.parallel)
	var wg sync.WaitGroup
	for _, pop := range pops {
		if len(pop.Relays) == 0 {
			continue
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func(pop POP) {
			defer wg.Done()
			defer func() { <-sem }()
			if r, ok := p.probe(ctx, pop); ok {
				done(pop.ID, r)
			}
		}(pop)
	}
	wg.Wait()
}

func (p *POPProber) probe(ctx context.Context, pop POP) (POPPing, bool) {
	targets := pickRelays(pop.Relays, p.relays)
	if len(targets) == 0 {
		return POPPing{Note: "IPv6 relays only; Lowpath pings over IPv4", At: p.now()}, true
	}
	var r POPPing
	var hardErr error
	for i := 0; i < p.perRelay; i++ {
		for _, a := range targets {
			if ctx.Err() != nil {
				return POPPing{}, false
			}
			rtt, err := p.pinger.Ping(a, p.timeout)
			switch {
			case err == nil:
				r.Sent++
				r.Received++
				if r.Received == 1 || rtt < r.RTT {
					r.RTT = rtt
				}
			case errors.Is(err, ErrNoReply):
				r.Sent++
			default:
				// Not sent at all (no permission, no route): not loss.
				hardErr = err
			}
		}
		if hardErr != nil && r.Received == 0 {
			break // pinging itself fails here; more tries won't help
		}
	}
	r.At = p.now()
	if r.Received == 0 {
		if hardErr != nil {
			r.Note = "Can't ping from this PC: " + hardErr.Error()
		} else {
			r.Note = "No reply to ping"
		}
	}
	return r, true
}

// pickRelays chooses up to n IPv4 relays spread across the list (a POP's
// relays are listed by address, so neighbors often share a rack).
func pickRelays(rs []SDRRelay, n int) []netip.Addr {
	var v4 []netip.Addr
	for _, r := range rs {
		if r.Addr.Is4() {
			v4 = append(v4, r.Addr)
		}
	}
	if len(v4) <= n {
		return v4
	}
	out := make([]netip.Addr, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, v4[i*len(v4)/n])
	}
	return out
}
