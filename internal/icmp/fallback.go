package icmp

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"sync"
	"time"
)

// ttlSender is one way of sending a TTL-limited echo: a raw ICMP socket, or
// one of the unprivileged ICMP sockets Linux and macOS offer.
type ttlSender interface {
	sendTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error)
}

// ttlChain sends through the first of its senders the OS allows. It moves
// on when the OS refuses a sender (an error matching os.ErrPermission) or
// doesn't have it (errors.ErrUnsupported); any other answer, ErrTimeout
// and ErrNoRoute included, is the probe's result.
//
// A refused sender is skipped from then on, except the last one: a process
// doesn't gain privileges while it runs (getting them restarts Lowpath),
// but a setting such as net.ipv4.ping_group_range can change at any time,
// and the next trace should notice.
type ttlChain struct {
	senders []ttlSender
	// needs says what the OS wants when every sender is refused, for the
	// error: "root or CAP_NET_RAW".
	needs string

	mu      sync.Mutex
	refused []error // per sender: why the OS refused it, nil while it may work
}

func newTTLChain(needs string, senders ...ttlSender) *ttlChain {
	return &ttlChain{senders: senders, needs: needs, refused: make([]error, len(senders))}
}

func (c *ttlChain) sendTTL(dst netip.Addr, ttl int, timeout time.Duration) (Reply, error) {
	var denied, missing error
	for i, s := range c.senders {
		last := i == len(c.senders)-1
		c.mu.Lock()
		skip := c.refused[i]
		c.mu.Unlock()
		if skip != nil {
			denied = skip
			continue
		}
		r, err := s.sendTTL(dst, ttl, timeout)
		switch {
		case errors.Is(err, os.ErrPermission):
			denied = err
			if !last {
				c.mu.Lock()
				c.refused[i] = err
				c.mu.Unlock()
			}
		case errors.Is(err, errors.ErrUnsupported):
			missing = err
		default:
			return r, err
		}
	}
	if denied != nil {
		return Reply{}, fmt.Errorf("icmp: a TTL-limited echo on %s needs %s: %w", runtime.GOOS, c.needs, denied)
	}
	if missing != nil {
		return Reply{}, missing
	}
	return Reply{}, errors.ErrUnsupported
}
