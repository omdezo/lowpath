package engine

import (
	"context"
	"encoding/binary"
	"time"

	"lowpath/internal/icmp"
)

// The "before" number is the route the game would take without Lowpath.
//
// Proxy mode in the demo sends UDP echo probes along the Direct route; only
// the bundled simulator answers those. Real game servers (capture mode, and
// proxy mode with Config.PingBefore) are pinged over ICMP instead: the primary
// server, or Target. Many servers ignore ping; then there is simply no before
// number (Snapshot.BeforeKnown is false).

var directMagic = [4]byte{'L', 'P', 'B', 'P'}

func directProbe(seq uint32) [8]byte {
	var b [8]byte
	copy(b[:], directMagic[:])
	binary.BigEndian.PutUint32(b[4:], seq)
	return b
}

// parseDirectProbe returns the sequence number of an echoed direct probe.
func parseDirectProbe(b []byte) (uint32, bool) {
	if len(b) != 8 || [4]byte(b[:4]) != directMagic {
		return 0, false
	}
	return binary.BigEndian.Uint32(b[4:]), true
}

// pingsBefore reports whether "before" is ICMP to a real game server rather
// than UDP echo along Direct.
func (e *Engine) pingsBefore() bool { return e.capture || e.cfg.PingBefore }

// beforeWindow is how many baseline samples make up a live snapshot: about
// five seconds, like the path stats.
func (e *Engine) beforeWindow() int {
	if e.pingsBefore() {
		return int(5 * time.Second / pingInterval)
	}
	return statsWindow
}

func (e *Engine) beforeLoop(ctx context.Context) {
	every := probeInterval
	if e.pingsBefore() {
		every = pingInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if e.pingsBefore() {
			e.pingServer()
		} else {
			e.probeDirect()
		}
	}
}

func (e *Engine) probeDirect() {
	b := directProbe(e.before.send(time.Now()))
	e.direct.Write(b[:])
}

func (e *Engine) readDirectProbes() {
	buf := make([]byte, 2048)
	for {
		n, err := e.direct.Read(buf)
		if err != nil {
			if isClosed(err) {
				return
			}
			continue
		}
		if seq, ok := parseDirectProbe(buf[:n]); ok {
			e.before.recv(seq, time.Now())
		}
	}
}

// pingServer sends one ICMP echo to the game server: the primary one in
// capture mode, Target in proxy mode. Each ping runs in its own goroutine so
// a server that never answers (1 s timeout) can't stretch the 5 Hz schedule.
// Stop doesn't wait for them: each ends by its own timeout and only touches
// the tracker.
func (e *Engine) pingServer() {
	srv := e.probeAddr()
	if !srv.IsValid() {
		return // the game hasn't talked to any server yet
	}
	dst := srv.Addr()
	seq := e.before.send(time.Now())
	go func() {
		if rtt, err := icmp.Ping(dst, probeTimeout); err == nil {
			e.before.recvRTT(seq, rtt)
		}
	}()
}
