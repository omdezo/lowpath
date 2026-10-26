// Package diag measures the user's own connection: bufferbloat (latency
// under load, graded A+ to F), a passive detector that notices during play
// when something else on the network fills the line, and the per-OS readers
// for the default route and interface byte counters they need.
//
// Nothing here needs administrator rights, and every measurement can come
// back unknown: callers show "—" or say why, never a made-up number.
package diag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Bufferbloat test phases, in order.
const (
	PhaseIdle     = "idle"
	PhaseDownload = "download"
	PhaseUpload   = "upload"
	PhaseRecovery = "recovery"
)

// phaseOrder is the order the phases run in.
func phaseOrder() [4]string { return [4]string{PhaseIdle, PhaseDownload, PhaseUpload, PhaseRecovery} }

// Grade turns the latency added under load into a letter, on the scale the
// Waveform test popularized: A+ < 5 ms, A < 30, B < 60, C < 200, D < 400,
// else F.
func Grade(increaseMS float64) string {
	switch {
	case math.IsNaN(increaseMS):
		return ""
	case increaseMS < 5:
		return "A+"
	case increaseMS < 30:
		return "A"
	case increaseMS < 60:
		return "B"
	case increaseMS < 200:
		return "C"
	case increaseMS < 400:
		return "D"
	default:
		return "F"
	}
}

// Candidate is a possible internet latency target. The test probes every
// candidate briefly and keeps the fastest that answers in the lowest tier
// (e.g. tier 0 = the user's relays, 1 = public ICMP, 2 = TCP connect).
type Candidate struct {
	Tier   int
	Prober Prober
}

// BloatConfig describes one test. Durations left zero get the defaults
// from the roadmap: 5 s per phase, 10 Hz probes.
type BloatConfig struct {
	// Internet targets measure the latency the game feels; the grade comes
	// from them. Gateway (nil: unknown) shows whether the home network
	// itself queues.
	Internet []Candidate
	Gateway  Prober
	Loader   Loader // nil: the line can't be loaded, so no grade

	// Net and Route give NIC throughput during each phase (nil / index 0:
	// throughput comes from the bytes the loader moved only).
	Net   NetStats
	Route Route

	Phase    time.Duration // per phase (5 s)
	Ramp     time.Duration // start of a loaded phase that is not graded while TCP ramps up (1 s)
	Interval time.Duration // between probes (100 ms)
	Timeout  time.Duration // per probe (1 s); a later reply counts as lost
	Pick     time.Duration // longest time to choose the internet target (2 s)
}

func (c *BloatConfig) defaults() {
	set := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	set(&c.Phase, 5*time.Second)
	set(&c.Ramp, time.Second)
	set(&c.Interval, 100*time.Millisecond)
	set(&c.Timeout, time.Second)
	set(&c.Pick, 2*time.Second)
	if c.Ramp >= c.Phase {
		c.Ramp = c.Phase / 5
	}
}

// LatencyStats summarizes the probes of one phase to one target. Nil
// numbers: no probe was answered.
type LatencyStats struct {
	Sent int      `json:"sent"`
	Lost int      `json:"lost"`
	Min  *float64 `json:"min_ms"`
	P50  *float64 `json:"p50_ms"`
	P95  *float64 `json:"p95_ms"`
}

func (s LatencyStats) lossFrac() float64 {
	if s.Sent == 0 {
		return 1
	}
	return float64(s.Lost) / float64(s.Sent)
}

// PhaseResult is one phase of the test.
type PhaseResult struct {
	Phase    string        `json:"phase"`
	Internet LatencyStats  `json:"internet"`
	Gateway  *LatencyStats `json:"gateway"` // nil: no gateway target
	// LoadMbps is what the test itself moved in the graded window; RxMbps
	// and TxMbps are the network adapter's totals (all traffic on the PC).
	LoadMbps  *float64 `json:"load_mbps"`
	RxMbps    *float64 `json:"rx_mbps"`
	TxMbps    *float64 `json:"tx_mbps"`
	LoadError string   `json:"load_error,omitempty"`
}

// BloatResult is a finished test.
type BloatResult struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Grade is "" when the test could not tell; Note says why. Note also
	// carries caveats next to a grade.
	Grade     string `json:"grade"`
	Note      string `json:"note,omitempty"`
	Target    string `json:"target"`  // internet latency target, "" none answered
	Gateway   string `json:"gateway"` // gateway target, "" none
	Interface string `json:"interface"`
	Link      string `json:"link"` // LinkWiFi, LinkEthernet, ...

	IdleMS         *float64 `json:"idle_ms"`
	DownIncreaseMS *float64 `json:"down_increase_ms"`
	UpIncreaseMS   *float64 `json:"up_increase_ms"`
	// Line throughput while loaded (adapter counters when readable, else
	// the test's own bytes). The passive detector uses it as line rate.
	DownMbps *float64      `json:"down_mbps"`
	UpMbps   *float64      `json:"up_mbps"`
	DataMB   float64       `json:"data_mb"`
	Phases   []PhaseResult `json:"phases"`
}

