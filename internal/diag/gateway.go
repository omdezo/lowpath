package diag

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"lowpath/internal/icmp"
)

// The gateway pinger measures the first link of every game's route: the
// one between this PC and the home router (roadmap §3.6, "Gateway:
// IcmpSendEcho2 to the default gateway, 20 Hz during a session"). Wi-Fi
// trouble shows up here first: lost pings and stalls of 100 ms and more
// that a 1 Hz ping, or a game's own ping meter, averages away.
//
// It runs while someone looks (the Diagnostics tab polls View through the
// app and keeps it going for Idle after the last look) and while a boost
// runs (Hold). It never pings while Pause says another test is loading the
// line: the bufferbloat test pings the router itself, and the router's
// latency under a full line is that test's answer, not this one's.
//
// What it does not count as loss: pings that couldn't be sent (no rights,
// no route, no router address), a router that answers no ping at all
// (many ignore ping), and pings still in flight. A router that rate-limits
// its replies is found by the shape of the loss (the same number of
// replies every second) and pinged twice a second instead; if it still
// limits, its loss is reported as unusable rather than as Wi-Fi loss.

// Gateway pinger states (GatewayView.State).
const (
	GatewayIdle         = "idle"          // not pinging: nobody looks and no boost runs
	GatewayMeasuring    = "measuring"     // pinging; too few replies for numbers yet
	GatewayOn           = "on"            // pinging; Stats are filled in
	GatewayPaused       = "paused"        // another test loads the line (Message says which)
	GatewayNoGateway    = "no_gateway"    // the connection in use has no router address
	GatewayOffline      = "offline"       // no default route at all
	GatewayNoReply      = "no_reply"      // the router answers no ping
	GatewayNoPermission = "no_permission" // the system won't let Lowpath send pings
	GatewayCantSend     = "cant_send"     // sending or reading the route failed otherwise
	GatewayUnsupported  = "unsupported"   // this system can't read the route or ping this router
)

// spikeMS: a reply this slow is a stall a player feels (roadmap L9 looks
// for "no periodic 100 ms+ spikes" on Wi-Fi).
const spikeMS = 100

// minGatewayReplies is how many replies a series needs before it shows
// numbers; minGatewaySent how many pings must go unanswered before the
// router counts as one that ignores ping.
const (
	minGatewayReplies = 10
	minGatewaySent    = 20
)

// echoHopLimit is the hop limit of IPv6 echoes; the router is one hop away,
// any value works.
const echoHopLimit = 64

var errNoIPv6Ping = errors.New("diag: no IPv6 ping on this system")

// GatewayConfig wires a GatewayPinger. Net and Ping are required; zero
// durations take the defaults.
type GatewayConfig struct {
	Net NetStats
	// Ping sends an IPv4 echo (icmp.Ping); PingTTL an IPv6 one, for a router
	// with only an IPv6 (usually link-local) address. Nil PingTTL: such a
	// router isn't pinged.
	Ping    PingFunc
	PingTTL TTLPingFunc
	// Interval between pings: 50 ms (20 Hz). Slow is the interval once the
	// router turned out to limit its replies: 500 ms.
	Interval time.Duration
	Slow     time.Duration
	// Timeout per ping (1 s); a later reply counts as lost.
	Timeout time.Duration
	// Window of results the numbers cover (60 s).
	Window time.Duration
	// Idle: stop this long after the last Touch unless Hold (15 s).
	Idle time.Duration
	// RouteEvery: how often the default route is read again (2 s), to
	// notice Wi-Fi to Ethernet, a VPN, or a lost connection.
	RouteEvery time.Duration
	// MaxInFlight caps pings waiting for an answer (40). Each one holds an
	// OS thread on Windows (IcmpSendEcho blocks).
	MaxInFlight int
	// Hold keeps the pinger running (a boost is on). Nil: never.
	Hold func() bool
	// Pause, when it returns a reason, stops pinging for now. Nil: never.
	Pause func() string
	Now   func() time.Time
}

