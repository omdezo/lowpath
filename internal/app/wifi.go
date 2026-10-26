package app

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"lowpath/internal/diag"
)

// The Wi-Fi card in Settings → Diagnostics, the verdict's radio evidence,
// and the scan pause during a boost. Every call into the system's Wi-Fi
// service runs off the request with a time limit: the service answers in
// milliseconds, but a driver that hangs must not freeze the UI or a boost.

const (
	// wifiCallTimeout bounds one Link or Tune call; wifiSurveyTimeout the
	// survey, which may wait on Windows' location prompt.
	wifiCallTimeout   = 3 * time.Second
	wifiSurveyTimeout = 8 * time.Second
	// wifiCacheFor: polls closer together than this share one reading.
	wifiCacheFor = 2 * time.Second
	// wifiFreshFor: older readings aren't used as verdict evidence.
	wifiFreshFor = 30 * time.Second
	// wifiSurveyFreshFor: the access point's load report counts in the
	// verdict for this long (it describes the channel, which changes
	// slowly, and is only read when the user asks).
	wifiSurveyFreshFor = 15 * time.Minute
	// wifiRefreshEvery: how often a boost applies the scan pause again
	// (a reconnect resets it).
	wifiRefreshEvery = 10 * time.Second
)

// errWiFiTimeout: the Wi-Fi service didn't answer in time.
var errWiFiTimeout = errors.New("the Wi-Fi service didn't answer in time")

// withTimeout runs fn on its own goroutine and gives up waiting after d.
// late, when not nil, receives a result that arrived after the give-up
// (a tuning that must still be reverted).
func withTimeout[T any](d time.Duration, fn func() (T, error), late func(T)) (T, error) {
	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-t.C:
		if late != nil {
			go func() {
				if r := <-ch; r.err == nil {
					late(r.v)
				}
			}()
		}
		var zero T
		return zero, errWiFiTimeout
	}
}

// Wi-Fi card states.
const (
	wifiOK           = "ok"           // a reading
	wifiNone         = "no_wifi"      // no Wi-Fi adapter (or none in use)
	wifiDisconnected = "disconnected" // an adapter, not connected
	wifiUnsupported  = "unsupported"  // no Wi-Fi reader on this OS yet
	wifiError        = "error"        // the reader failed; Message says how
	wifiLocation     = "location"     // survey only: Windows wants location access
)

// wifiView is GET /api/diag/wifi.
type wifiView struct {
	At      time.Time `json:"at"`
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
	// Link is the reading (nil unless State is ok). InUse: its adapter
	// carries the internet traffic now; false when the PC uses a cable
	// and the Wi-Fi is connected too, or the route is unknown.
	Link  *diag.WiFiLink `json:"link"`
	InUse bool           `json:"in_use"`
	// Route is the connection in use: "wifi", "ethernet", "other" or "".
	Route  string          `json:"route"`
	Survey *wifiSurveyView `json:"survey"`
	// Tuning is the running boost's scan pause; nil without a boost.
	Tuning *wifiTuneView `json:"tuning"`
}

// wifiSurveyView is the last survey the user asked for.
type wifiSurveyView struct {
	State   string           `json:"state"` // ok, location, disconnected, no_wifi, unsupported, error
	Message string           `json:"message,omitempty"`
	Survey  *diag.WiFiSurvey `json:"result"`
	At      time.Time        `json:"at"`
}

// wifiWatch reads the Wi-Fi link when asked and keeps the newest reading.
type wifiWatch struct {
	wlan diag.WLAN
	net  diag.NetStats
	log  *slog.Logger
	// The time limits (the constants above; tests shorten them).
	callTimeout, surveyTimeout, refreshEvery time.Duration

	mu        sync.Mutex
	last      *wifiView
	inflight  chan struct{} // closed when the running read ends; nil: none
	survey    *wifiSurveyView
	surveying bool
}

func newWiFiWatch(w diag.WLAN, ns diag.NetStats, log *slog.Logger) *wifiWatch {
	return &wifiWatch{wlan: w, net: ns, log: log, callTimeout: wifiCallTimeout, surveyTimeout: wifiSurveyTimeout, refreshEvery: wifiRefreshEvery}
}

// routeIndex is the interface to read: the default route's when it is a
// Wi-Fi link, else 0 (any connected Wi-Fi adapter, to show next to the
// cable).
func (w *wifiWatch) routeIndex() (index int, kind string) {
	r, err := w.net.DefaultRoute()
	if err != nil {
		return 0, ""
	}
	if r.Kind == diag.LinkWiFi {
		return r.Index, r.Kind
	}
	return 0, r.Kind
}

// current returns a reading no older than wifiCacheFor, waiting up to
// wait for a new one; on a timeout it returns the last one it has.
func (w *wifiWatch) current(wait time.Duration) wifiView {
	w.mu.Lock()
	if w.last != nil && time.Since(w.last.At) < wifiCacheFor {
		v := *w.last
		w.mu.Unlock()
		return v
	}
	done := w.inflight
	if done == nil {
		done = make(chan struct{})
		w.inflight = done
		go w.read(done)
	}
	w.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	// Still waiting: an old reading would show numbers that may no longer
	// hold, so say why there is none instead.
	if w.inflight == done && (w.last == nil || time.Since(w.last.At) > w.callTimeout) {
		return wifiView{At: time.Now(), State: wifiError, Message: "The system's Wi-Fi service isn't answering, so there is no current reading."}
	}
	return *w.last
}

