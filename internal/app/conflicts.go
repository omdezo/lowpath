package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"lowpath/internal/compat"
	"lowpath/internal/platform"
)

const (
	// conflictEvery is how often the background scan looks again, so the
	// banner appears and goes within seconds of a booster or VPN starting
	// or closing.
	conflictEvery = 15 * time.Second
	// conflictWait is how long Boost and the API wait for a fresh scan. A
	// scan takes tens of milliseconds; one that hangs (a service manager
	// that stopped answering) must not hold up a boost.
	conflictWait = 3 * time.Second
	// conflictLimit cuts a slow scan's remaining steps short. An OS call
	// that never returns can't be ended: the next scan starts once it does,
	// and until then callers get the last report (see scan).
	conflictLimit = 30 * time.Second
)

// conflictWatch keeps the latest conflict report. Scans run one at a time
// in their own goroutine; callers wait a bounded time and otherwise get the
// last report, so a slow or broken OS call never blocks the UI or a boost
// (fail open: no report means nothing blocks).
type conflictWatch struct {
	det      *compat.Detector
	log      *slog.Logger
	lifetime context.Context
	wait     time.Duration // how long a caller waits for a fresh scan (conflictWait; tests shorten it)

	mu       sync.Mutex
	last     *compat.Report // nil until the first scan ends
	inflight chan struct{}  // closed when the running scan ends; nil when none runs
}

func newConflictWatch(plat *platform.Platform, lifetime context.Context, log *slog.Logger) *conflictWatch {
	// WinDivert loaded from one of these folders is Lowpath's own.
	var own []string
	for _, elevated := range []bool{true, false} {
		dirs, _ := plat.Paths.DriverDirs(elevated)
		for _, d := range dirs {
			if !slices.Contains(own, d) {
				own = append(own, d)
			}
		}
	}
	return &conflictWatch{
		det:      compat.NewDetector(compat.Config{System: plat.Inventory, OwnDriverDirs: own}),
		log:      log,
		lifetime: lifetime,
		wait:     conflictWait,
	}
}

// run scans every conflictEvery until ctx ends.
func (w *conflictWatch) run(ctx context.Context) {
	t := time.NewTicker(conflictEvery)
	defer t.Stop()
	for {
		w.scan()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// scan starts a scan (or joins the one running) and waits up to w.wait for
// it. When it takes longer, the previous report comes back with a note.
func (w *conflictWatch) scan() compat.Report {
	r, _ := w.check()
	return r
}

// check is scan, also saying whether the report comes from a scan that
// finished while the caller waited. A stale report must not refuse a boost:
// what it named may have closed since, and the scan that would say so hangs.
func (w *conflictWatch) check() (r compat.Report, fresh bool) {
	wait := w.wait
	w.mu.Lock()
	ch := w.inflight
	if ch == nil {
		ch = make(chan struct{})
		w.inflight = ch
		go w.scanOnce(ch)
	}
	w.mu.Unlock()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ch:
		return w.current(), true
	case <-t.C:
	}
	r = w.current()
	r.Unchecked = append(slices.Clone(r.Unchecked), fmt.Sprintf("the check didn't finish within %v; showing the last result", wait))
	return r, false
}

func (w *conflictWatch) scanOnce(done chan struct{}) {
	var r compat.Report
	defer func() {
		if p := recover(); p != nil {
			// A crash in an OS call: report it and block nothing.
			w.log.Error("conflict scan panicked", "panic", p)
			r = compat.Report{At: time.Now(), Conflicts: []compat.Conflict{}, Unchecked: []string{fmt.Sprintf("the check failed: %v", p)}}
		}
		w.mu.Lock()
		prev := w.last
		w.last = &r
		w.inflight = nil
		w.mu.Unlock()
		close(done)
		if summary(prev) != summary(&r) {
			w.log.Info("other network software", "found", summary(&r), "unchecked", r.Unchecked)
		}
	}()
	ctx, cancel := context.WithTimeout(w.lifetime, conflictLimit)
	defer cancel()
	r = w.det.Scan(ctx)
}

// current is the last report, or an empty one before the first scan.
func (w *conflictWatch) current() compat.Report {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.last == nil {
		return compat.Report{Conflicts: []compat.Conflict{}, Unchecked: []string{}}
	}
	return *w.last
}

// view is the last report for the live feed (nil before the first scan).
func (w *conflictWatch) view() *compat.Report {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.last
}

// summary is "exitlag:block vpn:warn", for logging changes only.
func summary(r *compat.Report) string {
	if r == nil {
		return ""
	}
	parts := make([]string, 0, len(r.Conflicts))
	for _, c := range r.Conflicts {
		parts = append(parts, c.ID+":"+string(c.Level))
	}
	return strings.Join(parts, " ")
}

// handleConflicts answers with the last report; ?fresh=1 scans again first
// (the UI's "Check again").
func (s *Server) handleConflicts(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("fresh") == "1" || s.conflicts.view() == nil {
		writeJSON(w, 200, s.conflicts.scan())
		return
	}
	writeJSON(w, 200, s.conflicts.current())
}

// conflictErr is Boost's answer when route mode would fight another
// program for the game's packets.
func conflictErr(block []compat.Conflict) *apiError {
	verb, them := "is", "it"
	if len(block) > 1 {
		verb, them = "are", "them"
	}
	return &apiError{
		code: 409, kind: "conflict", conflicts: block,
		err: fmt.Errorf("%s %s running and captures game traffic too: close %s first, then press Boost", compat.Names(block), verb, them),
	}
}

// withConflictHint adds the running conflicts to a capture failure, which
// they often cause (another driver holding the packets, a different
// WinDivert version loaded).
func (s *Server) withConflictHint(e *apiError) *apiError {
	if e.kind != "capture_failed" {
		return e
	}
	active := s.conflicts.current().Active()
	if len(active) == 0 {
		return e
	}
	return &apiError{code: e.code, kind: e.kind, conflicts: active,
		err: fmt.Errorf("%w (%s may be in the way: see the notice above)", e.err, compat.Names(active))}
}
