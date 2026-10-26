package app

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/platform/fakeplatform"
)

func ip[T any](v T) *T { return &v }

// exampleLink is a reading from the fake adapter: 5 GHz channel 36, Wi-Fi 6.
func exampleLink(rssi int) diag.WiFiLink {
	return diag.WiFiLink{At: time.Now(), Interface: "Wi-Fi", Adapter: "Example Wi-Fi 6 Adapter", RSSI: ip(rssi), Quality: ip(60),
		Band: "5", Channel: ip(36), FreqMHz: ip(5180), WidthMHz: ip(80), Standard: "802.11ax", Generation: "Wi-Fi 6",
		TxMbps: ip(1201.0), RxMbps: ip(866.7), Source: "fake"}
}

// cableLine is a home line on Ethernet.
type cableLine struct{ *fakeHomeLine }

func (c cableLine) DefaultRoute() (diag.Route, error) {
	return diag.Route{Index: 3, Name: "Ethernet", Gateway: c.gw, Kind: diag.LinkEthernet}, nil
}

func newWiFiServer(t *testing.T, fp *fakeplatform.Fake, ns diag.NetStats) (*Server, func(method, path string, out any) int) {
	t.Helper()
	line := newFakeHomeLine()
	if ns == nil {
		ns = line
	}
	ts, srv := newRealServer(t, fp, func(c *Config) { c.NetStats, c.BloatLoader, c.Ping = ns, line, line.ping })
	srv.wifi.callTimeout, srv.wifi.surveyTimeout, srv.wifi.refreshEvery = 200*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond
	return srv, func(method, path string, out any) int { return call(t, ts, method, path, "", out) }
}

func getWiFi(t *testing.T, api func(string, string, any) int) wifiView {
	t.Helper()
	var v wifiView
	if code := api("GET", "/api/diag/wifi", &v); code != 200 {
		t.Fatalf("GET wifi: %d", code)
	}
	return v
}

// stateWait bounds the waits for what the Wi-Fi goroutines do (the scan
// pause's tuner, a reading or survey after a hang). A busy machine (a
// shared CI host, a VM whose CPUs the host is busy with) may not run them
// for seconds, so it is generous: the waits end as soon as the state is
// there, and it only counts when the state never comes.
const stateWait = 10 * time.Second

// waitFor polls cond until it holds, for up to stateWait.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(stateWait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("no %s after %v", what, stateWait)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWiFiAPIReadingFeedsTheVerdict(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetLink(exampleLink(-78), nil)
	srv, api := newWiFiServer(t, fp, nil)

	v := getWiFi(t, api)
	if v.State != wifiOK || !v.InUse || v.Route != diag.LinkWiFi || v.Link == nil || *v.Link.RSSI != -78 || v.Message != "" {
		t.Fatalf("view: %+v", v)
	}
	if idx := fp.WiFi.Indexes(); len(idx) == 0 || idx[0] != 7 {
		t.Errorf("read interfaces %v, want the default route's (7)", idx)
	}

	// Router loss plus a weak signal: the Wi-Fi verdict, sure of it.
	if err := srv.store.SetBloat(lossyWiFiBloat(time.Now().Add(-3 * time.Minute))); err != nil {
		t.Fatal(err)
	}
	d := srv.diagnosis()
	if d.Verdict.ID != diag.VerdictWiFi || d.Verdict.Confidence != diag.ConfidenceHigh ||
		!strings.Contains(strings.Join(d.Verdict.Facts, "\n"), "-78 dBm on 5 GHz (weak or crowded)") {
		t.Errorf("verdict: %+v", d.Verdict)
	}
	ev := srv.evidence(time.Now())
	rep := diag.DiagnosisReport{Diagnosis: srv.verdict.engine.Diagnose(ev), Evidence: ev}.Text()
	if !strings.Contains(rep, "Wi-Fi: -78 dBm, 5 GHz channel 36 (80 MHz), Wi-Fi 6, 1201 Mbit/s Tx.") {
		t.Errorf("report:\n%s", rep)
	}

	// A strong signal turns it into "home network".
	fp.WiFi.SetLink(exampleLink(-52), nil)
	time.Sleep(wifiCacheFor + 50*time.Millisecond)
	if d := srv.diagnosis(); d.Verdict.ID != diag.VerdictHome {
		t.Errorf("strong signal: %s", d.Verdict.ID)
	}
}

