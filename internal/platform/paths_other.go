//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// UnixPaths has no driver folder: the capture backends for Linux and macOS
// (TUN devices) need no driver files. Logs go where each OS keeps them:
// ~/Library/Logs on macOS (Console.app shows them), the XDG state folder
// elsewhere.
type UnixPaths struct {
	userDirs
	logs func() (string, error)
}

func NewUnixPaths() *UnixPaths {
	return &UnixPaths{userDirs: newUserDirs(), logs: func() (string, error) {
		return logBase(runtime.GOOS, os.Getenv, os.UserHomeDir)
	}}
}

func (p *UnixPaths) LogDir() (string, error) { return under(p.logs, "log") }

func (p *UnixPaths) DriverDirs(bool) ([]string, []error) {
	return nil, []error{fmt.Errorf("platform: no capture driver on %s: %w", runtime.GOOS, errors.ErrUnsupported)}
}