// BloatProgress is reported while a test runs.
type BloatProgress struct {
	Phase    string   `json:"phase"` // "picking" before the first phase
	Elapsed  float64  `json:"elapsed_s"`
	Total    float64  `json:"total_s"`
	LastMS   *float64 `json:"last_ms"`   // latest internet probe, nil: lost or none yet
	LoadMbps *float64 `json:"load_mbps"` // this phase so far
	Target   string   `json:"target"`
}

// ErrNoTarget: no internet latency target answered.
var ErrNoTarget = errors.New("diag: no latency target answered")

// BloatTest runs the active bufferbloat test: idle, download, upload and
// recovery phases, probing latency throughout.
type BloatTest struct {
	cfg BloatConfig
}

// NewBloatTest applies the defaults to cfg.
func NewBloatTest(cfg BloatConfig) *BloatTest {
	cfg.defaults()
	return &BloatTest{cfg: cfg}
}

// Duration is how long the phases take (target picking comes first).
func (t *BloatTest) Duration() time.Duration { return time.Duration(len(phaseOrder())) * t.cfg.Phase }

type sample struct {
	gateway bool
	at      time.Duration // sent, since the first phase began
	rtt     time.Duration
	ok      bool
}

// runState is what the probe and load goroutines share during one run.
type runState struct {
	mu       sync.Mutex
	samples  []sample
	lastMS   *float64
	phase    string
	moved    atomic.Int64 // bytes the loader moved in the current phase
	total    atomic.Int64 // bytes moved in the whole test
	phaseAt  time.Time
	progress func(BloatProgress)
}

func (s *runState) add(sm sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.samples = append(s.samples, sm)
	if !sm.gateway {
		if sm.ok {
			v := ms(sm.rtt)
			s.lastMS = &v
		} else {
			s.lastMS = nil
		}
	}
}

// Run performs the test. It returns an error only when ctx ends first;
// everything else (offline, ICMP blocked, load server refusing) is a
// result with no grade and a Note.
func (t *BloatTest) Run(ctx context.Context, progress func(BloatProgress)) (BloatResult, error) {
	cfg := t.cfg
	if progress == nil {
		progress = func(BloatProgress) {}
	}
	res := BloatResult{StartedAt: time.Now(), Interface: cfg.Route.Name, Link: cfg.Route.Kind}
	total := t.Duration()
	progress(BloatProgress{Phase: "picking", Total: total.Seconds()})

	target, err := pickTarget(ctx, cfg.Internet, cfg.Pick, cfg.Timeout)
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if err == nil {
		res.Target = target.Name()
	}
	if cfg.Gateway != nil {
		res.Gateway = cfg.Gateway.Name()
	}
	if target == nil && cfg.Gateway == nil {
		res.FinishedAt = time.Now()
		res.Note = "Nothing answered the latency probes, so bufferbloat can't be measured. Check that you are online."
		return res, nil
	}

	start := time.Now()
	// The progress loop may report before the first phase begins.
	st := &runState{progress: progress, phase: PhaseIdle, phaseAt: start}
	end := start.Add(total)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var probes sync.WaitGroup
	probes.Add(1)
	go func() {
		defer probes.Done()
		t.probeLoop(runCtx, st, start, end, target, &probes)
	}()
	stopProgress := t.progressLoop(runCtx, st, start, total, res.Target)

	for i, name := range phaseOrder() {
		pr, err := t.runPhase(ctx, runCtx, st, name, start.Add(time.Duration(i)*cfg.Phase))
		if err != nil {
			return res, err
		}
		res.Phases = append(res.Phases, pr)
	}
	stopProgress()
	cancel()
	probes.Wait()
	if ctx.Err() != nil {
		return res, ctx.Err()
	}

	st.mu.Lock()
	samples := slices.Clone(st.samples)
	st.mu.Unlock()
	for i := range res.Phases {
		pStart := time.Duration(i) * cfg.Phase
		from := pStart
		if p := res.Phases[i].Phase; p == PhaseDownload || p == PhaseUpload {
			from += cfg.Ramp
		}
		window := func(gw bool) LatencyStats { return summarize(samples, gw, from, pStart+cfg.Phase) }
		if target != nil {
			res.Phases[i].Internet = window(false)
		}
		if cfg.Gateway != nil {
			g := window(true)
			res.Phases[i].Gateway = &g
		}
	}
	res.DataMB = float64(st.total.Load()) / 1e6
	res.FinishedAt = time.Now()
	t.grade(&res, target != nil)
	return res, nil
}

