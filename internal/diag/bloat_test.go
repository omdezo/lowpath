package diag

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLine is a home connection: latency rises by downBloat / upBloat
// while the line is loaded in that direction.
type fakeLine struct {
	base, downBloat, upBloat time.Duration
	lossUnderLoad            bool
	downErr, upErr           error
	state                    atomic.Int32 // 0 idle, 1 down, 2 up
	rx, tx                   atomic.Uint64
}

func (f *fakeLine) Endpoint() string { return "fake:443" }

func (f *fakeLine) load(ctx context.Context, dir int32, err error, add func(int64)) error {
	if err != nil {
		return err
	}
	f.state.Store(dir)
	defer f.state.Store(0)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			add(10_000) // about 2 MB/s
			if dir == 1 {
				f.rx.Add(10_000)
			} else {
				f.tx.Add(10_000)
			}
		}
	}
}

func (f *fakeLine) Download(ctx context.Context, add func(int64)) error {
	return f.load(ctx, 1, f.downErr, add)
}

func (f *fakeLine) Upload(ctx context.Context, add func(int64)) error {
	return f.load(ctx, 2, f.upErr, add)
}

// probe answers at once with the latency the line would have now.
func (f *fakeLine) probe(base time.Duration, bloats bool) func(context.Context) (time.Duration, error) {
	return func(ctx context.Context) (time.Duration, error) {
		d := base
		st := f.state.Load()
		if bloats && f.lossUnderLoad && st != 0 {
			<-ctx.Done()
			return 0, ctx.Err()
		}
		if bloats {
			switch st {
			case 1:
				d += f.downBloat
			case 2:
				d += f.upBloat
			}
		}
		return d, nil
	}
}

func (f *fakeLine) DefaultRoute() (Route, error) { return Route{Index: 1, Name: "eth0"}, nil }

func (f *fakeLine) Counters(int) (Counters, error) {
	return Counters{At: time.Now(), RxBytes: f.rx.Load(), TxBytes: f.tx.Load()}, nil
}

func fastConfig(f *fakeLine) BloatConfig {
	return BloatConfig{
		Internet: []Candidate{{Tier: 1, Prober: NewFuncProber("internet", f.probe(f.base, true))}},
		Gateway:  NewFuncProber("gateway", f.probe(time.Millisecond, false)),
		Loader:   f,
		Net:      f,
		Route:    Route{Index: 1, Name: "eth0", Kind: LinkEthernet},
		Phase:    400 * time.Millisecond,
		Ramp:     100 * time.Millisecond,
		Interval: 10 * time.Millisecond,
		Timeout:  150 * time.Millisecond,
		Pick:     500 * time.Millisecond,
	}
}

func TestBloatTestGradesTheWorseDirection(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 20 * time.Millisecond, downBloat: 120 * time.Millisecond, upBloat: 10 * time.Millisecond}
	var phases []string
	res, err := NewBloatTest(fastConfig(f)).Run(context.Background(), func(p BloatProgress) {
		if len(phases) == 0 || phases[len(phases)-1] != p.Phase {
			phases = append(phases, p.Phase)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Grade != "C" {
		t.Fatalf("grade = %q (note %q), want C for +120 ms on download", res.Grade, res.Note)
	}
	if res.IdleMS == nil || *res.IdleMS != 20 {
		t.Fatalf("idle = %v", deref(res.IdleMS))
	}
	if res.DownIncreaseMS == nil || *res.DownIncreaseMS != 120 || res.UpIncreaseMS == nil || *res.UpIncreaseMS != 10 {
		t.Fatalf("increases = %v / %v", deref(res.DownIncreaseMS), deref(res.UpIncreaseMS))
	}
	if res.DownMbps == nil || *res.DownMbps < 8 || res.UpMbps == nil || *res.UpMbps < 8 {
		t.Fatalf("line rate = %v / %v Mbit/s", deref(res.DownMbps), deref(res.UpMbps))
	}
	if res.DataMB <= 0 || res.Target != "internet" || res.Gateway != "gateway" || res.Interface != "eth0" || res.Link != LinkEthernet {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Phases) != 4 || res.Phases[0].Gateway == nil || res.Phases[0].Gateway.P50 == nil || *res.Phases[0].Gateway.P50 != 1 {
		t.Fatalf("phases = %+v", res.Phases)
	}
	if p := res.Phases[3].Internet.P50; p == nil || *p != 20 {
		t.Fatalf("recovery p50 = %v, want back to 20", deref(p))
	}
	if got := strings.Join(phases, ","); got != "picking,idle,download,upload,recovery" {
		t.Fatalf("progress phases = %v", got)
	}
}

func TestBloatTestLossUnderLoad(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 15 * time.Millisecond, lossUnderLoad: true}
	res, err := NewBloatTest(fastConfig(f)).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Lost pings count as at least the probe timeout (150 ms here).
	if res.Grade != "C" || res.DownIncreaseMS == nil || *res.DownIncreaseMS < 150 {
		t.Fatalf("grade = %q, down %v", res.Grade, deref(res.DownIncreaseMS))
	}
	if !strings.Contains(res.Note, "lost") {
		t.Fatalf("note = %q", res.Note)
	}
}

func TestBloatTestNoLoadMeansNoGrade(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 10 * time.Millisecond, downErr: errors.New("HTTP 429"), upErr: errors.New("HTTP 429")}
	res, err := NewBloatTest(fastConfig(f)).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Grade != "" || res.DownIncreaseMS != nil || res.DownMbps != nil {
		t.Fatalf("grade %q down %v mbps %v: must be unknown", res.Grade, deref(res.DownIncreaseMS), deref(res.DownMbps))
	}
	if !strings.Contains(res.Note, "HTTP 429") || !strings.Contains(res.Note, "couldn't be loaded") {
		t.Fatalf("note = %q", res.Note)
	}
	if res.IdleMS == nil {
		t.Fatal("idle latency is still measured")
	}
}

