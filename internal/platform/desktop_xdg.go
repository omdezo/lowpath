//go:build !windows && !darwin

package platform

import "os"

// NewDesktop returns the freedesktop (Linux) desktop shell: a WebKitGTK
// window, else a Chromium-family browser in app mode, else the default
// browser through xdg-open.
func NewDesktop(cfg DesktopConfig) Desktop {
	return newXDGDesktop(cfg, osExec{}, os.Getenv, os.UserHomeDir)
}