// runPhase runs one phase starting at pStart: the load for loaded phases,
// and the adapter counters and loader bytes over the graded window.
func (t *BloatTest) runPhase(ctx, runCtx context.Context, st *runState, name string, pStart time.Time) (PhaseResult, error) {
	cfg := t.cfg
	pEnd := pStart.Add(cfg.Phase)
	loaded := name == PhaseDownload || name == PhaseUpload
	from := pStart
	if loaded {
		from = pStart.Add(cfg.Ramp)
	}
	st.mu.Lock()
	st.phase, st.phaseAt = name, pStart
	st.mu.Unlock()
	st.moved.Store(0)

	pr := PhaseResult{Phase: name}
	lctx, lcancel := context.WithDeadline(runCtx, pEnd)
	defer lcancel()
	var loadDone chan error
	switch {
	case loaded && cfg.Loader != nil:
		loadDone = make(chan error, 1)
		add := func(n int64) { st.moved.Add(n); st.total.Add(n) }
		go func() {
			if name == PhaseDownload {
				loadDone <- cfg.Loader.Download(lctx, add)
			} else {
				loadDone <- cfg.Loader.Upload(lctx, add)
			}
		}()
	case loaded:
		pr.LoadError = "no load server configured"
	}

	if !sleepUntil(ctx, from) {
		return pr, ctx.Err()
	}
	meter := cfg.Net != nil && cfg.Route.Index > 0
	var c0 Counters
	if meter {
		var err error
		c0, err = cfg.Net.Counters(cfg.Route.Index)
		meter = err == nil
	}
	b0 := st.moved.Load()
	if !sleepUntil(ctx, pEnd) {
		return pr, ctx.Err()
	}
	b1 := st.moved.Load()
	if meter {
		if c1, err := cfg.Net.Counters(cfg.Route.Index); err == nil {
			if tp, ok := Rate(c0, c1); ok {
				pr.RxMbps, pr.TxMbps = ptr(tp.RxBps*8/1e6), ptr(tp.TxBps*8/1e6)
			}
		}
	}
	if loadDone != nil {
		lcancel()
		if lerr := <-loadDone; lerr != nil {
			pr.LoadError = lerr.Error()
		} else if b1 == 0 {
			pr.LoadError = "the load server sent almost nothing"
		} else if b1 <= b0 {
			// Every byte moved before the graded window: the data cap ran
			// out first, so the window measured an idle line.
			pr.LoadError = "the line is faster than the test's data cap can keep full"
		}
		if b1 > b0 {
			pr.LoadMbps = ptr(float64(b1-b0) * 8 / pEnd.Sub(from).Seconds() / 1e6)
		}
	}
	return pr, nil
}

// grade fills the increases, line rates, grade and note from the phases.
func (t *BloatTest) grade(res *BloatResult, haveTarget bool) {
	idle, down, up := res.Phases[0], res.Phases[1], res.Phases[2]
	// The adapter sees everything on the PC; the loader only its own
	// bytes, but those may take another interface (VPN) or never touch the
	// adapter (a local test server). The line carried at least the larger.
	res.DownMbps = maxKnown(down.RxMbps, down.LoadMbps)
	res.UpMbps = maxKnown(up.TxMbps, up.LoadMbps)
	if down.LoadError != "" {
		res.DownMbps = nil
	}
	if up.LoadError != "" {
		res.UpMbps = nil
	}
	if !haveTarget {
		res.Note = "No internet target answered (ICMP may be blocked and no relay replied), so there is no grade. The gateway numbers below are still measured."
		return
	}
	res.IdleMS = idle.Internet.P50
	if res.IdleMS == nil {
		res.Note = "The latency target stopped answering, so there is no grade. Try again."
		return
	}
	timeout := ms(t.cfg.Timeout)
	var notes []string
	increase := func(p PhaseResult, dir string) *float64 {
		if p.LoadError != "" {
			notes = append(notes, fmt.Sprintf("The %s couldn't be tested: %s.", dir, p.LoadError))
			return nil
		}
		lossy := p.Internet.lossFrac() >= 0.5 && idle.Internet.lossFrac() < 0.2
		if p.Internet.P50 == nil {
			if lossy {
				notes = append(notes, fmt.Sprintf("Almost every ping was lost during the %s: latency rose by %.0f ms or more.", dir, timeout))
				return ptr(timeout)
			}
			return nil
		}
		v := max(*p.Internet.P50-*res.IdleMS, 0)
		if lossy {
			notes = append(notes, fmt.Sprintf("Most pings were lost during the %s: latency rose by %.0f ms or more.", dir, timeout))
			v = max(v, timeout)
		}
		return ptr(v)
	}
	res.DownIncreaseMS = increase(down, "download")
	res.UpIncreaseMS = increase(up, "upload")
	worst := math.NaN()
	for _, v := range []*float64{res.DownIncreaseMS, res.UpIncreaseMS} {
		if v != nil && (math.IsNaN(worst) || *v > worst) {
			worst = *v
		}
	}
	res.Grade = Grade(worst)
	if res.Grade == "" {
		notes = append([]string{"The connection couldn't be loaded, so there is no grade."}, notes...)
	}
	if idle.Internet.lossFrac() >= 0.2 {
		notes = append(notes, "The target dropped pings even when the line was idle, so the result is less certain.")
	}
	for _, n := range notes {
		if res.Note != "" {
			res.Note += " "
		}
		res.Note += n
	}
}

