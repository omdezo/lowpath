package platform

import (
	"errors"
	"fmt"
	"slices"
)

// SettingsPage names a page of the operating system's own settings that the
// app may send the user to, to change something only they may change. The
// UI asks for a page by name and the platform alone turns the name into
// what the OS opens, so no page can make the app open anything else.
type SettingsPage string

// SettingsLocation is where Windows lets desktop apps use the location,
// which Windows 11 24H2 and later want before they share the Wi-Fi scan
// (the access point's load and the networks nearby).
const SettingsLocation SettingsPage = "location"

// settingsPages are every page the app may ask for, on any OS.
var settingsPages = []SettingsPage{SettingsLocation}

// Known reports whether p is a page the app may ask for at all, whether or
// not this OS has it: anything else is not a request to honour.
func (p SettingsPage) Known() bool { return slices.Contains(settingsPages, p) }

// SettingsURL returns what opens page on this OS (a URI its shell
// understands), and whether this OS has the page at all.
func SettingsURL(page SettingsPage) (string, bool) {
	u, ok := settingsURLs[page]
	return u, ok
}

// HasSettings reports whether this OS has page.
func HasSettings(page SettingsPage) bool {
	_, ok := SettingsURL(page)
	return ok
}

// OpenSettings opens page in the OS's settings through desk. An error
// matches errors.ErrUnsupported when this OS has no such page.
func OpenSettings(desk Desktop, page SettingsPage) error {
	u, ok := SettingsURL(page)
	if !ok {
		return fmt.Errorf("settings page %q: %w", page, errors.ErrUnsupported)
	}
	return desk.OpenBrowser(u)
}
