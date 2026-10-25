package divert

import "golang.org/x/sys/windows"

// IsAdmin reports whether this process runs elevated, which loading the
// driver and opening a handle require.
func IsAdmin() bool { return windows.GetCurrentProcessToken().IsElevated() }
