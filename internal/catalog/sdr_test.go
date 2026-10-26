package catalog

import (
	"errors"
	"math"
	"net/netip"
	"os"
	"strings"
	"testing"
)

func loadFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/sdr-730.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func popByID(t *testing.T, cfg *SDRConfig, id string) POP {
	t.Helper()
	for _, p := range cfg.POPs {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("POP %q missing", id)
	return POP{}
}

// The fixture is a trimmed copy of the live CS2 config (October 2026).
func TestParseRealConfig(t *testing.T) {
	cfg, err := ParseSDRConfig(730, loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppID != 730 || cfg.Revision == 0 || len(cfg.POPs) != 14 {
		t.Fatalf("app %d rev %d pops %d", cfg.AppID, cfg.Revision, len(cfg.POPs))
	}
	for i := 1; i < len(cfg.POPs); i++ {
		if cfg.POPs[i-1].ID >= cfg.POPs[i].ID {
			t.Fatal("POPs not sorted by id")
		}
	}
	dxb := popByID(t, cfg, "dxb")
	if dxb.City != "Dubai" || dxb.Country != "United Arab Emirates" || len(dxb.Relays) != 2 {
		t.Fatalf("dxb = %+v", dxb)
	}
	if r := dxb.Relays[0]; r.Addr != netip.MustParseAddr("185.25.183.163") || r.PortFirst != 27015 || r.PortLast != 27060 {
		t.Fatalf("dxb relay = %+v", r)
	}
	if bom := popByID(t, cfg, "bom2"); len(bom.Aliases) != 1 || bom.Aliases[0] != "bom" {
		t.Fatalf("bom2 aliases = %v", bom.Aliases)
	}
	if ctu := popByID(t, cfg, "ctu"); len(ctu.Relays) != 0 {
		t.Fatalf("ctu has %d relays; the config lists none", len(ctu.Relays))
	}
	if sgp := popByID(t, cfg, "sgp"); sgp.City != "Singapore" || sgp.Country != "" {
		t.Fatalf("sgp city %q country %q", sgp.City, sgp.Country)
	}
}

// Every bad coordinate seen in the live config is fixed, and good ones are
// left alone.
func TestSanitizeRealConfig(t *testing.T) {
	cfg, err := ParseSDRConfig(730, loadFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		id       string
		fix      GeoFix
		lat, lon float64
	}{
		{"dxb", GeoOK, 25.25, 55.3},
		{"fra", GeoOK, 50.12, 8.68},
		{"bom2", GeoOK, 19.18, 72.99},
		{"dfjr", GeoOK, 25.1164, 56.3414},
		{"sto2", GeoOK, 59.34, 17.87},
		{"tyo", GeoOK, 35.68, 139.68},
		{"mdb1", GeoOK, 25.25, 55.3}, // Multiplay Dubai: checked against Dubai
		{"dfra", GeoSwapped, 50.11, 8.68},
		{"dvie", GeoSwapped, 48.21, 16.36},
		{"dlhr", GeoSwapped, 51.51, 0.12},
		{"mad", GeoCorrected, 40.42, -3.70},  // the config says +3.34: east of Valencia
		{"dsto", GeoCorrected, 59.33, 18.07}, // the config puts it on Moscow
	}
	for _, c := range cases {
		p := popByID(t, cfg, c.id)
		if p.GeoFix != c.fix || !p.Geo.Known || math.Abs(p.Geo.Lat-c.lat) > 1e-6 || math.Abs(p.Geo.Lon-c.lon) > 1e-6 {
			t.Errorf("%s: fix %q geo %+v, want %q %v,%v", c.id, p.GeoFix, p.Geo, c.fix, c.lat, c.lon)
		}
	}
}

func TestFixGeoWithoutReference(t *testing.T) {
	cases := []struct {
		in   Geo
		fix  GeoFix
		want Geo
	}{
		{Geo{Lat: 10, Lon: 20, Known: true}, GeoOK, Geo{Lat: 10, Lon: 20, Known: true}},
		{Geo{Lat: 120, Lon: 45, Known: true}, GeoSwapped, Geo{Lat: 45, Lon: 120, Known: true}},
		{Geo{Lat: 200, Lon: 300, Known: true}, GeoUnknown, Geo{}},
		{Geo{}, GeoUnknown, Geo{}},
	}
	for _, c := range cases {
		got, fix := fixGeo(c.in, Geo{})
		if got != c.want || fix != c.fix {
			t.Errorf("fixGeo(%+v) = %+v %q, want %+v %q", c.in, got, fix, c.want, c.fix)
		}
	}
	if g := rawGeo([]float64{0, 0}); g.Known {
		t.Error("[0,0] is no location")
	}
	if g := rawGeo([]float64{1}); g.Known {
		t.Error("one number is no location")
	}
}

func TestDistanceKM(t *testing.T) {
	dubai := Geo{Lat: 25.20, Lon: 55.27, Known: true}
	doha := Geo{Lat: 25.29, Lon: 51.53, Known: true}
	if d := DistanceKM(dubai, doha); d < 350 || d > 400 {
		t.Fatalf("Dubai-Doha = %.0f km, want about 380", d)
	}
	if d := DistanceKM(dubai, dubai); d != 0 {
		t.Fatalf("distance to itself = %v", d)
	}
}

// A broken or hostile reply must not make Lowpath ping the user's own
// network or crash the parser.
func TestParseRejectsBadData(t *testing.T) {
	cfg, err := ParseSDRConfig(1, []byte(`{"revision":7,"pops":{
		"ok1":{"desc":"Okay City (Somewhere)","geo":[10,20],"relays":[
			{"ipv4":"192.168.1.1","port_range":[27015,27060]},
			{"ipv4":"10.0.0.1"},{"ipv4":"127.0.0.1"},{"ipv4":"100.64.1.1"},{"ipv4":"0.0.0.0"},
			{"ipv4":"not an ip"},{"ipv4":"8.8.8.8","port_range":[27060,27015]},
			{"ipv4":"8.8.8.8"},{"ipv6":"2606:4700::1111"},{"ipv6":"::ffff:10.0.0.1"}]},
		"BAD ID!":{"desc":"x","relays":[{"ipv4":"8.8.4.4"}]},
		"ctl":{"desc":"Bell\u0007 City","geo":[1,2]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.POPs) != 2 {
		t.Fatalf("pops = %+v", cfg.POPs)
	}
	ok := popByID(t, cfg, "ok1")
	if len(ok.Relays) != 2 {
		t.Fatalf("relays = %+v, want 8.8.8.8 and 2606:4700::1111 only", ok.Relays)
	}
	if r := ok.Relays[0]; r.Addr.String() != "8.8.8.8" || r.PortFirst != 0 {
		t.Fatalf("relay 0 = %+v (bad port range must be dropped)", r)
	}
	if !ok.Relays[1].Addr.Is6() {
		t.Fatalf("relay 1 = %+v", ok.Relays[1])
	}
	if c := popByID(t, cfg, "ctl"); strings.ContainsRune(c.Desc, 7) {
		t.Fatalf("control character kept in %q", c.Desc)
	}

	for name, body := range map[string]string{
		"not json":   `<html>`,
		"no pops":    `{"revision":1,"pops":{}}`,
		"failure":    `{"success":false,"message":"Failed to get appinfo"}`,
		"only bad":   `{"pops":{"!!":{"desc":"x"}}}`,
		"wrong type": `{"pops":[1,2,3]}`,
	} {
		if _, err := ParseSDRConfig(1, []byte(body)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := ParseSDRConfig(1, []byte(`{"success":false,"message":"Failed to get appinfo"}`)); !errors.Is(err, ErrNotSDR) {
		t.Errorf("success:false = %v, want ErrNotSDR", err)
	}
	if _, err := ParseSDRConfig(1, make([]byte, maxConfigBytes+1)); err == nil {
		t.Error("oversized config accepted")
	}
}

func TestSplitDesc(t *testing.T) {
	for in, want := range map[string][2]string{
		"Mumbai (India)":             {"Mumbai", "India"},
		"Stockholm - Kista (Sweden)": {"Stockholm - Kista", "Sweden"},
		"Guam":                       {"Guam", ""},
		"Multiplay Washington DC":    {"Multiplay Washington DC", ""},
		"(odd)":                      {"(odd)", ""},
	} {
		if c, k := splitDesc(in); c != want[0] || k != want[1] {
			t.Errorf("splitDesc(%q) = %q, %q", in, c, k)
		}
	}
}

func TestParseDuplicateIDsKeepOne(t *testing.T) {
	// "DXB" and "dxb" are one POP once lowercased: one row, whichever map
	// order the decoder used.
	body := []byte(`{"pops":{
		"dxb":{"desc":"Dubai (United Arab Emirates)","relays":[{"ipv4":"8.8.8.8"}]},
		"DXB":{"desc":"Dubai (United Arab Emirates)","relays":[{"ipv4":"1.1.1.1"}]},
		" dxb ":{"desc":"Dubai (United Arab Emirates)","relays":[{"ipv4":"9.9.9.9"}]}}}`)
	for range 20 {
		cfg, err := ParseSDRConfig(1, body)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.POPs) != 1 {
			t.Fatalf("pops = %+v, want one dxb", cfg.POPs)
		}
		// Sorted keys: " dxb " < "DXB" < "dxb", so the first one wins.
		if got := cfg.POPs[0].Relays[0].Addr.String(); got != "9.9.9.9" {
			t.Fatalf("kept relay %s; the choice must not depend on map order", got)
		}
	}
}
