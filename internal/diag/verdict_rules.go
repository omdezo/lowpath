package diag

import (
	"fmt"
	"strings"
)

// The verdict engine's rules, in the order NewVerdictEngine runs them.

// Rule IDs (Check.Rule).
const (
	ruleHome    = "home"
	ruleLine    = "line"
	ruleISP     = "isp"
	ruleServer  = "server"
	ruleOptimal = "optimal"
)

// VerdictRelay: the route to the user's relay loses traffic on its last
// link or at the relay (a UDP trace that loses only at the relay's port).
const VerdictRelay = "relay"

// homeRule: the router loses pings or answers slowly (gateway loss > 0.5%
// or p99 > 15 ms), or a trace finds loss starting inside the home network.
// On Wi-Fi a weak signal (RSSI < -70 dBm) or the 2.4 GHz band confirms the
// radio link as the cause.
type homeRule struct{}

func (homeRule) check(v *view) (Check, *Verdict) {
	c := Check{Rule: ruleHome, Name: "Wi-Fi and home network"}
	g := v.gateway
	var facts []string
	if g != nil && g.Lost == g.Sent && !g.Stopped {
		// Many routers don't answer ping at all; that is no loss.
		facts = append(facts, "Your router didn't answer any ping ("+g.Source+"): many routers ignore ping, so this says nothing about loss.")
		g = nil
	}
	hop := v.homeLossHop()
	// A trace that didn't reach its destination can't confirm any loss.
	traced := v.trace != nil && v.trace.Reached
	if g == nil && !traced {
		c.Status, c.Facts = CheckUnknown, facts
		c.Summary = "Keep Diagnostics open for a minute while Lowpath pings your router, or run the bufferbloat test on this connection."
		if v.ev.Bloat != nil && v.bloat == nil {
			c.Summary = v.bloatWhy + " Keep Diagnostics open for a minute while Lowpath pings your router, or run the test again on this connection."
		}
		return c, nil
	}
	lossBad, tailBad := false, false
	conf := ConfidenceLow
	if g != nil {
		facts = append(facts, gatewayFact(*g, v))
		// A router that limits its replies drops pings, not traffic.
		lossBad = !g.Limited && g.lossPct() > gatewayLossPct && g.Lost >= 2
		tailBad = g.TailMS != nil && *g.TailMS > gatewayTailMS
		if g.Limited {
			facts = append(facts, "Your router limits how many pings it answers, so its lost pings aren't counted as loss.")
		}
		if g.Stopped {
			facts = append(facts, "Your router answered at first and then stopped answering: every ping since was lost.")
		}
		conf = v.gatewayConf
	}
	if hop != nil {
		facts = append(facts, fmt.Sprintf("The route trace (%s ago) finds loss starting at hop %d, inside your home network, that persists to the destination: %.1f%% of probes lost.",
			ago(v.ev.At.Sub(v.trace.FinishedAt)), hop.TTL, *v.trace.DestLossPct))
		if !lossBad && !tailBad {
			conf = v.traceConf
		} else {
			conf = maxConf(conf, v.traceConf)
		}
	}
	if !lossBad && !tailBad && hop == nil {
		c.Status, c.Facts = CheckClear, facts
		switch {
		case g != nil:
			c.Summary = "Your router answers pings quickly and without loss."
		default:
			c.Summary = "The route trace finds no loss starting inside your home network."
		}
		if g != nil && g.TailPct < 99 && g.TailMS != nil {
			c.Summary += fmt.Sprintf(" (Measured to the %dth percentile; the rarest spikes need a longer gateway test.)", g.TailPct)
		}
		return c, nil
	}
	if tailBad && !lossBad && hop == nil {
		// Routers answer pings to themselves at low priority, so slow
		// replies alone can come from the router's CPU, not the link.
		conf = conf.lower()
		facts = append(facts, "Only the router's replies are slow, with no loss; routers answer ping at low priority, so this is less certain.")
	}

	kind := v.linkKind()
	vd := &Verdict{Segment: SegmentHome, Confidence: conf}
	switch kind {
	case LinkWiFi:
		weak, known, fact := wifiSignal(v.ev.WiFi)
		facts = append(facts, fact)
		if known && !weak {
			vd.ID = VerdictHome
			vd.Title = "Your home network is the problem"
			vd.Detail = "The link to your router loses pings or stalls, though the Wi-Fi signal is strong: the router, interference, or another busy device on the network is the likely cause."
			vd.Advice = "Restart the router, and try a cable to see whether the problem follows the Wi-Fi."
			vd.Confidence = minConf(conf, ConfidenceMedium)
		} else {
			vd.ID = VerdictWiFi
			vd.Title = "Your Wi-Fi is the problem"
			vd.Detail = "The Wi-Fi link to your router loses pings or stalls, so every game lags before its traffic even leaves your home."
			vd.Advice = "Use a cable if you can. Otherwise move closer to the router, switch to its 5 GHz network, and keep microwaves and other 2.4 GHz devices away. Run the diagnostics again on a cable to compare."
			if !known {
				vd.Confidence = minConf(conf, ConfidenceMedium)
			}
		}
	case LinkEthernet:
		vd.ID = VerdictHome
		vd.Title = "Your home network is the problem"
		vd.Detail = "Even on a cable, the link to your router loses pings or stalls: the router, the cable or its port is the likely cause."
		vd.Advice = "Restart the router, and try another cable and another port on it."
		facts = append(facts, "This PC is on Ethernet.")
	default:
		vd.ID = VerdictHome
		vd.Title = "Your home network is the problem"
		vd.Detail = "The link to your router loses pings or stalls, so games lag before their traffic leaves your home."
		vd.Advice = "Restart the router, and if you are on Wi-Fi, try a cable to compare."
		if kind == LinkOther {
			facts = append(facts, "The connection in use is a VPN or another kind of link, so the \"router\" may be the VPN's server, not your own.")
			vd.Confidence = ConfidenceLow
		} else {
			facts = append(facts, "Lowpath couldn't tell whether this PC uses Wi-Fi or a cable.")
			vd.Confidence = minConf(conf, ConfidenceMedium)
		}
	}
	vd.Facts = facts
	c.Status, c.Facts = CheckFound, facts
	c.Summary = vd.Title + "."
	return c, vd
}

