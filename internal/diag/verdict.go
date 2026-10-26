package diag

import (
	"fmt"
	"time"
)

// The verdict engine answers "why am I lagging?" from the measurements
// Lowpath already has: the bufferbloat test, the passive detector, the
// route trace, a running boost and the route analysis. Its rules follow the
// roadmap (§3.6) and run in order; the first that finds a problem gives the
// verdict:
//
//  1. Wi-Fi or the home network: the router loses pings or answers slowly.
//  2. Overloaded line: latency rises under load (bufferbloat), or other
//     traffic fills the line during a boost.
//  3. ISP route: loss starts past the home network and persists to the
//     destination. Lowpath's relays can route around it.
//  4. Game server: the route is clean up to the relay, the server end is
//     not. Nobody but the game's operator can fix it.
//  5. Already optimal: the direct route is within 2 ms of the relay route
//     and no lossier, so Lowpath isn't needed.
//
// Every verdict carries a confidence level and the measured facts behind
// it. A rule that has nothing to go on says so (CheckUnknown) instead of
// guessing, and the diagnosis lists what to run for a surer answer.

// Verdict IDs.
const (
	VerdictWiFi    = "wifi"    // the Wi-Fi link
	VerdictHome    = "home"    // the home network: router, cable or a link of unknown kind
	VerdictLine    = "line"    // the internet line is full (bufferbloat, other traffic)
	VerdictISP     = "isp"     // the ISP's route loses traffic
	VerdictServer  = "server"  // the game server's end
	VerdictOptimal = "optimal" // the direct route is already as good as a relay
	VerdictClean   = "clean"   // no rule found a problem
	VerdictUnknown = "unknown" // not enough measurements to say anything
	VerdictOffline = "offline" // no route to the internet at all
)

// Segments of the way from the PC to the game server a verdict can blame.
const (
	SegmentHome    = "home"
	SegmentISP     = "isp"
	SegmentLowpath = "lowpath"
	SegmentServer  = "server"
)

// Confidence says how sure a verdict is.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// lower is one step less sure (low stays low).
func (c Confidence) lower() Confidence {
	if c == ConfidenceHigh {
		return ConfidenceMedium
	}
	return ConfidenceLow
}

// Verdict is the answer, in plain words.
type Verdict struct {
	ID         string     `json:"id"`
	Segment    string     `json:"segment,omitempty"` // which part of the way is to blame; "" none
	Title      string     `json:"title"`
	Detail     string     `json:"detail"`
	Advice     string     `json:"advice,omitempty"`
	Confidence Confidence `json:"confidence"`
	// LowpathHelps: routing through a relay can improve this (an ISP route
	// problem). Home, line and server problems a relay can't fix.
	LowpathHelps bool `json:"lowpath_helps"`
	// Facts are the measurements behind the verdict.
	Facts []string `json:"facts,omitempty"`
}

// Check statuses.
const (
	CheckFound   = "found"   // the rule found its problem (or, for "optimal", that Lowpath isn't needed)
	CheckClear   = "clear"   // measured, and the rule's problem isn't there
	CheckUnknown = "unknown" // nothing to go on; Summary says what is missing
)

// Check is one rule's outcome, a traffic-light row in the UI.
type Check struct {
	Rule    string   `json:"rule"`
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Summary string   `json:"summary"`
	Facts   []string `json:"facts,omitempty"`
}

// Next-step IDs: what the user can run for a surer verdict.
const (
	NextBloat   = "bloat"   // the bufferbloat test on this connection
	NextTrace   = "trace"   // a route trace
	NextMeasure = "measure" // measure routes on a game page, or boost a game
)

// Suggestion is a measurement that would make the diagnosis surer.
type Suggestion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Diagnosis is what the engine concluded from one set of evidence.
type Diagnosis struct {
	At        time.Time    `json:"at"`
	Verdict   Verdict      `json:"verdict"`
	Checks    []Check      `json:"checks"`
	Link      string       `json:"link"`      // LinkWiFi, LinkEthernet, ... of the connection in use
	Interface string       `json:"interface"` // its adapter name, "" unknown
	Next      []Suggestion `json:"next,omitempty"`
}

