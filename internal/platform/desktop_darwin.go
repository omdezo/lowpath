package platform

import "os"

// NewDesktop returns the macOS desktop shell: a WKWebView window, else a
// Chromium-family browser in app mode, else the default browser.
func NewDesktop(cfg DesktopConfig) Desktop {
	return newMacDesktop(cfg, osExec{}, os.UserHomeDir)
}
