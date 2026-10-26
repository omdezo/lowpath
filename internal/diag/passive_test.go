package diag

import (
	"testing"
	"time"
)

// feeder drives a detector one simulated second at a time.
type feeder struct {
	d  *PassiveDetector
	at time.Time
}

func (f *feeder) step(rttMS, rxMbps, txMbps, ownRxMbps float64) PassiveState {
	f.at = f.at.Add(time.Second)
	return f.d.Observe(PassiveSample{
		At: f.at, RTT: time.Duration(rttMS * float64(time.Millisecond)), RTTKnown: rttMS > 0,
		Rx: rxMbps * 1e6 / 8, Tx: txMbps * 1e6 / 8, RateKnown: true, OwnRx: ownRxMbps * 1e6 / 8,
	})
}

func TestPassiveDetectsADownloadWithinTenSeconds(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{DownBps: 100e6 / 8, UpBps: 20e6 / 8}), at: time.Unix(0, 0)}
	var st PassiveState
	for range 20 { // normal play: 30 ms, a little background traffic
		if st = f.step(30, 1, 0.5, 0.3); st.Active {
			t.Fatal("fired during normal play")
		}
	}
	if !st.Watching || st.BaselineMS == nil || *st.BaselineMS != 30 || st.LineSource != "test" {
		t.Fatalf("state after warm-up = %+v", st)
	}
	// A launcher download fills the line and the game's ping climbs.
	fired := -1
	for i := range 10 {
		if st = f.step(30+float64(i+1)*8, 92, 1, 0.3); st.Active && fired < 0 {
			fired = i + 1
		}
	}
	if fired < 0 || fired > 10 {
		t.Fatalf("not fired within 10 s: %+v", st)
	}
	if st.Direction != "download" || st.OtherDownMbps == nil || *st.OtherDownMbps < 91 || st.Since == nil {
		t.Fatalf("state = %+v", st)
	}
	// The download ends: clears after five clean seconds, not before.
	for i := range 4 {
		if st = f.step(31, 1, 0.5, 0.3); !st.Active {
			t.Fatalf("cleared after only %d clean seconds", i+1)
		}
	}
	if st = f.step(31, 1, 0.5, 0.3); st.Active {
		t.Fatal("still active after five clean seconds")
	}
}

func TestPassiveNeedsBothHighLatencyAndAFullLine(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{DownBps: 100e6 / 8, UpBps: 20e6 / 8}), at: time.Unix(0, 0)}
	for range 10 {
		f.step(25, 1, 1, 0)
	}
	for range 15 { // full line, but the router keeps latency flat (SQM): fine
		if st := f.step(27, 95, 1, 0); st.Active {
			t.Fatal("fired without a latency rise")
		}
	}
	for range 15 { // latency rises with an idle line: a route problem, not bloat
		if st := f.step(80, 2, 1, 0); st.Active {
			t.Fatal("fired with an idle line")
		}
	}
	for range 15 { // the traffic is the game itself (Lowpath's tunnel)
		if st := f.step(80, 95, 1, 94); st.Active {
			t.Fatal("fired on the game's own traffic")
		}
	}
}

func TestPassiveUpload(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{DownBps: 100e6 / 8, UpBps: 10e6 / 8}), at: time.Unix(0, 0)}
	for range 10 {
		f.step(20, 1, 0.2, 0)
	}
	var st PassiveState
	for range 6 { // a cloud backup saturates the upload
		st = f.step(140, 2, 9.5, 0)
	}
	if !st.Active || st.Direction != "upload" {
		t.Fatalf("state = %+v", st)
	}
}

func TestPassiveLongDownloadKeepsBaseline(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{DownBps: 50e6 / 8}), at: time.Unix(0, 0)}
	for range 10 {
		f.step(30, 1, 0, 0)
	}
	var st PassiveState
	for range 300 { // five minutes of download, longer than the baseline window
		st = f.step(90, 48, 0, 0)
	}
	if !st.Active || st.BaselineMS == nil || *st.BaselineMS != 30 {
		t.Fatalf("after a long download: %+v (baseline must stay at the idle RTT)", st)
	}
}

func TestPassiveLearnsLineRateWithoutATest(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{}), at: time.Unix(0, 0)}
	st := f.step(30, 3, 1, 0)
	if st.LineSource != "peak" || st.LineDownMbps == nil || *st.LineDownMbps != 3 {
		t.Fatalf("state = %+v", st)
	}
	for range 10 {
		f.step(30, 1, 1, 0)
	}
	for range 5 {
		st = f.step(85, 40, 1, 0) // the peak rises with the download itself
	}
	if !st.Active || st.LineSource != "peak" {
		t.Fatalf("state = %+v", st)
	}
	// A later test with a higher rate takes over.
	f.d.SetLineRate(LineRate{DownBps: 200e6 / 8})
	if st = f.step(30, 1, 1, 0); st.LineSource != "test" || *st.LineDownMbps != 200 {
		t.Fatalf("state = %+v", st)
	}
}