// linkKind is the kind of the connection in use, or, when the system
// didn't say, of the one the router pings or the bufferbloat test ran on.
func (v *view) linkKind() string {
	if l := v.ev.Link; l != nil && l.Kind != LinkUnknown {
		return l.Kind
	}
	if v.gateway != nil && v.gateway.Link != LinkUnknown {
		return v.gateway.Link
	}
	if v.bloat != nil {
		return v.bloat.Link
	}
	return LinkUnknown
}

// wifiSignal says whether the radio link is weak: RSSI below -70 dBm or the
// 2.4 GHz band. known is false when the system gave neither.
func wifiSignal(w *WiFiSignal) (weak, known bool, fact string) {
	if w == nil || (w.RSSI == nil && w.Band == "") {
		return false, false, "Lowpath doesn't read the Wi-Fi signal strength yet, so it can't confirm the radio link is the cause."
	}
	var parts []string
	if w.RSSI != nil {
		parts = append(parts, fmt.Sprintf("%d dBm", *w.RSSI))
		weak = *w.RSSI < weakRSSI
	}
	if w.Band != "" {
		parts = append(parts, w.Band+" GHz")
		weak = weak || w.Band == "2.4"
	}
	word := "strong enough"
	if weak {
		word = "weak or crowded"
	}
	return weak, true, fmt.Sprintf("Wi-Fi signal: %s (%s).", strings.Join(parts, " on "), word)
}

