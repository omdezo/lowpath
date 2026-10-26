package netlink

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Families this program talks to.
const (
	Route     = unix.NETLINK_ROUTE
	Netfilter = unix.NETLINK_NETFILTER
	SockDiag  = unix.NETLINK_SOCK_DIAG
)

// replyTimeout bounds every wait for the kernel, which normally answers in
// microseconds: a request must never hang the caller.
const replyTimeout = 3 * time.Second

// Conn is a netlink socket. Its methods are safe for concurrent use; one
// request runs at a time.
type Conn struct {
	mu  sync.Mutex
	fd  int
	seq uint32
	buf []byte
}

// Dial opens a netlink socket of the given family (Route, Netfilter).
func Dial(family int) (*Conn, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, family)
	if err != nil {
		return nil, fmt.Errorf("netlink: socket: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink: bind: %w", err)
	}
	tv := unix.NsecToTimeval(replyTimeout.Nanoseconds())
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("netlink: receive timeout: %w", err)
	}
	unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)
	// Ask for the kernel's explanation with errors (Linux 4.12+); older
	// kernels just don't give one.
	unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_EXT_ACK, 1)
	// Errors without a copy of the request (Linux 4.3+): an nftables set
	// update can be bigger than the read buffer, and so would its echo.
	unix.SetsockoptInt(fd, unix.SOL_NETLINK, unix.NETLINK_CAP_ACK, 1)
	return &Conn{fd: fd, seq: uint32(time.Now().Unix()), buf: make([]byte, 1<<16)}, nil
}

// Close closes the socket. Objects the kernel ties to it (an nftables table
// with the owner flag) go with it.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil
	}
	err := unix.Close(c.fd)
	c.fd = -1
	return err
}

// Do sends msgs in one write (an nftables batch must arrive whole) and waits
// for the acknowledgement of each message that asks for one (FlagAck). It
// returns the first refusal; the kernel answers every message either way.
func (c *Conn) Do(msgs ...Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return errors.New("netlink: connection closed")
	}
	var b []byte
	a := newAcks(c.seq+1, msgs)
	for _, m := range msgs {
		c.seq++
		m.Flags |= FlagRequest
		b = m.Append(b, c.seq)
	}
	if err := unix.Sendto(c.fd, b, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return fmt.Errorf("netlink: send: %w", err)
	}
	for !a.done() {
		replies, err := c.read()
		if err != nil {
			return err
		}
		for _, r := range replies {
			a.take(r)
		}
	}
	return a.err
}

// Dump sends a dump request and returns every message of the answer.
func (c *Conn) Dump(m Message) ([]Reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fd < 0 {
		return nil, errors.New("netlink: connection closed")
	}
	c.seq++
	seq := c.seq
	m.Flags |= FlagRequest | FlagDump
	if err := unix.Sendto(c.fd, m.Append(nil, seq), 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return nil, fmt.Errorf("netlink: send: %w", err)
	}
	var out []Reply
	for {
		replies, err := c.read()
		if err != nil {
			return nil, err
		}
		for _, r := range replies {
			if r.Seq != seq {
				continue
			}
			switch r.Type {
			case typeDone:
				return out, nil
			case typeError:
				if errno, msg, _ := r.ack(); errno != 0 {
					return nil, &Error{Op: m.Op, Errno: errno, Msg: msg}
				}
			case typeNoop:
			default:
				r.Data = append([]byte(nil), r.Data...) // the buffer is reused
				out = append(out, r)
			}
		}
	}
}

func (c *Conn) read() ([]Reply, error) {
	n, _, err := unix.Recvfrom(c.fd, c.buf, 0)
	if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("netlink: no answer from the kernel within %v", replyTimeout)
	}
	if err != nil {
		return nil, fmt.Errorf("netlink: receive: %w", err)
	}
	return ParseReplies(c.buf[:n])
}