func (c *GatewayConfig) defaults() {
	if c.Interval <= 0 {
		c.Interval = 50 * time.Millisecond
	}
	if c.Slow <= 0 {
		c.Slow = 500 * time.Millisecond
	}
	if c.Slow < c.Interval {
		c.Slow = c.Interval
	}
	if c.Timeout <= 0 {
		c.Timeout = time.Second
	}
	if c.Window <= 0 {
		c.Window = time.Minute
	}
	if c.Idle <= 0 {
		c.Idle = 15 * time.Second
	}
	if c.RouteEvery <= 0 {
		c.RouteEvery = 2 * time.Second
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 40
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// GatewayView is what the pinger knows now (GET /api/diag/gateway).
type GatewayView struct {
	State   string `json:"state"`
	Message string `json:"message,omitempty"` // why there are no numbers, in words
	// Note is something that happened to the series: it started over
	// because the connection changed, or the rate went down.
	Note      string    `json:"note,omitempty"`
	Running   bool      `json:"running"`
	RateHz    float64   `json:"rate_hz"`
	Gateway   string    `json:"gateway,omitempty"` // the router's address (a LAN address)
	Interface string    `json:"interface,omitempty"`
	Link      string    `json:"link,omitempty"`
	Since     time.Time `json:"since"` // first ping of this series; zero: none
	// Stats is nil until the series has minGatewayReplies replies.
	Stats  *GatewayStats   `json:"stats"`
	Series []GatewaySecond `json:"series"` // one entry per second, oldest first
}

// GatewayPinger pings the default gateway; see the comment at the top of
// this file. Create it with NewGatewayPinger and start Run once.
type GatewayPinger struct {
	cfg  GatewayConfig
	wake chan struct{}

	mu       sync.Mutex
	wanted   time.Time // keep pinging until (Touch)
	running  bool
	paused   string // why it isn't pinging, while active
	lastStep time.Time
	checked  time.Time // last limiter check

	route   Route
	routeOK bool
	routeAt time.Time
	dst     netip.Addr
	// blocked: a route problem (GatewayOffline, ...) and its words.
	blocked, blockedMsg string
	// sendState: the last ping couldn't be sent (GatewayNoPermission, ...).
	sendState, sendMsg string

	slow     bool
	epoch    uint64 // results of pings sent in an older epoch are dropped
	inflight int
	since    time.Time
	note     string
	results  []gwResult // sorted by send time, within Window
	last     *GatewaySample
}

// NewGatewayPinger applies the defaults to cfg.
func NewGatewayPinger(cfg GatewayConfig) *GatewayPinger {
	cfg.defaults()
	return &GatewayPinger{cfg: cfg, wake: make(chan struct{}, 1)}
}

// Touch keeps the pinger running for another Idle period and starts it at
// once if it was idle.
func (g *GatewayPinger) Touch() {
	g.mu.Lock()
	g.wanted = g.cfg.Now().Add(g.cfg.Idle)
	g.mu.Unlock()
	select {
	case g.wake <- struct{}{}:
	default:
	}
}

// Run pings until ctx ends. It never blocks on a ping: each one runs in its
// own goroutine, and a router that stops answering only fills the in-flight
// slots until their timeouts.
func (g *GatewayPinger) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.stopLocked()
			g.mu.Unlock()
			return
		case <-t.C:
		case <-g.wake:
		}
		t.Reset(g.step(g.cfg.Now()))
	}
}

