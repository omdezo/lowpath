package capture

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/divert"
)

// fakeDev is a WinDivert handle without the driver: Recv reads from a
// channel, Send records what would have gone out.
type fakeDev struct {
	in      chan recvd
	recvErr error // returned by Recv instead of reading in, when set

	keep   bool // record sent packets (off for allocation tests)
	mu     sync.Mutex
	sent   []recvd
	nSent  atomic.Int64
	nSums  atomic.Int64
	shut   sync.Once
	closed atomic.Bool
}

type recvd struct {
	pkt  []byte
	addr divert.Address
}

func newFakeDev() *fakeDev { return &fakeDev{in: make(chan recvd, 64), keep: true} }

func (d *fakeDev) Recv(buf []byte, addr *divert.Address) (int, error) {
	if d.recvErr != nil {
		return 0, d.recvErr
	}
	r, ok := <-d.in
	if !ok {
		return 0, divert.ErrClosed
	}
	*addr = r.addr
	return copy(buf, r.pkt), nil
}

func (d *fakeDev) Send(pkt []byte, addr *divert.Address) (int, error) {
	if d.closed.Load() {
		return 0, divert.ErrClosed
	}
	d.nSent.Add(1)
	if d.keep {
		d.mu.Lock()
		d.sent = append(d.sent, recvd{bytes.Clone(pkt), *addr})
		d.mu.Unlock()
	}
	return len(pkt), nil
}

func (d *fakeDev) CalcChecksums(pkt []byte, addr *divert.Address) error {
	d.nSums.Add(1)
	return nil
}

func (d *fakeDev) ShutdownRecv() error {
	d.shut.Do(func() { close(d.in) })
	return nil
}

func (d *fakeDev) Close() error {
	d.closed.Store(true)
	return nil
}

func (d *fakeDev) sentPackets() []recvd {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]recvd{}, d.sent...)
}

type call struct {
	local, remote netip.AddrPort
	payload       string
}

// harness wires a Capture to a fake device and fake OS. OnPacket records
// each call and returns consume.
type harness struct {
	c       *Capture
	dev     *fakeDev
	os      *fakeOS
	consume atomic.Bool
	mu      sync.Mutex
	calls   []call
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{dev: newFakeDev(), os: newFakeOS()}
	h.consume.Store(true)
	cfg := Config{
		Processes: []string{"game.exe"},
		Owners:    h.os,
		OnPacket: func(local, remote netip.AddrPort, payload []byte) bool {
			h.mu.Lock()
			h.calls = append(h.calls, call{local, remote, string(payload)})
			h.mu.Unlock()
			return h.consume.Load()
		},
	}
	if err := cfg.check(); err != nil {
		t.Fatal(err)
	}
	h.c = newCapture(cfg, h.dev, newTestOwners(t, h.os, cfg.Processes...))
	return h
}

func (h *harness) onPacketCalls() []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]call{}, h.calls...)
}

func outboundOn(ifIdx, sub uint32) divert.Address {
	var a divert.Address
	a.SetOutbound(true)
	a.SetNetwork(ifIdx, sub)
	return a
}

