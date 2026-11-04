package engine

import (
	"testing"
	"time"

	"lowpath/internal/proto"
)

// The relay's report on its leg to the server goes stale as a whole, and is
// dropped when the game moves to another server.
func TestServerLegReportStaleness(t *testing.T) {
	e := &Engine{flows: newFlowTable()}
	now := time.Now()
	if l := e.serverLeg(now); l != (proto.ServerLeg{}) {
		t.Fatalf("before any reply: %+v", l)
	}
	rep := proto.ServerLeg{RTT: 30 * time.Millisecond, LossPct: 4, Jitter: 2 * time.Millisecond, HasRTT: true, HasLoss: true, HasJitter: true}
	e.setServerLeg(rep, now)
	if l := e.serverLeg(now.Add(legStale)); l != rep {
		t.Fatalf("fresh report: %+v, want %+v", l, rep)
	}
	if l := e.serverLeg(now.Add(legStale + time.Millisecond)); l != (proto.ServerLeg{}) {
		t.Fatalf("stale report still known: %+v", l)
	}

	// A later reply that leaves loss and jitter out (an older relay on
	// another path) doesn't inherit them from the earlier one.
	e.setServerLeg(proto.ServerLeg{RTT: 31 * time.Millisecond, HasRTT: true}, now)
	if l := e.serverLeg(now); !l.HasRTT || l.HasLoss || l.HasJitter {
		t.Fatalf("RTT-only reply: %+v", l)
	}

	e.setServerLeg(rep, now)
	e.primaryChanged()
	if l := e.serverLeg(now); l != (proto.ServerLeg{}) {
		t.Fatalf("after the primary server changed: %+v", l)
	}
}
