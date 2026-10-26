package diag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/icmp"
)

// Route trace (MTR): echo requests with rising TTL find every router on the
// way to a target and measure loss and latency at each of them, many times
// over, like mtr and WinMTR. Routers answer these probes from their control
// plane, which most of them rate-limit (Linux allows about one "time
// exceeded" per second per destination), so loss at a router that later
// hops don't show is the router being busy, not the route losing traffic.
// The persistence rule applies that: loss only counts when it persists to
// the destination.

// Trace modes.
const (
	// TraceICMP measures every hop, the destination included, with ICMP echo.
	TraceICMP = "icmp"
	// TraceUDP measures the hops with ICMP and the destination (a relay)
	// with UDP probes to its own port: the protocol and port game traffic
	// uses, which networks may treat differently from ICMP.
	TraceUDP = "udp"
)

// TTLPingFunc sends one echo that may cross at most ttl routers
// (icmp.PingTTL in production).
type TTLPingFunc func(dst netip.Addr, ttl int, timeout time.Duration) (icmp.Reply, error)

// NameFunc looks up a hop's host name (reverse DNS), "" when it has none.
// It must honor ctx.
type NameFunc func(ctx context.Context, a netip.Addr) string

// TraceConfig describes one trace. Zero values get WinMTR's defaults: 30
// hops, one probe per hop per second, and the roadmap's 200 probes per hop.
type TraceConfig struct {
	Target netip.Addr
	// Host is what the user asked for (a host name, a relay's label or the
	// address itself), shown next to the address.
	Host string
	Mode string // TraceICMP (default) or TraceUDP
	Ping TTLPingFunc
	// End measures the destination itself in UDP mode (the relay probe).
	// The persistence rule then uses it as the destination.
	End   Prober
	Names NameFunc // nil: no host names
	// Route is the connection the trace runs on (zero: unknown), kept in
	// the result so a later verdict can tell it from another connection.
	Route Route

	MaxHops  int           // 30
	Count    int           // probes per hop (200)
	Interval time.Duration // between probes to the same hop (1 s)
	Timeout  time.Duration // per probe (2 s); a later reply counts as lost
}

func (c *TraceConfig) defaults() {
	if c.Mode == "" {
		c.Mode = TraceICMP
	}
	if c.MaxHops <= 0 {
		c.MaxHops = 30
	}
	c.MaxHops = min(c.MaxHops, 64)
	if c.Count <= 0 {
		c.Count = 200
	}
	if c.Interval <= 0 {
		c.Interval = time.Second
	}
	if c.Timeout <= 0 {
		c.Timeout = 2 * time.Second
	}
}

// TraceHop is one row of the trace: a TTL (or, for End, the destination
// measured over UDP). Nil numbers: nothing answered, or not measurable.
type TraceHop struct {
	TTL int `json:"ttl"` // 0 for the UDP destination row
	// Addr is the router that answered most often at this TTL; Others are
	// the rest (load balancing sends probes along parallel routers).
	Addr   string   `json:"addr"` // "" nothing answered
	Others []string `json:"others,omitempty"`
	Name   string   `json:"name,omitempty"` // reverse DNS of Addr
	// Private: Addr is a private, link-local or carrier-grade NAT address,
	// so the hop is inside a home or ISP network.
	Private bool   `json:"private"`
	Probe   string `json:"probe"` // "icmp" or "udp"

	Sent    int     `json:"sent"`
	Recv    int     `json:"recv"`
	LossPct float64 `json:"loss_pct"` // raw, as mtr and WinMTR show it
	// PersistPct is the loss that persists from this hop to the
	// destination: the lowest loss at this hop or any later one. Nil when
	// the destination didn't answer, so nothing can be confirmed.
	PersistPct *float64 `json:"persist_pct"`
	// Limited: this hop loses more replies than persist to the
	// destination, so the router limits its replies; it isn't losing the
	// traffic that passes through it.
	Limited bool `json:"limited"`
	// LossStarts: the persisting loss rises at this hop.
	LossStarts  bool   `json:"loss_starts"`
	Unreachable int    `json:"unreachable,omitempty"` // "destination unreachable" answers
	Error       string `json:"error,omitempty"`       // the latest error other than a timeout

	BestMS  *float64 `json:"best_ms"`
	AvgMS   *float64 `json:"avg_ms"`
	WorstMS *float64 `json:"worst_ms"`
	LastMS  *float64 `json:"last_ms"` // nil: the latest probe was lost
	StDevMS *float64 `json:"stdev_ms"`
	// PersistMS is the lowest average latency from this hop to the
	// destination: delay a router adds only to its own replies doesn't
	// carry on, delay on the route does.
	PersistMS *float64 `json:"persist_ms"`
}

