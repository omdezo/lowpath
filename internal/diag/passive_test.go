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
