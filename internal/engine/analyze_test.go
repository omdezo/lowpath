package engine

import (
	"testing"
	"time"
)

// A stall at the end of an analysis, while its last replies are on their
// way, doesn't make them loss: they are judged in flight, the path keeps
// its measured round trip, and a dead path is still reported down (#218).
func TestAnalysisThroughAStall(t *testing.T) {
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	const (
		every = probeInterval / 2 // Analyze's probe rate
		dur   = 2500 * time.Millisecond
	)
	for _, tc := range []struct {
		name              string
		stallAt, stallFor time.Duration
		drain             time.Duration // replies queued in the stall are read this long after it (< 0: not yet)
		judge             time.Duration
	}{
		{name: "stall in the final wait, replies not read yet", stallAt: ms(2600), stallFor: ms(1500), drain: -1, judge: ms(4101)},
		{name: "stall in the final wait, replies read on resume", stallAt: ms(2600), stallFor: ms(1500), drain: ms(2), judge: ms(4105)},
		{name: "stall mid-analysis", stallAt: ms(1500), stallFor: ms(1500), drain: ms(2), judge: ms(4000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Unix(1_700_000_000, 0)
			at := func(d time.Duration) time.Time { return start.Add(d) }
			resume := tc.stallAt + tc.stallFor
			running := func(d time.Duration) bool { return d < tc.stallAt || d >= resume }

			awake := &awakeClock{}
			before := tracker{awake: awake}
			alive := &path{spec: PathSpec{ID: "alive"}}
			dead := &path{spec: PathSpec{ID: "dead"}}
			alive.probes.awake, dead.probes.awake = awake, awake
			beats(awake, start, 0, tc.stallAt) // the heartbeat is back only after the judging

			// Replies arrive after rtt; the ones due while stalled are read
			// on resume plus drain, or not by the time of judging.
			answer := func(tr *tracker, seq uint32, sent, rtt time.Duration) {
				read := sent + rtt
				if !running(read) {
					if tc.drain < 0 {
						return
					}
					read = resume + tc.drain
				}
				if read <= tc.judge {
					tr.recv(seq, at(read))
				}
			}
			for sent := every; sent <= dur; sent += every {
				if !running(sent) {
					continue
				}
				answer(&alive.probes, alive.probes.send(at(sent)), sent, ms(300))
				dead.probes.send(at(sent))
				answer(&before, before.send(at(sent)), sent, ms(20))
			}

			out := judgeAnalysis(awake, &before, true, []*path{alive, dead}, at(tc.judge), int(dur/every))
			if a := out.Paths["alive"]; a.Loss != 0 || !a.Up || a.RTT != 300 || a.Jitter != 0 {
				t.Errorf("alive path: %+v, want no loss and 300 ms", a)
			}
			if d := out.Paths["dead"]; d.Loss != 100 || d.Up {
				t.Errorf("dead path: %+v, want 100%% loss and down", d)
			}
			if !out.DirectKnown || out.Direct.Loss != 0 || out.Direct.RTT != 20 {
				t.Errorf("direct: %+v (known %v), want no loss and 20 ms", out.Direct, out.DirectKnown)
			}
		})
	}
}
