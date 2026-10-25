package dedup

import (
	"math/rand"
	"testing"
)

func TestDuplicatesDropped(t *testing.T) {
	var w Window
	for seq := uint32(1); seq <= 5000; seq++ {
		if !w.Accept(seq) {
			t.Fatalf("first copy of %d rejected", seq)
		}
		if w.Accept(seq) {
			t.Fatalf("second copy of %d accepted", seq)
		}
	}
}

func TestReorderWithinWindow(t *testing.T) {
	var w Window
	w.Accept(100)
	if !w.Accept(90) {
		t.Fatal("late but in-window packet rejected")
	}
	if w.Accept(90) {
		t.Fatal("duplicate of reordered packet accepted")
	}
	if !w.Accept(101) {
		t.Fatal("next packet rejected")
	}
}

func TestTooOldRejected(t *testing.T) {
	var w Window
	w.Accept(5000)
	if w.Accept(5000 - WindowSize) {
		t.Fatal("packet older than window accepted")
	}
}

// A sender that starts over (relay restart, egress failover) numbers from
// somewhere unrelated; its packets must flow at once, not after its numbers
// catch up with the old sender's.
func TestSenderRestartStartsOver(t *testing.T) {
	var w Window
	for seq := uint32(1); seq <= 38000; seq++ {
		w.Accept(seq)
	}
	for seq := uint32(1); seq <= 100; seq++ {
		if !w.Accept(seq) {
			t.Fatalf("seq %d of the restarted sender rejected", seq)
		}
		if w.Accept(seq) {
			t.Fatalf("duplicate %d of the restarted sender accepted", seq)
		}
	}
	// Late but within the window of the new sender still dedups normally.
	if w.Accept(50) {
		t.Fatal("duplicate accepted after the restart")
	}
}

func TestRestartGapBoundary(t *testing.T) {
	var w Window
	w.Accept(100000)
	if w.Accept(100000 - RestartGap + 1) {
		t.Fatal("packet just inside the restart gap accepted as a restart")
	}
	if !w.Accept(100000 - RestartGap) {
		t.Fatal("packet a full restart gap behind not treated as a restart")
	}
	// Half the number space away is the most ambiguous case; it must not
	// wedge the window either way.
	var h Window
	h.Accept(0)
	if !h.Accept(1<<31) || !h.Accept(1<<31+1) {
		t.Fatal("window stuck after a jump of half the sequence space")
	}
}

func TestBigJumpClearsWindow(t *testing.T) {
	var w Window
	w.Accept(10)
	w.Accept(10 + 3*WindowSize)
	// 10+WindowSize*2 is within the new window and was never seen.
	if !w.Accept(10 + 2*WindowSize + 5) {
		t.Fatal("unseen in-window seq rejected after jump")
	}
}

func TestWraparound(t *testing.T) {
	var w Window
	seq := uint32(0xFFFFFFF0)
	for i := 0; i < 64; i++ {
		if !w.Accept(seq) {
			t.Fatalf("seq %#x rejected across wrap", seq)
		}
		if w.Accept(seq) {
			t.Fatalf("dup %#x accepted across wrap", seq)
		}
		seq++
	}
}

// Simulates 3 paths delivering shuffled copies; every seq must be delivered
// exactly once.
func TestMultipathExactlyOnce(t *testing.T) {
	var w Window
	rng := rand.New(rand.NewSource(1))
	const n = 20000
	type pkt struct{ seq uint32 }
	var stream []pkt
	for s := uint32(1); s <= n; s++ {
		for c := 0; c < 3; c++ {
			stream = append(stream, pkt{s})
		}
	}
	// Local shuffles only (bounded reordering, like real paths).
	for i := range stream {
		j := i + rng.Intn(40)
		if j < len(stream) {
			stream[i], stream[j] = stream[j], stream[i]
		}
	}
	got := make(map[uint32]int)
	for _, p := range stream {
		if w.Accept(p.seq) {
			got[p.seq]++
		}
	}
	if len(got) != n {
		t.Fatalf("delivered %d distinct seqs, want %d", len(got), n)
	}
	for s, c := range got {
		if c != 1 {
			t.Fatalf("seq %d delivered %d times", s, c)
		}
	}
}

func BenchmarkAccept(b *testing.B) {
	var w Window
	for i := 0; i < b.N; i++ {
		s := uint32(i)
		w.Accept(s)
		w.Accept(s)
	}
}