// step does one tick: decides whether to ping, reads the route when due,
// sends one ping, and returns how long to wait for the next tick.
func (g *GatewayPinger) step(now time.Time) time.Duration {
	hold := g.cfg.Hold != nil && g.cfg.Hold()
	pause := ""
	if g.cfg.Pause != nil {
		pause = g.cfg.Pause()
	}
	g.mu.Lock()
	if !hold && !now.Before(g.wanted) {
		g.stopLocked()
		g.paused = ""
		g.mu.Unlock()
		return time.Second
	}
	if pause != "" {
		g.stopLocked()
		g.paused = pause
		g.mu.Unlock()
		return 250 * time.Millisecond
	}
	g.paused = ""
	switch gap := now.Sub(g.lastStep); {
	case !g.running:
		g.running, g.slow, g.routeOK = true, false, false
		g.resetLocked("")
	case gap > max(time.Second, g.cfg.RouteEvery) && len(g.results) > 0:
		// The process didn't run for a while (the PC slept, or the
		// system was frozen): pings in flight then time out as "lost"
		// that the router never saw.
		g.resetLocked(fmt.Sprintf("Measuring started over after a %s pause (the PC may have been asleep).", roundGap(gap)))
	}
	g.lastStep = now
	needRoute := !g.routeOK || now.Sub(g.routeAt) >= g.cfg.RouteEvery
	g.mu.Unlock()

	if needRoute {
		r, err := g.cfg.Net.DefaultRoute()
		g.mu.Lock()
		g.applyRoute(now, r, err)
		g.mu.Unlock()
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.running {
		return time.Second
	}
	if g.blocked != "" {
		return g.cfg.RouteEvery
	}
	if now.Sub(g.checked) >= time.Second {
		g.checked = now
		g.trimLocked(now)
		g.checkLimitLocked(now)
		g.refreshSampleLocked(now)
	}
	if g.inflight < g.cfg.MaxInFlight {
		g.inflight++
		go g.ping(g.epoch, g.dst, now)
	}
	return g.interval()
}

func roundGap(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	}
	return ago(d)
}

func (g *GatewayPinger) interval() time.Duration {
	if g.slow {
		return g.cfg.Slow
	}
	return g.cfg.Interval
}

// stopLocked ends pinging; pings in flight are dropped when they return.
// The last sample stays for the verdict.
func (g *GatewayPinger) stopLocked() {
	if g.running {
		g.refreshSampleLocked(g.cfg.Now())
	}
	g.running = false
	g.epoch++
	g.lastStep = time.Time{}
}

// resetLocked starts a new series; note says why (empty: a plain start).
func (g *GatewayPinger) resetLocked(note string) {
	g.epoch++
	g.results = g.results[:0]
	g.since = time.Time{}
	g.note = note
	g.sendState, g.sendMsg = "", ""
	g.checked = time.Time{}
}

// applyRoute takes a fresh reading of the default route.
func (g *GatewayPinger) applyRoute(now time.Time, r Route, err error) {
	g.routeAt = now
	if err != nil {
		g.routeOK, g.dst = false, netip.Addr{}
		switch {
		case errors.Is(err, ErrNoRoute):
			g.blocked, g.blockedMsg = GatewayOffline, "This PC has no route to the internet right now, so there is no router to ping."
		case errors.Is(err, ErrUnsupported) || errors.Is(err, errors.ErrUnsupported):
			g.blocked, g.blockedMsg = GatewayUnsupported, "Lowpath can't read the routing table on this system yet, so it can't find your router."
		default:
			g.blocked, g.blockedMsg = GatewayCantSend, "Lowpath couldn't read the routing table: "+err.Error()
		}
		g.resetLocked(g.note)
		return
	}
	dst := gatewayAddr(r)
	if g.routeOK && (r.Index != g.route.Index || dst != g.dst) {
		g.resetLocked(fmt.Sprintf("The connection changed (%s to %s), so measuring started over.",
			linkWords(g.route.Kind, g.route.Name), linkWords(r.Kind, r.Name)))
		g.slow = false
	}
	g.route, g.routeOK, g.dst = r, true, dst
	g.blocked, g.blockedMsg = "", ""
	if !dst.IsValid() {
		g.blocked, g.blockedMsg = GatewayNoGateway, "The connection in use has no router address (a VPN or a point-to-point link), so there is nothing to ping."
	}
}

