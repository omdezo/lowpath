package engine_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"lowpath/internal/engine"
	"lowpath/internal/relay"
)

// A real egress relay whose pings to the game server lose every fifth reply
// and whose replies swing by 4 ms: the client's snapshot shows that leg's
// loss and jitter next to its RTT.
func TestServerLegLossReachesSnapshot(t *testing.T) {
	var calls, replies atomic.Int64
	ping := func(netip.Addr, time.Duration) (time.Duration, error) {
		if calls.Add(1)%5 == 0 {
			return 0, errors.New("no reply")
		}
		if replies.Add(1)%2 == 0 {
			return 24 * time.Millisecond, nil
		}
		return 20 * time.Millisecond, nil
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eg, err := relay.ListenEgressOpts("127.0.0.1:0", relay.Options{AllowPrivate: true, Ping: ping, PingInterval: 2 * time.Millisecond, StatsInterval: -1}, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go eg.Serve(ctx)
	addr := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), eg.Addr().Port())
	e, _ := startCapture(t, engine.Config{Paths: []engine.PathSpec{{ID: "r", Label: "r", Addr: addr}}})

	// The game's first packet makes the server primary; probes name it.
	e.Send(gameLocal, netip.MustParseAddrPort("127.0.0.1:27015"), []byte("hi"))
	var snap engine.Snapshot
	// The RFC 3550 estimate climbs towards the 4 ms swings; wait until it
	// has settled rather than for a fixed time.
	if !waitFor(t, 5*time.Second, func() bool {
		snap = e.Snapshot()
		return snap.ServerLegLossKnown && snap.ServerLegJitterKnown && snap.ServerLegJitter > 3.5
	}) {
		t.Fatalf("server leg loss %v/%v jitter %v/%v after %d pings",
			snap.ServerLegLoss, snap.ServerLegLossKnown, snap.ServerLegJitter, snap.ServerLegJitterKnown, calls.Load())
	}
	if !snap.ServerLegKnown || snap.ServerLeg < 20 || snap.ServerLeg > 24 {
		t.Errorf("server leg %v ms known=%v, want between the 20 and 24 ms replies", snap.ServerLeg, snap.ServerLegKnown)
	}
	// One in five lost: 12 of a full window of 60, a little less while the
	// window still fills.
	if snap.ServerLegLoss < 16 || snap.ServerLegLoss > 20 {
		t.Errorf("server leg loss %.2f%%, want 20%%", snap.ServerLegLoss)
	}
	if snap.ServerLegJitter > 4 {
		t.Errorf("server leg jitter %.2f ms, more than the 4 ms swings", snap.ServerLegJitter)
	}
}
