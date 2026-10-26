package app

import (
	"net/http/httptest"
	"testing"
	"time"

	"lowpath/internal/platform/fakeplatform"
)

// newFake is the platform every app test runs on: route mode, process lists
// and elevation work without admin rights, the driver or a particular OS.
func newFake(t *testing.T) *fakeplatform.Fake {
	t.Helper()
	return fakeplatform.New(t.TempDir())
}

// lastCapture is the capture session the app started most recently.
func lastCapture(t *testing.T, fp *fakeplatform.Fake) *fakeplatform.Session {
	t.Helper()
	c := fp.Capture.Last()
	if c == nil {
		t.Fatal("capture was never started")
	}
	return c
}

// newRealServer runs the API in real (relay) mode on a fake platform.
func newRealServer(t *testing.T, fp *fakeplatform.Fake, opts ...func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	games, err := LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Store: openTestStore(t), Games: games, Platform: fp.Platform(), Log: discard, Session: testSession}
	for _, o := range opts {
		o(&cfg)
	}
	srv, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { srv.Close(); ts.Close() })
	// Tests tick right away; the first watch waits for start-up's reset.
	waitReset(t, srv)
	return ts, srv
}

// waitReset waits for start-up's flow reset to return; one that never does
// fails the test instead of hanging the package.
func waitReset(t *testing.T, srv *Server) {
	t.Helper()
	select {
	case <-srv.current.resetDone:
	case <-time.After(5 * time.Second):
		t.Fatal("start-up's flow reset never returned")
	}
}