// WiFiSignal is the Wi-Fi link's radio state. Fields are nil or "" when
// the system doesn't say.
type WiFiSignal struct {
	RSSI *int   `json:"rssi_dbm"` // received signal strength, dBm
	Band string `json:"band"`     // "2.4", "5" or "6" (GHz)
	// BusyPct is how busy the access point says the channel is (its BSS
	// Load report); nil: not surveyed, or the access point doesn't say.
	BusyPct *float64 `json:"channel_busy_pct"`
	// The rest is for the report only.
	Channel    *int     `json:"channel"`
	WidthMHz   *int     `json:"width_mhz"`
	Generation string   `json:"generation"`
	TxMbps     *float64 `json:"tx_mbps"`
}

// GatewaySample is a series of pings to the home router.
type GatewaySample struct {
	Source string    `json:"source"` // what measured it, in words
	At     time.Time `json:"at"`
	Sent   int       `json:"sent"`
	Lost   int       `json:"lost"`
	P50MS  *float64  `json:"p50_ms"`
	// TailMS is the TailPct-th percentile (95 or 99) of the answered pings.
	TailMS  *float64 `json:"tail_ms"`
	TailPct int      `json:"tail_pct"`
}

func (g GatewaySample) lossPct() float64 {
	if g.Sent == 0 {
		return 0
	}
	return 100 * float64(g.Lost) / float64(g.Sent)
}

// PathStats is one route's measured latency, loss and jitter.
type PathStats struct {
	RTTMS    float64 `json:"rtt_ms"`
	LossPct  float64 `json:"loss_pct"`
	JitterMS float64 `json:"jitter_ms"`
	Samples  int     `json:"samples"`
}

// PathEvidence compares the direct route with Lowpath's best relay route to
// one game server, from a running boost or the last route analysis.
type PathEvidence struct {
	Source string    `json:"source"` // PathBoost or PathAnalysis
	At     time.Time `json:"at"`
	Game   string    `json:"game"` // the game's name
	// Direct is the PC straight to the game server; nil when the server
	// doesn't answer ping or none is known yet.
	Direct *PathStats `json:"direct"`
	// Relay is the PC to the relay: the leg Lowpath adds. Nil: no relay
	// route is up.
	Relay *PathStats `json:"relay"`
	// ServerMS is the relay's own leg to the game server (nil: the relay
	// hasn't measured it). ServerLossPct and ServerJitterMS are that leg's
	// loss and jitter, nil while relays don't report them.
	ServerMS       *float64 `json:"server_ms"`
	ServerLossPct  *float64 `json:"server_loss_pct"`
	ServerJitterMS *float64 `json:"server_jitter_ms"`
	// SameMethod: direct and relay were measured the same way, to the same
	// target. False today: direct is ICMP, the relay route UDP probes.
	SameMethod bool `json:"same_method"`
}

// Path evidence sources.
const (
	PathBoost    = "boost"
	PathAnalysis = "analysis"
)

// relayTotal is the whole relay route the game would feel: the relay leg
// plus the relay's leg to the server; ok is false when that second leg
// isn't measured.
func (p PathEvidence) relayTotal() (float64, bool) {
	if p.Relay == nil || p.ServerMS == nil {
		return 0, false
	}
	return p.Relay.RTTMS + *p.ServerMS, true
}

// Trace target kinds (Evidence.TraceKind).
const (
	TraceKindServer = "server" // a game server
	TraceKindRelay  = "relay"  // one of the user's relays
	TraceKindPublic = "public" // anything else
)

// Evidence is everything the engine may use. Every field is optional: nil
// means not measured, and the rules that need it say so. Addresses in it
// should already be redacted when the diagnosis is to be shared; the rules
// never quote a public address themselves.
type Evidence struct {
	At time.Time // now
	// Offline: the PC has no default route right now (ErrNoRoute). The
	// stored tests then describe an earlier connection.
	Offline bool
	// Link is the connection in use now (the default route); nil unknown.
	Link *Route
	// WiFi is the radio state on a Wi-Fi link; nil unknown.
	WiFi *WiFiSignal
	// Gateway is a dedicated series of pings to the router. Nil: the
	// engine falls back to the idle phase of a bufferbloat test taken on
	// this connection.
	Gateway *GatewaySample
	Bloat   *BloatResult  // the last bufferbloat test
	Passive *PassiveState // the passive detector during a boost; nil: no boost
	Trace   *TraceResult  // the last route trace
	// TraceKind says what the trace's target is (TraceKind*).
	TraceKind string
	// Boost is the running boost; Analysis the last route analysis. The
	// boost is preferred: it measures the game's real route right now.
	Boost    *PathEvidence
	Analysis *PathEvidence
}

