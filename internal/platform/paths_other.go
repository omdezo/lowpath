//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"runtime"
)

// UnixPaths has no driver folder: the capture backends planned for Linux
// and macOS (TUN devices) need no driver files.
type UnixPaths struct{ userDirs }

func NewUnixPaths() *UnixPaths { return &UnixPaths{newUserDirs()} }

func (p *UnixPaths) DriverDirs(bool) ([]string, []error) {
	return nil, []error{fmt.Errorf("platform: no capture driver on %s: %w", runtime.GOOS, errors.ErrUnsupported)}
}
