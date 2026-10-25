// Package dedup implements the first-copy-wins sliding window used by egress
// relays (upstream) and the client engine (downstream) to drop the duplicate
// copies that multipath sending produces.
package dedup

// WindowSize is how many sequence numbers behind the newest one we still
// accept. Anything older is considered too late to be useful to a game.
const WindowSize = 1024

const words = WindowSize / 64

// RestartGap is how far behind the newest sequence number a packet must be to
// mean the sender started over rather than arrived late. Real stragglers are
// at most a few hundred packets behind; a jump this far back is a relay that
// restarted, or a failover to another egress whose numbering is unrelated.
// Rejecting those as "too old" would drop every packet of the new sender
// until its numbers caught up with the old ones, which can take minutes.
const RestartGap = 4 * WindowSize

// Window is not safe for concurrent use; callers hold their own lock.
type Window struct {
	started bool
	top     uint32 // highest sequence number seen
	bits    [words]uint64
}

// Accept reports whether seq is the first copy we've seen and should be
// delivered. It is O(1) except when the window jumps forward.
func (w *Window) Accept(seq uint32) bool {
	if !w.started {
		w.restart(seq)
		return true
	}
	// Serial-number arithmetic, so the window survives wraparound.
	if diff := int32(seq - w.top); diff > 0 {
		w.advance(uint32(diff), seq)
		return true
	}
	switch back := w.top - seq; {
	case back >= RestartGap:
		w.restart(seq)
		return true
	case back >= WindowSize:
		return false // too old
	case w.has(seq):
		return false
	default:
		w.set(seq)
		return true
	}
}

// restart forgets everything and starts the window at seq.
func (w *Window) restart(seq uint32) {
	w.started = true
	w.bits = [words]uint64{}
	w.top = seq
	w.set(seq)
}

func (w *Window) advance(by uint32, seq uint32) {
	if by >= WindowSize {
		w.bits = [words]uint64{}
	} else {
		for s := w.top + 1; s != seq; s++ {
			w.clear(s)
		}
		w.clear(seq)
	}
	w.top = seq
	w.set(seq)
}

func (w *Window) idx(seq uint32) (int, uint64) {
	i := seq % WindowSize
	return int(i / 64), 1 << (i % 64)
}

func (w *Window) has(seq uint32) bool {
	i, m := w.idx(seq)
	return w.bits[i]&m != 0
}

func (w *Window) set(seq uint32) {
	i, m := w.idx(seq)
	w.bits[i] |= m
}

func (w *Window) clear(seq uint32) {
	i, m := w.idx(seq)
	w.bits[i] &^= m
}