// VerdictConfig tunes the engine. Zero values get the defaults.
type VerdictConfig struct {
	// Fresh: evidence older than this lowers the confidence one step (1 h).
	Fresh time.Duration
	// MaxAge: evidence older than this is ignored (24 h). A test from
	// yesterday may describe another network, or a problem that is gone.
	MaxAge time.Duration
}

func (c *VerdictConfig) defaults() {
	if c.Fresh <= 0 {
		c.Fresh = time.Hour
	}
	if c.MaxAge <= 0 {
		c.MaxAge = 24 * time.Hour
	}
	if c.Fresh > c.MaxAge {
		c.Fresh = c.MaxAge
	}
}

// Thresholds from the roadmap's rules.
const (
	// gatewayLossPct and gatewayTailMS: a router that loses more than 0.5%
	// of pings, or whose p99 is above 15 ms, is a bad first link.
	gatewayLossPct = 0.5
	gatewayTailMS  = 15
	// weakRSSI: below -70 dBm a Wi-Fi link is moderate to weak.
	weakRSSI = -70
	// busyChannelPct: an access point that senses its channel busy more
	// than this share of the time queues packets, which adds delay.
	busyChannelPct = 60
	// optimalMS: a direct route within this of the relay route needs no relay.
	optimalMS = 2
	// routeLossPct: loss on a route that is worth a verdict; cleanLossPct:
	// below this a route counts as clean.
	routeLossPct = 2
	cleanLossPct = 1
	// cleanJitterMS: a relay leg with less jitter than this is clean;
	// badJitterMS: a server leg with more is not.
	cleanJitterMS = 10
	badJitterMS   = 10
	// minPathSamples: fewer probes than this say too little about a route.
	minPathSamples = 20
)

// rule is one of the engine's checks. Check returns the row for the UI and,
// when the rule found its problem, the verdict.
type rule interface {
	check(v *view) (Check, *Verdict)
}

// VerdictEngine runs the rules in order.
type VerdictEngine struct {
	cfg   VerdictConfig
	rules []rule
}

// NewVerdictEngine applies the defaults to cfg and sets up the roadmap's
// five rules.
func NewVerdictEngine(cfg VerdictConfig) *VerdictEngine {
	cfg.defaults()
	return &VerdictEngine{cfg: cfg, rules: []rule{homeRule{}, lineRule{}, ispRule{}, serverRule{}, optimalRule{}}}
}

// Diagnose turns the evidence into a diagnosis. It never fails: with no
// evidence the verdict is VerdictUnknown and says what to run.
func (e *VerdictEngine) Diagnose(ev Evidence) Diagnosis {
	if ev.At.IsZero() {
		ev.At = time.Now()
	}
	v := newView(ev, e.cfg)
	d := Diagnosis{At: ev.At, Checks: make([]Check, 0, len(e.rules))}
	if ev.Link != nil {
		d.Link, d.Interface = ev.Link.Kind, ev.Link.Name
	}
	var found *Verdict
	for _, r := range e.rules {
		c, vd := r.check(v)
		d.Checks = append(d.Checks, c)
		if vd != nil && found == nil {
			found = vd
		}
	}
	switch {
	case ev.Offline:
		// Nothing the tests say matters while no game can connect at all,
		// and none of them can run.
		d.Verdict = offlineVerdict()
		return d
	case found != nil:
		d.Verdict = *found
	default:
		d.Verdict = noProblem(d.Checks)
	}
	d.Next = suggestions(d.Checks, d.Verdict, ev.Boost != nil)
	return d
}

