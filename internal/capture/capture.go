// Package capture takes a game's outbound UDP packets off the network with
// WinDivert, hands them to a callback (the engine tunnels them over the
// relays) and puts the relays' replies back into the game's socket, so the
// game needs no settings changed.
//
// One network-layer WinDivert handle diverts outbound IPv4 UDP to public
// addresses. Each packet's local port is mapped to the owning process; only
// packets from the selected game processes go to Config.OnPacket, everything
// else is put back unchanged at once. TCP, IPv6, fragments and LAN traffic
// are never touched.
//
// Nothing persistent is installed besides the driver files: when the handle
// is closed (Close, or the process exits) networking is back to normal.
// Capture also fails open on its own (watchdog.go): when a packet is stuck
// in the handler, the handler panics or the handle keeps failing, it closes
// the handle and reports why through Done and Err.
//
// Backend (backend.go) is the OS-neutral contract the app depends on;
// WinDivert implements it on Windows, TUN (tun.go) on Linux with the same
// packet loop over a TUN device, and Unavailable stands in where no capture
// method exists yet.
package capture

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lowpath/internal/divert"
	"lowpath/internal/owner"
)

type Config struct {
	DriverDir string           // where divert.Install put the files
	Processes []string         // image names to capture, case-insensitive ("cs2.exe")
	PIDs      []uint32         // extra exact PIDs to capture (optional)
	Exclude   []netip.AddrPort // relay endpoints: never captured (port 0 = whole host)
	Owners    owner.Resolver   // who owns each local UDP socket (required)

	// OnPacket is called for every captured game packet, from several
	// goroutines at once; payload is only valid during the call. true =
	// consumed (tunneled, or dropped by the kill switch); false = the original
	// packet is put back unchanged and goes out directly.
	OnPacket func(local, remote netip.AddrPort, payload []byte) bool

	// StallLimit is how long one packet may stay in the handler before the
	// watchdog stops capturing (0 = DefaultStallLimit).
	StallLimit time.Duration

	Log *slog.Logger
}

type Stats struct {
	Captured  uint64   `json:"captured"` // game packets handed to OnPacket
	Passed    uint64   `json:"passed"`   // packets put back unchanged
	Injected  uint64   `json:"injected"` // replies delivered to the game
	Errors    uint64   `json:"errors"`
	GamePIDs  []uint32 `json:"game_pids"` // running game processes
	LastError string   `json:"last_error"`
	// RouteRepairs counts the times another program removed part of the
	// routing that brings the game's packets here, and it was put back
	// (TUN backend). Packets in between took the normal route.
	RouteRepairs uint64 `json:"route_repairs"`
}

// Process is a running program with at least one UDP socket.
type Process = owner.Process

var (
	ErrNeedAdmin   = divert.ErrNeedAdmin
	ErrNoDriver    = divert.ErrNoDriver
	ErrUnsupported = divert.ErrUnsupported
	ErrClosed      = errors.New("capture: closed")
	// ErrRouteRemoved means another program removed the routing that brings
	// the game's packets to the capture (a network manager or a firewall
	// reload), and it couldn't be put back or keeps being removed: capture
	// stopped and the game is on the normal route.
	ErrRouteRemoved = errors.New("capture: another program removed the routing to the capture")
)

// routeLost is an ErrRouteRemoved that says which part went and why it
// couldn't come back.
type routeLost struct {
	part  string // "routing rule", "nftables table"
	err   error  // why putting it back failed; nil when it keeps going
	times int    // how often it was put back within routeRepairWindow
}

func (e *routeLost) Error() string {
	if e.err == nil {
		return fmt.Sprintf("capture stopped: another program keeps removing the %s (%d times within a minute)", e.part, e.times)
	}
	return fmt.Sprintf("capture stopped: another program removed the %s and putting it back failed: %v", e.part, e.err)
}

func (e *routeLost) Is(target error) bool { return target == ErrRouteRemoved }
func (e *routeLost) Unwrap() error        { return e.err }

// device is the part of a WinDivert handle the packet loop uses; tests swap
// in a fake so the loop runs without the driver.
type device interface {
	Recv(buf []byte, addr *divert.Address) (int, error)
	Send(pkt []byte, addr *divert.Address) (int, error)
	CalcChecksums(pkt []byte, addr *divert.Address) error
	ShutdownRecv() error
	Close() error
}

