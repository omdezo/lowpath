package engine

import (
	"slices"
	"testing"
	"time"
)

// probeRun is part of one path's probe history: probes sent every
// probeInterval from `from` (after Start) until before `to`, each answered
// after rtt, or never when rtt is noReply.
type probeRun struct{ from, to, rtt time.Duration }

const noReply = time.Duration(-1)

// selectionEngine is an Engine with only what choose needs: paths a, b and c
// (in that order) and their probe trackers; no sockets or loops.
func selectionEngine(start time.Time, cfg Config) *Engine {
	e := &Engine{cfg: cfg, start: start, flows: newFlowTable()}
	e.routing.Store(&routing{})
	for _, id := range []string{"a", "b", "c"} {
		e.paths = append(e.paths, &path{spec: PathSpec{ID: id, Label: id}})
	}
	return e
}

// replay records the probes in h as the engine's probe loop would have,
// with only the replies that arrived by tick.
func replay(p *path, start time.Time, tick time.Duration, h []probeRun) {
	for _, r := range h {
		for at := r.from; at < r.to; at += probeInterval {
			seq := p.probes.send(start.Add(at))
			if r.rtt != noReply && at+r.rtt <= tick {
				p.probes.recv(seq, start.Add(at+r.rtt))
			}
		}
	}
}

func activeSet(e *Engine) []string {
	var ids []string
	for _, p := range e.routing.Load().active {
		ids = append(ids, p.spec.ID)
	}
	slices.Sort(ids)
	return ids
}

// The first selection ticks judge a path on what its probes said, not on
// replies that had no chance to arrive yet (#178), and dead routes still
// fall back (fail-open).
func TestSelectionJudgesPathsOnProbeOutcomes(t *testing.T) {
	fast := func(to time.Duration) []probeRun { return []probeRun{{probeInterval, to, 5 * time.Millisecond}} }
	silent := func(to time.Duration) []probeRun { return []probeRun{{probeInterval, to, noReply}} }
	upSeed := Stats{RTT: 10, Up: true, Samples: 10}
	downSeed := Stats{Loss: 100}

	tests := []struct {
		name     string
		seed     map[string]Stats
		probes   map[string][]probeRun
		tick     time.Duration
		want     []string // active paths after the tick
		fallback bool
	}{
		{
			name: "replies still in flight at the first tick",
			probes: map[string][]probeRun{
				"a": silent(time.Second), "b": silent(time.Second), "c": silent(time.Second),
			},
			tick: time.Second,
			want: []string{"a", "b"},
		},
		{
			name: "probe loop starved: nothing sent by the first tick",
			tick: time.Second,
			want: []string{"a", "b"},
		},
		{
			name: "first tick ran late after a stall",
			// The process stopped for 1.5 s just after Start: one probe went
			// out on resume and its reply is not read yet.
			probes: map[string][]probeRun{
				"a": {{1500 * time.Millisecond, 1600 * time.Millisecond, noReply}},
				"b": {{1500 * time.Millisecond, 1600 * time.Millisecond, noReply}},
			},
			tick: 1600 * time.Millisecond,
			want: []string{"a", "b"},
		},
		{
			name: "a slow path is kept until its first probe has its full timeout",
			probes: map[string][]probeRun{
				"a": fast(time.Second), "b": {{probeInterval, time.Second, 950 * time.Millisecond}},
			},
			tick: time.Second,
			want: []string{"a", "b"},
		},
		{
			name: "paths not judged yet give way to a spare that has answered",
			// a and b may be dead; only c is known to work, so it must carry
			// the game now rather than at the next tick.
			probes: map[string][]probeRun{
				"a": silent(time.Second), "b": silent(time.Second), "c": fast(time.Second),
			},
			tick: time.Second,
			want: []string{"a", "c"},
		},
		{
			name: "a path whose probes timed out is dropped within the grace window",
			probes: map[string][]probeRun{
				"a": silent(1500 * time.Millisecond), "b": fast(1500 * time.Millisecond), "c": fast(1500 * time.Millisecond),
			},
			tick: 1500 * time.Millisecond,
			want: []string{"b", "c"},
		},
		{
			name: "every route dead: fall back once the first probes time out",
			probes: map[string][]probeRun{
				"a": silent(2 * time.Second), "b": silent(2 * time.Second), "c": silent(2 * time.Second),
			},
			tick:     2 * time.Second,
			fallback: true,
		},
		{
			name:     "no probe ever ran: the grace window still ends",
			tick:     startGrace,
			fallback: true,
		},
		{
			name: "grace only keeps paths in use, not ones the seed found down",
			seed: map[string]Stats{"a": upSeed, "b": downSeed, "c": downSeed},
			probes: map[string][]probeRun{
				"a": silent(time.Second), "b": silent(time.Second), "c": silent(time.Second),
			},
			tick: time.Second,
			want: []string{"a"},
		},
		{
			name: "seed with every route down stays in fallback until one answers",
			seed: map[string]Stats{"a": downSeed, "b": downSeed, "c": downSeed},
			probes: map[string][]probeRun{
				"a": silent(time.Second), "b": silent(time.Second), "c": fast(time.Second),
			},
			tick: time.Second,
			want: []string{"c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			e := selectionEngine(start, Config{DupLevel: 2, Seed: tc.seed})
			e.choose(start, tc.seed)
			for _, p := range e.paths {
				replay(p, start, tc.tick, tc.probes[p.spec.ID])
			}
			e.choose(start.Add(tc.tick), nil)

			r := e.routing.Load()
			if r.fallback != tc.fallback {
				t.Errorf("fallback = %v, want %v (events %+v)", r.fallback, tc.fallback, e.events)
			}
			if got := activeSet(e); !slices.Equal(got, tc.want) {
				t.Errorf("active %v, want %v", got, tc.want)
			}
			fellBack := slices.ContainsFunc(e.events, func(ev Event) bool { return ev.Kind == "fallback" })
			if fellBack != tc.fallback && tc.seed == nil {
				t.Errorf("fallback event logged = %v, want %v: %+v", fellBack, tc.fallback, e.events)
			}
		})
	}
}