func offlineVerdict() Verdict {
	return Verdict{ID: VerdictOffline, Segment: SegmentHome, Confidence: ConfidenceHigh,
		Title:  "No internet connection",
		Detail: "This PC has no route to the internet right now, so no game can connect. The checks below come from tests taken while it was online.",
		Advice: "Check the Wi-Fi or the cable and the router's lights, and restart the router if the connection doesn't come back.",
		Facts:  []string{"The system has no default route on any network adapter."}}
}

// noProblem is the verdict when no rule fired: how sure "it's fine" is
// depends on how many of the four problem checks could be measured.
func noProblem(checks []Check) Verdict {
	measured := 0
	for _, c := range checks {
		if c.Rule != ruleOptimal && c.Status == CheckClear {
			measured++
		}
	}
	if measured == 0 {
		return Verdict{ID: VerdictUnknown, Confidence: ConfidenceLow,
			Title:  "Not enough measurements yet",
			Detail: "Lowpath can't tell where lag comes from without measurements on this connection.",
			Advice: "Run the bufferbloat test and a route trace below, or boost a game: each one fills in part of the answer."}
	}
	conf := ConfidenceLow
	switch {
	case measured >= 4:
		conf = ConfidenceHigh
	case measured >= 2:
		conf = ConfidenceMedium
	}
	v := Verdict{ID: VerdictClean, Confidence: conf,
		Title:  "No problem found",
		Detail: fmt.Sprintf("%d of the 4 checks were measured and found nothing wrong.", measured)}
	if measured < 4 {
		v.Advice = "The checks marked as not measured could still hide a problem; run them for a surer answer."
	}
	return v
}

// suggestions turns unmeasured checks into next steps, without repeats.
// boosting: a boost is running, so "boost the game" is no next step.
func suggestions(checks []Check, vd Verdict, boosting bool) []Suggestion {
	var out []Suggestion
	seen := map[string]bool{}
	add := func(id, text string) {
		if !seen[id] {
			seen[id] = true
			out = append(out, Suggestion{id, text})
		}
	}
	for _, c := range checks {
		if c.Status != CheckUnknown {
			continue
		}
		switch c.Rule {
		case ruleHome, ruleLine:
			add(NextBloat, "Run the bufferbloat test on this connection: it measures your router and how your line copes with load.")
		case ruleISP:
			add(NextTrace, "Run a route trace to a game server or a relay: it shows where on the way loss starts.")
		case ruleServer, ruleOptimal:
			add(NextMeasure, "Measure routes on a game's page, or boost the game: that compares the direct route with a relay.")
		}
	}
	if vd.ID == VerdictISP && !boosting {
		add(NextMeasure, "Boost the game through a relay and compare its ping and loss with the direct route.")
	}
	return out
}

// view is the evidence with the per-measurement decisions made once: which
// results are recent enough and taken on the connection in use.
type view struct {
	ev  Evidence
	cfg VerdictConfig

	bloat     *BloatResult // usable bufferbloat test, nil none
	bloatConf Confidence   // how much its age and link allow
	bloatWhy  string       // why there is none, or a caveat

	trace     *TraceResult
	traceConf Confidence
	traceWhy  string

	gateway     *GatewaySample
	gatewayConf Confidence

	path *PathEvidence // boost, else analysis
}

func newView(ev Evidence, cfg VerdictConfig) *view {
	v := &view{ev: ev, cfg: cfg}
	v.useBloat()
	v.useTrace()
	v.useGateway()
	switch {
	case ev.Boost != nil:
		v.path = ev.Boost
	case ev.Analysis != nil && ev.At.Sub(ev.Analysis.At) <= cfg.MaxAge:
		v.path = ev.Analysis
	}
	return v
}

// ageConf is how sure evidence taken at t can be, or ok false when it is
// too old to use.
func (v *view) ageConf(t time.Time) (Confidence, bool) {
	age := v.ev.At.Sub(t)
	switch {
	case t.IsZero() || age > v.cfg.MaxAge:
		return ConfidenceLow, false
	case age > v.cfg.Fresh:
		return ConfidenceMedium, true
	}
	return ConfidenceHigh, true
}

