package platform

import (
	"errors"
	"runtime"
	"testing"
)

// openRecorder is a Desktop that records what it was asked to open.
type openRecorder struct {
	Desktop
	opened []string
}

func (o *openRecorder) OpenBrowser(url string) error {
	o.opened = append(o.opened, url)
	return nil
}

// Only the platform turns a page into what the OS opens: the location page
// on Windows, nothing elsewhere, and never a name it doesn't know.
func TestOpenSettings(t *testing.T) {
	d := &openRecorder{}
	err := OpenSettings(d, SettingsLocation)
	if runtime.GOOS == "windows" {
		if err != nil || len(d.opened) != 1 || d.opened[0] != "ms-settings:privacy-location" || !HasSettings(SettingsLocation) {
			t.Errorf("location on Windows: %v, opened %q", err, d.opened)
		}
	} else if !errors.Is(err, errors.ErrUnsupported) || len(d.opened) != 0 || HasSettings(SettingsLocation) {
		t.Errorf("location on %s: %v, opened %q, want unsupported", runtime.GOOS, err, d.opened)
	}
	d.opened = nil
	for _, p := range []SettingsPage{"", "privacy-location", "ms-settings:privacy-location", "https://example.com/"} {
		if err := OpenSettings(d, p); !errors.Is(err, errors.ErrUnsupported) {
			t.Errorf("page %q: %v, want unsupported", p, err)
		}
	}
	if len(d.opened) != 0 {
		t.Errorf("opened %q for unknown pages", d.opened)
	}
	if !SettingsLocation.Known() || SettingsPage("ms-settings:privacy-location").Known() || SettingsPage("").Known() {
		t.Error("Known accepts the wrong names")
	}
	// Every page an OS can open is one the app may ask for.
	for p := range settingsURLs {
		if !p.Known() {
			t.Errorf("this OS opens %q, which isn't a known page", p)
		}
	}
}
