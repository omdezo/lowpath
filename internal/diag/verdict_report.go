package diag

import (
	"fmt"
	"strings"
	"time"
)

// DiagnosisReport is the exportable text version of a diagnosis: the
// verdict, every check, and a summary of the measurements behind them, for
// a forum post or a support ticket. Build it from redacted evidence and a
// redacted location (Location.Redacted, TraceResult.Redacted): the report
// may be handed to anyone.
type DiagnosisReport struct {
	Version   string
	OS        string
	Diagnosis Diagnosis
	Evidence  Evidence
	Location  *Location // nil: not looked up
}

// reportWidth is where the report's prose wraps.
const reportWidth = 78

// Text renders the report. Unknown numbers print as "—".
func (r DiagnosisReport) Text() string {
	var b strings.Builder
	d := r.Diagnosis
	b.WriteString("Lowpath diagnosis\n")
	fmt.Fprintf(&b, "Generated %s", d.At.Format(time.RFC3339))
	if r.Version != "" {
		fmt.Fprintf(&b, " by Lowpath %s", r.Version)
	}
	if r.OS != "" {
		fmt.Fprintf(&b, " on %s", r.OS)
	}
	b.WriteString(".\n")
	conn := "unknown"
	if d.Link != "" || d.Interface != "" {
		conn = linkWords(d.Link, d.Interface)
	}
	fmt.Fprintf(&b, "Connection: %s.\n", conn)
	if w := wifiLine(r.Evidence.WiFi); w != "" && d.Link == LinkWiFi {
		fmt.Fprintf(&b, "Wi-Fi: %s.\n", w)
	}
	if n := networkLine(r.Location); n != "" {
		fmt.Fprintf(&b, "Network: %s.\n", n)
	}

	v := d.Verdict
	fmt.Fprintf(&b, "\nVERDICT: %s (confidence: %s)\n", v.Title, v.Confidence)
	wrap(&b, "", v.Detail)
	if v.Advice != "" {
		wrap(&b, "", "What to do: "+v.Advice)
	}
	if v.LowpathHelps {
		wrap(&b, "", "Lowpath can help with this: a relay sends the game's traffic another way.")
	}
	if len(v.Facts) > 0 {
		b.WriteString("Why:\n")
		for _, f := range v.Facts {
			wrap(&b, "  - ", f)
		}
	}

	b.WriteString("\nCHECKS\n")
	for _, c := range d.Checks {
		wrap(&b, fmt.Sprintf("  [%s] ", checkWord(c)), c.Name+": "+c.Summary)
		for _, f := range c.Facts {
			wrap(&b, "      - ", f)
		}
	}
	if len(d.Next) > 0 {
		b.WriteString("\nFOR A SURER ANSWER\n")
		for _, n := range d.Next {
			wrap(&b, "  - ", n.Text)
		}
	}

	b.WriteString("\nMEASUREMENTS\n")
	r.measurements(&b)
	b.WriteString("\nAddresses are cut to their /24 (IPv4) or /48 (IPv6) network and routers' host\nnames are left out. The full route trace is a separate export.\n")
	return b.String()
}

func checkWord(c Check) string {
	switch {
	case c.Status == CheckFound && c.Rule == ruleOptimal:
		return "not needed"
	case c.Status == CheckFound:
		return "problem"
	case c.Status == CheckClear:
		return "ok"
	}
	return "not measured"
}