func TestWiFiAPIStates(t *testing.T) {
	for _, c := range []struct {
		name  string
		err   error
		state string
		msg   string
	}{
		{"no adapter", diag.ErrNoWiFi, wifiNone, "no Wi-Fi adapter"},
		{"service off", fmt.Errorf("%w: the WLAN AutoConfig service isn't running", diag.ErrNoWiFi), wifiNone, "No Wi-Fi reading: the WLAN AutoConfig service isn't running."},
		{"disconnected", diag.ErrWiFiDisconnected, wifiDisconnected, "isn't connected"},
		{"unsupported", diag.ErrUnsupported, wifiUnsupported, "isn't available on this system"},
		{"driver error", errors.New("diag: WlanQueryInterface: The device is not ready."), wifiError, "couldn't be read: WlanQueryInterface"},
	} {
		fp := newFake(t)
		fp.WiFi.SetLink(diag.WiFiLink{}, c.err)
		_, api := newWiFiServer(t, fp, nil)
		v := getWiFi(t, api)
		if v.State != c.state || v.Link != nil || !strings.Contains(v.Message, c.msg) || v.Message == "" {
			t.Errorf("%s: %+v", c.name, v)
		}
	}
}

func TestWiFiAPIOnACable(t *testing.T) {
	// A cable in use and no Wi-Fi: say why there is no reading.
	fp := newFake(t)
	srv, api := newWiFiServer(t, fp, cableLine{newFakeHomeLine()})
	if v := getWiFi(t, api); v.State != wifiNone || v.Route != diag.LinkEthernet || !strings.Contains(v.Message, "over a cable") {
		t.Errorf("cable only: %+v", v)
	}
	// A cable and a connected Wi-Fi: shown, but not in use and not evidence.
	fp.WiFi.SetLink(exampleLink(-80), nil)
	time.Sleep(wifiCacheFor + 50*time.Millisecond)
	v := getWiFi(t, api)
	if v.State != wifiOK || v.InUse || !strings.Contains(v.Message, "isn't carrying your internet traffic") {
		t.Errorf("cable and Wi-Fi: %+v", v)
	}
	if idx := fp.WiFi.Indexes(); idx[len(idx)-1] != 0 {
		t.Errorf("asked for interface %d on a cable, want any (0)", idx[len(idx)-1])
	}
	if ev := srv.evidence(time.Now()); ev.WiFi != nil {
		t.Errorf("Wi-Fi evidence on a cable: %+v", ev.WiFi)
	}
}

func TestWiFiAPIHungService(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetLink(exampleLink(-60), nil)
	release := make(chan struct{})
	fp.WiFi.Hang(release)
	srv, api := newWiFiServer(t, fp, nil)
	start := time.Now()
	v := getWiFi(t, api)
	if v.State != wifiError || v.Link != nil || !strings.Contains(v.Message, "isn't answering") {
		t.Errorf("hung: %+v", v)
	}
	if d := srv.diagnosis(); d.Verdict.ID == "" {
		t.Error("no verdict while the Wi-Fi service hangs")
	}
	// The service hangs until release is closed below, so answering at all
	// shows the card and the verdict don't wait it out; the bound is
	// stateWait because a busy machine can make the requests slow too.
	if took := time.Since(start); took > stateWait {
		t.Errorf("waited %v on a hung Wi-Fi service", took)
	}
	close(release)
	waitFor(t, "reading after the service answered again", func() bool { return getWiFi(t, api).State == wifiOK })
}

