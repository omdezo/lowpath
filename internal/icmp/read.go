//go:build !windows

package icmp

import (
	"errors"
	"net"
	"os"
	"time"
)

// drainGrace is how long a replyReader may wait past its deadline when
// nothing is queued. Go tries the read before it waits, so a queued reply
// never waits for it: it only has to cover getting from setting the new
// deadline to the read, which on the overloaded PC that stalled us in the
// first place can take more than a millisecond. It is also how late a
// reply that arrives after the deadline can still be taken, and what a
// probe that really got no answer waits beyond its timeout.
const drainGrace = 5 * time.Millisecond

// packetReader is the part of a socket served by Go's network poller
// (net.PacketConn, *icmp.PacketConn) that a replyReader uses.
type packetReader interface {
	ReadFrom(b []byte) (int, net.Addr, error)
	SetReadDeadline(t time.Time) error
}

// replyReader reads a probe's answers from a socket until a deadline
// without losing an answer that was already waiting on the socket when the
// deadline passed.
//
// Go's poller checks the deadline before it reads: once the deadline has
// passed, a read returns os.ErrDeadlineExceeded without looking at the
// socket. When the process stalls past the deadline (a descheduled VM, an
// overloaded PC), the deadline's timer fires on resume ahead of the read,
// and a reply that arrived in time and is queued on the socket would count
// as lost. So the first time the deadline passes, replyReader reads once
// more with the deadline moved drainGrace ahead. A truly non-blocking read
// (recvfrom with MSG_DONTWAIT through a syscall.RawConn) is not possible
// portably: RawConn.Read checks the deadline the same way, so it would have
// to clear it first, and it would bypass what golang.org/x/net/icmp does
// with each OS's addresses and IP headers.
//
// A reply read during the drain is timed when it is read, like any other:
// the echo carries no send time and the socket gives no receive time (that
// needs kernel timestamps, SO_TIMESTAMP), so its RTT includes the stall.
// That is still better than a loss that didn't happen. A caller that keeps
// RTT statistics, such as the engine's probe tracker, should leave out the
// RTT of a probe whose send and receive straddle a stall of its own clock
// while still counting the probe as answered.
type replyReader struct {
	c        packetReader
	draining bool
}

// newReplyReader reads from c until deadline (plus the drain).
func newReplyReader(c packetReader, deadline time.Time) *replyReader {
	c.SetReadDeadline(deadline)
	return &replyReader{c: c}
}

// ReadFrom reads the next message. Callers skip a message that isn't their
// answer and read again, as before the deadline: every read of the drain
// shares its one deadline, so skipped messages can't extend the wait past
// drainGrace, and once it has passed ReadFrom returns the timeout.
func (r *replyReader) ReadFrom(b []byte) (int, net.Addr, error) {
	n, from, err := r.c.ReadFrom(b)
	if err == nil || r.draining || !errors.Is(err, os.ErrDeadlineExceeded) {
		return n, from, err
	}
	r.draining = true
	if r.c.SetReadDeadline(time.Now().Add(drainGrace)) != nil {
		return n, from, err
	}
	return r.c.ReadFrom(b)
}
