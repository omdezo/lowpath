package stun

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Dialer opens the UDP socket a Binding request goes out on. *net.Dialer
// satisfies it; tests pass their own.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Client sends Binding requests. Make one with NewClient; it is safe for
// concurrent use and keeps no state between requests.
type Client struct {
	dialer   Dialer
	rto      time.Duration // wait after the first send
	maxRTO   time.Duration // the wait doubles after each resend, up to this
	attempts int           // sends per request, the first one included
}

// ClientOptions tune a Client; zero fields take the defaults.
type ClientOptions struct {
	Dialer   Dialer        // default: a plain *net.Dialer
	RTO      time.Duration // default 250 ms
	MaxRTO   time.Duration // default 1 s
	Attempts int           // default 4: about 2.75 s before giving up
}

// NewClient returns a Client. RFC 5389 §7.2.1 suggests a 500 ms first wait
// and 7 sends (39.5 s in all); that suits a call setup, not a status line
// in a UI, so the defaults retry sooner and give up after under 3 s. The
// caller's context can cut that shorter still.
func NewClient(o ClientOptions) *Client {
	c := &Client{dialer: o.Dialer, rto: o.RTO, maxRTO: o.MaxRTO, attempts: o.Attempts}
	if c.dialer == nil {
		c.dialer = &net.Dialer{}
	}
	if c.rto <= 0 {
		c.rto = 250 * time.Millisecond
	}
	if c.maxRTO < c.rto {
		c.maxRTO = max(time.Second, c.rto)
	}
	if c.attempts <= 0 {
		c.attempts = 4
	}
	return c
}

// Binding is one answered Binding request.
type Binding struct {
	Mapped netip.AddrPort // the public address and port the server saw
	Server netip.AddrPort // the server address that answered
	// RTT is known only when the first send was answered before any resend:
	// a resend is byte-identical, so a later reply can't be matched to the
	// send it answers (RFC 7982 §3).
	RTT      time.Duration
	RTTKnown bool
}

// ErrTimeout means no reply arrived after every resend.
var ErrTimeout = errors.New("stun: no reply from the server")

// Bind asks server ("host:port") which address it sees this host as.
// network is "udp4" or "udp6" and picks the address family both of the
// server and of the answer. Datagrams that are not a response to this
// request are ignored.
func (c *Client) Bind(ctx context.Context, network, server string) (Binding, error) {
	if network != "udp4" && network != "udp6" {
		return Binding{}, fmt.Errorf("stun: network %q is not udp4 or udp6", network)
	}
	conn, err := c.dialer.DialContext(ctx, network, server)
	if err != nil {
		return Binding{}, err
	}
	defer conn.Close()
	// Unblock the read below as soon as the caller gives up.
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	var id TxID
	rand.Read(id[:])
	req := AppendBindingRequest(make([]byte, 0, HeaderLen), id)
	buf := make([]byte, 1500)
	wait := c.rto
	lastErr := ErrTimeout
	var first time.Time
	for attempt := range c.attempts {
		now := time.Now()
		if attempt == 0 {
			first = now
		}
		if _, err := conn.Write(req); err != nil {
			if ctx.Err() != nil {
				return Binding{}, ctx.Err()
			}
			return Binding{}, err
		}
		conn.SetReadDeadline(now.Add(wait))
		// The deadline just set may have replaced the one the cancel hook
		// set a moment ago; don't wait out a whole round for nothing.
		if ctx.Err() != nil {
			return Binding{}, ctx.Err()
		}
		b, err := c.await(conn, buf, id)
		if err == nil {
			if attempt == 0 {
				b.RTT, b.RTTKnown = time.Since(first), true
			}
			return b, nil
		}
		if ctx.Err() != nil {
			return Binding{}, ctx.Err()
		}
		if errors.Is(err, errRound) {
			wait = min(wait*2, c.maxRTO)
			continue
		}
		var ne net.Error
		if errors.As(err, &ne) || errors.Is(err, net.ErrClosed) {
			// Windows reports an ICMP port unreachable as a read error on
			// the connected socket. Resend at once: if the port is really
			// closed every round ends this way and the caller hears why
			// quickly.
			lastErr = err
			continue
		}
		return Binding{}, err // the server answered with an error or a broken message
	}
	return Binding{}, lastErr
}

// errRound ends a round whose wait ran out with no reply.
var errRound = errors.New("stun: round timed out")

// await reads until a response to id arrives, the read deadline passes
// (errRound) or the socket fails.
func (c *Client) await(conn net.Conn, buf []byte, id TxID) (Binding, error) {
	for {
		n, err := conn.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return Binding{}, errRound
			}
			return Binding{}, err
		}
		got, mapped, err := ParseBindingResponse(buf[:n])
		if errors.Is(err, ErrNotSTUN) || errors.Is(err, ErrNotReply) || got != id {
			continue // not a response to this request
		}
		if err != nil {
			return Binding{}, err
		}
		b := Binding{Mapped: netip.AddrPortFrom(mapped.Addr().Unmap(), mapped.Port())}
		if ra, ok := conn.RemoteAddr().(*net.UDPAddr); ok {
			b.Server = netip.AddrPortFrom(ra.AddrPort().Addr().Unmap(), ra.AddrPort().Port())
		}
		return b, nil
	}
}