// gatewayAddr is the address to ping for r's gateway: IPv4-mapped
// addresses unmapped, and a link-local IPv6 router given the interface as
// its zone (it can't be reached without one).
func gatewayAddr(r Route) netip.Addr {
	gw := r.Gateway
	if !gw.IsValid() || gw.IsUnspecified() {
		return netip.Addr{}
	}
	gw = gw.Unmap()
	if gw.Is6() && gw.IsLinkLocalUnicast() && gw.Zone() == "" && r.Index > 0 {
		gw = gw.WithZone(strconv.Itoa(r.Index))
	}
	return gw
}

// ping sends one echo and records the outcome, unless the series it
// belongs to has ended meanwhile.
func (g *GatewayPinger) ping(epoch uint64, dst netip.Addr, sent time.Time) {
	rtt, err := g.echo(dst)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	if epoch != g.epoch {
		return
	}
	switch {
	case err == nil:
		g.addLocked(gwResult{sent: sent, rtt: rtt})
	case errors.Is(err, icmp.ErrTimeout):
		g.addLocked(gwResult{sent: sent, lost: true})
	case errors.Is(err, os.ErrPermission):
		g.sendState, g.sendMsg = GatewayNoPermission, permissionWords()
	case errors.Is(err, errNoIPv6Ping) || errors.Is(err, icmp.ErrUnsupported):
		g.sendState, g.sendMsg = GatewayUnsupported, "Your router only has an IPv6 address, which Lowpath can't ping on this system yet."
	case errors.Is(err, icmp.ErrNoRoute):
		g.sendState, g.sendMsg = GatewayCantSend, "This PC has no route to your router right now."
	default:
		g.sendState, g.sendMsg = GatewayCantSend, "Pings to your router can't be sent: "+err.Error()
	}
}

func permissionWords() string {
	if runtime.GOOS == "linux" {
		return "This system doesn't let Lowpath send pings: it needs root, CAP_NET_RAW, or net.ipv4.ping_group_range covering your group."
	}
	return "This system doesn't let Lowpath send pings without more rights."
}

func (g *GatewayPinger) echo(dst netip.Addr) (time.Duration, error) {
	if dst.Is4() {
		return g.cfg.Ping(dst, g.cfg.Timeout)
	}
	if g.cfg.PingTTL == nil {
		return 0, errNoIPv6Ping
	}
	r, err := g.cfg.PingTTL(dst, echoHopLimit, g.cfg.Timeout)
	if err != nil {
		return 0, err
	}
	if r.Kind != icmp.Echo {
		// "Unreachable" from the router's own address: the echo didn't
		// make it, which for a game packet is loss too.
		return 0, icmp.ErrTimeout
	}
	return r.RTT, nil
}

// addLocked inserts r in send order (replies come back out of order).
func (g *GatewayPinger) addLocked(r gwResult) {
	i := len(g.results)
	for i > 0 && g.results[i-1].sent.After(r.sent) {
		i--
	}
	g.results = append(g.results, gwResult{})
	copy(g.results[i+1:], g.results[i:])
	g.results[i] = r
	if g.since.IsZero() || r.sent.Before(g.since) {
		g.since = r.sent
	}
	g.sendState, g.sendMsg = "", ""
}

func (g *GatewayPinger) trimLocked(now time.Time) {
	cut := now.Add(-g.cfg.Window)
	i := 0
	for i < len(g.results) && g.results[i].sent.Before(cut) {
		i++
	}
	if i > 0 {
		g.results = append(g.results[:0], g.results[i:]...)
	}
}

// settled is the time before which every ping has its outcome.
func (g *GatewayPinger) settled(now time.Time) time.Time {
	return now.Add(-g.cfg.Timeout - g.interval())
}

// checkLimitLocked drops to the slow rate when the router limits its
// replies at the fast one.
func (g *GatewayPinger) checkLimitLocked(now time.Time) {
	if g.slow {
		return
	}
	limited, perSec := limitedPattern(g.results, g.settled(now))
	if !limited {
		return
	}
	g.slow = true
	g.resetLocked(fmt.Sprintf("Your router answers only about %d pings a second, so Lowpath pings it %s a second instead.",
		perSec, timesWord(time.Second/g.cfg.Slow)))
}