const (
	// flowIdle is how long Inject remembers a flow's interface after its
	// last outbound packet.
	flowIdle = 2 * time.Minute
	// closeWait bounds how long Close waits for receivers to drain.
	closeWait = 3 * time.Second
	// maxRecvFailures consecutive receive errors (~1 s) mean the handle is
	// broken; capture then stops so matching traffic isn't held in the queue.
	maxRecvFailures = 100
	// A program that removes the routing more than maxRouteRepairs times
	// within routeRepairWindow (a script that keeps re-applying its own
	// rules) would send the game's packets on the normal route part of the
	// time while the UI says boosted; capture stops instead and says why.
	maxRouteRepairs   = 5
	routeRepairWindow = time.Minute // routeLost's message says "a minute"
)

type Capture struct {
	cfg    Config
	log    *slog.Logger
	dev    device
	owners *owners

	captured, passed, injected, errs atomic.Uint64
	lastErr                          atomic.Pointer[string]
	routeRepairs                     atomic.Uint64
	repairedAt                       []time.Time // within routeRepairWindow; maintain's own

	flowMu  sync.RWMutex
	flows   map[flowKey]*route
	byLocal map[netip.AddrPort]*route // latest route per game socket
	tick    atomic.Int64              // coarse clock (unix seconds) for route.last

	ipID    atomic.Uint32
	injBufs sync.Pool // *injectBuf

	workers []*workerState // one per receive goroutine, for the watchdog

	wg        sync.WaitGroup
	stop      chan struct{}
	done      chan struct{} // closed once the handle is closed
	closed    atomic.Bool
	reason    atomic.Pointer[error] // why capture stopped on its own
	closeOnce sync.Once
	closeErr  error
}

type flowKey struct{ local, remote netip.AddrPort }

// route is the interface a flow's outbound packets left on; replies are
// injected inbound on the same one.
type route struct {
	iface atomic.Uint64 // IfIdx<<32 | SubIfIdx
	last  atomic.Int64
}

type injectBuf struct {
	pkt  []byte
	addr divert.Address
}

func (cfg *Config) check() error {
	if cfg.OnPacket == nil {
		return errors.New("capture: Config.OnPacket is required")
	}
	if len(cfg.Processes) == 0 && len(cfg.PIDs) == 0 {
		return errors.New("capture: no processes to capture")
	}
	if cfg.Owners == nil {
		return errors.New("capture: Config.Owners is required")
	}
	return nil
}

func newCapture(cfg Config, dev device, o *owners) *Capture {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	c := &Capture{
		cfg:     cfg,
		log:     cfg.Log,
		dev:     dev,
		owners:  o,
		flows:   map[flowKey]*route{},
		byLocal: map[netip.AddrPort]*route{},
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	c.tick.Store(time.Now().Unix())
	c.injBufs.New = func() any { return &injectBuf{pkt: make([]byte, 0, 1500)} }
	return c
}

// run starts the receive goroutines, the background refresher and the
// watchdog.
func (c *Capture) run(workers int) {
	c.workers = make([]*workerState, workers)
	for i := range c.workers {
		c.workers[i] = &workerState{}
	}
	for _, w := range c.workers {
		c.wg.Add(1)
		go c.recvLoop(w)
	}
	c.wg.Add(2)
	go c.maintain()
	go c.watch()
}

func (c *Capture) recvLoop(w *workerState) {
	defer c.wg.Done()
	// A panic in the packet path (OnPacket is the engine) must not leave the
	// handle open with nobody reading it: stop capturing so traffic goes
	// direct, and keep the app alive to say what happened.
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("capture: packet handler panicked", "panic", r, "stack", string(debug.Stack()))
			c.abort(fmt.Errorf("capture stopped: the packet handler crashed (%v)", r))
		}
	}()
	buf := make([]byte, divert.MTUMax) // one per goroutine: no per-packet allocation
	var addr divert.Address
	fails := 0
	for {
		n, err := c.dev.Recv(buf, &addr)
		if err != nil {
			if c.closed.Load() || errors.Is(err, divert.ErrClosed) {
				return
			}
			c.fail(err)
			if fails++; fails >= maxRecvFailures {
				c.abort(fmt.Errorf("capture stopped: %w", err))
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		fails = 0
		w.enter()
		c.handle(buf[:n], &addr)
		w.leave()
	}
}

// watch is the watchdog: when a packet has been in the handler for longer
// than the stall limit (the engine is deadlocked, an owner lookup hangs),
// capture stops, so the game and everything else the filter matches goes
// out directly instead of waiting in the driver's queue.
func (c *Capture) watch() {
	defer c.wg.Done()
	sw := newStallWatch(c.cfg.StallLimit, c.workers, time.Now())
	t := time.NewTicker(sw.every)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			if d, stuck := sw.check(now); stuck {
				c.abort(fmt.Errorf("capture stopped: a packet was stuck in the handler for %v", d.Round(time.Millisecond)))
				return
			}
		}
	}
}