func gatewayFact(g GatewaySample, v *view) string {
	s := fmt.Sprintf("Router: %d of %d pings lost (%.1f%%), median %s ms", g.Lost, g.Sent, g.lossPct(), msText(g.P50MS))
	if g.TailPct > 0 {
		s += fmt.Sprintf(", %d%% within %s ms", g.TailPct, msText(g.TailMS))
	}
	if g.Spikes > 0 {
		s += fmt.Sprintf(", %d %s of %d ms or more", g.Spikes, plural(g.Spikes, "reply", "replies"), spikeMS)
	}
	return s + fmt.Sprintf(" (%s, %s ago).", g.Source, ago(v.ev.At.Sub(g.At)))
}

// lineRule: the bufferbloat grade is C or worse, or the passive detector
// sees other traffic filling the line while the game's ping rises.
type lineRule struct{}

func (lineRule) check(v *view) (Check, *Verdict) {
	c := Check{Rule: ruleLine, Name: "Internet line under load"}
	var facts []string
	vd := &Verdict{ID: VerdictLine, Segment: SegmentHome,
		Advice: "Pause downloads, game updates, cloud backups and streams, on this PC and others, while you play. A router with Smart Queue / SQM (fq_codel or CAKE) keeps a full line from adding lag."}
	if p := v.ev.Passive; p != nil && p.Active {
		facts = append(facts, passiveFact(p))
		vd.Title = "Something is filling your connection"
		vd.Detail = "Other traffic on your network is using most of the line right now, and the game's ping rose with it."
		vd.Confidence = ConfidenceHigh
	}
	b := v.bloat
	bad := false
	if b != nil && b.Grade != "" {
		facts = append(facts, bloatFact(b, v))
		bad = b.Grade == "C" || b.Grade == "D" || b.Grade == "F"
		if bad && vd.Title == "" {
			vd.Title = "Your connection lags when it's busy"
			vd.Detail = fmt.Sprintf("Your line has bufferbloat (grade %s): when a download or upload fills it, the ping rises a lot, so games lag whenever something else uses the connection.", b.Grade)
			vd.Confidence = v.bloatConf
		}
	}
	if vd.Title != "" {
		vd.Facts = facts
		c.Status, c.Facts, c.Summary = CheckFound, facts, vd.Title+"."
		return c, vd
	}
	watching := v.ev.Passive != nil && v.ev.Passive.Watching
	if watching {
		facts = append(facts, "During this boost, nothing else is filling the connection.")
	}
	c.Facts = facts
	switch {
	case b != nil && b.Grade != "":
		c.Status = CheckClear
		c.Summary = fmt.Sprintf("Bufferbloat grade %s: the ping stays steady when the line is full.", b.Grade)
		if b.Grade == "B" {
			c.Summary = "Bufferbloat grade B: the ping rises a little when the line is full, not enough to blame it."
		}
	case watching:
		c.Status = CheckClear
		c.Summary = "Nothing else is filling the connection during this boost."
	default:
		c.Status = CheckUnknown
		switch {
		case b != nil && b.Note != "":
			c.Summary = "The last bufferbloat test has no grade: " + b.Note
		case v.bloatWhy != "":
			c.Summary = v.bloatWhy + " Run the bufferbloat test on this connection."
		default:
			c.Summary = "Run the bufferbloat test on this connection."
		}
	}
	return c, nil
}

func bloatFact(b *BloatResult, v *view) string {
	return fmt.Sprintf("Bufferbloat grade %s (%s ago): ping %s ms downloading and %s ms uploading, from %s ms idle.",
		b.Grade, ago(v.ev.At.Sub(b.FinishedAt)), plusText(b.DownIncreaseMS), plusText(b.UpIncreaseMS), msText(b.IdleMS))
}