// TraceResult is a finished (or, while running, the current) trace.
type TraceResult struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Running    bool      `json:"running"`
	Stopped    bool      `json:"stopped"` // ended by the user before Count rounds

	Target    string  `json:"target"` // the address traced
	Host      string  `json:"host"`
	Mode      string  `json:"mode"`
	EndName   string  `json:"end_name,omitempty"` // UDP mode: what End measures
	Rounds    int     `json:"rounds"`             // probes sent to the first hop
	Planned   int     `json:"planned"`            // Count
	IntervalS float64 `json:"interval_s"`
	MaxHops   int     `json:"max_hops"`
	// Interface and Link are the connection the trace ran on ("" unknown).
	Interface string `json:"interface,omitempty"`
	Link      string `json:"link,omitempty"`

	// Reached: the destination answered (in UDP mode: the relay's port).
	Reached bool       `json:"reached"`
	Hops    []TraceHop `json:"hops"`
	End     *TraceHop  `json:"end,omitempty"` // UDP mode

	// DestLossPct is the destination's loss (nil: not reached). LossFrom is
	// the first hop from which at least ThresholdPct of the probes are
	// lost all the way to the destination (0: only the UDP destination row
	// loses them); nil when no loss persists.
	DestLossPct  *float64 `json:"dest_loss_pct"`
	LossFrom     *int     `json:"loss_from"`
	ThresholdPct float64  `json:"threshold_pct"`
	// JumpTTL is the hop that adds the most persisting delay, JumpMS how
	// much (nil: not measured).
	JumpTTL *int     `json:"jump_ttl"`
	JumpMS  *float64 `json:"jump_ms"`

	Verdict string   `json:"verdict"`
	Notes   []string `json:"notes,omitempty"`
}

// Trace runs a route trace.
type Trace struct {
	cfg TraceConfig
}

// NewTrace applies the defaults to cfg.
func NewTrace(cfg TraceConfig) *Trace {
	cfg.defaults()
	return &Trace{cfg: cfg}
}

// Config is the trace's configuration, defaults applied.
func (t *Trace) Config() TraceConfig { return t.cfg }

// Duration is how long a full trace takes, about.
func (t *Trace) Duration() time.Duration {
	return time.Duration(t.cfg.Count)*t.cfg.Interval + t.cfg.Timeout
}

// hopStats accumulates one TTL's probes.
type hopStats struct {
	sent, recv, unreach int
	addrs               map[netip.Addr]int
	order               []netip.Addr // first-seen order, for stable ties
	n                   int
	sum, sumSq          float64
	best, worst, last   float64
	lastOK              bool
	err                 string
}

func (h *hopStats) add(r icmp.Reply, err error) {
	h.sent++
	if err != nil {
		h.lastOK = false
		if !errors.Is(err, icmp.ErrTimeout) && !errors.Is(err, context.DeadlineExceeded) {
			h.err = err.Error()
		}
		return
	}
	if r.From.IsValid() {
		if h.addrs == nil {
			h.addrs = map[netip.Addr]int{}
		}
		if h.addrs[r.From] == 0 {
			h.order = append(h.order, r.From)
		}
		h.addrs[r.From]++
	}
	if r.Kind == icmp.Unreachable {
		// WinMTR counts only echo and time exceeded answers as returned.
		h.unreach++
		h.lastOK = false
		return
	}
	h.recv++
	v := ms(r.RTT)
	if h.n == 0 || v < h.best {
		h.best = v
	}
	if h.n == 0 || v > h.worst {
		h.worst = v
	}
	h.n++
	h.sum += v
	h.sumSq += v * v
	h.last, h.lastOK = v, true
}

