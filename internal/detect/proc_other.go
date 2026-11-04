//go:build !windows

package detect

import (
	"errors"
	"fmt"
)

// Outside Windows the process list comes from an owner.ProcessTable; a
// System built without one can't tell which programs run, and says so
// rather than reporting that none do.
func running() (map[string]bool, error) {
	return nil, fmt.Errorf("detect: no process table on this system: %w", errors.ErrUnsupported)
}

func runningExe(names []string) string { return "" }
