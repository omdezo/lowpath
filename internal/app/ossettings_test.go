package app

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"lowpath/internal/diag"
	"lowpath/internal/platform"
)

// settingsSpy is a SettingsOpener that records the pages it opened and
// answers err.
type settingsSpy struct {
	opened []platform.SettingsPage
	err    error
}

func (s *settingsSpy) open(p platform.SettingsPage) error {
	s.opened = append(s.opened, p)
	return s.err
}

// POST /api/os/settings/{page} opens only pages the app defines, through
// the window's opener, and says why when it can't.
func TestOpenSettingsAPI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opener bool  // the server has an opener
		err    error // what it answers
		method string
		path   string
		hdr    map[string]string
		code   int
		opened int // pages it was asked to open
	}{
		{"opened", true, nil, "POST", "/api/os/settings/location", csrf, http.StatusNoContent, 1},
		{"no opener (the service)", false, nil, "POST", "/api/os/settings/location", csrf, http.StatusNotImplemented, 0},
		{"this OS has no such page", true, errors.ErrUnsupported, "POST", "/api/os/settings/location", csrf, http.StatusNotImplemented, 1},
		{"the shell failed", true, errors.New("rundll32: not found"), "POST", "/api/os/settings/location", csrf, http.StatusInternalServerError, 1},
		{"not a page the app defines", true, nil, "POST", "/api/os/settings/ms-settings:privacy-location", csrf, http.StatusNotFound, 0},
		{"a URL", true, nil, "POST", "/api/os/settings/https:%2F%2Fexample.com", csrf, http.StatusNotFound, 0},
		{"GET", true, nil, "GET", "/api/os/settings/location", csrf, http.StatusMethodNotAllowed, 0},
		{"no CSRF header", true, nil, "POST", "/api/os/settings/location", nil, http.StatusForbidden, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spy := &settingsSpy{err: tc.err}
			ts, _ := newRealServer(t, newFake(t), func(c *Config) {
				if tc.opener {
					c.OpenSettings = spy.open
				}
			})
			code, body := do(t, ts, tc.method, tc.path, "", tc.hdr)
			if code != tc.code || len(spy.opened) != tc.opened {
				t.Errorf("%d %s, opened %v; want %d and %d pages", code, body, spy.opened, tc.code, tc.opened)
			}
			if tc.opened == 1 && spy.opened[0] != platform.SettingsLocation {
				t.Errorf("opened %q", spy.opened[0])
			}
		})
	}
}

// In front of the service, the window opens the page itself: the service
// runs as another account with no screen, and never sees the request.
func TestFrontendOpensSettingsItself(t *testing.T) {
	fp := newFake(t)
	svc := startService(t, fp)
	spy := &settingsSpy{}
	fe, err := NewFrontend(FrontendConfig{Dial: fp.ServiceAPI.Dial, Log: discard, Session: testSession, OpenSettings: spy.open})
	if err != nil {
		t.Fatal(err)
	}
	ts := newHTTPTest(t, fe.Handler())
	t.Cleanup(fe.Close)
	before := len(svc.requests())
	if code, body := do(t, ts, "POST", "/api/os/settings/location", "", csrf); code != http.StatusNoContent || len(spy.opened) != 1 {
		t.Fatalf("%d %s, opened %v", code, body, spy.opened)
	}
	if n := len(svc.requests()) - before; n != 0 {
		t.Errorf("the service got %d requests for the window's settings page", n)
	}
	// The service itself never opens one.
	direct := newHTTPTest(t, svc.srv.ServiceHandler())
	if code, _ := do(t, direct, "POST", "/api/os/settings/location", "", csrf); code != http.StatusNotImplemented {
		t.Errorf("the service answered %d, want 501", code)
	}
}

// A survey refused for location names the settings page that lifts it,
// where this system has that page; other refusals and other systems don't.
func TestWiFiSurveyNamesTheSettingsPage(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		has   bool // the system has the location page (Windows)
		want  platform.SettingsPage
		state string
	}{
		{"location on Windows", diag.ErrLocationDenied, true, platform.SettingsLocation, wifiLocation},
		{"location elsewhere", diag.ErrLocationDenied, false, "", wifiLocation},
		{"disconnected", diag.ErrWiFiDisconnected, true, "", wifiDisconnected},
		{"read", nil, true, "", wifiOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := newFake(t)
			fp.WiFi.SetLink(exampleLink(-58), nil)
			fp.WiFi.SetSurvey(diag.WiFiSurvey{FreqMHz: 5180, Networks: 3}, tc.err)
			srv, api := newWiFiServer(t, fp, nil)
			srv.wifi.hasSettings = func(p platform.SettingsPage) bool { return tc.has && p == platform.SettingsLocation }
			var v wifiView
			if code := api("POST", "/api/diag/wifi/survey", &v); code != 200 || v.Survey == nil {
				t.Fatalf("survey: %d %+v", code, v.Survey)
			}
			if v.Survey.State != tc.state || v.Survey.Settings != tc.want {
				t.Errorf("survey %q, settings %q; want %q, %q", v.Survey.State, v.Survey.Settings, tc.state, tc.want)
			}
		})
	}
}

// newHTTPTest serves h for the test.
func newHTTPTest(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return ts
}