// A LAN copy at gigabit speed passes through the same adapter counters.
// It must not become the "line rate" when a test measured the real line,
// or no internet download would ever count as filling it again.
func TestPassiveLANBurstDoesNotHideLaterDownloads(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{DownBps: 100e6 / 8, UpBps: 20e6 / 8}), at: time.Unix(0, 0)}
	for range 10 {
		f.step(30, 1, 0.5, 0)
	}
	for range 5 { // copy to a NAS: 900 Mbit/s, the game's ping unaffected
		if st := f.step(31, 900, 5, 0); st.Active {
			t.Fatalf("fired on a LAN copy without a latency rise: %+v", st)
		}
	}
	for range 10 {
		f.step(30, 1, 0.5, 0)
	}
	var st PassiveState
	for range 5 { // then a launcher download fills the 100 Mbit/s line
		st = f.step(80, 92, 1, 0)
	}
	if !st.Active || st.LineSource != "test" || st.LineDownMbps == nil || *st.LineDownMbps != 100 {
		t.Fatalf("download after a LAN burst not flagged: %+v", st)
	}
}

func TestPassiveUnknownInputs(t *testing.T) {
	d := NewPassiveDetector(PassiveConfig{}, LineRate{})
	st := d.Observe(PassiveSample{At: time.Unix(1, 0), RateKnown: true, Rx: 1000})
	if st.Watching || st.Reason == "" || st.RTTMS != nil {
		t.Fatalf("no RTT: %+v", st)
	}
	st = d.Observe(PassiveSample{At: time.Unix(2, 0), RTT: 20 * time.Millisecond, RTTKnown: true})
	if st.Watching || st.OtherDownMbps != nil {
		t.Fatalf("no counters: %+v", st)
	}
	st = d.Observe(PassiveSample{At: time.Unix(3, 0), RTT: 20 * time.Millisecond, RTTKnown: true, RateKnown: true})
	if st.Watching || st.Reason != "learning your normal latency" {
		t.Fatalf("warming up: %+v", st)
	}
	d.Reset()
	if st = d.Observe(PassiveSample{At: time.Unix(4, 0), RTT: 20 * time.Millisecond, RTTKnown: true, RateKnown: true}); st.BaselineMS != nil {
		t.Fatalf("after reset: %+v", st)
	}
}

// Without a test the line rate is the most other traffic seen, but only
// over the last ten minutes: a LAN copy the counters couldn't tell apart
// stops hiding internet downloads once it is that old.
func TestPassivePeakAgesOut(t *testing.T) {
	f := &feeder{d: NewPassiveDetector(PassiveConfig{}, LineRate{}), at: time.Unix(0, 0)}
	for range 10 {
		f.step(30, 1, 0.5, 0)
	}
	var st PassiveState
	for range 5 { // copy to a NAS at 900 Mbit/s, the game's ping unaffected
		st = f.step(31, 900, 5, 0)
	}
	if st.Active || st.LineSource != "peak" || *st.LineDownMbps != 900 || !st.NeedsTest {
		t.Fatalf("during the copy: %+v", st)
	}
	download := func() PassiveState {
		var st PassiveState
		for range 5 { // a launcher download fills the real 100 Mbit/s line
			st = f.step(80, 92, 1, 0)
		}
		for range 10 { // and ends
			f.step(30, 1, 0.5, 0)
		}
		return st
	}
	for range 4 * 60 { // four quiet minutes
		f.step(30, 1, 0.5, 0)
	}
	if st = download(); st.Active || *st.LineDownMbps != 900 {
		t.Fatalf("five minutes after the copy, the 900 Mbit/s peak still holds: %+v", st)
	}
	for range 6 * 60 { // over ten minutes since the copy
		f.step(30, 1, 0.5, 0)
	}
	if st = download(); !st.Active || st.Direction != "download" || *st.LineDownMbps != 92 {
		t.Fatalf("ten minutes after the copy, the download must count: %+v", st)
	}
}

