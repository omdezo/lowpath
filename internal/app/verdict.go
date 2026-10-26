package app

import (
	"errors"
	"net/http"
	"net/netip"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/engine"
)

// The verdict in Settings → Diagnostics: the evidence Lowpath already has
// (the last bufferbloat test and route trace, the running boost, the last
// route analysis) run through diag's verdict engine. Nothing is measured
// here, so a verdict answers at once and the UI can poll it; the tests that
// feed it run on their own.

// verdicts builds the evidence and asks the engine.
type verdicts struct {
	engine *diag.VerdictEngine
	net    diag.NetStats
}

func newVerdicts(ns diag.NetStats) *verdicts {
	return &verdicts{engine: diag.NewVerdictEngine(diag.VerdictConfig{}), net: ns}
}

// evidence gathers what the server knows now. The trace is redacted, so
// every diagnosis built from it can be shared.
func (s *Server) evidence(now time.Time) diag.Evidence {
	ev := diag.Evidence{At: now, Bloat: s.store.Bloat(), Gateway: s.gateway.Sample()}
	switch r, err := s.verdict.net.DefaultRoute(); {
	case err == nil:
		ev.Link = &r
	case errors.Is(err, diag.ErrNoRoute):
		ev.Offline = true
	}
	if raw := s.store.Trace(); raw != nil {
		ev.TraceKind = s.traceKind(raw.Target)
		red := raw.Redacted()
		ev.Trace = &red
	}

	s.mu.Lock()
	sess := s.sess
	var last *analysisResult
	lastID := ""
	for id, a := range s.analysis {
		if last == nil || a.At.After(last.At) {
			a := a
			last, lastID = &a, id
		}
	}
	s.mu.Unlock()

	if sess != nil {
		snap := sess.eng.Snapshot()
		ev.Boost = boostEvidence(snap, s.gameName(sess.GameID), now)
		ev.Passive = s.line.view()
	}
	if last != nil {
		ev.Analysis = analysisEvidence(*last, s.gameName(lastID))
	}
	return ev
}

// traceKind says what a traced address is: one of the user's relays, a
// server a game last connected to, or anything else.
func (s *Server) traceKind(target string) string {
	a, err := netip.ParseAddr(target)
	if err != nil {
		return diag.TraceKindPublic
	}
	for _, p := range s.net.Paths("") {
		if p.Addr.Addr().Unmap() == a {
			return diag.TraceKindRelay
		}
	}
	for _, g := range s.allGames() {
		for _, sv := range s.store.Servers(g.ID) {
			if sv.Addr().Unmap() == a {
				return diag.TraceKindServer
			}
		}
	}
	return diag.TraceKindPublic
}

func (s *Server) gameName(id string) string {
	if g, ok := s.game(id); ok {
		return g.Name
	}
	return id
}

// boostEvidence is a running boost's routes: direct (ICMP to the server,
// when it answers) and the relay route split into the relay leg and the
// relay's own leg to the server.
func boostEvidence(snap engine.Snapshot, game string, now time.Time) *diag.PathEvidence {
	p := &diag.PathEvidence{Source: diag.PathBoost, At: now, Game: game}
	if snap.BeforeKnown {
		p.Direct = pathStats(snap.Before, false)
	}
	if a := snap.After; a.Up && a.Samples > 0 && !snap.Fallback {
		p.Relay = pathStats(a, true)
		switch {
		case a.Server > 0:
			p.ServerMS = ptrTo(a.Server)
		case snap.ServerLegKnown:
			p.ServerMS = ptrTo(snap.ServerLeg)
		}
	}
	return p
}

// analysisEvidence is the last route analysis: direct against the best
// relay that answered.
func analysisEvidence(a analysisResult, game string) *diag.PathEvidence {
	p := &diag.PathEvidence{Source: diag.PathAnalysis, At: a.At, Game: game}
	if a.DirectKnown && a.Direct.Up && a.Direct.Samples > 0 {
		p.Direct = pathStats(a.Direct, false)
	}
	var best *engine.Stats
	for _, st := range a.Paths {
		if !st.Up || st.Samples == 0 {
			continue
		}
		if best == nil || st.Score() < best.Score() {
			st := st
			best = &st
		}
	}
	if best != nil {
		p.Relay = pathStats(*best, true)
		if best.Server > 0 {
			p.ServerMS = ptrTo(best.Server)
		}
	}
	return p
}

// pathStats converts engine stats; for a relay route, leg drops the
// relay's part to the server, which RTT includes when it was measured.
func pathStats(st engine.Stats, leg bool) *diag.PathStats {
	rtt := st.RTT
	if leg && st.Server > 0 {
		rtt = max(st.RTT-st.Server, 0)
	}
	return &diag.PathStats{RTTMS: rtt, LossPct: st.Loss, JitterMS: st.Jitter, Samples: st.Samples}
}

func ptrTo[T any](v T) *T { return &v }

// diagnosis is the current verdict.
func (s *Server) diagnosis() diag.Diagnosis {
	return s.verdict.engine.Diagnose(s.evidence(time.Now()))
}

// --- HTTP ---------------------------------------------------------------

func (s *Server) handleVerdict(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.diagnosis())
}

// handleVerdictReport is the diagnosis as plain text, ready to paste into
// a forum post or a support ticket. Addresses are cut to their /24 (/48)
// and host names left out: unlike the route trace export, this one is
// meant to be shared.
func (s *Server) handleVerdictReport(w http.ResponseWriter, r *http.Request) {
	ev := s.evidence(time.Now())
	rep := diag.DiagnosisReport{Version: Version, OS: s.plat.OS, Diagnosis: s.verdict.engine.Diagnose(ev), Evidence: ev, Location: s.lastLocation()}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="lowpath-diagnosis.txt"`)
	w.Write([]byte(rep.Text()))
}
