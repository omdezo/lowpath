package app

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"lowpath/internal/diag"
	"lowpath/internal/engine"
	"lowpath/internal/proto"
)

// maxBloatRelays is how many of the user's relays the bufferbloat test
// considers as its latency target.
const maxBloatRelays = 6

var (
	errBloatRunning = errors.New("a bufferbloat test is already running")
	errBloatSession = errors.New("stop the boost first: the bufferbloat test fills your connection for 20 seconds, which would lag the game")
)

// bloatView is GET /api/diag/bloat.
type bloatView struct {
	Running  bool                `json:"running"`
	Progress *diag.BloatProgress `json:"progress"`
	Result   *diag.BloatResult   `json:"result"` // the last finished test, also after a restart
	Error    string              `json:"error,omitempty"`
}

// bloatRunner owns the on-demand bufferbloat test: at most one at a time,
// run in the background so the UI only polls.
type bloatRunner struct {
	net    diag.NetStats
	loader diag.Loader
	ping   diag.PingFunc
	relays func() []engine.PathSpec
	store  *Store
	log    *slog.Logger
	done   func(diag.BloatResult)  // after a finished test (new line rate)
	tune   func(*diag.BloatConfig) // tests shorten the phases

	mu       sync.Mutex
	running  bool
	progress *diag.BloatProgress
	err      string
	cancel   context.CancelFunc
	finished chan struct{}
}

func (b *bloatRunner) view() bloatView {
	b.mu.Lock()
	v := bloatView{Running: b.running, Progress: b.progress, Error: b.err}
	b.mu.Unlock()
	v.Result = b.store.Bloat()
	return v
}

func (b *bloatRunner) isRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// start begins a test in the background.
func (b *bloatRunner) start(parent context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return errBloatRunning
	}
	ctx, cancel := context.WithCancel(parent)
	b.running, b.err, b.cancel = true, "", cancel
	b.progress = &diag.BloatProgress{Phase: "picking"}
	b.finished = make(chan struct{})
	go b.run(ctx, b.finished)
	return nil
}

// stop cancels a running test and waits for it to end.
func (b *bloatRunner) stop(reason string) {
	b.mu.Lock()
	cancel, fin := b.cancel, b.finished
	if b.running && reason != "" {
		b.err = reason
	}
	b.mu.Unlock()
	if cancel != nil {
		cancel()
		<-fin
	}
}

func (b *bloatRunner) run(ctx context.Context, finished chan struct{}) {
	defer close(finished)
	defer func() {
		b.mu.Lock()
		b.running, b.progress, b.cancel = false, nil, nil
		b.mu.Unlock()
	}()
	cfg := b.config(ctx)
	test := diag.NewBloatTest(cfg)
	res, err := test.Run(ctx, func(p diag.BloatProgress) {
		b.mu.Lock()
		b.progress = &p
		b.mu.Unlock()
	})
	if err != nil {
		b.mu.Lock()
		if b.err == "" {
			b.err = "The test was stopped."
		}
		b.mu.Unlock()
		b.log.Info("bufferbloat test stopped", "err", err)
		return
	}
	b.log.Info("bufferbloat test", "grade", res.Grade, "target", res.Target, "idle_ms", fmtMS(res.IdleMS),
		"down_ms", fmtMS(res.DownIncreaseMS), "up_ms", fmtMS(res.UpIncreaseMS), "data_mb", int(res.DataMB), "note", res.Note)
	b.mu.Lock()
	b.err = ""
	b.mu.Unlock()
	if err := b.store.SetBloat(res); err != nil {
		b.log.Warn("save bufferbloat result", "err", err)
	}
	if b.done != nil {
		b.done(res)
	}
}

func fmtMS(v *float64) string {
	if v == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.1f", *v)
}

// config picks the targets: the gateway over ICMP; for the internet leg the
// user's relays (tier 0, the path games take with Lowpath), then a public
// anycast address over ICMP, then a TCP handshake with the load server,
// which works even where ICMP is blocked or needs privileges.
func (b *bloatRunner) config(ctx context.Context) diag.BloatConfig {
	cfg := diag.BloatConfig{Loader: b.loader, Net: b.net}
	if r, err := b.net.DefaultRoute(); err == nil {
		cfg.Route = r
		if r.Gateway.Is4() {
			cfg.Gateway = diag.NewICMPProber("Gateway", r.Gateway, b.ping)
		}
	} else if !errors.Is(err, diag.ErrUnsupported) {
		b.log.Info("bufferbloat test: no default route", "err", err)
	}
	n := 0
	for _, p := range b.relays() {
		a := p.Addr.Addr().Unmap()
		if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || n == maxBloatRelays {
			continue // a relay on this PC or LAN doesn't cross the internet line
		}
		n++
		cfg.Internet = append(cfg.Internet, diag.Candidate{Tier: 0, Prober: newRelayProber(p)})
	}
	cfg.Internet = append(cfg.Internet, diag.Candidate{Tier: 1, Prober: diag.NewICMPProber("Cloudflare", netip.MustParseAddr("1.1.1.1"), b.ping)})
	if b.loader != nil {
		for _, ap := range resolveEndpoint(ctx, b.loader.Endpoint()) {
			cfg.Internet = append(cfg.Internet, diag.Candidate{Tier: 2, Prober: diag.NewTCPProber("Load server", ap)})
		}
	}
	if b.tune != nil {
		b.tune(&cfg)
	}
	return cfg
}