// traceState is what the probe goroutines share during one run.
type traceState struct {
	mu      sync.Mutex
	hops    []hopStats // index ttl-1
	end     hopStats
	names   map[netip.Addr]string
	asked   map[netip.Addr]bool
	reached atomic.Int32 // lowest TTL the destination answered at, MaxHops+1 until then
}

func (s *traceState) limit() int { return int(s.reached.Load()) }

// reach lowers the limit to ttl.
func (s *traceState) reach(ttl int) {
	for {
		cur := s.reached.Load()
		if int32(ttl) >= cur || s.reached.CompareAndSwap(cur, int32(ttl)) {
			return
		}
	}
}

// ErrTraceTarget: the trace has no usable target.
var ErrTraceTarget = errors.New("diag: no address to trace")

// Run performs the trace and reports a snapshot about twice a second. It
// returns an error only for a config it can't run; a run ended by ctx
// returns what it measured, marked Stopped, and a run that can't send
// (offline, no privileges) returns a result whose Verdict says why.
func (t *Trace) Run(ctx context.Context, progress func(TraceResult)) (TraceResult, error) {
	cfg := t.cfg
	if !cfg.Target.IsValid() || cfg.Ping == nil {
		return TraceResult{}, ErrTraceTarget
	}
	if cfg.Mode != TraceICMP && cfg.Mode != TraceUDP {
		return TraceResult{}, fmt.Errorf("diag: unknown trace mode %q", cfg.Mode)
	}
	if cfg.Mode == TraceUDP && cfg.End == nil {
		return TraceResult{}, errors.New("diag: UDP mode needs a UDP destination probe")
	}
	if progress == nil {
		progress = func(TraceResult) {}
	}
	st := &traceState{hops: make([]hopStats, cfg.MaxHops), names: map[netip.Addr]string{}, asked: map[netip.Addr]bool{}}
	st.reached.Store(int32(cfg.MaxHops + 1))
	started := time.Now()
	progress(t.build(st, started, true, false))

	// One probe first: when the PC can't send at all, say so at once
	// instead of after 200 rounds of identical errors.
	if _, err := cfg.Ping(cfg.Target, 1, cfg.Timeout); err != nil && !errors.Is(err, icmp.ErrTimeout) {
		res := t.build(st, started, false, false)
		res.Verdict, res.Notes = sendFailure(err), nil
		return res, nil
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var loops, probes sync.WaitGroup
	// Spread each round's probes over half the interval, as WinMTR starts
	// its per-hop threads 30 ms apart, so the first router doesn't get a
	// burst of 30 probes at once.
	stagger := cfg.Interval / time.Duration(2*cfg.MaxHops)
	for ttl := 1; ttl <= cfg.MaxHops; ttl++ {
		loops.Add(1)
		go func() {
			defer loops.Done()
			t.hopLoop(runCtx, st, &probes, started.Add(time.Duration(ttl-1)*stagger), ttl)
		}()
	}
	if cfg.End != nil {
		loops.Add(1)
		go func() {
			defer loops.Done()
			t.endLoop(runCtx, st, &probes, started)
		}()
	}
	done := make(chan struct{})
	go func() {
		loops.Wait()
		probes.Wait()
		close(done)
	}()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			res := t.build(st, started, false, ctx.Err() != nil)
			return res, nil
		case <-tick.C:
			progress(t.build(st, started, true, false))
		}
	}
}

// hopLoop sends Count probes with the given TTL, one per interval, until
// the destination turns out to be nearer.
func (t *Trace) hopLoop(ctx context.Context, st *traceState, probes *sync.WaitGroup, first time.Time, ttl int) {
	cfg := t.cfg
	for i := range cfg.Count {
		if !sleepUntil(ctx, first.Add(time.Duration(i)*cfg.Interval)) || ttl > st.limit() {
			return
		}
		probes.Add(1)
		go func() {
			defer probes.Done()
			// icmp.PingTTL blocks until its own timeout, so a probe in
			// flight when the trace stops still finishes and is counted.
			r, err := cfg.Ping(cfg.Target, ttl, cfg.Timeout)
			if err == nil && r.RTT > cfg.Timeout {
				err = icmp.ErrTimeout
			}
			if err == nil && r.From.WithZone("") == cfg.Target.WithZone("") {
				st.reach(ttl)
			}
			st.mu.Lock()
			st.hops[ttl-1].add(r, err)
			st.mu.Unlock()
			if err == nil {
				t.lookup(ctx, st, probes, r.From)
			}
		}()
	}
}

