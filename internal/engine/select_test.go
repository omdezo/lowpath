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