// stallTimeline is a boost whose process stops at stallAt for stallFor.
// While it runs, the heartbeat beats every heartbeatEvery and every path
// sends a probe every probeInterval, answered after rtt (never when the path
// is dead by then). Replies that arrive while it is stopped wait in the
// socket until drain after it resumes (drain < 0: not read by the end). The
// selection ticks every second before the stall, then at each of ticks.
type stallTimeline struct {
	stallAt, stallFor time.Duration
	rtt               time.Duration
	drain             time.Duration
	deadFrom          map[string]time.Duration // probes sent from then on go unanswered
}

// run replays the timeline on e, in time order, and calls check after each
// of ticks with the time of that tick.
func (tl stallTimeline) run(e *Engine, start time.Time, ticks []time.Duration, check func(at time.Duration)) {
	resume := tl.stallAt + tl.stallFor
	running := func(at time.Duration) bool { return at < tl.stallAt || at >= resume }
	end := ticks[len(ticks)-1]
	type event struct {
		at time.Duration
		do func()
	}
	var evs []event
	add := func(at time.Duration, do func()) { evs = append(evs, event{at, do}) }

	// The heartbeat comes back only one beat after resume, so the stall is
	// on record at the first tick only because the tick itself beats.
	for at := time.Duration(0); at <= end; at += heartbeatEvery {
		if running(at) && (at < tl.stallAt || at >= resume+heartbeatEvery) {
			add(at, func() { e.awake.beat(start.Add(at)) })
		}
	}
	for at := probeInterval; at <= end; at += probeInterval {
		if !running(at) {
			continue
		}
		for _, p := range e.paths {
			var seq uint32
			add(at, func() { seq = p.probes.send(start.Add(at)) })
			if dead, ok := tl.deadFrom[p.spec.ID]; ok && at >= dead {
				continue
			}
			read := at + tl.rtt
			if !running(read) {
				if tl.drain < 0 {
					continue
				}
				read = resume + tl.drain
			}
			add(read, func() { p.probes.recv(seq, start.Add(read)) })
		}
	}
	tick := func(at time.Duration) func() {
		return func() {
			now := start.Add(at)
			e.awake.beat(now) // as selectLoop does
			e.choose(now, nil)
		}
	}
	for at := time.Second; at < tl.stallAt; at += time.Second {
		add(at, tick(at))
	}
	for _, at := range ticks {
		add(at, tick(at))
		add(at, func() { check(at) })
	}
	slices.SortStableFunc(evs, func(a, b event) int { return int(a.at - b.at) })
	for _, ev := range evs {
		ev.do()
	}
}

