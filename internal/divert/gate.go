package divert

import (
	"sync"
	"sync/atomic"
	"time"
)

// callGate keeps a handle from being released while a call on it is still
// running. A closed flag alone is not enough: a call can check the flag, Close
// can release the handle, and only then does the call reach the driver with a
// HANDLE value Windows may already have given to another object. WinDivert's
// DLL even uses the handle twice per call, once to start the I/O and again in
// GetOverlappedResult when it is pending (Recv).
//
// Every call goes through enter and exit; Close goes through shut and wait.
// The zero value is not usable: create one with newCallGate.
type callGate struct {
	state   atomic.Int64  // gateShut | number of calls between enter and exit
	drained chan struct{} // closed once the gate is shut and no call is left
	once    sync.Once
}

const gateShut = 1 << 62 // far above any number of goroutines

func newCallGate() *callGate { return &callGate{drained: make(chan struct{})} }

// enter registers a call. It returns false, and registers nothing, once the
// gate is shut. A true result must be paired with exit.
func (g *callGate) enter() bool {
	if g.state.Add(1)&gateShut != 0 {
		g.exit()
		return false
	}
	return true
}

func (g *callGate) exit() {
	if g.state.Add(-1) == gateShut {
		g.once.Do(g.signal)
	}
}

func (g *callGate) signal() { close(g.drained) }

// shut makes every later enter fail. It reports how many calls were in
// flight at that moment and false when the gate was already shut.
func (g *callGate) shut() (inFlight int64, first bool) {
	old := g.state.Or(gateShut)
	return old &^ gateShut, old&gateShut == 0
}

// wait waits up to d, after shut, for the calls in flight to exit. It returns
// how many are still running when it gives up (0 when all of them left).
func (g *callGate) wait(d time.Duration) int64 {
	if g.state.Load() == gateShut {
		return 0
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-g.drained:
		return 0
	case <-t.C:
		return g.state.Load() &^ gateShut
	}
}