func passiveFact(p *PassiveState) string {
	var use []string
	if p.OtherDownMbps != nil {
		use = append(use, fmt.Sprintf("%.0f of %s Mbit/s downloading", *p.OtherDownMbps, optMbps(p.LineDownMbps)))
	}
	if p.OtherUpMbps != nil {
		use = append(use, fmt.Sprintf("%.0f of %s Mbit/s uploading", *p.OtherUpMbps, optMbps(p.LineUpMbps)))
	}
	s := "During the boost, other traffic is filling the connection"
	if len(use) > 0 {
		s += " (" + strings.Join(use, ", ") + ")"
	}
	if p.RTTMS != nil && p.BaselineMS != nil {
		s += fmt.Sprintf(" and the game's ping rose from %.0f to %.0f ms", *p.BaselineMS, *p.RTTMS)
	}
	return s + "."
}

func optMbps(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f", *v)
}

// ispRule: loss starts at a hop past the home network and persists to the
// destination; or, during a boost, the direct route to the game server
// loses traffic while the relay route doesn't.
type ispRule struct{}

func (ispRule) check(v *view) (Check, *Verdict) {
	c := Check{Rule: ruleISP, Name: "ISP route"}
	var facts []string
	t := v.trace
	traceClean := false
	if t != nil && t.Reached && v.homeLossHop() == nil {
		age := ago(v.ev.At.Sub(t.FinishedAt))
		switch from := t.LossFrom; {
		case from == nil:
			traceClean = true
			facts = append(facts, fmt.Sprintf("The route trace to %s (%s ago) finds no loss persisting to the destination.", traceHost(t), age))
		case *from == 0:
			fact := fmt.Sprintf("The UDP route trace to %s (%s ago) loses %.1f%% of probes at the relay's port, but no router before it loses traffic.", traceHost(t), age, *t.DestLossPct)
			vd := &Verdict{ID: VerdictRelay, Segment: SegmentLowpath, Confidence: v.traceConf, LowpathHelps: true,
				Title:  "The link to your relay loses traffic",
				Detail: "Probes to the relay's own port are lost on its last link or at the relay itself, while the routers on the way are clean.",
				Advice: "Pick another relay, or check the relay's server: its provider's network or the server may be overloaded.",
				Facts:  []string{fact}}
			c.Status, c.Facts, c.Summary = CheckFound, vd.Facts, vd.Title+"."
			return c, vd
		case *from == len(t.Hops) && t.End == nil:
			// Only the destination loses replies: the route up to it is
			// clean (the server rule looks at the destination).
			traceClean = true
			facts = append(facts, fmt.Sprintf("The route trace to %s (%s ago) finds no router on the way losing traffic; only the destination loses %.1f%% of its replies.", traceHost(t), age, *t.DestLossPct))
		case *from >= 1 && *from <= len(t.Hops):
			h := t.Hops[*from-1]
			where := ""
			if h.Name != "" {
				where = " (" + h.Name + ")"
			}
			fact := fmt.Sprintf("The route trace to %s (%s ago) finds loss starting at hop %d%s, past your home network, that persists to the destination: %.1f%% of probes lost.",
				traceHost(t), age, h.TTL, where, *t.DestLossPct)
			vd := ispVerdict(v.traceConf, fact, v.ev.Boost != nil)
			c.Status, c.Facts, c.Summary = CheckFound, vd.Facts, vd.Title+"."
			return c, vd
		}
	}
	if p := v.path; p != nil && p.Direct != nil && p.Relay != nil && p.ServerMS != nil && p.Direct.Samples >= minPathSamples && p.Relay.Samples >= minPathSamples {
		// A lossy relay->server leg means the loss is at the server's end,
		// which both routes share: the server rule's case, not the ISP's.
		legClean := p.ServerLossPct == nil || *p.ServerLossPct < routeLossPct
		if p.Direct.LossPct >= routeLossPct && p.Relay.LossPct < cleanLossPct && legClean {
			fact := fmt.Sprintf("%s: the direct route to the game server loses %.1f%% of pings, while the relay route loses %.1f%% and the relay reaches the server in %.0f ms.",
				pathWords(p, v), p.Direct.LossPct, p.Relay.LossPct, *p.ServerMS)
			conf := ConfidenceMedium
			if p.Source == PathAnalysis {
				conf = ConfidenceLow
			}
			vd := ispVerdict(conf, fact, p.Source == PathBoost)
			vd.Facts = append(vd.Facts, methodFact(p))
			c.Status, c.Facts, c.Summary = CheckFound, vd.Facts, vd.Title+"."
			return c, vd
		}
		if p.Direct.LossPct < cleanLossPct {
			facts = append(facts, fmt.Sprintf("%s: the direct route to the game server loses %.1f%% of pings.", pathWords(p, v), p.Direct.LossPct))
			traceClean = true
		}
	}
	c.Facts = facts
	switch {
	case traceClean:
		c.Status = CheckClear
		c.Summary = "No loss persists along the route past your home network."
	case t != nil && !t.Reached:
		c.Status = CheckUnknown
		c.Summary = "The last route trace didn't reach its destination, so loss on the way can't be confirmed. Trace one of your relays, or a game server that answers ping."
	case t != nil && v.homeLossHop() != nil:
		c.Status = CheckUnknown
		c.Summary = "Loss already starts inside your home network, so the ISP's route can't be judged until that is fixed."
	default:
		c.Status = CheckUnknown
		c.Summary = v.traceWhy + " Run a route trace."
		if v.traceWhy == "" {
			c.Summary = "Run a route trace to a game server or one of your relays."
		}
	}
	return c, nil
}