// resolveEndpoint looks host:port up once, with a short timeout so a slow
// resolver can't stall the test. It returns the first IPv4 and the first
// IPv6 address: on an IPv6-only network (or one whose IPv4 is broken) the
// IPv4 address never answers while the load itself still flows over IPv6,
// so both become candidates and the target pick keeps whichever answers.
func resolveEndpoint(ctx context.Context, hostport string) []netip.AddrPort {
	if ap, err := netip.ParseAddrPort(hostport); err == nil {
		return []netip.AddrPort{netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())}
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil
	}
	pn, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return nil // Endpoint always gives a numeric port
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil
	}
	return firstPerFamily(addrs, uint16(pn))
}

// firstPerFamily keeps the first IPv4 and the first IPv6 address, IPv4 first.
func firstPerFamily(addrs []netip.Addr, port uint16) []netip.AddrPort {
	var v4, v6 netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		switch {
		case a.Is4() && !v4.IsValid():
			v4 = a
		case a.Is6() && !v6.IsValid():
			v6 = a
		}
	}
	var out []netip.AddrPort
	for _, a := range []netip.Addr{v4, v6} {
		if a.IsValid() {
			out = append(out, netip.AddrPortFrom(a, port))
		}
	}
	return out
}

// relayProber sends one overlay probe per sample to a relay and waits for
// its reply, the same exchange the engine uses to rank routes.
type relayProber struct {
	spec  engine.PathSpec
	token uint64
	flow  uint64
}

func newRelayProber(spec engine.PathSpec) *relayProber {
	var b [8]byte
	rand.Read(b[:])
	return &relayProber{spec: spec, token: proto.TokenFromKey(spec.Key), flow: binary.BigEndian.Uint64(b[:])}
}

func (p *relayProber) Name() string {
	return fmt.Sprintf("Relay %s %s (UDP probe)", p.spec.Label, p.spec.Addr)
}

func (p *relayProber) Probe(ctx context.Context) (time.Duration, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", p.spec.Addr.String())
	if err != nil {
		return 0, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	var sb [4]byte
	rand.Read(sb[:])
	seq := binary.BigEndian.Uint32(sb[:])
	pkt := proto.Append(nil, proto.Header{Type: proto.TProbe, Token: p.token, Flow: p.flow, Seq: seq}, nil)
	start := time.Now()
	if _, err := c.Write(pkt); err != nil {
		return 0, err
	}
	buf := make([]byte, 2048)
	for {
		n, err := c.Read(buf)
		if err != nil {
			return 0, err
		}
		if h, _, err := proto.Parse(buf[:n]); err == nil && h.Type == proto.TProbeReply && h.Flow == p.flow && h.Seq == seq {
			return time.Since(start), nil
		}
	}
}

// --- HTTP ---------------------------------------------------------------

func (s *Server) handleBloat(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.bloat.view())
}

func (s *Server) handleStartBloat(w http.ResponseWriter, r *http.Request) {
	// s.life orders this against a boost starting (apply holds it while it
	// stops any test and brings the session up): the test either starts
	// first and is stopped, or sees the session and is refused.
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	busy, closed := s.sess != nil, s.closed
	s.mu.Unlock()
	switch {
	case closed:
		writeErr(w, 503, "shutting_down", errors.New("Lowpath is shutting down"))
		return
	case busy:
		writeErr(w, 409, "session_active", errBloatSession)
		return
	case s.trace.isRunning():
		writeErr(w, 409, "trace_running", errBloatTrace)
		return
	}
	if err := s.bloat.start(s.lifetime); err != nil {
		writeErr(w, 409, "bloat_running", err)
		return
	}
	writeJSON(w, 202, s.bloat.view())
}

func (s *Server) handleStopBloat(w http.ResponseWriter, r *http.Request) {
	s.bloat.stop("The test was stopped.")
	writeJSON(w, 200, s.bloat.view())
}
