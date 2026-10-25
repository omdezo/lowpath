package app

import (
	"errors"
	"net/http/httptest"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"

	"lowpath/internal/capture"
)

// fakeCapturer stands in for WinDivert so route mode runs without admin
// rights or the driver: the test plays the game by calling OnPacket and sees
// what would have been injected back.
type fakeCapturer struct {
	avail    error // what Available reports
	admin    bool
	startErr error
	procs    []capture.Process

	mu       sync.Mutex
	sessions []*fakeCapture
}

func (f *fakeCapturer) Available() error                      { return f.avail }
func (f *fakeCapturer) Admin() bool                           { return f.admin }
func (f *fakeCapturer) Processes() ([]capture.Process, error) { return f.procs, nil }

func (f *fakeCapturer) Start(cfg capture.Config) (CaptureSession, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	c := &fakeCapture{cfg: cfg, injected: make(chan injected, 1024)}
	f.mu.Lock()
	f.sessions = append(f.sessions, c)
	f.mu.Unlock()
	return c, nil
}

func (f *fakeCapturer) last(t *testing.T) *fakeCapture {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) == 0 {
		t.Fatal("capture was never started")
	}
	return f.sessions[len(f.sessions)-1]
}

type injected struct {
	local, remote netip.AddrPort
	payload       string
}

type fakeCapture struct {
	cfg      capture.Config
	injected chan injected
	captured atomic.Uint64
	closed   atomic.Bool
}

// send plays one outbound game packet through the capture callback.
func (c *fakeCapture) send(local, remote netip.AddrPort, payload string) bool {
	c.captured.Add(1)
	return c.cfg.OnPacket(local, remote, []byte(payload))
}

func (c *fakeCapture) Inject(local, remote netip.AddrPort, payload []byte) error {
	if c.closed.Load() {
		return errors.New("closed")
	}
	c.injected <- injected{local, remote, string(payload)}
	return nil
}

func (c *fakeCapture) Stats() capture.Stats {
	return capture.Stats{Captured: c.captured.Load(), Injected: uint64(len(c.injected)), GamePIDs: []uint32{}}
}

func (c *fakeCapture) Close() error {
	c.closed.Store(true)
	return nil
}

// newRealServer runs the API in real (relay) mode with a fake capturer.
func newRealServer(t *testing.T, fc *fakeCapturer, opts ...func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: openTestStore(t), Games: games, Capture: fc, Log: discard, Session: testSession}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); ts.Close() })
	return ts, srv
}