// ispVerdict is the ISP route verdict; boosting: a boost is running, so the
// advice is to keep it on rather than to start one.
func ispVerdict(conf Confidence, fact string, boosting bool) *Verdict {
	advice := "Boost the game through a relay and compare ping and loss with the direct route on the game page. If it keeps happening, send the exported report to your ISP."
	if boosting {
		advice = "Keep the boost on: the relay route avoids the loss on the direct one. If it keeps happening, send the exported report to your ISP."
	}
	return &Verdict{ID: VerdictISP, Segment: SegmentISP, Confidence: conf, LowpathHelps: true,
		Title:  "Your ISP's route is losing traffic",
		Detail: "Loss starts past your home network, on the way to the destination, and carries on to the end. This is what a relay can route around.",
		Advice: advice,
		Facts:  []string{fact}}
}

func traceHost(t *TraceResult) string {
	switch {
	case t.Host != "":
		return t.Host
	case t.Target != "":
		return t.Target
	}
	return "the target"
}

// pathWords says where path numbers come from: "During the Counter-Strike
// 2 boost" or "The route analysis for Dota 2 (5 min ago)".
func pathWords(p *PathEvidence, v *view) string {
	game := p.Game
	if game == "" {
		game = "the game"
	}
	if p.Source == PathBoost {
		return "During the " + game + " boost"
	}
	return fmt.Sprintf("The route analysis for %s (%s ago)", game, ago(v.ev.At.Sub(p.At)))
}

func methodFact(p *PathEvidence) string {
	if p.SameMethod {
		return "Both routes were measured the same way, to the same server."
	}
	return "The direct route is measured with ICMP ping and the relay route with UDP probes, so the two aren't exactly alike."
}

// serverRule: the route is clean up to the relay, and the relay's leg to
// the game server loses traffic or jitters; or, during a boost, both the
// direct route and the relay fail to get steady answers from the server.
type serverRule struct{}