// abort stops capturing on its own (watchdog, panic, broken handle) and
// records why; the first reason wins. It never waits: it may run on a
// goroutine Close would wait for.
func (c *Capture) abort(err error) {
	if c.closed.Load() {
		return // already stopping; a failure while draining isn't news
	}
	if c.reason.CompareAndSwap(nil, &err) {
		c.errs.Add(1)
		msg := err.Error()
		c.lastErr.Store(&msg)
		c.log.Error("capture: failing open, game traffic goes direct", "err", err)
	}
	go c.shutdown(abortWait)
}

// handle decides one diverted packet: game packets go to OnPacket, everything
// else (and whatever OnPacket declines) goes back out unchanged.
func (c *Capture) handle(pkt []byte, addr *divert.Address) {
	if !addr.IPv6() {
		if local, remote, payload, ok := parseUDP4(pkt); ok {
			game, err := c.owners.isGame(local)
			if err != nil {
				c.fail(err)
			}
			if game {
				c.remember(local, remote, addr)
				c.captured.Add(1)
				if c.cfg.OnPacket(local, remote, payload) {
					return
				}
			}
		}
	}
	if _, err := c.dev.Send(pkt, addr); err != nil {
		if !c.closed.Load() {
			c.fail(err)
		}
		return
	}
	c.passed.Add(1)
}

// remember records the interface a flow uses so Inject can answer on it.
func (c *Capture) remember(local, remote netip.AddrPort, addr *divert.Address) {
	k := flowKey{local, remote}
	c.flowMu.RLock()
	r := c.flows[k]
	c.flowMu.RUnlock()
	if r == nil {
		c.flowMu.Lock()
		if r = c.flows[k]; r == nil {
			r = &route{}
			c.flows[k] = r
		}
		c.byLocal[local] = r
		c.flowMu.Unlock()
	}
	r.iface.Store(uint64(addr.IfIdx())<<32 | uint64(addr.SubIfIdx()))
	r.last.Store(c.tick.Load())
}

func (c *Capture) route(local, remote netip.AddrPort) *route {
	c.flowMu.RLock()
	defer c.flowMu.RUnlock()
	if r := c.flows[flowKey{local, remote}]; r != nil {
		return r
	}
	// A reply from an address the game never sent to (another port of the
	// same server, say) still belongs on the game socket's interface.
	return c.byLocal[local]
}

// Inject delivers a reply to the game: an IPv4+UDP packet from remote to
// local, injected inbound on the interface the flow's outbound packets used.
// It is safe for concurrent use and does not retain payload.
func (c *Capture) Inject(local, remote netip.AddrPort, payload []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	r := c.route(local, remote)
	if r == nil {
		err := fmt.Errorf("capture: inject: no outbound flow from %s", local)
		c.fail(err)
		return err
	}
	b := c.injBufs.Get().(*injectBuf)
	defer c.injBufs.Put(b)
	pkt, err := appendUDP4(b.pkt[:0], remote, local, uint16(c.ipID.Add(1)), payload)
	b.pkt = pkt[:0]
	if err != nil {
		c.fail(err)
		return err
	}
	iface := r.iface.Load()
	b.addr = divert.Address{} // network layer, inbound
	b.addr.SetNetwork(uint32(iface>>32), uint32(iface))
	// The DLL's checksum pass also marks the checksums valid in the address,
	// which tells the stack not to verify them again.
	if err := c.dev.CalcChecksums(pkt, &b.addr); err != nil {
		c.fail(err)
		return err
	}
	if _, err := c.dev.Send(pkt, &b.addr); err != nil {
		if errors.Is(err, divert.ErrClosed) {
			return ErrClosed
		}
		c.fail(err)
		return err
	}
	c.injected.Add(1)
	return nil
}

// maintain refreshes the owner table every second (new game processes, new
// sockets, PID reuse), checks that the routing to the device is still there
// (TUN backend) and forgets idle flows.
func (c *Capture) maintain() {
	defer c.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	rk, _ := c.dev.(routeKeeper)
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.tick.Store(now.Unix())
			if rk != nil && !c.keepRoute(rk, now) {
				return // stopping: the routing is gone for good
			}
			if err := c.owners.refresh(); err != nil {
				c.fail(err)
			} else if ds, ok := c.dev.(directSetter); ok {
				// The kernel's fast path for other programs' ports follows
				// the table (TUN backend).
				if err := ds.setDirect(c.owners.cur.Load().directPorts(maxDirectPorts)); err != nil {
					c.fail(err)
				}
			}
			c.pruneFlows(now.Add(-flowIdle).Unix())
		}
	}
}

