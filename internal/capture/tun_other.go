//go:build !linux

package capture

import (
	"errors"
	"fmt"
	"runtime"
)

// The TUN backend's routing is Linux's (nftables, fwmark rules); elsewhere
// it is never available.
type noTUN struct{}

func newTUNSystem() tunSystem { return noTUN{} }

func (noTUN) check() error {
	return fmt.Errorf("capture: the TUN backend is for Linux, not %s: %w", runtime.GOOS, errors.ErrUnsupported)
}

func (n noTUN) open(tunRoute) (tunDevice, error) { return nil, n.check() }
func (noTUN) recover() error                     { return nil }
