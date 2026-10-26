package diag

import (
	"fmt"
	"strings"
	"time"
)

// Report is the trace as a plain-text table in WinMTR's column order
// (loss, sent, received, best, average, worst, last), plus the loss that
// persists to the destination, for pasting into a support ticket. Unknown
// numbers print as "-".
func (r TraceResult) Report() string {
	var b strings.Builder
	mode := "ICMP"
	if r.Mode == TraceUDP {
		mode = "UDP to the relay port (hops over ICMP)"
	}
	target := r.Target
	if r.Host != "" && r.Host != r.Target {
		target = fmt.Sprintf("%s (%s)", r.Host, r.Target)
	}
	fmt.Fprintf(&b, "Lowpath route trace to %s\n", target)
	fmt.Fprintf(&b, "Mode: %s. %d of %d rounds, one probe per hop every %s.\n", mode, r.Rounds, r.Planned, fmtInterval(r.IntervalS))
	if !r.StartedAt.IsZero() {
		fmt.Fprintf(&b, "Started %s", r.StartedAt.Format(time.RFC3339))
		if !r.FinishedAt.IsZero() {
			fmt.Fprintf(&b, ", took %s", r.FinishedAt.Sub(r.StartedAt).Round(time.Second))
		}
		b.WriteString(".\n")
	}
	b.WriteString("\n")

	const head = "%3s  %-44s %6s %8s %5s %5s %7s %7s %7s %7s %7s\n"
	fmt.Fprintf(&b, head, "#", "Host", "Loss%", "Persist%", "Sent", "Recv", "Best", "Avg", "Worst", "Last", "StDev")
	row := func(h TraceHop) {
		nr := fmt.Sprint(h.TTL)
		host := h.Addr
		if h.Probe == "udp" {
			nr, host = "UDP", r.EndName
		}
		if host == "" {
			host = "(no reply)"
		}
		if h.Name != "" {
			host = fmt.Sprintf("%s [%s]", h.Name, host)
		}
		if rs := []rune(host); len(rs) > 44 {
			// Cut by character: a relay label may not be ASCII.
			host = string(rs[:43]) + "~"
		}
		fmt.Fprintf(&b, head, nr, host, fmt.Sprintf("%.1f", h.LossPct), optPct(h.PersistPct), fmt.Sprint(h.Sent), fmt.Sprint(h.Recv),
			optMS(h.BestMS), optMS(h.AvgMS), optMS(h.WorstMS), optMS(h.LastMS), optMS(h.StDevMS))
		for _, o := range h.Others {
			fmt.Fprintf(&b, "%3s  %-44s\n", "", "  also "+o)
		}
	}
	for _, h := range r.Hops {
		row(h)
	}
	if r.End != nil {
		row(*r.End)
	}
	b.WriteString("\n")
	if r.Verdict != "" {
		b.WriteString(r.Verdict + "\n")
	}
	for _, n := range r.Notes {
		b.WriteString(n + "\n")
	}
	b.WriteString("\nPersist% is the loss that carries on from that hop to the destination. Loss a router shows only\n" +
		"for its own replies (it limits answers to trace probes) is not lost traffic.\n")
	return b.String()
}

func optPct(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}

func optMS(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}

func fmtInterval(s float64) string {
	if s >= 1 {
		return fmt.Sprintf("%g s", s)
	}
	return fmt.Sprintf("%g ms", s*1000)
}