// keepRoute has the device put back routing that another program removed,
// counts and logs each time, and stops capturing (false) when that fails or
// keeps happening: then the game's packets take the normal route for good,
// and the UI must stop saying they are boosted.
func (c *Capture) keepRoute(rk routeKeeper, now time.Time) bool {
	fixed, err := rk.keepRoute()
	if errors.Is(err, ErrRouteRemoved) {
		c.abort(err)
		return false
	}
	if err != nil {
		c.fail(err) // the check itself failed; the next one tries again
	}
	if len(fixed) == 0 {
		return true
	}
	n := c.routeRepairs.Add(1)
	c.repairedAt = append(slices.DeleteFunc(c.repairedAt, func(t time.Time) bool {
		return now.Sub(t) >= routeRepairWindow
	}), now)
	c.log.Warn("capture: another program removed the routing to the capture; put it back",
		"parts", fixed, "times", n, "within_window", len(c.repairedAt))
	if len(c.repairedAt) > maxRouteRepairs {
		c.abort(&routeLost{part: strings.Join(fixed, " and "), times: len(c.repairedAt)})
		return false
	}
	return true
}

func (c *Capture) pruneFlows(before int64) {
	c.flowMu.Lock()
	defer c.flowMu.Unlock()
	for k, r := range c.flows {
		if r.last.Load() < before {
			delete(c.flows, k)
		}
	}
	for k, r := range c.byLocal {
		if r.last.Load() < before {
			delete(c.byLocal, k)
		}
	}
}

func (c *Capture) fail(err error) {
	c.errs.Add(1)
	msg := err.Error()
	if old := c.lastErr.Swap(&msg); old == nil || *old != msg {
		c.log.Warn("capture", "err", err) // log each distinct error once in a row
	}
}

func (c *Capture) Stats() Stats {
	s := Stats{
		Captured:     c.captured.Load(),
		Passed:       c.passed.Load(),
		Injected:     c.injected.Load(),
		Errors:       c.errs.Load(),
		GamePIDs:     c.owners.gamePIDs(),
		RouteRepairs: c.routeRepairs.Load(),
	}
	if s.GamePIDs == nil {
		s.GamePIDs = []uint32{}
	}
	if e := c.lastErr.Load(); e != nil {
		s.LastError = *e
	}
	return s
}

// Done is closed once capture has stopped and the handle is closed, whether
// by Close or on its own.
func (c *Capture) Done() <-chan struct{} { return c.done }

// Err says why capture stopped on its own (watchdog, panic, broken handle,
// routing removed: ErrRouteRemoved).
// It is nil while capturing and after a plain Close.
func (c *Capture) Err() error {
	if e := c.reason.Load(); e != nil {
		return *e
	}
	return nil
}

// Close stops capturing and releases the handle, after which the driver no
// longer sees any traffic. Packets already taken off the wire are still put
// back (or handed to OnPacket) while the queue drains. It is safe to call more
// than once and from any goroutine.
func (c *Capture) Close() error { return c.shutdown(closeWait) }

// shutdown stops receiving, waits up to wait for the workers to finish the
// packets they hold, then closes the handle. Packets still in the driver's
// queue are reinjected by the driver when the handle closes.
func (c *Capture) shutdown(wait time.Duration) error {
	c.closeOnce.Do(func() {
		defer close(c.done)
		c.closed.Store(true)
		close(c.stop)
		if err := c.dev.ShutdownRecv(); err != nil && !errors.Is(err, divert.ErrClosed) {
			c.log.Warn("capture: shutdown", "err", err)
		}
		c.awaitWorkers(wait)
		c.closeErr = c.dev.Close()
		c.log.Info("capture stopped", "captured", c.captured.Load(), "passed", c.passed.Load(),
			"injected", c.injected.Load(), "errors", c.errs.Load())
	})
	return c.closeErr
}

// awaitWorkers waits up to wait for the receive goroutines to return. It
// gives up early when one of them has held the same packet for abortWait:
// that worker is stuck (the watchdog has already stopped with c.stop), and
// waiting the rest of wait would only keep the queued packets past the
// driver's queue time, after which closing the handle drops them instead
// of sending them on.
func (c *Capture) awaitWorkers(wait time.Duration) {
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	sw := newStallWatch(abortWait, c.workers, time.Now())
	tick := time.NewTicker(sw.every)
	defer tick.Stop()
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case <-done:
			return
		case <-deadline.C:
			c.log.Warn("capture: receivers did not stop; closing the handle anyway")
			return
		case now := <-tick.C:
			if _, stuck := sw.check(now); stuck {
				c.log.Warn("capture: a receiver is stuck in the handler; closing the handle anyway")
				return
			}
		}
	}
}