func TestWiFiSurvey(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetLink(exampleLink(-58), nil)
	fp.WiFi.SetSurvey(diag.WiFiSurvey{}, diag.ErrLocationDenied)
	srv, api := newWiFiServer(t, fp, nil)
	var v wifiView
	if code := api("POST", "/api/diag/wifi/survey", &v); code != 200 || v.Survey == nil || v.Survey.State != wifiLocation ||
		!strings.Contains(v.Survey.Message, "Let desktop apps access your location") || v.Survey.Survey != nil {
		t.Fatalf("location denied: %d %+v", code, v.Survey)
	}

	fp.WiFi.SetSurvey(diag.WiFiSurvey{At: time.Now(), FreqMHz: 5180, Networks: 9, SameChannel: 3, LoudestNeighbor: ip(-67),
		Load: &diag.BSSLoad{Stations: 14, BusyPct: 71.8}}, nil)
	if code := api("POST", "/api/diag/wifi/survey", &v); code != 200 || v.Survey.State != wifiOK || v.Survey.Survey.Load.Stations != 14 {
		t.Fatalf("survey: %d %+v", code, v.Survey)
	}
	if v := getWiFi(t, api); v.Survey == nil || v.Survey.Survey.SameChannel != 3 {
		t.Errorf("GET keeps the survey: %+v", v.Survey)
	}
	// The busy channel joins the verdict's evidence: router loss on a
	// strong signal is now blamed on the crowded channel.
	if err := srv.store.SetBloat(lossyWiFiBloat(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if d := srv.diagnosis(); d.Verdict.ID != diag.VerdictWiFi || !strings.Contains(strings.Join(d.Verdict.Facts, "\n"), "channel 72% busy") {
		t.Errorf("verdict: %+v", d.Verdict)
	}
	// Another channel since (the router moved): the load no longer applies.
	l := exampleLink(-58)
	l.FreqMHz, l.Channel = ip(5500), ip(100)
	fp.WiFi.SetLink(l, nil)
	time.Sleep(wifiCacheFor + 50*time.Millisecond)
	if ev := srv.evidence(time.Now()); ev.WiFi == nil || ev.WiFi.BusyPct != nil {
		t.Errorf("load from another channel used: %+v", ev.WiFi)
	}
}

// boostLP starts a route-mode boost of a custom game on the loopback relay.
func boostLP(t *testing.T, fp *fakeplatform.Fake, ts func(method, path, body string, out any) int) {
	t.Helper()
	fp.Privileges.SetElevated(true)
	ts("PUT", "/api/settings", `{"capture_mode":"route","loopback_relay":true}`, nil)
	var g gameView
	ts("POST", "/api/games/custom", `{"name":"LP Check","process":"lpcheck.exe"}`, &g)
	var lv liveView
	if code := ts("POST", "/api/apply", `{"game_id":"`+g.ID+`","mode":"smart"}`, &lv); code != 200 {
		t.Fatalf("apply: %d", code)
	}
}

func newTuneServer(t *testing.T, fp *fakeplatform.Fake, ns diag.NetStats) (*Server, func(method, path, body string, out any) int) {
	t.Helper()
	line := newFakeHomeLine()
	if ns == nil {
		ns = line
	}
	ts, srv := newRealServer(t, fp, func(c *Config) { c.NetStats, c.BloatLoader, c.Ping = ns, line, line.ping })
	srv.wifi.callTimeout, srv.wifi.refreshEvery = 300*time.Millisecond, 30*time.Millisecond
	return srv, func(method, path, body string, out any) int { return call(t, ts, method, path, body, out) }
}

func waitTune(t *testing.T, srv *Server, state string) *wifiTuneView {
	t.Helper()
	deadline := time.Now().Add(stateWait)
	for {
		v := srv.live().WiFi
		if v != nil && v.State == state {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan pause %+v, want %s", v, state)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sessionTuner is the running boost's scan pause.
func sessionTuner(t *testing.T, srv *Server) *wifiTuner {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.sess == nil || srv.sess.wifi == nil {
		t.Fatal("no scan pause running")
	}
	return srv.sess.wifi
}

// waitReverted waits for a stopped tuner to end. Stop waits for the revert
// only so long (a hung Wi-Fi service isn't waited out), so the revert may
// still be on its way when Stop answers; once the tuner has ended, no more
// can come.
func waitReverted(t *testing.T, tu *wifiTuner) {
	t.Helper()
	select {
	case <-tu.done:
	case <-time.After(stateWait):
		t.Fatal("the scan pause didn't end after Stop")
	}
}

func TestWiFiScanPauseDuringBoost(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true, Streaming: true}, nil)
	srv, api := newTuneServer(t, fp, nil)
	// Nothing here is about a slow Wi-Fi service: the app's own time limit
	// (3 s, not 300 ms) keeps a busy machine that holds a goroutine up for a
	// moment from turning the first Tune into a timeout.
	srv.wifi.callTimeout = wifiCallTimeout
	boostLP(t, fp, api)
	v := waitTune(t, srv, tuneOn)
	if !v.ScanPaused || !v.Streaming {
		t.Errorf("tuning %+v", v)
	}
	tuner := sessionTuner(t, srv)
	tu := fp.WiFi.Tunings()
	if len(tu) != 1 {
		t.Fatalf("%d tunings", len(tu))
	}
	// Applied again while the boost runs (a reconnect resets it).
	waitFor(t, "second refresh", func() bool { return tu[0].Refreshes() >= 2 })
	var wv wifiView
	if api("GET", "/api/diag/wifi", "", &wv); wv.Tuning == nil || wv.Tuning.State != tuneOn {
		t.Errorf("wifi view tuning: %+v", wv.Tuning)
	}
	var lv liveView
	if code := api("POST", "/api/stop", "", &lv); code != 200 || lv.WiFi != nil {
		t.Fatalf("stop: %d %+v", code, lv.WiFi)
	}
	waitReverted(t, tuner)
	if n := len(fp.WiFi.Tunings()); n != 1 || tu[0].Closes() != 1 {
		t.Errorf("%d tunings, the first reverted %d times; want one, reverted once", n, tu[0].Closes())
	}
}

func TestWiFiScanPauseNotApplied(t *testing.T) {
	// Turned off in Settings: nothing touched.
	fp := newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true, Streaming: true}, nil)
	srv, api := newTuneServer(t, fp, nil)
	api("PUT", "/api/settings", `{"wifi_tuning":false}`, nil)
	boostLP(t, fp, api)
	time.Sleep(100 * time.Millisecond)
	if len(fp.WiFi.Tunings()) != 0 || srv.live().WiFi != nil {
		t.Error("tuned with the setting off")
	}
	api("POST", "/api/stop", "", nil)

	// On a cable: nothing to pause.
	fp = newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true}, nil)
	srv, api = newTuneServer(t, fp, cableLine{newFakeHomeLine()})
	boostLP(t, fp, api)
	time.Sleep(100 * time.Millisecond)
	if len(fp.WiFi.Tunings()) != 0 || srv.live().WiFi != nil {
		t.Error("tuned on a cable")
	}

	// The driver refuses: the boost runs, the live view says why.
	fp = newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{}, diag.ErrWiFiDisconnected)
	srv, api = newTuneServer(t, fp, nil)
	boostLP(t, fp, api)
	v := waitTune(t, srv, tuneFailed)
	if !strings.Contains(v.Message, "isn't connected") || srv.live().Status != "optimizing" {
		t.Errorf("failed tuning: %+v", v)
	}

	// Only one of the two settings held.
	fp = newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true, Notes: []string{"Streaming mode couldn't be turned on: the Wi-Fi driver doesn't support it."}}, nil)
	srv, api = newTuneServer(t, fp, nil)
	boostLP(t, fp, api)
	if v := waitTune(t, srv, tunePartial); !v.ScanPaused || v.Streaming || len(v.Notes) != 1 {
		t.Errorf("partial: %+v", v)
	}
}

