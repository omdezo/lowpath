package diag

import (
	"errors"
	"testing"
)

// The Native Wifi calls on this PC, without admin rights. A desktop on a
// cable has a disconnected adapter or none; a laptop on Wi-Fi a reading.
// Either way a missing number must come with a reason, and nothing is
// left changed.
func TestWLANWindowsNoAdmin(t *testing.T) {
	w := NewWLAN()
	l, err := w.Link(0)
	switch {
	case errors.Is(err, ErrNoWiFi), errors.Is(err, ErrWiFiDisconnected):
		t.Logf("no connected Wi-Fi: %v", err)
		if _, err := w.Tune(0); !errors.Is(err, ErrNoWiFi) && !errors.Is(err, ErrWiFiDisconnected) {
			t.Errorf("Tune without a connected Wi-Fi: %v", err)
		}
		if _, err := w.Survey(0); !errors.Is(err, ErrNoWiFi) && !errors.Is(err, ErrWiFiDisconnected) {
			t.Errorf("Survey without a connected Wi-Fi: %v", err)
		}
		return
	case err != nil:
		t.Fatalf("Link: %v", err)
	}
	t.Logf("source %s, band %q, standard %q, notes %v", l.Source, l.Band, l.Standard, l.Notes)
	if l.RSSI != nil && (*l.RSSI >= 0 || *l.RSSI < -120) {
		t.Errorf("RSSI %d dBm", *l.RSSI)
	}
	if l.Band != "" && l.Band != "2.4" && l.Band != "5" && l.Band != "6" {
		t.Errorf("band %q", l.Band)
	}
	if l.RSSI == nil && len(l.Notes) == 0 {
		t.Error("no RSSI and no reason")
	}
}

// An unknown interface index is a clear "not Wi-Fi", not a crash.
func TestWLANWindowsNotWiFiIndex(t *testing.T) {
	_, err := NewWLAN().Link(1) // the loopback pseudo-interface
	if err == nil {
		t.Fatal("interface 1 read as Wi-Fi")
	}
	if !errors.Is(err, ErrNoWiFi) && !errors.Is(err, ErrWiFiDisconnected) {
		t.Logf("interface 1: %v", err)
	}
}