// endLoop measures the destination over UDP on the same schedule.
func (t *Trace) endLoop(ctx context.Context, st *traceState, probes *sync.WaitGroup, first time.Time) {
	cfg := t.cfg
	for i := range cfg.Count {
		if !sleepUntil(ctx, first.Add(time.Duration(i)*cfg.Interval)) {
			return
		}
		probes.Add(1)
		go func() {
			defer probes.Done()
			pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.Timeout)
			defer cancel()
			rtt, err := cfg.End.Probe(pctx)
			r := icmp.Reply{From: cfg.Target, RTT: rtt, Kind: icmp.Echo}
			if err == nil && rtt > cfg.Timeout {
				err = icmp.ErrTimeout
			}
			st.mu.Lock()
			st.end.add(r, err)
			st.mu.Unlock()
		}()
	}
}

// lookup starts a reverse DNS lookup the first time an address answers.
func (t *Trace) lookup(ctx context.Context, st *traceState, wg *sync.WaitGroup, a netip.Addr) {
	// Loopback names come from the hosts file and say nothing about a route.
	if t.cfg.Names == nil || !a.IsValid() || a.IsLoopback() {
		return
	}
	st.mu.Lock()
	asked := st.asked[a]
	st.asked[a] = true
	st.mu.Unlock()
	if asked {
		return
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if name := t.cfg.Names(lctx, a); name != "" {
			st.mu.Lock()
			st.names[a] = name
			st.mu.Unlock()
		}
	}()
}

// build turns the counters into a result and applies the persistence rule.
func (t *Trace) build(st *traceState, started time.Time, running, stopped bool) TraceResult {
	cfg := t.cfg
	st.mu.Lock()
	defer st.mu.Unlock()
	res := TraceResult{
		StartedAt: started, Running: running,
		Target: cfg.Target.String(), Host: cfg.Host, Mode: cfg.Mode,
		Planned: cfg.Count, IntervalS: cfg.Interval.Seconds(), MaxHops: cfg.MaxHops,
		Interface: cfg.Route.Name, Link: cfg.Route.Kind,
		Hops: []TraceHop{},
	}
	if !running {
		res.FinishedAt = time.Now()
	}
	if cfg.End != nil {
		res.EndName = cfg.End.Name()
	}
	res.Rounds = st.hops[0].sent
	res.Stopped = stopped && res.Rounds < cfg.Count

	reachedICMP := st.limit() <= cfg.MaxHops
	limit := min(st.limit(), cfg.MaxHops)
	last := limit
	if !reachedICMP {
		// Trailing hops that never answered are the destination (or the
		// routers before it) not replying, not more routers.
		last = 0
		for i := range limit {
			if len(st.hops[i].addrs) > 0 {
				last = i + 1
			}
		}
	}
	for i := range last {
		res.Hops = append(res.Hops, hopRow(i+1, &st.hops[i], st.names, "icmp"))
	}
	if cfg.End != nil && st.end.sent > 0 {
		e := hopRow(0, &st.end, st.names, "udp")
		res.End = &e
	}

	// The destination row the persistence rule measures against.
	var dest *TraceHop
	switch {
	case res.End != nil:
		if res.End.Recv > 0 {
			dest = res.End
		}
	case reachedICMP && len(res.Hops) > 0 && res.Hops[len(res.Hops)-1].Recv > 0:
		// A destination that only answers "unreachable" (a firewall on it
		// refusing ping) was found, not reached: nothing measured its loss.
		dest = &res.Hops[len(res.Hops)-1]
	}
	res.Reached = dest != nil
	minSent := 0
	for _, h := range append(slices.Clone(res.Hops), endRows(res.End)...) {
		if h.Sent > 0 && (minSent == 0 || h.Sent < minSent) {
			minSent = h.Sent
		}
	}
	res.ThresholdPct = lossThreshold(minSent)
	if dest != nil {
		applyPersistence(&res, dest)
	}
	if !running {
		res.Verdict, res.Notes = verdict(&res, cfg.MaxHops)
	}
	return res
}