func timesWord(n time.Duration) string {
	switch n {
	case 1:
		return "once"
	case 2:
		return "twice"
	}
	return fmt.Sprintf("%d times", n)
}

// refreshSampleLocked keeps the latest usable series as the verdict's
// evidence; it outlives the pinger stopping.
func (g *GatewayPinger) refreshSampleLocked(now time.Time) {
	st := gatewayStats(g.results, g.settled(now))
	if st.Sent < minGatewaySent {
		return
	}
	answered := st.Sent - st.Lost
	s := &GatewaySample{Source: fmt.Sprintf("live router pings, %s a second", timesWord(time.Second/g.interval())),
		At: g.results[len(g.results)-1].sent, Sent: st.Sent, Lost: st.Lost, P50MS: st.P50MS,
		Link: g.route.Kind, Interface: g.route.Name, Limited: st.Limited, Spikes: st.Spikes}
	switch {
	case st.P99MS != nil:
		s.TailMS, s.TailPct = st.P99MS, 99
	case answered > 0:
		s.TailMS, s.TailPct = st.P95MS, 95
	}
	g.last = s
}

// Sample is the latest series as verdict evidence; nil: none yet.
func (g *GatewayPinger) Sample() *GatewaySample {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running {
		g.refreshSampleLocked(g.cfg.Now())
	}
	if g.last == nil {
		return nil
	}
	s := *g.last
	return &s
}

// View is the pinger's state now. It never waits for a ping.
func (g *GatewayPinger) View() GatewayView {
	now := g.cfg.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	v := GatewayView{Running: g.running, Note: g.note, Since: g.since}
	if g.routeOK {
		v.Interface, v.Link = g.route.Name, g.route.Kind
		if g.dst.IsValid() {
			v.Gateway = g.dst.WithZone("").String()
		}
	}
	if g.running {
		v.RateHz = float64(time.Second) / float64(g.interval())
	}
	settled := g.settled(now)
	cut := now.Add(-g.cfg.Window)
	var rs []gwResult
	for _, r := range g.results {
		if !r.sent.Before(cut) {
			rs = append(rs, r)
		}
	}
	st := gatewayStats(rs, settled)
	v.Series = gatewaySeries(rs, now.Add(-g.cfg.Window), settled)
	switch {
	case !g.running && g.paused != "":
		v.State, v.Message = GatewayPaused, g.paused
	case !g.running && now.Before(g.wanted):
		// Touched, and the loop hasn't taken its first step yet.
		v.State, v.Message = GatewayMeasuring, "Starting…"
	case !g.running:
		v.State, v.Message = GatewayIdle, "Not measuring: Lowpath pings your router while this tab is open and during a boost."
	case g.blocked != "":
		v.State, v.Message = g.blocked, g.blockedMsg
	case g.sendState != "":
		v.State, v.Message = g.sendState, g.sendMsg
	case st.Sent >= minGatewaySent && st.Lost == st.Sent:
		v.State, v.Message = GatewayNoReply, "Your router doesn't answer ping (many routers don't), so this link can't be measured. That is not loss."
	case st.Sent-st.Lost < minGatewayReplies:
		v.State, v.Message = GatewayMeasuring, "Measuring…"
	default:
		v.State = GatewayOn
		v.Stats = &st
	}
	return v
}

// Redacted is the view for the diagnostics export: a router with a public
// address (no NAT at home, or a VPN's) is cut to its /24 or /48, and the
// per-second chart is left out.
func (v GatewayView) Redacted() GatewayView {
	if a, err := netip.ParseAddr(v.Gateway); err == nil && !privateAddr(a) {
		v.Gateway = redactAddr(a).String()
	}
	v.Message, v.Note = redactText(v.Message), redactText(v.Note)
	v.Series = nil
	return v
}