func (w *wifiWatch) read(done chan struct{}) {
	index, kind := w.routeIndex()
	l, err := w.wlan.Link(index)
	v := wifiView{At: time.Now(), Route: kind}
	switch {
	case err == nil:
		v.State, v.Link, v.InUse = wifiOK, &l, index > 0
		if kind != diag.LinkWiFi && kind != "" {
			v.Message = "This Wi-Fi connection isn't carrying your internet traffic now: " + routeWords(kind) + " is."
		}
	default:
		v.State, v.Message = wifiState(err)
		if kind == diag.LinkEthernet && v.State != wifiError {
			v.Message = "This PC reaches the internet over a cable, so Wi-Fi doesn't affect your games now."
		}
		if v.State == wifiError {
			w.log.Warn("read Wi-Fi link", "err", err)
		}
	}
	w.mu.Lock()
	w.last = &v
	w.inflight = nil
	w.mu.Unlock()
	close(done)
}

func routeWords(kind string) string {
	switch kind {
	case diag.LinkEthernet:
		return "a cable"
	case diag.LinkOther:
		return "a VPN or another link"
	}
	return "another connection"
}

// wifiState words a reader error for the card.
func wifiState(err error) (state, message string) {
	switch {
	case errors.Is(err, diag.ErrWiFiDisconnected):
		msg := "The Wi-Fi adapter isn't connected to a network."
		if d := detail(err, diag.ErrWiFiDisconnected); d != "" {
			msg = "The Wi-Fi adapter isn't connected: " + d + "."
		}
		return wifiDisconnected, msg
	case errors.Is(err, diag.ErrNoWiFi):
		msg := "This PC has no Wi-Fi adapter in use."
		if d := detail(err, diag.ErrNoWiFi); d != "" {
			msg = "No Wi-Fi reading: " + d + "."
		}
		return wifiNone, msg
	case errors.Is(err, diag.ErrLocationDenied):
		return wifiLocation, "Windows shares the access point's load and the nearby networks only with apps allowed to use your location. " +
			"Turn on Settings → Privacy & security → Location → \"Let desktop apps access your location\", then try again. Lowpath doesn't read or keep your location."
	case errors.Is(err, diag.ErrUnsupported):
		return wifiUnsupported, "Reading the Wi-Fi link isn't available on this system yet."
	case errors.Is(err, errWiFiTimeout):
		return wifiError, "The system's Wi-Fi service didn't answer in time."
	}
	return wifiError, "The Wi-Fi link couldn't be read: " + strings.TrimPrefix(err.Error(), "diag: ") + "."
}

// detail is what an error adds to the sentinel it wraps ("diag: no Wi-Fi
// adapter: the WLAN AutoConfig service isn't running" gives the part after
// the sentinel).
func detail(err, sentinel error) string {
	msg, prefix := err.Error(), sentinel.Error()+": "
	if strings.HasPrefix(msg, prefix) {
		return msg[len(prefix):]
	}
	return ""
}

// signal is the Wi-Fi evidence for the verdict: a fresh reading of the
// adapter that carries the internet traffic, with the access point's
// channel load from a recent survey of the same channel.
func (w *wifiWatch) signal() *diag.WiFiSignal {
	v := w.current(time.Second)
	if v.State != wifiOK || !v.InUse || v.Link == nil || time.Since(v.At) > wifiFreshFor {
		return nil
	}
	s := v.Link.Signal()
	w.mu.Lock()
	sv := w.survey
	w.mu.Unlock()
	if sv != nil && sv.Survey != nil && sv.Survey.Load != nil && time.Since(sv.At) < wifiSurveyFreshFor &&
		v.Link.FreqMHz != nil && sv.Survey.FreqMHz == *v.Link.FreqMHz {
		b := sv.Survey.Load.BusyPct
		s.BusyPct = &b
	}
	return &s
}

// runSurvey reads the scan cache now (the user pressed the button).
func (w *wifiWatch) runSurvey() *wifiSurveyView {
	w.mu.Lock()
	if w.surveying {
		sv := w.survey
		w.mu.Unlock()
		return sv
	}
	w.surveying = true
	w.mu.Unlock()
	index, _ := w.routeIndex()
	s, err := withTimeout(w.surveyTimeout, func() (diag.WiFiSurvey, error) { return w.wlan.Survey(index) }, nil)
	sv := &wifiSurveyView{At: time.Now(), State: wifiOK}
	if err != nil {
		sv.State, sv.Message = wifiState(err)
		if sv.State == wifiError {
			w.log.Warn("Wi-Fi survey", "err", err)
		}
	} else {
		sv.Survey = &s
	}
	w.mu.Lock()
	w.survey, w.surveying = sv, false
	w.mu.Unlock()
	return sv
}