func (serverRule) check(v *view) (Check, *Verdict) {
	c := Check{Rule: ruleServer, Name: "Game server"}
	var facts []string
	vd := &Verdict{ID: VerdictServer, Segment: SegmentServer,
		Title:  "The game server's end is the problem",
		Detail: "Your connection and the route up to the relay are clean; the trouble is between the relay and the game server, or at the server itself. Neither you nor Lowpath can fix that.",
		Advice: "Try another server or region in the game if it offers one, or check the game's server status page."}
	p := v.path
	relayClean := p != nil && p.Relay != nil && p.Relay.Samples >= minPathSamples && p.Relay.LossPct < cleanLossPct && p.Relay.JitterMS < cleanJitterMS
	if relayClean {
		legLoss := p.ServerLossPct != nil && *p.ServerLossPct >= routeLossPct
		legJitter := p.ServerJitterMS != nil && *p.ServerJitterMS >= badJitterMS
		if legLoss || legJitter {
			fact := fmt.Sprintf("%s: the relay leg loses %.1f%% with %.0f ms jitter; the relay's leg to the server loses %s%% with %s ms jitter.",
				pathWords(p, v), p.Relay.LossPct, p.Relay.JitterMS, pctText(p.ServerLossPct), msText(p.ServerJitterMS))
			vd.Confidence = ConfidenceHigh
			if p.Source == PathAnalysis {
				vd.Confidence = ConfidenceMedium
			}
			vd.Facts = []string{fact}
			c.Status, c.Facts, c.Summary = CheckFound, vd.Facts, vd.Title+"."
			return c, vd
		}
		if p.Source == PathBoost && p.ServerMS == nil && p.Direct != nil && p.Direct.Samples >= minPathSamples && p.Direct.LossPct >= routeLossPct {
			// Two different routes, one shared end. Only a hint: the relay
			// reporting no time can also mean it can't ping at all (no ICMP
			// rights, an older relay, a boost a few seconds old), and an ISP
			// problem on the direct route would look the same.
			vd.Confidence = ConfidenceLow
			vd.Title = "The game server's end may be the problem"
			vd.Detail = "The direct route to the game server loses pings while the route to your relay is clean, and the relay reports no time for its own leg to the server. The server's end is what both routes share, but this is a hint: a problem on your ISP's direct route would look the same."
			vd.Advice = "Keep the boost on if the game feels better through the relay. Otherwise try another server or region in the game, or check the game's server status page."
			vd.Facts = []string{
				fmt.Sprintf("%s: the direct route loses %.1f%% of pings to the game server, the route to the relay is clean (%.1f%% loss), and the relay reports no time to the server.",
					pathWords(p, v), p.Direct.LossPct, p.Relay.LossPct),
				"The relay may be unable to ping the server, and servers often limit ping, so this is a hint rather than proof.",
			}
			c.Status, c.Facts, c.Summary = CheckFound, vd.Facts, vd.Title+"."
			return c, vd
		}
	}
	if t := v.trace; t != nil && v.ev.TraceKind == TraceKindServer && t.Reached && t.LossFrom != nil && *t.LossFrom == len(t.Hops) && t.End == nil {
		facts = append(facts, fmt.Sprintf("Only the game server itself loses replies to the route trace (%.1f%%). Servers often limit ping, so this alone doesn't show a problem.", *t.DestLossPct))
	}
	switch {
	case p != nil && p.Direct != nil && p.Direct.Samples >= minPathSamples && p.Direct.LossPct < cleanLossPct && p.Direct.JitterMS < badJitterMS:
		facts = append(facts, fmt.Sprintf("%s: the game server answers %.1f%% of pings straight from your PC, with %.0f ms jitter.", pathWords(p, v), 100-p.Direct.LossPct, p.Direct.JitterMS))
		c.Status = CheckClear
		c.Summary = "The game server answers steadily."
	case p != nil && p.ServerLossPct != nil && *p.ServerLossPct < cleanLossPct && (p.ServerJitterMS == nil || *p.ServerJitterMS < badJitterMS):
		c.Status = CheckClear
		c.Summary = "The relay's leg to the game server is clean."
	case p != nil && p.Direct == nil:
		c.Status = CheckUnknown
		c.Summary = "The game server doesn't answer ping from your PC, so its end can't be measured directly."
	default:
		c.Status = CheckUnknown
		c.Summary = "Measure routes on a game's page, or boost a game."
	}
	if p != nil && p.ServerMS != nil {
		facts = append(facts, fmt.Sprintf("The relay reaches the game server in %.0f ms (relays don't report that leg's loss yet).", *p.ServerMS))
	}
	c.Facts = facts
	return c, nil
}