func endRows(e *TraceHop) []TraceHop {
	if e == nil {
		return nil
	}
	return []TraceHop{*e}
}

func hopRow(ttl int, h *hopStats, names map[netip.Addr]string, probe string) TraceHop {
	row := TraceHop{TTL: ttl, Probe: probe, Sent: h.sent, Recv: h.recv, Unreachable: h.unreach, Error: h.err}
	if h.sent > 0 {
		row.LossPct = 100 * float64(h.sent-h.recv) / float64(h.sent)
	}
	if len(h.order) > 0 && probe == "icmp" {
		addrs := slices.Clone(h.order)
		slices.SortStableFunc(addrs, func(a, b netip.Addr) int { return h.addrs[b] - h.addrs[a] })
		row.Addr = addrs[0].String()
		row.Name = names[addrs[0]]
		row.Private = privateAddr(addrs[0])
		for _, a := range addrs[1:] {
			row.Others = append(row.Others, a.String())
		}
	}
	if h.n > 0 {
		avg := h.sum / float64(h.n)
		row.BestMS, row.AvgMS, row.WorstMS = ptr(h.best), ptr(avg), ptr(h.worst)
		row.StDevMS = ptr(math.Sqrt(max(h.sumSq/float64(h.n)-avg*avg, 0)))
	}
	if h.lastOK {
		row.LastMS = ptr(h.last)
	}
	return row
}

// lossThreshold is the smallest loss the rule acts on: 1 percentage point,
// or two probes when fewer than 200 were sent, so one unlucky probe in a
// short trace isn't called a fault.
func lossThreshold(sent int) float64 {
	if sent <= 0 {
		return 1
	}
	return max(1, 200/float64(sent))
}

// applyPersistence fills the persisting loss and delay of every hop, the
// hop where persisting loss starts, and the biggest persisting delay step.
func applyPersistence(res *TraceResult, dest *TraceHop) {
	thr := res.ThresholdPct
	res.DestLossPct = ptr(dest.LossPct)
	rows := make([]*TraceHop, 0, len(res.Hops)+1)
	for i := range res.Hops {
		rows = append(rows, &res.Hops[i])
	}
	if res.End != nil {
		rows = append(rows, res.End)
	}
	// Walk back from the destination keeping the lowest loss and delay
	// seen so far: what a hop shows beyond that didn't carry on.
	minLoss := dest.LossPct
	minMS := dest.AvgMS
	for i := len(rows) - 1; i >= 0; i-- {
		h := rows[i]
		if h.Sent > 0 {
			minLoss = min(minLoss, h.LossPct)
		}
		h.PersistPct = ptr(minLoss)
		if h.AvgMS != nil && (minMS == nil || *h.AvgMS < *minMS) {
			minMS = h.AvgMS
		}
		if minMS != nil {
			h.PersistMS = ptr(*minMS)
		}
		h.Limited = h.Recv > 0 && h.LossPct-minLoss >= thr
	}
	// A hop that never answered can't be named as the place loss or delay
	// starts. Nor can one whose own replies are limited (or slow): its
	// numbers can't tell its own loss from the route's, so a rise seen
	// there is credited to the next hop whose numbers match the route's.
	prevLoss, prevMS := 0.0, 0.0
	bestJump := -1.0
	pending := false
	for _, h := range rows {
		if h.Recv == 0 {
			continue
		}
		p := *h.PersistPct
		if p-prevLoss >= thr {
			pending = true
		}
		if (pending || (res.LossFrom == nil && p >= thr)) && !h.Limited {
			h.LossStarts, pending = true, false
			if res.LossFrom == nil {
				res.LossFrom = ptr(h.TTL)
			}
		}
		prevLoss = max(prevLoss, p)
		if h.PersistMS == nil || (h.AvgMS != nil && *h.AvgMS-*h.PersistMS >= slowReplyMS) {
			continue
		}
		if step := *h.PersistMS - prevMS; step > bestJump {
			bestJump = step
			res.JumpTTL, res.JumpMS = ptr(h.TTL), ptr(step)
		}
		prevMS = max(prevMS, *h.PersistMS)
	}
}