func TestBloatTestOneDirectionFails(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 10 * time.Millisecond, downBloat: 40 * time.Millisecond, upErr: errors.New("upload refused")}
	res, err := NewBloatTest(fastConfig(f)).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Grade != "B" || res.UpIncreaseMS != nil || !strings.Contains(res.Note, "upload couldn't be tested") {
		t.Fatalf("grade %q up %v note %q", res.Grade, deref(res.UpIncreaseMS), res.Note)
	}
}

func TestBloatTestPicksLowestTierThenFastest(t *testing.T) {
	t.Parallel()
	fixed := func(name string, d time.Duration) *FuncProber {
		return NewFuncProber(name, func(context.Context) (time.Duration, error) { return d, nil })
	}
	fail := NewFuncProber("dead relay", func(context.Context) (time.Duration, error) { return 0, errors.New("no reply") })
	cands := []Candidate{
		{Tier: 2, Prober: fixed("tcp", time.Millisecond)},
		{Tier: 0, Prober: fail},
		{Tier: 1, Prober: fixed("icmp far", 30*time.Millisecond)},
		{Tier: 1, Prober: fixed("icmp near", 9*time.Millisecond)},
	}
	p, err := pickTarget(context.Background(), cands, time.Second, 100*time.Millisecond)
	if err != nil || p.Name() != "icmp near" {
		t.Fatalf("picked %v, %v", p, err)
	}
	if _, err := pickTarget(context.Background(), []Candidate{{Prober: fail}}, time.Second, 50*time.Millisecond); !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v", err)
	}
}

func TestBloatTestNothingAnswers(t *testing.T) {
	t.Parallel()
	cfg := fastConfig(&fakeLine{})
	cfg.Internet = []Candidate{{Prober: NewFuncProber("x", func(context.Context) (time.Duration, error) { return 0, errors.New("offline") })}}
	cfg.Gateway = nil
	start := time.Now()
	res, err := NewBloatTest(cfg).Run(context.Background(), nil)
	if err != nil || res.Grade != "" || res.Note == "" {
		t.Fatalf("res = %+v, %v", res, err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("offline must not run the whole test")
	}
}

func TestBloatTestGatewayOnly(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 10 * time.Millisecond}
	cfg := fastConfig(f)
	cfg.Internet = nil
	res, err := NewBloatTest(cfg).Run(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Grade != "" || res.Target != "" || res.Phases[0].Gateway == nil || res.Phases[0].Gateway.P50 == nil {
		t.Fatalf("res = %+v", res)
	}
	if res.Phases[0].Internet.P50 != nil {
		t.Fatal("no internet target: internet stats must be unknown")
	}
}

func TestBloatTestCancel(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	cfg := fastConfig(f)
	cfg.Phase = 5 * time.Second
	start := time.Now()
	if _, err := NewBloatTest(cfg).Run(ctx, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("cancel took too long")
	}
	time.Sleep(50 * time.Millisecond)
	if f.state.Load() != 0 {
		t.Fatal("the load kept running after cancel")
	}
}

// Stopped mid-download, Run returns only once the load has stopped and the
// progress reports are over: the app shows "not running" right after, and
// a boost may start at once.
func TestBloatTestCancelMidLoadLeavesNothingRunning(t *testing.T) {
	t.Parallel()
	f := &fakeLine{base: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := fastConfig(f)
	cfg.Phase = 2 * time.Second
	var returned, late atomic.Bool
	_, err := NewBloatTest(cfg).Run(ctx, func(p BloatProgress) {
		if returned.Load() {
			late.Store(true)
		}
		if p.Phase == PhaseDownload && p.LoadMbps != nil {
			cancel()
		}
	})
	returned.Store(true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if f.state.Load() != 0 {
		t.Fatal("Run returned while the load was still running")
	}
	time.Sleep(600 * time.Millisecond) // more than two progress ticks
	if late.Load() {
		t.Fatal("progress was reported after Run returned")
	}
}

func TestPercentile(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	if percentile(v, 0.5) != 5 || percentile(v, 0.95) != 10 || percentile(v[:1], 0.95) != 1 {
		t.Fatal("percentile")
	}
}

func deref(p *float64) any {
	if p == nil {
		return "nil"
	}
	return *p
}