func (t *BloatTest) probeLoop(ctx context.Context, st *runState, start, end time.Time, target Prober, wg *sync.WaitGroup) {
	tick := time.NewTicker(t.cfg.Interval)
	defer tick.Stop()
	send := func(p Prober, gw bool) {
		at := time.Since(start)
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Probes started before the end run to their own timeout, even
			// once the test has stopped sending.
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), t.cfg.Timeout)
			defer cancel()
			rtt, err := p.Probe(pctx)
			ok := err == nil && rtt <= t.cfg.Timeout
			st.add(sample{gateway: gw, at: at, rtt: rtt, ok: ok})
		}()
	}
	for {
		now := time.Now()
		if now.After(end) || ctx.Err() != nil {
			return
		}
		if target != nil {
			send(target, false)
		}
		if t.cfg.Gateway != nil {
			send(t.cfg.Gateway, true)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (t *BloatTest) progressLoop(ctx context.Context, st *runState, start time.Time, total time.Duration, target string) (stop func()) {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			st.mu.Lock()
			p := BloatProgress{Phase: st.phase, Elapsed: min(time.Since(start), total).Seconds(), Total: total.Seconds(), LastMS: st.lastMS, Target: target}
			if el := time.Since(st.phaseAt).Seconds(); (st.phase == PhaseDownload || st.phase == PhaseUpload) && el > 0.2 {
				p.LoadMbps = ptr(float64(st.moved.Load()) * 8 / el / 1e6)
			}
			st.mu.Unlock()
			st.progress(p)
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// pickTarget probes every candidate three times at once and returns the
// fastest that answered in the lowest tier.
func pickTarget(ctx context.Context, cands []Candidate, budget, timeout time.Duration) (Prober, error) {
	if len(cands) == 0 {
		return nil, ErrNoTarget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	type found struct {
		tier int
		rtt  time.Duration
		p    Prober
	}
	var (
		mu   sync.Mutex
		best []found
		wg   sync.WaitGroup
	)
	for _, c := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			low := time.Duration(-1)
			for i := range 3 {
				if i > 0 {
					select {
					case <-ctx.Done():
					case <-time.After(150 * time.Millisecond):
					}
				}
				if ctx.Err() != nil {
					break
				}
				pctx, pcancel := context.WithTimeout(ctx, timeout)
				rtt, err := c.Prober.Probe(pctx)
				pcancel()
				if err == nil && (low < 0 || rtt < low) {
					low = rtt
				}
			}
			if low >= 0 {
				mu.Lock()
				best = append(best, found{c.Tier, low, c.Prober})
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(best) == 0 {
		return nil, ErrNoTarget
	}
	slices.SortStableFunc(best, func(a, b found) int {
		if a.tier != b.tier {
			return a.tier - b.tier
		}
		return int(a.rtt - b.rtt)
	})
	return best[0].p, nil
}

func summarize(samples []sample, gateway bool, from, to time.Duration) LatencyStats {
	var st LatencyStats
	var rtts []float64
	for _, s := range samples {
		if s.gateway != gateway || s.at < from || s.at >= to {
			continue
		}
		st.Sent++
		if !s.ok {
			st.Lost++
			continue
		}
		rtts = append(rtts, ms(s.rtt))
	}
	if len(rtts) > 0 {
		slices.Sort(rtts)
		st.Min = ptr(rtts[0])
		st.P50 = ptr(percentile(rtts, 0.50))
		st.P95 = ptr(percentile(rtts, 0.95))
	}
	return st
}

// percentile of sorted values, nearest rank.
func percentile(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[min(max(i, 0), len(sorted)-1)]
}

func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-tm.C:
		return true
	}
}

func maxKnown(vs ...*float64) *float64 {
	var out *float64
	for _, v := range vs {
		if v != nil && (out == nil || *v > *out) {
			out = v
		}
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func ptr[T any](v T) *T { return &v }
