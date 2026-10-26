//go:build !windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Root is the Unix privilege helper: elevated means running as root. There
// is no prompt yet (pkexec on Linux and an authorization prompt on macOS are
// tracked in issue #44), so CanElevate is false and the UI offers proxy mode
// instead of a restart.
type Root struct{ euid func() int }

func NewRoot() *Root { return &Root{euid: os.Geteuid} }

func (r *Root) Elevated() bool { return r.euid() == 0 }
func (*Root) CanElevate() bool { return false }

func (*Root) Relaunch([]string) error {
	return fmt.Errorf("platform: restarting with more rights is not implemented on %s yet: %w", runtime.GOOS, errors.ErrUnsupported)
}