func (w *wifiWatch) lastSurvey() *wifiSurveyView {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.survey
}

// --- the scan pause during a boost ----------------------------------------

// Tuning states.
const (
	tuneApplying = "applying" // Tune is running
	tuneOn       = "on"       // scans paused and streaming mode on
	tunePartial  = "partial"  // one of the two (Notes say why not the other)
	tuneFailed   = "failed"   // neither; Message says why
	tuneOff      = "off"      // reverted (the boost ended)
)

// wifiTuneView is the scan pause in /api/live and /api/diag/wifi.
type wifiTuneView struct {
	State      string   `json:"state"`
	ScanPaused bool     `json:"scan_paused"`
	Streaming  bool     `json:"streaming"`
	Message    string   `json:"message,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// wifiTuner holds the scan pause for one boost: it applies it, applies it
// again every 10 s (a reconnect resets it), and reverts it on stop. If
// Lowpath dies instead, the Wi-Fi service drops the pause with the closed
// handle, so nothing outlives the process.
type wifiTuner struct {
	w      *wifiWatch
	index  int
	stopCh chan struct{}
	done   chan struct{}

	mu   sync.Mutex
	view wifiTuneView
}

// startTuning starts the scan pause when the boost runs over Wi-Fi; nil
// when it doesn't (a cable, a VPN) or the setting is off.
func (w *wifiWatch) startTuning(enabled bool) *wifiTuner {
	if !enabled {
		return nil
	}
	r, err := w.net.DefaultRoute()
	if err != nil || r.Kind != diag.LinkWiFi {
		return nil
	}
	t := &wifiTuner{w: w, index: r.Index, stopCh: make(chan struct{}), done: make(chan struct{}), view: wifiTuneView{State: tuneApplying}}
	go t.run()
	return t
}

func (t *wifiTuner) set(v wifiTuneView) {
	t.mu.Lock()
	t.view = v
	t.mu.Unlock()
}

func tuneView(st diag.WiFiTuningState) wifiTuneView {
	v := wifiTuneView{ScanPaused: st.ScanPaused, Streaming: st.Streaming, Notes: st.Notes}
	switch {
	case st.ScanPaused && st.Streaming:
		v.State = tuneOn
	case st.ScanPaused || st.Streaming:
		v.State = tunePartial
	default:
		v.State = tuneFailed
		v.Message = "Neither setting could be applied."
	}
	return v
}

func (t *wifiTuner) run() {
	defer close(t.done)
	revert := func(tu diag.WiFiTuning) {
		if err := tu.Close(); err != nil {
			t.w.log.Warn("revert Wi-Fi scan pause", "err", err)
		}
	}
	tu, err := withTimeout(t.w.callTimeout, func() (diag.WiFiTuning, error) { return t.w.wlan.Tune(t.index) }, revert)
	if err != nil {
		_, msg := wifiState(err)
		if errors.Is(err, diag.ErrUnsupported) {
			msg = "Pausing Wi-Fi scans isn't available on this system yet."
		}
		t.set(wifiTuneView{State: tuneFailed, Message: msg})
		t.w.log.Info("Wi-Fi scan pause not applied", "err", err)
		return
	}
	t.set(tuneView(tu.State()))
	t.w.log.Info("Wi-Fi scans paused for the boost", "state", tu.State())
	tick := time.NewTicker(t.w.refreshEvery)
	defer tick.Stop()
	for {
		select {
		case <-t.stopCh:
			revert(tu)
			t.set(wifiTuneView{State: tuneOff})
			t.w.log.Info("Wi-Fi scan pause reverted")
			return
		case <-tick.C:
			t.set(tuneView(tu.Refresh()))
		}
	}
}

// stop reverts the pause, waiting a little for it so the next boost
// doesn't overlap; a hung Wi-Fi service is not waited out (the revert
// still runs when it answers). Safe on a nil tuner.
func (t *wifiTuner) stop() {
	if t == nil {
		return
	}
	select {
	case <-t.stopCh:
	default:
		close(t.stopCh)
	}
	select {
	case <-t.done:
	case <-time.After(t.w.callTimeout):
		t.w.log.Warn("Wi-Fi scan pause: revert still waiting on the Wi-Fi service")
	}
}

func (t *wifiTuner) current() *wifiTuneView {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	v := t.view
	v.Notes = append([]string(nil), v.Notes...)
	return &v
}

// --- HTTP ---------------------------------------------------------------

func (s *Server) wifiView() wifiView {
	v := s.wifi.current(s.wifi.callTimeout)
	v.Survey = s.wifi.lastSurvey()
	s.mu.Lock()
	if s.sess != nil {
		v.Tuning = s.sess.wifi.current()
	}
	s.mu.Unlock()
	return v
}

func (s *Server) handleWiFi(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.wifiView())
}

// handleWiFiSurvey reads the scan cache: on Windows 11 24H2 and later this
// may show Windows' location prompt, so only a button press calls it.
func (s *Server) handleWiFiSurvey(w http.ResponseWriter, r *http.Request) {
	s.wifi.runSurvey()
	writeJSON(w, http.StatusOK, s.wifiView())
}
