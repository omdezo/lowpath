//go:build windows && amd64

package divert

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeDLL plays the WinDivert functions that take a HANDLE, so Handle can be
// tested without the driver or admin rights. The callbacks in useFake are C
// function pointers made by syscall.NewCallback, so a Handle on fakeProcs
// makes exactly the calls it makes into the real DLL. They leave the
// out-parameters alone: the tests only look at the order of the calls.
type fakeDLL struct {
	sendGate  chan struct{} // when set, send waits for it to close
	recvBlock bool          // recv waits for a shutdown, like a real empty queue

	shutOnce sync.Once
	shut     chan struct{} // closed by the first shutdown

	released atomic.Bool  // close was called
	late     atomic.Int32 // calls that started after close
	calls    atomic.Int32 // every call but close
	closes   atomic.Int32
	entered  chan string // each call's name as it starts, when set

	mu  sync.Mutex
	log []string
}

func newFakeDLL() *fakeDLL { return &fakeDLL{shut: make(chan struct{})} }

func (f *fakeDLL) record(ev string) {
	f.mu.Lock()
	f.log = append(f.log, ev)
	f.mu.Unlock()
}

func (f *fakeDLL) events() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.log)
}

func (f *fakeDLL) start(name string) {
	f.calls.Add(1)
	if f.released.Load() {
		f.late.Add(1)
	}
	if f.entered != nil {
		f.entered <- name
	}
}

var (
	fakeNow   atomic.Pointer[fakeDLL] // the fake the callbacks below play
	fakeOnce  sync.Once
	fakeProcs procTable
)

// useFake returns a new Handle on fake. The callbacks are made once per
// process (Windows never frees them) and play whichever fake is current.
func useFake(t *testing.T, fake *fakeDLL) *Handle {
	t.Helper()
	fakeOnce.Do(func() {
		fakeProcs = procTable{
			recv: syscall.NewCallback(func(h, buf, n, readLen, addr uintptr) uintptr {
				f := fakeNow.Load()
				f.start("recv")
				if f.recvBlock {
					<-f.shut
					f.record("recv returned")
					return 0 // ERROR_NO_DATA in the real DLL
				}
				return 1
			}),
			send: syscall.NewCallback(func(h, pkt, n, writeLen, addr uintptr) uintptr {
				f := fakeNow.Load()
				f.start("send")
				if f.sendGate != nil {
					<-f.sendGate
				}
				f.record("send returned")
				return 1
			}),
			shutdown: syscall.NewCallback(func(h, how uintptr) uintptr {
				f := fakeNow.Load()
				f.start("shutdown")
				f.record("shutdown")
				f.shutOnce.Do(func() { close(f.shut) })
				return 1
			}),
			close: syscall.NewCallback(func(h uintptr) uintptr {
				f := fakeNow.Load()
				f.closes.Add(1)
				f.record("close")
				f.released.Store(true)
				return 1
			}),
			setParam: syscall.NewCallback(func(h, p, v uintptr) uintptr {
				fakeNow.Load().start("setParam")
				return 1
			}),
			getParam: syscall.NewCallback(func(h, p, v uintptr) uintptr {
				fakeNow.Load().start("getParam")
				return 1
			}),
		}
	})
	fakeNow.Store(fake)
	return newHandle(0x1234, &fakeProcs)
}

// waitShut waits until Close has started, i.e. the gate refuses new calls.
func waitShut(t *testing.T, h *Handle) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for h.gate.state.Load()&gateShut == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Close never started")
		}
		time.Sleep(time.Millisecond)
	}
}

var testPkt = []byte{0x45, 0, 0, 20}

// A Send already in the driver finishes before Close releases the handle.
func TestCloseWaitsForSend(t *testing.T) {
	f := newFakeDLL()
	f.sendGate = make(chan struct{})
	f.entered = make(chan string, 1)
	h := useFake(t, f)

	sent := make(chan error, 1)
	go func() {
		var a Address
		_, err := h.Send(testPkt, &a)
		sent <- err
	}()
	<-f.entered
	closed := make(chan error, 1)
	go func() { closed <- h.Close() }()
	waitShut(t, h)
	if f.released.Load() {
		t.Fatal("handle released while a Send was in the driver")
	}
	close(f.sendGate)
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := []string{"shutdown", "send returned", "close"}
	if got := f.events(); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if n := f.late.Load(); n != 0 {
		t.Fatalf("%d calls started after the release", n)
	}
}

// A Recv blocked on an empty queue doesn't hold Close up: Close shuts down
// receiving, which makes it return, and only then releases the handle.
func TestCloseReleasesBlockedRecv(t *testing.T) {
	f := newFakeDLL()
	f.recvBlock = true
	f.entered = make(chan string, 1)
	h := useFake(t, f)

	received := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		var a Address
		_, err := h.Recv(buf, &a)
		received <- err
	}()
	<-f.entered
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-received:
		if err == nil {
			t.Fatal("Recv on a shut-down, empty handle succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Recv still blocked after Close")
	}
	want := []string{"shutdown", "recv returned", "close"}
	if got := f.events(); !slices.Equal(got, want) {
		t.Fatalf("events = %q, want %q", got, want)
	}
}