func (v *view) useBloat() {
	b := v.ev.Bloat
	if b == nil {
		v.bloatWhy = "No bufferbloat test yet."
		return
	}
	conf, ok := v.ageConf(b.FinishedAt)
	if !ok {
		v.bloatWhy = fmt.Sprintf("The last bufferbloat test is %s old.", ago(v.ev.At.Sub(b.FinishedAt)))
		return
	}
	switch v.otherLink(b.Link, b.Interface) {
	case linkOther:
		v.bloatWhy = fmt.Sprintf("The last bufferbloat test ran on %s, not on the connection in use now.", linkWords(b.Link, b.Interface))
		return
	case linkUnsure:
		conf = conf.lower()
		v.bloatWhy = "Lowpath can't tell whether the last bufferbloat test ran on the connection in use now."
	}
	v.bloat, v.bloatConf = b, conf
}

// Answers of otherLink.
const (
	linkSame   = iota // the same connection, or the measurement doesn't say
	linkOther         // another adapter or another kind of link
	linkUnsure        // the connection in use now is unknown
)

// otherLink compares the connection a measurement ran on (kind, name; ""
// unknown) with the one in use now.
func (v *view) otherLink(kind, name string) int {
	l := v.ev.Link
	switch {
	case l == nil:
		return linkUnsure
	case (l.Name != "" && name != "" && l.Name != name) || (l.Kind != "" && kind != "" && l.Kind != kind):
		return linkOther
	}
	return linkSame
}

func (v *view) useTrace() {
	t := v.ev.Trace
	if t == nil {
		v.traceWhy = "No route trace yet."
		return
	}
	conf, ok := v.ageConf(t.FinishedAt)
	if !ok {
		v.traceWhy = fmt.Sprintf("The last route trace is %s old.", ago(v.ev.At.Sub(t.FinishedAt)))
		return
	}
	// A trace taken on Wi-Fi says nothing about the cable plugged in since.
	switch v.otherLink(t.Link, t.Interface) {
	case linkOther:
		v.traceWhy = fmt.Sprintf("The last route trace ran on %s, not on the connection in use now.", linkWords(t.Link, t.Interface))
		return
	case linkUnsure:
		conf = conf.lower()
	}
	if t.Stopped || t.Rounds < 100 {
		conf = conf.lower()
	}
	v.trace, v.traceConf = t, conf
}

// useGateway takes the dedicated gateway pings, else the idle phase of a
// usable bufferbloat test (the router pinged at 10 Hz for 5 s while the
// line was quiet).
func (v *view) useGateway() {
	if g := v.ev.Gateway; g != nil && g.Sent > 0 {
		if conf, ok := v.ageConf(g.At); ok {
			v.gateway, v.gatewayConf = g, conf
			return
		}
	}
	if v.bloat == nil {
		return
	}
	for _, p := range v.bloat.Phases {
		if p.Phase != PhaseIdle || p.Gateway == nil || p.Gateway.Sent == 0 {
			continue
		}
		v.gateway = &GatewaySample{Source: "the bufferbloat test's idle phase", At: v.bloat.FinishedAt,
			Sent: p.Gateway.Sent, Lost: p.Gateway.Lost, P50MS: p.Gateway.P50, TailMS: p.Gateway.P95, TailPct: 95}
		v.gatewayConf = v.bloatConf
	}
}

// homeLossHop is the trace hop inside the home network where persisting
// loss starts, nil when loss doesn't start at home.
func (v *view) homeLossHop() *TraceHop {
	t := v.trace
	if t == nil || !t.Reached || t.LossFrom == nil || *t.LossFrom < 1 || *t.LossFrom > len(t.Hops) {
		return nil
	}
	if h := &t.Hops[*t.LossFrom-1]; h.inHome() {
		return h
	}
	return nil
}

// ago is a rounded age for the facts: "3 min", "2 h", "4 days".
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "under a minute"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	}
	return fmt.Sprintf("%d days", int(d/(24*time.Hour)))
}

// linkWords names a connection: "Wi-Fi (WLAN 2)", "Ethernet", "another connection".
func linkWords(kind, name string) string {
	k := map[string]string{LinkWiFi: "Wi-Fi", LinkEthernet: "Ethernet", LinkOther: "a VPN or other link"}[kind]
	switch {
	case k != "" && name != "" && name != k:
		return fmt.Sprintf("%s (%s)", k, name)
	case k != "":
		return k
	case name != "":
		return name
	}
	return "another connection"
}
