package catalog

import (
	"context"
	"os"
	"testing"
)

// TestInternetValveSDR downloads the live configs of CS2, Dota 2 and
// Deadlock and pings Dubai and Frankfurt. Needs the internet:
//
//	$env:LOWPATH_INTERNET=1; go test -run TestInternet ./internal/catalog
func TestInternetValveSDR(t *testing.T) {
	if os.Getenv("LOWPATH_INTERNET") != "1" {
		t.Skip("set LOWPATH_INTERNET=1 to download Valve's live server lists")
	}
	src := NewSDRSource(nil, SteamAPI)
	for _, app := range []int{730, 570, 1422450} {
		b, err := src.Fetch(context.Background(), app)
		if err != nil {
			t.Fatalf("app %d: %v", app, err)
		}
		cfg, err := ParseSDRConfig(app, b)
		if err != nil {
			t.Fatalf("app %d: %v", app, err)
		}
		withRelays := 0
		for _, p := range cfg.POPs {
			if len(p.Relays) > 0 {
				withRelays++
			}
			if p.GeoFix != GeoOK {
				t.Logf("app %d: %s (%s) geo %s → %.2f,%.2f", app, p.ID, p.Desc, p.GeoFix, p.Geo.Lat, p.Geo.Lon)
			}
		}
		if withRelays < 10 {
			t.Fatalf("app %d: only %d POPs with relays", app, withRelays)
		}
		t.Logf("app %d: revision %d, %d POPs, %d with relays", app, cfg.Revision, len(cfg.POPs), withRelays)

		if app != 730 {
			continue
		}
		var pick []POP
		for _, p := range cfg.POPs {
			if p.ID == "dxb" || p.ID == "fra" {
				pick = append(pick, p)
			}
		}
		NewPOPProber(ICMPPinger{}, nil).Probe(context.Background(), pick, func(id string, r POPPing) {
			if r.Received > 0 {
				t.Logf("%s: %.1f ms (%d/%d replies)", id, float64(r.RTT.Microseconds())/1000, r.Received, r.Sent)
			} else {
				t.Logf("%s: %s", id, r.Note)
			}
		})
	}
}
