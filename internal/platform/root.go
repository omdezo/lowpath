package platform

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Root is the base of the Unix privilege helpers: elevated means running as
// root. On its own it has no prompt to show (Polkit on Linux and
// AdminPrompt on macOS add one), so CanElevate is false and the UI offers
// proxy mode instead of a restart.
type Root struct{ euid func() int }

func NewRoot() *Root { return &Root{euid: os.Geteuid} }

func (r *Root) Elevated() bool { return r.euid() == 0 }
func (*Root) CanElevate() bool { return false }

func (*Root) Relaunch([]string) error {
	return fmt.Errorf("platform: restarting with more rights is not available on %s: %w", runtime.GOOS, errors.ErrUnsupported)
}
