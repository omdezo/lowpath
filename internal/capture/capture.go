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
//
// Backend (backend.go) is the OS-neutral contract the app depends on;
// WinDivert implements it on Windows, and Unavailable stands in where no
// capture method exists yet.
package capture

import (
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
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

	Log *slog.Logger
}

type Stats struct {
	Captured  uint64   `json:"captured"` // game packets handed to OnPacket
	Passed    uint64   `json:"passed"`   // packets put back unchanged
	Injected  uint64   `json:"injected"` // replies delivered to the game
	Errors    uint64   `json:"errors"`
	GamePIDs  []uint32 `json:"game_pids"` // running game processes
	LastError string   `json:"last_error"`
}

// Process is a running program with at least one UDP socket.
type Process = owner.Process

var (
	ErrNeedAdmin   = divert.ErrNeedAdmin
	ErrNoDriver    = divert.ErrNoDriver
	ErrUnsupported = divert.ErrUnsupported
	ErrClosed      = errors.New("capture: closed")
)

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
)

type Capture struct {
	cfg    Config
	log    *slog.Logger
	dev    device
	owners *owners

	captured, passed, injected, errs atomic.Uint64
	lastErr                          atomic.Pointer[string]

	flowMu  sync.RWMutex
	flows   map[flowKey]*route
	byLocal map[netip.AddrPort]*route // latest route per game socket
	tick    atomic.Int64              // coarse clock (unix seconds) for route.last

	ipID    atomic.Uint32
	injBufs sync.Pool // *injectBuf

	wg        sync.WaitGroup
	stop      chan struct{}
	closed    atomic.Bool
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
	}
	c.tick.Store(time.Now().Unix())
	c.injBufs.New = func() any { return &injectBuf{pkt: make([]byte, 0, 1500)} }
	return c
}

// run starts the receive goroutines and the background refresher.
func (c *Capture) run(workers int) {
	for range workers {
		c.wg.Add(1)
		go c.recvLoop()
	}
	c.wg.Add(1)
	go c.maintain()
}

func (c *Capture) recvLoop() {
	defer c.wg.Done()
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
				c.fail(fmt.Errorf("capture stopped: %w", err))
				go c.Close() // Close waits for this goroutine, so not inline
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		fails = 0
		c.handle(buf[:n], &addr)
	}
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
// sockets, PID reuse) and forgets idle flows.
func (c *Capture) maintain() {
	defer c.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case now := <-t.C:
			c.tick.Store(now.Unix())
			if err := c.owners.refresh(); err != nil {
				c.fail(err)
			}
			c.pruneFlows(now.Add(-flowIdle).Unix())
		}
	}
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
		Captured: c.captured.Load(),
		Passed:   c.passed.Load(),
		Injected: c.injected.Load(),
		Errors:   c.errs.Load(),
		GamePIDs: c.owners.gamePIDs(),
	}
	if s.GamePIDs == nil {
		s.GamePIDs = []uint32{}
	}
	if e := c.lastErr.Load(); e != nil {
		s.LastError = *e
	}
	return s
}

// Close stops capturing and releases the handle, after which the driver no
// longer sees any traffic. Packets already taken off the wire are still put
// back (or handed to OnPacket) while the queue drains. It is safe to call more
// than once and from any goroutine.
func (c *Capture) Close() error {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.stop)
		if err := c.dev.ShutdownRecv(); err != nil && !errors.Is(err, divert.ErrClosed) {
			c.log.Warn("capture: shutdown", "err", err)
		}
		done := make(chan struct{})
		go func() { c.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(closeWait):
			c.log.Warn("capture: receivers did not stop; closing the handle anyway")
		}
		c.closeErr = c.dev.Close()
		c.log.Info("capture stopped", "captured", c.captured.Load(), "passed", c.passed.Load(),
			"injected", c.injected.Load(), "errors", c.errs.Load())
	})
	return c.closeErr
}
