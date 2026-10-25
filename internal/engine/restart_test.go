package engine_test

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"lowpath/internal/engine"
	"lowpath/internal/gamesim"
	"lowpath/internal/relay"
)

// startRelay serves an open egress relay on addr until the returned stop is
// called. Binding a port that was just released can fail for a moment, so it
// retries briefly.
func startRelay(t *testing.T, addr string) (*relay.Egress, func()) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var eg *relay.Egress
	var err error
	for end := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if eg, err = relay.ListenEgress(addr, log); err == nil || time.Now().After(end) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { eg.Serve(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return eg, stop
}

// A restarted egress relay (deploy, crash, systemd restart) numbers its
// replies afresh. The client still holds the old numbers in its dedup window
// and must not drop the new ones as stale duplicates: probes keep the path
// "healthy", so nothing would fall back and the game would just time out.
func TestRepliesSurviveEgressRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	game, err := gamesim.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go game.Serve(ctx)
	eg, stop := startRelay(t, "127.0.0.1:0")
	addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), eg.Addr().Port())
	e, s := startCapture(t, engine.Config{Paths: []engine.PathSpec{{ID: "r", Label: "r", Addr: addr}}})

	next := uint32(0)
	sendBatch := func(n int) {
		pkt := make([]byte, 8)
		for i := range n {
			next++
			binary.BigEndian.PutUint32(pkt, next)
			e.Send(gameLocal, game.Addr(), pkt)
			if i%20 == 19 {
				time.Sleep(2 * time.Millisecond) // don't overrun loopback socket buffers
			}
		}
	}
	// Well past the dedup window, so "too old" would apply after a restart.
	const before = 1500
	sendBatch(before)
	if !waitFor(t, 3*time.Second, func() bool { return len(s.all()) >= before*99/100 }) {
		t.Fatalf("%d/%d replies before the restart", len(s.all()), before)
	}

	stop()
	startRelay(t, addr.String())
	if !waitFor(t, 3*time.Second, func() bool { return !e.Snapshot().Fallback }) {
		t.Fatal("engine stuck in fallback after the relay came back")
	}
	got := len(s.all())
	const after = 100
	sendBatch(after)
	if !waitFor(t, 3*time.Second, func() bool { return len(s.all())-got >= after*95/100 }) {
		t.Fatalf("%d/%d replies after the relay restarted (%d dropped as duplicates)",
			len(s.all())-got, after, e.Snapshot().Dups)
	}
}
