package app

import (
	"context"
	"sync"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/engine"
)

// lineWatchEvery is how often the passive detector samples the adapter
// counters and the session's latency.
const lineWatchEvery = time.Second

// routeRefresh is how often the default interface is looked up again
// (Wi-Fi to Ethernet, VPN up or down).
const routeRefresh = 10 * time.Second

// lineWatch runs the passive bufferbloat detector during a session: once a
// second it reads the default interface's byte counters (GetIfEntry2Ex on
// Windows) and the relay latency from the engine, and flags when other
// traffic fills the line while the game's ping rises.
type lineWatch struct {
	net diag.NetStats
	det *diag.PassiveDetector

	// Only the watch goroutine touches these.
	meter    *diag.Meter
	route    diag.Route
	routeOK  bool
	routeAt  time.Time
	sessKey  time.Time
	wireUp   uint64
	wireDown uint64
	wireAt   time.Time

	mu      sync.Mutex
	state   *diag.PassiveState // nil: no session
	newLine *diag.LineRate     // from a test that just finished, for the next tick
}

func newLineWatch(ns diag.NetStats, line diag.LineRate) *lineWatch {
	return &lineWatch{net: ns, det: diag.NewPassiveDetector(diag.PassiveConfig{}, line), meter: diag.NewMeter()}
}

// lineRate is the line rate a finished bufferbloat test measured.
func lineRate(r *diag.BloatResult) diag.LineRate {
	var l diag.LineRate
	if r == nil {
		return l
	}
	if r.DownMbps != nil {
		l.DownBps = *r.DownMbps * 1e6 / 8
	}
	if r.UpMbps != nil {
		l.UpBps = *r.UpMbps * 1e6 / 8
	}
	return l
}

func (w *lineWatch) view() *diag.PassiveState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

// setLineRate hands the watch goroutine a new line rate.
func (w *lineWatch) setLineRate(l diag.LineRate) {
	w.mu.Lock()
	w.newLine = &l
	w.mu.Unlock()
}

func (w *lineWatch) set(st *diag.PassiveState) {
	w.mu.Lock()
	w.state = st
	w.mu.Unlock()
}

// lineInput is what the watch needs from the server each second.
type lineInput struct {
	snap      *engine.Snapshot // nil: no session
	started   time.Time        // session start, to notice a new session
	bloatTest bool             // the active test is loading the line
}

func (w *lineWatch) run(ctx context.Context, input func() lineInput) {
	t := time.NewTicker(lineWatchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			w.tick(now, input())
		}
	}
}

func (w *lineWatch) tick(now time.Time, in lineInput) {
	w.mu.Lock()
	nl := w.newLine
	w.newLine = nil
	w.mu.Unlock()
	if nl != nil {
		w.det.SetLineRate(*nl)
	}
	if !w.routeOK || now.Sub(w.routeAt) >= routeRefresh {
		w.route, w.routeAt = diag.Route{}, now
		r, err := w.net.DefaultRoute()
		w.routeOK = err == nil
		if w.routeOK {
			w.route = r
		}
	}
	var tp diag.Throughput
	rateOK := false
	if w.routeOK {
		if c, err := w.net.Counters(w.route.Index); err == nil {
			tp, rateOK = w.meter.Add(w.route.Index, c)
		} else {
			w.meter.Reset()
			w.routeOK = false
		}
	}
	if in.snap == nil {
		w.set(nil)
		w.sessKey = time.Time{}
		return
	}
	snap := in.snap
	if !in.started.Equal(w.sessKey) {
		w.sessKey = in.started
		w.det.Reset()
		w.wireUp, w.wireDown, w.wireAt = snap.WireUp, snap.WireDown, now
		w.set(&diag.PassiveState{Reason: "learning your normal latency"})
		return
	}
	s := diag.PassiveSample{At: now, Rx: tp.RxBps, Tx: tp.TxBps, RateKnown: rateOK}
	if dt := now.Sub(w.wireAt).Seconds(); dt > 0 && snap.WireUp >= w.wireUp && snap.WireDown >= w.wireDown {
		s.OwnTx = float64(snap.WireUp-w.wireUp) / dt
		s.OwnRx = float64(snap.WireDown-w.wireDown) / dt
	}
	w.wireUp, w.wireDown, w.wireAt = snap.WireUp, snap.WireDown, now
	// The relay leg is what queues on the user's line; the relay's own
	// leg to the game server moves for other reasons.
	if a := snap.After; a.Up && a.Samples > 0 {
		s.RTT, s.RTTKnown = time.Duration(max(a.RTT-a.Server, 0)*float64(time.Millisecond)), true
	}
	if in.bloatTest {
		w.set(&diag.PassiveState{Reason: "the bufferbloat test is loading the line"})
		return
	}
	st := w.det.Observe(s)
	w.set(&st)
}