func TestWiFiScanPauseHungService(t *testing.T) {
	// The Wi-Fi service hangs in Tune: the boost starts and stops at once
	// anyway, and the pause it grants late is reverted.
	fp := newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true, Streaming: true}, nil)
	release := make(chan struct{})
	fp.WiFi.Hang(release)
	srv, api := newTuneServer(t, fp, nil)
	// Tune hangs until release is closed below, so the boost answering at
	// all shows it doesn't wait for Tune; the bound is stateWait because a
	// busy machine can make the requests themselves slow.
	start := time.Now()
	boostLP(t, fp, api)
	if took := time.Since(start); took > stateWait {
		t.Errorf("boost waited %v on the Wi-Fi service", took)
	}
	if v := waitTune(t, srv, tuneFailed); !strings.Contains(v.Message, "didn't answer in time") {
		t.Errorf("hung tuning: %+v", v)
	}
	api("POST", "/api/stop", "", nil)
	close(release)
	waitFor(t, "revert of the late tuning", func() bool {
		tu := fp.WiFi.Tunings()
		return len(tu) == 1 && tu[0].Closes() == 1
	})
}

func TestWiFiTuningSettingDefaultsOn(t *testing.T) {
	if !DefaultSettings().WiFiTuning {
		t.Error("the scan pause is off by default")
	}
	st := openTestStore(t)
	if !st.Settings().WiFiTuning {
		t.Error("a new store has the scan pause off")
	}
}