func networkLine(l *Location) string {
	if l == nil {
		return ""
	}
	var parts []string
	for _, e := range []*Endpoint{l.IPv4, l.IPv6} {
		if e == nil {
			continue
		}
		s := e.IP.String()
		if e.Prefix.IsValid() {
			s = e.Prefix.String()
		}
		if e.ASN != 0 {
			s = fmt.Sprintf("AS%d, %s", e.ASN, s)
		}
		if e.ISP != "" {
			s = e.ISP + " (" + s + ")"
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		if l.Message != "" {
			return "— (" + strings.TrimSuffix(l.Message, ".") + ")"
		}
		return ""
	}
	where := ""
	for _, e := range []*Endpoint{l.IPv4, l.IPv6} {
		if e != nil && e.City != "" {
			where = e.City
			if e.Country != "" {
				where += " (" + e.Country + ")"
			}
			break
		}
	}
	s := strings.Join(parts, "; ")
	if where != "" {
		s += "; " + where
	}
	return s
}

// measurements summarizes each input the engine had.
func (r DiagnosisReport) measurements(b *strings.Builder) {
	ev := r.Evidence
	at := r.Diagnosis.At
	line := func(s string) { wrap(b, "  ", s) }
	if g := ev.Gateway; g != nil {
		line(fmt.Sprintf("Router pings: %d sent, %d lost, median %s ms, p%d %s ms (%s ago).", g.Sent, g.Lost, msText(g.P50MS), g.TailPct, msText(g.TailMS), ago(at.Sub(g.At))))
	}
	if bl := ev.Bloat; bl != nil {
		grade := bl.Grade
		if grade == "" {
			grade = "—"
		}
		line(fmt.Sprintf("Bufferbloat test (%s ago, %s): grade %s; idle %s ms, downloading %s ms at %s Mbit/s, uploading %s ms at %s Mbit/s.",
			ago(at.Sub(bl.FinishedAt)), linkWords(bl.Link, bl.Interface), grade, msText(bl.IdleMS), plusText(bl.DownIncreaseMS), optMbps(bl.DownMbps),
			plusText(bl.UpIncreaseMS), optMbps(bl.UpMbps)))
		if bl.Note != "" {
			line("  " + bl.Note)
		}
	} else {
		line("Bufferbloat test: not run.")
	}
	if p := ev.Passive; p != nil {
		state := "watching, nothing else filling the line"
		switch {
		case p.Active:
			state = passiveFact(p)
		case !p.Watching:
			state = "not watching: " + p.Reason
		}
		line("Passive detector during the boost: " + state)
	}
	if t := ev.Trace; t != nil {
		mode := "ICMP"
		if t.Mode == TraceUDP {
			mode = "UDP to the relay port"
		}
		line(fmt.Sprintf("Route trace to %s (%s ago, %s, %d of %d rounds, %d hops): destination loss %s%%.",
			traceHost(t), ago(at.Sub(t.FinishedAt)), mode, t.Rounds, t.Planned, len(t.Hops), pctText(t.DestLossPct)))
		if t.Verdict != "" {
			line("  " + t.Verdict)
		}
	} else {
		line("Route trace: not run.")
	}
	for _, p := range []*PathEvidence{ev.Boost, ev.Analysis} {
		if p == nil {
			continue
		}
		what := "Route analysis"
		if p.Source == PathBoost {
			what = "Running boost"
		}
		s := fmt.Sprintf("%s for %s (%s ago): direct %s; relay leg %s; relay to server %s ms.", what, p.Game, ago(at.Sub(p.At)),
			pathText(p.Direct), pathText(p.Relay), msText(p.ServerMS))
		line(s)
	}
}

func pathText(s *PathStats) string {
	if s == nil {
		return "—"
	}
	return fmt.Sprintf("%.0f ms, %.1f%% loss, %.1f ms jitter (%d samples)", s.RTTMS, s.LossPct, s.JitterMS, s.Samples)
}

// wrap writes text after prefix, wrapped at reportWidth with following
// lines indented to match the prefix.
func wrap(b *strings.Builder, prefix, text string) {
	indent := strings.Repeat(" ", len([]rune(prefix)))
	cur := prefix
	n := len([]rune(cur))
	empty := true
	for _, w := range strings.Fields(text) {
		wl := len([]rune(w))
		if !empty && n+1+wl > reportWidth {
			b.WriteString(cur + "\n")
			cur, n, empty = indent, len([]rune(indent)), true
		}
		if !empty {
			cur += " "
			n++
		}
		cur += w
		n += wl
		empty = false
	}
	b.WriteString(cur + "\n")
}

// wifiLine is the radio link in one line: "-61 dBm, 5 GHz channel 36
// (80 MHz), Wi-Fi 6, 866 Mbit/s Tx, channel 12% busy". No network name or
// access point address: those can locate a home.
func wifiLine(w *WiFiSignal) string {
	if w == nil {
		return ""
	}
	var parts []string
	if w.RSSI != nil {
		parts = append(parts, fmt.Sprintf("%d dBm", *w.RSSI))
	}
	if w.Band != "" {
		ch := w.Band + " GHz"
		if w.Channel != nil {
			ch += fmt.Sprintf(" channel %d", *w.Channel)
		}
		if w.WidthMHz != nil {
			ch += fmt.Sprintf(" (%d MHz)", *w.WidthMHz)
		}
		parts = append(parts, ch)
	}
	if w.Generation != "" {
		parts = append(parts, w.Generation)
	}
	if w.TxMbps != nil {
		parts = append(parts, fmt.Sprintf("%.0f Mbit/s Tx", *w.TxMbps))
	}
	if w.BusyPct != nil {
		parts = append(parts, fmt.Sprintf("channel %.0f%% busy", *w.BusyPct))
	}
	return strings.Join(parts, ", ")
}
