package catalog

import (
	"context"
	"errors"
	"math"
	"net/netip"
	"sync"
	"time"
)

// LivePing is the recent ping history of one server.
type LivePing struct {
	Sent     int           // echo requests with an answer or a timeout, in the window
	Received int           // replies among them
	Avg      time.Duration // mean reply time; meaningful only when Received > 0
	Min      time.Duration // fastest reply: the path's latency without queueing
	Jitter   time.Duration // standard deviation of the replies, as the routes table shows it
	Note     string        // why there is no number, when Received == 0
}

// LossPct is the share of requests in the window that got no reply.
func (p LivePing) LossPct() float64 {
	if p.Sent == 0 {
		return 0
	}
	return 100 * float64(p.Sent-p.Received) / float64(p.Sent)
}

// LiveOptions tune a LivePinger; zero fields take the defaults.
type LiveOptions struct {
	Interval time.Duration // between echo requests, default 1 s
	Timeout  time.Duration // per request, default 1 s (later = lost)
	Window   int           // results kept per server, default 30
	// Idle stops pinging a server nobody asked about for this long
	// (default 15 s): the card polls while it is on screen.
	Idle time.Duration
	Now  func() time.Time
}

// LivePinger pings the servers the UI is looking at, once a second each,
// and keeps the last Window results. Stats never blocks: the first call
// starts pinging and reports nothing yet.
type LivePinger struct {
	pinger Pinger
	opt    LiveOptions
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	loops map[netip.Addr]*pingLoop
}

func NewLivePinger(p Pinger, o LiveOptions) *LivePinger {
	if o.Interval <= 0 {
		o.Interval = time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = time.Second
	}
	if o.Window <= 0 {
		o.Window = 30
	}
	if o.Idle <= 0 {
		o.Idle = 15 * time.Second
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &LivePinger{pinger: p, opt: o, ctx: ctx, cancel: cancel, loops: map[netip.Addr]*pingLoop{}}
}

type pingLoop struct {
	mu      sync.Mutex
	asked   time.Time
	results []time.Duration // -1 = lost
	note    string          // pings can't be sent; set instead of results
}

// Stats returns what is known about addr and keeps it pinged for another
// Idle period.
func (l *LivePinger) Stats(addr netip.Addr) LivePing {
	addr = addr.Unmap()
	l.mu.Lock()
	lp := l.loops[addr]
	if lp == nil && l.ctx.Err() == nil {
		lp = &pingLoop{asked: l.opt.Now()}
		l.loops[addr] = lp
		go l.run(addr, lp)
	}
	l.mu.Unlock()
	if lp == nil {
		return LivePing{Note: "Lowpath is shutting down."}
	}
	lp.mu.Lock()
	defer lp.mu.Unlock()
	lp.asked = l.opt.Now()
	return summarize(lp.results, lp.note)
}

func (l *LivePinger) run(addr netip.Addr, lp *pingLoop) {
	defer func() {
		l.mu.Lock()
		if l.loops[addr] == lp {
			delete(l.loops, addr)
		}
		l.mu.Unlock()
	}()
	t := time.NewTicker(l.opt.Interval)
	defer t.Stop()
	for {
		rtt, err := l.pinger.Ping(addr, l.opt.Timeout)
		lp.mu.Lock()
		switch {
		case err == nil:
			lp.results, lp.note = appendWindow(lp.results, rtt, l.opt.Window), ""
		case errors.Is(err, ErrNoReply):
			lp.results, lp.note = appendWindow(lp.results, -1, l.opt.Window), ""
		case errors.Is(err, ErrIPv6):
			lp.note = "Lowpath can't ping IPv6 servers yet."
		default:
			// The PC can't send pings (permission, no route): that says
			// nothing about the server, so it isn't counted as loss.
			lp.note = "Can't send pings from this PC: " + err.Error()
		}
		idle := l.opt.Now().Sub(lp.asked) > l.opt.Idle
		lp.mu.Unlock()
		if idle {
			return
		}
		select {
		case <-l.ctx.Done():
			return
		case <-t.C:
		}
	}
}

func appendWindow(r []time.Duration, v time.Duration, n int) []time.Duration {
	r = append(r, v)
	if len(r) > n {
		r = r[len(r)-n:]
	}
	return r
}

func summarize(results []time.Duration, note string) LivePing {
	p := LivePing{Sent: len(results)}
	var sum float64
	for _, r := range results {
		if r < 0 {
			continue
		}
		p.Received++
		sum += float64(r)
		if p.Min == 0 || r < p.Min {
			p.Min = r
		}
	}
	switch {
	case p.Received > 0:
		mean := sum / float64(p.Received)
		var sq float64
		for _, r := range results {
			if r >= 0 {
				sq += (float64(r) - mean) * (float64(r) - mean)
			}
		}
		p.Avg = time.Duration(mean)
		p.Jitter = time.Duration(math.Sqrt(sq / float64(p.Received)))
	case note != "":
		p.Note = note
	case p.Sent >= 3:
		p.Note = "The server doesn't answer pings (many game servers don't)."
	case p.Sent == 0:
		p.Note = "Measuring…"
	default:
		p.Note = "No reply yet."
	}
	return p
}

// Close stops every ping loop.
func (l *LivePinger) Close() { l.cancel() }