func udp(t testing.TB, src, dst netip.AddrPort, payload string) []byte {
	t.Helper()
	p, err := appendUDP4(nil, src, dst, 42, []byte(payload))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGamePacketConsumed(t *testing.T) {
	h := newHarness(t)
	addr := outboundOn(7, 3)
	h.c.handle(udp(t, gameLocal, gameServer, "hello"), &addr)

	calls := h.onPacketCalls()
	if len(calls) != 1 || calls[0] != (call{gameLocal, gameServer, "hello"}) {
		t.Fatalf("OnPacket calls = %+v", calls)
	}
	if n := len(h.dev.sentPackets()); n != 0 {
		t.Fatalf("%d packets went out; a consumed packet must not", n)
	}
	if s := h.c.Stats(); s.Captured != 1 || s.Passed != 0 || s.Errors != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestDeclinedPacketReinjected(t *testing.T) {
	h := newHarness(t)
	h.consume.Store(false)
	pkt := udp(t, gameLocal, gameServer, "direct")
	addr := outboundOn(7, 3)
	h.c.handle(pkt, &addr)

	sent := h.dev.sentPackets()
	if len(sent) != 1 || !bytes.Equal(sent[0].pkt, pkt) || sent[0].addr != addr {
		t.Fatalf("sent = %+v", sent)
	}
	if s := h.c.Stats(); s.Captured != 1 || s.Passed != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestOtherTrafficPassedUnchanged(t *testing.T) {
	h := newHarness(t)
	tcp := udp(t, gameLocal, gameServer, "x")
	tcp[9] = 6
	frag := udp(t, gameLocal, gameServer, "x")
	frag[6] = 0x20 // more fragments
	v6 := outboundOn(1, 0)
	v6.SetIPv6(true)

	cases := []struct {
		name string
		pkt  []byte
		addr divert.Address
	}{
		{"other process", udp(t, netip.MustParseAddrPort("192.168.1.10:50001"), gameServer, "x"), outboundOn(1, 0)},
		{"own process", udp(t, netip.MustParseAddrPort("192.168.1.10:50002"), gameServer, "x"), outboundOn(1, 0)},
		{"unowned port", udp(t, netip.MustParseAddrPort("192.168.1.10:9"), gameServer, "x"), outboundOn(1, 0)},
		{"tcp", tcp, outboundOn(1, 0)},
		{"fragment", frag, outboundOn(1, 0)},
		{"ipv6", []byte{0x60, 0, 0, 0, 0, 8, 17, 64}, v6},
	}
	for _, c := range cases {
		h.c.handle(c.pkt, &c.addr)
	}
	if calls := h.onPacketCalls(); len(calls) != 0 {
		t.Fatalf("OnPacket called for non-game traffic: %+v", calls)
	}
	sent := h.dev.sentPackets()
	if len(sent) != len(cases) {
		t.Fatalf("%d of %d packets passed", len(sent), len(cases))
	}
	for i, c := range cases {
		if !bytes.Equal(sent[i].pkt, c.pkt) || sent[i].addr != c.addr {
			t.Errorf("%s: changed on the way out", c.name)
		}
	}
	if s := h.c.Stats(); s.Captured != 0 || s.Passed != uint64(len(cases)) {
		t.Fatalf("stats = %+v", s)
	}
}

func TestFirstPacketOfNewGameSocketCaptured(t *testing.T) {
	h := newHarness(t)
	local := netip.MustParseAddrPort("192.168.1.10:7000")
	h.os.add(socket{Local: local, PID: 500}, "game.exe") // game launched after Start
	addr := outboundOn(7, 3)
	h.c.handle(udp(t, local, gameServer, "first"), &addr)
	if calls := h.onPacketCalls(); len(calls) != 1 || calls[0].payload != "first" {
		t.Fatalf("first packet not captured: %+v", calls)
	}
	if len(h.dev.sentPackets()) != 0 {
		t.Fatal("first packet leaked out directly")
	}
}

func TestInject(t *testing.T) {
	h := newHarness(t)
	addr := outboundOn(7, 3)
	h.c.handle(udp(t, gameLocal, gameServer, "ping"), &addr)

	if err := h.c.Inject(gameLocal, gameServer, []byte("pong")); err != nil {
		t.Fatal(err)
	}
	// A reply from another port of the server still reaches the game socket.
	other := netip.AddrPortFrom(gameServer.Addr(), 27016)
	if err := h.c.Inject(gameLocal, other, []byte("pong2")); err != nil {
		t.Fatal(err)
	}

	sent := h.dev.sentPackets()
	if len(sent) != 2 {
		t.Fatalf("%d packets injected", len(sent))
	}
	src, dst, payload, ok := parseUDP4(sent[0].pkt)
	if !ok || src != gameServer || dst != gameLocal || string(payload) != "pong" {
		t.Fatalf("injected %v -> %v %q", src, dst, payload)
	}
	verifyChecksums(t, sent[0].pkt)
	if a := sent[0].addr; a.Outbound() || a.Loopback() || a.Layer() != divert.LayerNetwork || a.IfIdx() != 7 || a.SubIfIdx() != 3 {
		t.Fatalf("inject address: outbound=%v if=%d/%d", a.Outbound(), a.IfIdx(), a.SubIfIdx())
	}
	if src, _, _, _ := parseUDP4(sent[1].pkt); src != other {
		t.Fatalf("second reply from %v", src)
	}
	if binary.BigEndian.Uint16(sent[0].pkt[4:]) == binary.BigEndian.Uint16(sent[1].pkt[4:]) {
		t.Fatal("IP ID not incremented")
	}
	if h.dev.nSums.Load() != 2 {
		t.Fatalf("CalcChecksums called %d times", h.dev.nSums.Load())
	}
	if s := h.c.Stats(); s.Injected != 2 || s.Errors != 0 {
		t.Fatalf("stats = %+v", s)
	}

	err := h.c.Inject(netip.MustParseAddrPort("192.168.1.10:1"), gameServer, []byte("x"))
	if err == nil || h.c.Stats().Errors != 1 || len(h.dev.sentPackets()) != 2 {
		t.Fatalf("inject without a flow: %v", err)
	}
	if !strings.Contains(h.c.Stats().LastError, "no outbound flow") {
		t.Fatalf("last error = %q", h.c.Stats().LastError)
	}
}

func TestFlowsPruned(t *testing.T) {
	h := newHarness(t)
	addr := outboundOn(7, 3)
	h.c.handle(udp(t, gameLocal, gameServer, "x"), &addr)
	h.c.pruneFlows(h.c.tick.Load()) // nothing older than now
	if h.c.Inject(gameLocal, gameServer, nil) != nil {
		t.Fatal("live flow pruned")
	}
	h.c.pruneFlows(h.c.tick.Load() + 1)
	if h.c.Inject(gameLocal, gameServer, nil) == nil {
		t.Fatal("idle flow kept")
	}
}

func TestRunAndClose(t *testing.T) {
	h := newHarness(t)
	h.c.run(3)
	h.dev.in <- recvd{udp(t, gameLocal, gameServer, "game"), outboundOn(7, 3)}
	h.dev.in <- recvd{udp(t, netip.MustParseAddrPort("192.168.1.10:50001"), gameServer, "web"), outboundOn(7, 3)}
	waitFor(t, func() bool { s := h.c.Stats(); return s.Captured == 1 && s.Passed == 1 })

	done := make(chan error)
	go func() { done <- h.c.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(closeWait):
		t.Fatal("Close did not return")
	}
	if !h.dev.closed.Load() {
		t.Fatal("handle not closed")
	}
	if err := h.c.Close(); err != nil {
		t.Fatal("second Close:", err)
	}
	if err := h.c.Inject(gameLocal, gameServer, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Inject after Close: %v", err)
	}
}

// A handle that keeps failing must not stay open: matching traffic would sit
// in the driver's queue instead of going out.
func TestBrokenHandleStopsCapture(t *testing.T) {
	h := newHarness(t)
	h.dev.recvErr = errors.New("device gone")
	h.c.run(1)
	waitFor(t, h.dev.closed.Load)
	if s := h.c.Stats(); !strings.Contains(s.LastError, "capture stopped") || s.Errors < maxRecvFailures {
		t.Fatalf("stats = %+v", s)
	}
	<-h.c.Done()
	if err := h.c.Err(); err == nil || !strings.Contains(err.Error(), "device gone") {
		t.Fatalf("Err = %v, want the handle's error", err)
	}
}

func TestStatsJSON(t *testing.T) {
	h := newHarness(t)
	b, _ := json.Marshal(h.c.Stats())
	want := `{"captured":0,"passed":0,"injected":0,"errors":0,"game_pids":[100],"last_error":""}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	h.os.mu.Lock()
	delete(h.os.procs, pidGame)
	h.os.mu.Unlock()
	h.c.owners.refresh()
	if b, _ := json.Marshal(h.c.Stats().GamePIDs); string(b) != "[]" {
		t.Fatalf("no game running: game_pids = %s", b)
	}
}

func TestConfigCheck(t *testing.T) {
	if err := (&Config{Processes: []string{"a.exe"}}).check(); err == nil {
		t.Error("missing OnPacket accepted")
	}
	if err := (&Config{OnPacket: func(_, _ netip.AddrPort, _ []byte) bool { return true }}).check(); err == nil {
		t.Error("nothing to capture accepted")
	}
	if err := (&Config{Processes: []string{"a.exe"}, OnPacket: func(_, _ netip.AddrPort, _ []byte) bool { return true }}).check(); err == nil {
		t.Error("missing socket-owner resolver accepted")
	}
}

// The per-packet paths must not allocate: they run for every packet on the
// machine that matches the filter.
func TestNoAllocations(t *testing.T) {
	h := newHarness(t)
	h.dev.keep = false
	h.c.cfg.OnPacket = func(_, _ netip.AddrPort, _ []byte) bool { return true }
	game := udp(t, gameLocal, gameServer, "payload")
	other := udp(t, netip.MustParseAddrPort("192.168.1.10:50001"), gameServer, "payload")
	addr := outboundOn(7, 3)
	h.c.handle(game, &addr) // first packet creates the flow
	reply := []byte("reply payload")

	for name, f := range map[string]func(){
		"game packet":  func() { h.c.handle(game, &addr) },
		"pass-through": func() { h.c.handle(other, &addr) },
		"inject":       func() { h.c.Inject(gameLocal, gameServer, reply) },
	} {
		if n := testing.AllocsPerRun(1000, f); n != 0 {
			t.Errorf("%s: %.1f allocations per packet", name, n)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