// slowReplyMS: a hop whose average is this much above the delay that
// persists past it answers slowly itself (replies are low priority work
// for a router).
const slowReplyMS = 5

// verdict says in plain words what the trace shows.
func verdict(res *TraceResult, maxHops int) (string, []string) {
	var notes []string
	answered := 0
	for _, h := range res.Hops {
		if h.Recv > 0 {
			answered++
		}
	}
	if res.Stopped {
		notes = append(notes, fmt.Sprintf("Stopped after %d of %d rounds, so the numbers are less certain.", res.Rounds, res.Planned))
	}
	for _, h := range res.Hops {
		if len(h.Others) > 0 {
			notes = append(notes, fmt.Sprintf("Hop %d answered from %d addresses: the network balances traffic over parallel routers there.", h.TTL, len(h.Others)+1))
			break
		}
	}
	var limited []int
	for _, h := range res.Hops {
		if h.Limited {
			limited = append(limited, h.TTL)
		}
	}
	if len(limited) > 0 {
		verb := "limits"
		if len(limited) > 1 {
			verb = "limit"
		}
		notes = append(notes, fmt.Sprintf("%s some replies, but the hops after %s don't: %s %s answers to trace probes. That isn't lost traffic.",
			hopList("Hop", limited, "drops", "drop"), pronoun(limited), routerWord(limited), verb))
	}
	switch {
	case answered == 0 && (res.End == nil || res.End.Recv == 0):
		return "Nothing answered the trace. The PC may be offline, or the networks on the way block these probes.", notes
	case !res.Reached:
		if res.Mode == TraceUDP {
			return "The relay didn't answer on its UDP port, so loss on the way can't be confirmed. Check that the relay runs and that its key is right.", notes
		}
		n := len(res.Hops)
		last := res.Hops[n-1]
		if last.Recv == 0 && last.Unreachable > 0 {
			who := fmt.Sprintf("Hop %d (%s)", last.TTL, last.Addr)
			if last.Addr == res.Target {
				who = "The destination"
			}
			notes = append(notes, who+` answered every probe with "unreachable": a firewall there refuses them.`)
		}
		v := fmt.Sprintf("The destination didn't answer ping, so loss on the way can't be confirmed: many game servers ignore ping. Raw loss per hop is shown; the last hop that answered is hop %d.", n)
		if n < maxHops && last.Addr != res.Target {
			notes = append(notes, fmt.Sprintf("Hops %d to %d didn't answer.", n+1, maxHops))
		}
		return v, notes
	}
	if res.End != nil && len(res.Hops) > 0 && res.Hops[len(res.Hops)-1].Addr != res.Target {
		notes = append(notes, "The relay doesn't answer ping, so the hop list ends at the last router that did; its UDP port answered.")
	}
	var v string
	loss := *res.DestLossPct
	switch {
	case res.LossFrom == nil:
		v = "No loss reaches the destination: the route is clean."
		if loss > 0 {
			v = fmt.Sprintf("Only %.1f%% of probes were lost at the destination, too little to point at any hop.", loss)
		}
	case *res.LossFrom == 0:
		v = fmt.Sprintf("%.1f%% of UDP probes to the relay are lost, but no router before it loses traffic: the loss is on the last link or at the relay.", loss)
	default:
		h := res.Hops[*res.LossFrom-1]
		where := h.Addr
		if h.Name != "" {
			where = h.Name
		}
		v = fmt.Sprintf("Loss starts at hop %d (%s) and persists to the destination: %.1f%% of probes are lost there.", h.TTL, where, loss)
		switch {
		case *res.LossFrom == len(res.Hops) && res.End == nil:
			v = fmt.Sprintf("Only the destination itself loses replies (%.1f%%). It may limit ping replies, so this doesn't prove your traffic is lost; a UDP trace to a relay can tell.", loss)
		case h.inHome():
			v += " That is inside your home network: try a cable instead of Wi-Fi, or restart the router."
		}
	}
	if res.JumpTTL != nil && *res.JumpMS >= 10 {
		at := fmt.Sprintf("hop %d", *res.JumpTTL)
		if *res.JumpTTL == 0 {
			at = "the relay's UDP port"
		}
		notes = append(notes, fmt.Sprintf("The biggest delay is added at %s: +%.0f ms that carries on to the destination.", at, *res.JumpMS))
	}
	return v, notes
}