// optimalRule: the direct route is within 2 ms of the relay route and no
// lossier, so Lowpath isn't needed for this game.
type optimalRule struct{}

func (optimalRule) check(v *view) (Check, *Verdict) {
	c := Check{Rule: ruleOptimal, Name: "Relay or direct"}
	p := v.path
	switch {
	case p == nil || p.Relay == nil:
		c.Status = CheckUnknown
		c.Summary = "No comparison of the direct route with a relay yet: measure routes on a game's page, or boost a game."
		return c, nil
	case p.Direct == nil:
		c.Status = CheckUnknown
		c.Summary = "The game server doesn't answer ping, so the direct route can't be compared with the relay."
		return c, nil
	case p.Direct.Samples < minPathSamples || p.Relay.Samples < minPathSamples:
		c.Status = CheckUnknown
		c.Summary = "Too few measurements of the routes so far to compare them."
		return c, nil
	}
	d := p.Direct
	relayMS, whole := p.relayTotal()
	if !whole {
		relayMS = p.Relay.RTTMS
	}
	relayLoss := p.Relay.LossPct
	if p.ServerLossPct != nil {
		relayLoss += *p.ServerLossPct
	}
	route := "the relay route"
	if !whole {
		route = "the relay leg alone"
	}
	fact := fmt.Sprintf("%s: direct %.0f ms with %.1f%% loss, %s %.0f ms with %.1f%% loss.", pathWords(p, v), d.RTTMS, d.LossPct, route, relayMS, relayLoss)
	facts := []string{fact, methodFact(p)}
	// Without the relay's leg to the server the relay route is at least
	// relayMS, so "direct is close enough" still holds; "the relay is
	// better" can't be said.
	if d.RTTMS <= relayMS+optimalMS && d.LossPct <= relayLoss {
		vd := &Verdict{ID: VerdictOptimal, Segment: SegmentLowpath, Confidence: ConfidenceMedium,
			Title:  "Already optimal: Lowpath isn't needed here",
			Detail: fmt.Sprintf("Your direct route is within %d ms of the best relay route and loses no more, so a relay can't make this game feel better.", optimalMS),
			Advice: "Play this game without boosting. Run the comparison again at a busy hour: evening congestion can change the answer.",
			Facts:  facts}
		switch {
		case p.SameMethod:
			vd.Confidence = ConfidenceHigh
		case p.Source == PathAnalysis && v.ev.At.Sub(p.At) > v.cfg.Fresh:
			vd.Confidence = ConfidenceLow
		}
		c.Status, c.Facts, c.Summary = CheckFound, facts, vd.Title+"."
		return c, vd
	}
	c.Facts = facts
	if !whole {
		c.Status = CheckUnknown
		c.Summary = "The relay hasn't measured its leg to the game server yet, so the whole relay route can't be compared."
		return c, nil
	}
	c.Status = CheckClear
	c.Summary = "The relay route is better than the direct one: boosting helps this game."
	return c, nil
}

func maxConf(a, b Confidence) Confidence {
	if confRank(a) >= confRank(b) {
		return a
	}
	return b
}

func minConf(a, b Confidence) Confidence {
	if confRank(a) <= confRank(b) {
		return a
	}
	return b
}

func confRank(c Confidence) int {
	switch c {
	case ConfidenceHigh:
		return 2
	case ConfidenceMedium:
		return 1
	}
	return 0
}

// msText is a latency for the facts: one decimal below 10 ms, "—" unknown.
func msText(v *float64) string {
	switch {
	case v == nil:
		return "—"
	case *v < 10:
		return fmt.Sprintf("%.1f", *v)
	}
	return fmt.Sprintf("%.0f", *v)
}

// pctText is a loss for the facts, "—" unknown.
func pctText(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *v)
}

// plusText is a latency increase for the facts: "+12", "—" unknown.
func plusText(v *float64) string {
	if v == nil {
		return "—"
	}
	return "+" + msText(v)
}
