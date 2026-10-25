//go:build !windows

package divert

import "os"

// IsAdmin reports whether this process runs as root.
func IsAdmin() bool { return os.Geteuid() == 0 }
