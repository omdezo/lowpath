//go:build !windows && !linux

package diag

// newPlatformWLAN: macOS reads Wi-Fi through CoreWLAN, which needs cgo;
// until then the UI says the readings aren't available here.
func newPlatformWLAN() WLAN { return unsupportedWLAN{} }