// A boost that starts while the Wi-Fi reconnects gets the pause once the
// link is back; Stop reverts the one tuning it got.
func TestWiFiScanPauseRetriedAfterReconnect(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{}, diag.ErrWiFiDisconnected)
	srv, api := newTuneServer(t, fp, nil)
	srv.wifi.callTimeout = wifiCallTimeout // as in TestWiFiScanPauseDuringBoost
	boostLP(t, fp, api)
	if v := waitTune(t, srv, tuneFailed); !strings.Contains(v.Message, "tries again") {
		t.Errorf("failed tuning: %+v", v)
	}
	fp.WiFi.SetTune(diag.WiFiTuningState{ScanPaused: true, Streaming: true}, nil)
	waitTune(t, srv, tuneOn)
	tuner := sessionTuner(t, srv)
	api("POST", "/api/stop", "", nil)
	waitReverted(t, tuner)
	if tu := fp.WiFi.Tunings(); len(tu) != 1 || tu[0].Closes() != 1 {
		t.Errorf("%d tunings, want one reverted once", len(tu))
	}

	// Without the feature on this OS there is nothing to wait for.
	fp = newFake(t)
	fp.WiFi.SetTune(diag.WiFiTuningState{}, fmt.Errorf("pausing Wi-Fi scans: %w", diag.ErrUnsupported))
	srv, api = newTuneServer(t, fp, nil)
	boostLP(t, fp, api)
	if v := waitTune(t, srv, tuneFailed); strings.Contains(v.Message, "tries again") || !strings.Contains(v.Message, "isn't available") {
		t.Errorf("unsupported: %+v", v)
	}
}

// Presses of Check channel while the Wi-Fi service hangs don't start more
// blocked calls; once it answers, a press reads the channel again.
func TestWiFiSurveyHungService(t *testing.T) {
	fp := newFake(t)
	fp.WiFi.SetLink(exampleLink(-58), nil)
	release := make(chan struct{})
	fp.WiFi.Hang(release)
	_, api := newWiFiServer(t, fp, nil)
	var v wifiView
	for i := 0; i < 3; i++ {
		api("POST", "/api/diag/wifi/survey", &v)
		if v.Survey == nil || v.Survey.State != wifiError || !strings.Contains(v.Survey.Message, "didn't answer in time") {
			t.Fatalf("press %d: %+v", i, v.Survey)
		}
	}
	if n := fp.WiFi.SurveyCalls(); n != 1 {
		t.Errorf("%d survey calls behind a hung service, want 1", n)
	}
	fp.WiFi.SetSurvey(diag.WiFiSurvey{At: time.Now(), FreqMHz: 5180, Networks: 4}, nil)
	close(release)
	waitFor(t, "survey after the service answered", func() bool {
		api("POST", "/api/diag/wifi/survey", &v)
		return v.Survey != nil && v.Survey.State == wifiOK && v.Survey.Survey.Networks == 4
	})
}

// The link may give the center of an 80 MHz channel (5210 MHz for 36-48)
// while the scan gives the access point's primary channel (5180): the load
// still counts as the link's.
func TestWiFiSurveyLoadOnWideChannel(t *testing.T) {
	fp := newFake(t)
	l := exampleLink(-58)
	l.FreqMHz, l.Channel = ip(5210), ip(42)
	fp.WiFi.SetLink(l, nil)
	fp.WiFi.SetSurvey(diag.WiFiSurvey{At: time.Now(), FreqMHz: 5180, Networks: 6, Load: &diag.BSSLoad{Stations: 3, BusyPct: 64}}, nil)
	srv, api := newWiFiServer(t, fp, nil)
	var v wifiView
	if code := api("POST", "/api/diag/wifi/survey", &v); code != 200 || v.Survey == nil || v.Survey.State != wifiOK {
		t.Fatalf("survey: %d %+v", code, v.Survey)
	}
	if ev := srv.evidence(time.Now()); ev.WiFi == nil || ev.WiFi.BusyPct == nil || *ev.WiFi.BusyPct != 64 {
		t.Errorf("load on the 80 MHz channel not used: %+v", ev.WiFi)
	}
}