func hopList(word string, ttls []int, one, many string) string {
	if len(ttls) == 1 {
		return fmt.Sprintf("%s %d %s", word, ttls[0], one)
	}
	s := word + "s "
	for i, t := range ttls {
		switch {
		case i == 0:
		case i == len(ttls)-1:
			s += " and "
		default:
			s += ", "
		}
		s += fmt.Sprint(t)
	}
	return s + " " + many
}

func pronoun(ttls []int) string {
	if len(ttls) == 1 {
		return "it"
	}
	return "them"
}

func routerWord(ttls []int) string {
	if len(ttls) == 1 {
		return "that router"
	}
	return "those routers"
}

// sendFailure explains why no probe could be sent.
func sendFailure(err error) string {
	switch {
	case errors.Is(err, icmp.ErrNoRoute):
		return "There is no route to this address: the PC looks offline, or this network has no IPv6."
	case errors.Is(err, os.ErrPermission):
		return "On this system a route trace needs root rights (on Linux, CAP_NET_RAW is enough)."
	case errors.Is(err, errors.ErrUnsupported):
		return "Route traces aren't available on this system: " + err.Error()
	}
	return "The trace couldn't send probes: " + err.Error()
}

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// inHome reports whether the hop is inside the home network: the first
// router, or a private second one (a second router or a modem behind the
// ISP's box). A second hop in the shared address space 100.64.0.0/10 is
// the ISP's carrier-grade NAT (RFC 6598), not the home.
func (h TraceHop) inHome() bool {
	if h.TTL == 1 {
		return true
	}
	if h.TTL != 2 || !h.Private {
		return false
	}
	a, err := netip.ParseAddr(h.Addr)
	return err != nil || !cgnat.Contains(a.Unmap())
}

func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || cgnat.Contains(a)
}

// Redacted is the result with every hop's address cut to its /24 (IPv6:
// /48) and host names left out, for reports a user may hand to someone
// else. The second hop's address is often the user's own public one.
func (r TraceResult) Redacted() TraceResult {
	cut := func(s string) string {
		if a, err := netip.ParseAddr(s); err == nil {
			return redactAddr(a).String()
		}
		return s
	}
	row := func(h TraceHop) TraceHop {
		h.Addr, h.Name, h.Error = cut(h.Addr), "", redactText(h.Error)
		h.Others = slices.Clone(h.Others)
		for i := range h.Others {
			h.Others[i] = cut(h.Others[i])
		}
		return h
	}
	// The verdict names the hop where loss starts by its host name, and a
	// name often spells out the address it belongs to
	// ("198-51-100-2.dyn.example.net"), so names in the text become the
	// cut address too.
	type named struct{ name, addr string }
	var names []named
	for _, h := range append(slices.Clone(r.Hops), endRows(r.End)...) {
		if h.Name != "" {
			names = append(names, named{h.Name, cut(h.Addr)})
		}
	}
	// Longest first, so a name inside another one doesn't break it up.
	slices.SortFunc(names, func(a, b named) int { return len(b.name) - len(a.name) })
	text := func(s string) string {
		for _, n := range names {
			s = strings.ReplaceAll(s, n.name, n.addr)
		}
		return redactText(s)
	}
	r.Target, r.Host = cut(r.Target), redactText(r.Host)
	r.EndName, r.Verdict = redactText(r.EndName), text(r.Verdict)
	r.Notes = slices.Clone(r.Notes)
	for i := range r.Notes {
		r.Notes[i] = text(r.Notes[i])
	}
	hops := make([]TraceHop, len(r.Hops))
	for i, h := range r.Hops {
		hops[i] = row(h)
	}
	r.Hops = hops
	if r.End != nil {
		e := row(*r.End)
		r.End = &e
	}
	return r
}