// A call stuck in the driver can't keep the handle (and the diversion) alive:
// Close gives up after releaseWait, releases it and says so.
func TestCloseGivesUpOnStuckCall(t *testing.T) {
	old := releaseWait
	releaseWait = 20 * time.Millisecond
	t.Cleanup(func() { releaseWait = old })

	f := newFakeDLL()
	f.sendGate = make(chan struct{})
	f.entered = make(chan string, 1)
	h := useFake(t, f)

	sent := make(chan error, 1)
	go func() {
		var a Address
		_, err := h.Send(testPkt, &a)
		sent <- err
	}()
	<-f.entered
	err := h.Close()
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Close with a stuck Send: %v", err)
	}
	if !f.released.Load() {
		t.Fatal("handle not released")
	}
	close(f.sendGate)
	if err := <-sent; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// After Close every method fails without reaching the driver, and a second
// Close does nothing.
func TestCallsAfterClose(t *testing.T) {
	f := newFakeDLL()
	h := useFake(t, f)
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.events(); !slices.Equal(got, []string{"close"}) {
		t.Fatalf("Close of an idle handle: events = %q, want only close", got)
	}
	buf := make([]byte, 64)
	var a Address
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Recv", func() error { _, err := h.Recv(buf, &a); return err }},
		{"Send", func() error { _, err := h.Send(testPkt, &a); return err }},
		{"Shutdown", h.Shutdown},
		{"ShutdownRecv", h.ShutdownRecv},
		{"SetParam", func() error { return h.SetParam(ParamQueueLength, 1) }},
		{"GetParam", func() error { _, err := h.GetParam(ParamQueueLength); return err }},
		{"Close", h.Close},
	} {
		if err := c.call(); c.name == "Close" {
			if err != nil {
				t.Errorf("second Close: %v", err)
			}
		} else if !errors.Is(err, ErrClosed) {
			t.Errorf("%s after Close: %v, want ErrClosed", c.name, err)
		}
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("%d calls reached the driver after Close", n)
	}
	if n := f.closes.Load(); n != 1 {
		t.Errorf("WinDivertClose called %d times, want 1", n)
	}
}

// Engine goroutines inject and workers receive while the capture closes:
// whatever the timing, no call reaches the driver after the release.
func TestCloseRacingCalls(t *testing.T) {
	for round := range 100 {
		f := newFakeDLL()
		h := useFake(t, f)
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				buf := make([]byte, 64)
				var a Address
				for {
					var err error
					switch i % 4 {
					case 0:
						_, err = h.Send(testPkt, &a)
					case 1:
						_, err = h.Recv(buf, &a)
					case 2:
						err = h.SetParam(ParamQueueTime, 100)
					case 3:
						_, err = h.GetParam(ParamQueueTime)
					}
					if errors.Is(err, ErrClosed) {
						return
					}
				}
			}()
		}
		for f.calls.Load() < 100 {
			time.Sleep(10 * time.Microsecond)
		}
		if err := h.Close(); err != nil {
			t.Fatalf("round %d: Close: %v", round, err)
		}
		wg.Wait()
		if n := f.late.Load(); n != 0 {
			t.Fatalf("round %d: %d calls reached the driver after the release", round, n)
		}
	}
}

// The gate costs no allocation on the per-packet path.
func TestHandleCallsDoNotAllocate(t *testing.T) {
	h := useFake(t, newFakeDLL())
	buf := make([]byte, 64)
	var a Address
	if n := testing.AllocsPerRun(1000, func() { h.Send(testPkt, &a) }); n != 0 {
		t.Errorf("Send: %.1f allocations per call", n)
	}
	if n := testing.AllocsPerRun(1000, func() { h.Recv(buf, &a) }); n != 0 {
		t.Errorf("Recv: %.1f allocations per call", n)
	}
}

// The same against the real driver, as administrator: a Recv blocked on an
// empty queue returns ErrClosed whether Close's own WinDivertShutdown or the
// caller's ShutdownRecv releases it, and Close doesn't need releaseWait.
func TestCloseRealHandle(t *testing.T) {
	if !IsAdmin() {
		t.Skip("needs administrator rights to open a WinDivert handle")
	}
	loadForTest(t)
	for _, callerShutdown := range []bool{false, true} {
		h, err := Open("false", LayerNetwork, 0, FlagSniff|FlagRecvOnly)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		received := make(chan error, 1)
		go func() {
			buf := make([]byte, MTUMax)
			var a Address
			_, err := h.Recv(buf, &a)
			received <- err
		}()
		for h.gate.state.Load() == 0 { // the receiver is on its way into the driver
			time.Sleep(time.Millisecond)
		}
		time.Sleep(50 * time.Millisecond)
		if _, err := h.GetParam(ParamQueueLength); err != nil {
			t.Fatalf("GetParam: %v", err)
		}
		if callerShutdown {
			if err := h.ShutdownRecv(); err != nil {
				t.Fatalf("ShutdownRecv: %v", err)
			}
		}
		start := time.Now()
		if err := h.Close(); err != nil {
			t.Fatalf("Close (caller shutdown %v): %v", callerShutdown, err)
		}
		t.Logf("Close with a blocked Recv (caller shutdown %v): %v", callerShutdown, time.Since(start))
		select {
		case err := <-received:
			if !errors.Is(err, ErrClosed) {
				t.Errorf("blocked Recv: %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Recv still blocked after Close")
		}
		if _, err := h.GetParam(ParamQueueLength); !errors.Is(err, ErrClosed) {
			t.Errorf("GetParam after Close: %v, want ErrClosed", err)
		}
	}
}