// Replies queued in the path sockets while the whole process stalls are
// not loss: no path is judged on them, their late reads don't show as
// round trips, and dead routes still fall back afterwards (#214).
func TestSelectionThroughAStall(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	type want struct {
		at       time.Duration
		active   []string
		fallback bool
		clean    bool // every path's stats: no loss, round trips as sent
	}
	tests := []struct {
		name string
		tl   stallTimeline
		want []want
	}{
		{
			// Two probes per path have their replies queued; judged on
			// wall time that is 50% loss on every path and a fallback.
			name: "stall right after start, replies not read yet",
			tl:   stallTimeline{stallAt: ms(450), stallFor: ms(1500), rtt: ms(200), drain: -1},
			want: []want{{at: ms(1951), active: []string{"a", "b"}, clean: true}},
		},
		{
			name: "stall right after start, replies read on resume",
			tl:   stallTimeline{stallAt: ms(450), stallFor: ms(1500), rtt: ms(200), drain: ms(5)},
			want: []want{
				{at: ms(1951), active: []string{"a", "b"}, clean: true},
				{at: ms(2951), active: []string{"a", "b"}, clean: true},
			},
		},
		{
			name: "stall mid-session, replies not read yet",
			tl:   stallTimeline{stallAt: ms(4950), stallFor: ms(1500), rtt: ms(80), drain: -1},
			want: []want{{at: ms(6451), active: []string{"a", "b"}, clean: true}},
		},
		{
			name: "stall longer than a selection tick",
			tl:   stallTimeline{stallAt: ms(4950), stallFor: ms(3500), rtt: ms(80), drain: ms(2)},
			want: []want{
				{at: ms(8451), active: []string{"a", "b"}, clean: true},
				{at: ms(9451), active: []string{"a", "b"}, clean: true},
			},
		},
		{
			name: "a path dead before the stall is still dropped",
			tl:   stallTimeline{stallAt: ms(4950), stallFor: ms(1500), rtt: ms(80), drain: -1, deadFrom: map[string]time.Duration{"a": 2 * time.Second}},
			want: []want{{at: ms(6451), active: []string{"b", "c"}}},
		},
		{
			// Unknown on resume, so the routes are kept; dead once the
			// engine has been awake long enough to know (fail-open).
			name: "every route dies at the stall: kept on resume, then fall back",
			tl: stallTimeline{stallAt: ms(4950), stallFor: ms(1500), rtt: ms(80), drain: -1,
				deadFrom: map[string]time.Duration{"a": ms(4700), "b": ms(4700), "c": ms(4700)}},
			want: []want{
				{at: ms(6451), active: []string{"a", "b"}},
				{at: ms(10451), fallback: true},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			e := selectionEngine(start, Config{DupLevel: 2})
			e.watchStalls()
			e.choose(start, nil)
			var ticks []time.Duration
			for _, w := range tc.want {
				ticks = append(ticks, w.at)
			}
			i := 0
			tc.tl.run(e, start, ticks, func(at time.Duration) {
				w := tc.want[i]
				i++
				r := e.routing.Load()
				if r.fallback != w.fallback {
					t.Errorf("at %v: fallback = %v, want %v (events %+v)", at, r.fallback, w.fallback, e.events)
				}
				if got := activeSet(e); !slices.Equal(got, w.active) {
					t.Errorf("at %v: active %v, want %v (events %+v)", at, got, w.active, e.events)
				}
				if !w.clean {
					return
				}
				for _, p := range e.paths {
					s := p.probes.stats(start.Add(at))
					if s.Loss != 0 || !s.Up || s.RTT != float64(tc.tl.rtt.Milliseconds()) || s.Jitter != 0 {
						t.Errorf("at %v: path %s stats %+v, want no loss and %v round trips", at, p.spec.ID, s, tc.tl.rtt)
					}
				}
			})
		})
	}
}
