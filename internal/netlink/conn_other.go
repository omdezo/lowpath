//go:build !linux

package netlink

import (
	"errors"
	"fmt"
	"runtime"
)

// Families, for code that builds on every OS.
const (
	Route     = 0
	Netfilter = 12
	SockDiag  = 4
)

// Conn is a netlink socket; netlink exists only on Linux.
type Conn struct{}

// Dial fails with an error matching errors.ErrUnsupported.
func Dial(int) (*Conn, error) {
	return nil, fmt.Errorf("netlink: not available on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (*Conn) Close() error { return nil }

func (*Conn) Do(...Message) error { return errors.ErrUnsupported }

func (*Conn) Dump(Message) ([]Reply, error) { return nil, errors.ErrUnsupported }