func TestSlidingMax(t *testing.T) {
	m := slidingMax{window: 10 * time.Second}
	at := func(s int) time.Time { return time.Unix(int64(s), 0) }
	for _, c := range []struct {
		at   int
		add  float64
		want float64
	}{
		{0, 5, 5}, {1, 3, 5}, {2, 8, 8}, {3, 8, 8}, {4, 1, 8},
		{12, 2, 8},  // 3 s's 8 is 9 s old: still in the window
		{14, 0, 2},  // the 8s left; 12 s's 2 is the largest
		{30, 4, 4},  // everything older is gone
		{30, -1, 4}, // same second, smaller
	} {
		m.add(at(c.at), c.add)
		if got := m.max(at(c.at)); got != c.want {
			t.Errorf("at %d s after adding %v: max %v, want %v", c.at, c.add, got, c.want)
		}
	}
	if got := m.max(at(100)); got != 0 || len(m.q) != 0 {
		t.Errorf("long after: max %v with %d entries, want 0 and none", got, len(m.q))
	}
	// A steady rise never keeps more than the newest entry.
	for i := range 600 {
		m.add(at(200+i), float64(i))
	}
	if len(m.q) != 1 {
		t.Errorf("after a steady rise: %d entries, want 1", len(m.q))
	}
}

// With per-connection counters (Linux), LAN traffic leaves "other traffic"
// and the peak, so a NAS copy can neither fire the alarm nor raise the
// line rate, and the program filling the line gets a name.
func TestPassiveLeavesLANOutAndNamesTheTopProgram(t *testing.T) {
	d := NewPassiveDetector(PassiveConfig{}, LineRate{})
	at := time.Unix(0, 0)
	step := func(rttMS, rxMbps, lanMbps float64, talkers ...Talker) PassiveState {
		at = at.Add(time.Second)
		return d.Observe(PassiveSample{
			At: at, RTT: time.Duration(rttMS * float64(time.Millisecond)), RTTKnown: true,
			Rx: rxMbps * 1e6 / 8, Tx: 1e6 / 8, RateKnown: true, LANRx: lanMbps * 1e6 / 8, LANKnown: true, Talkers: talkers,
		})
	}
	for range 10 {
		step(30, 1, 0)
	}
	var st PassiveState
	for range 10 { // a NAS copy and, at the same time, the latency rises for another reason
		st = step(80, 901, 900)
	}
	if st.Active || st.LANDownMbps == nil || *st.LANDownMbps != 900 || *st.OtherDownMbps != 1 || *st.LineDownMbps > 2 {
		t.Fatalf("LAN copy: %+v", st)
	}
	for range 10 {
		step(30, 1, 0)
	}
	steam := Talker{PID: 300, Name: "steam", Rx: 80e6 / 8, Tx: 0.5e6 / 8}
	backup := Talker{PID: 400, Name: "backup", Rx: 0.1e6 / 8, Tx: 9e6 / 8}
	for range 5 {
		st = step(85, 90, 0, steam, backup)
	}
	if !st.Active || st.Direction != "download" || st.Top == nil || st.Top.Name != "steam" || st.Top.DownMbps != 80 {
		t.Fatalf("download by steam: %+v (top %+v)", st, st.Top)
	}
	// Not active: the program moving the most in both directions together.
	d2 := NewPassiveDetector(PassiveConfig{}, LineRate{})
	st = d2.Observe(PassiveSample{At: at, RateKnown: true, Rx: 1e6, Talkers: []Talker{backup, {Name: "idle"}}})
	if st.Top == nil || st.Top.Name != "backup" {
		t.Errorf("top while quiet: %+v", st.Top)
	}
	if st = d2.Observe(PassiveSample{At: at, RateKnown: true, Talkers: []Talker{{Name: "idle"}}}); st.Top != nil {
		t.Errorf("a program moving nothing is no top talker: %+v", st.Top)
	}
	// Without per-connection counters nothing is claimed about LAN traffic.
	if st = d2.Observe(PassiveSample{At: at, RateKnown: true, Rx: 1e6}); st.LANDownMbps != nil || st.LANUpMbps != nil {
		t.Errorf("LAN traffic without counters: %+v", st)
	}
}

func TestPassiveAsksForATestOnlyWithoutOne(t *testing.T) {
	sample := PassiveSample{At: time.Unix(1, 0), RTT: 20 * time.Millisecond, RTTKnown: true, RateKnown: true}
	// No test and no traffic yet: nothing to compare with, so not watching.
	st := NewPassiveDetector(PassiveConfig{}, LineRate{}).Observe(sample)
	if !st.NeedsTest || st.Watching || st.Reason != "your connection's speed isn't known yet" || st.LineSource != "" {
		t.Errorf("no test, no traffic: %+v", st)
	}
	for _, line := range []LineRate{{DownBps: 100e6 / 8}, {UpBps: 10e6 / 8}, {DownBps: 100e6 / 8, UpBps: 10e6 / 8}} {
		if st := NewPassiveDetector(PassiveConfig{}, line).Observe(sample); st.NeedsTest {
			t.Errorf("test %+v: still asks for one", line)
		}
	}
}
