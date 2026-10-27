package rebalance

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/eskhakha/vecdb/internal/core"
)

// drive runs the move forward, issuing writesPerTick client writes each tick,
// and asserts the safety invariants after EVERY step.
//
// Checking at the end only tells you that a run failed. Checking every step
// tells you which one.
func drive(t *testing.T, m *Move, ticks, writesPerTick int) (acked, refused int) {
	t.Helper()
	for i := 0; i < ticks; i++ {
		for w := 0; w < writesPerTick; w++ {
			if _, err := m.Write(fmt.Sprintf("t%d-%d", i, w)); err != nil {
				refused++
			} else {
				acked++
			}
		}
		m.Step()
		if err := m.Check(); err != nil {
			t.Fatalf("tick %d (phase %s): %v", i, m.Phase, err)
		}
		if m.Phase == PhaseDone || m.Phase == PhaseAborted {
			return
		}
	}
	return
}

// TestMoveUnderLoadLosesNothing is the happy path, and the liveness floors are
// as important as the safety assertion: a move that never got past snapshot
// would satisfy "no data lost" vacuously.
func TestMoveUnderLoadLosesNothing(t *testing.T) {
	m := NewMove(0, "node-1", "node-3", 20)
	m.ShipBatch = 4      // must exceed the write rate or the gap never closes
	m.SnapshotTicks = 12 // a real bulk copy takes time, and writes keep arriving
	acked, refused := drive(t, m, 200, 2)

	if m.Phase != PhaseDone {
		t.Fatalf("move ended in phase %s (%s), want done", m.Phase, m.Reason)
	}
	if lost := m.Lost(); len(lost) != 0 {
		t.Fatalf("%d acknowledged writes lost: %v", len(lost), lost[:1])
	}
	// Liveness floors.
	if total := len(m.Acked()); total < 50 {
		t.Fatalf("only %d writes were acknowledged in total (%d during the move); "+
			"the test proved nothing", total, acked)
	}
	if refused == 0 {
		t.Fatal("no write was ever refused, so the write pause never happened")
	}
	if m.Epoch != 2 {
		t.Fatalf("epoch is %d after one cutover, want 2", m.Epoch)
	}
	if !m.Src.dropped {
		t.Fatal("cleanup never ran; the source still holds a stale copy")
	}
}

// TestNaiveCutoverLosesAcknowledgedWrites pins the BROKEN behaviour on purpose.
//
// This is the reason the pause phase exists. Without a test that shows the loss,
// the pause is a step someone will optimise away as "an unnecessary outage".
func TestNaiveCutoverLosesAcknowledgedWrites(t *testing.T) {
	m := NewMove(0, "node-1", "node-3", 20)
	m.UnsafeCutover = true
	m.ShipBatch = 1

	for i := 0; i < 60; i++ {
		_, _ = m.Write(fmt.Sprintf("t%d", i))
		m.Step()
		if m.Phase == PhaseDone {
			break
		}
	}
	if m.Phase != PhaseDone {
		t.Fatalf("phase %s, want done", m.Phase)
	}
	lost := m.Lost()
	if len(lost) == 0 {
		t.Fatal("cutting over before the gap closed lost nothing; " +
			"the simulation is not modelling the failure it is supposed to")
	}
	t.Logf("naive cutover lost %d acknowledged writes (first: LSN %d)", len(lost), lost[0].LSN)
}

// TestAbortAtEveryPhaseKeepsTheSourceAuthoritative: a move is optional, so
// failing it must always be free. Up to the cutover, the source is untouched.
func TestAbortAtEveryPhaseKeepsTheSourceAuthoritative(t *testing.T) {
	for _, target := range []Phase{PhasePlanned, PhaseSnapshot, PhaseCatchUp, PhasePause} {
		t.Run(target.String(), func(t *testing.T) {
			m := NewMove(1, "node-1", "node-3", 15)
			m.ShipBatch = 4

			// Advance to the target phase with writes flowing.
			for i := 0; i < 100 && m.Phase != target; i++ {
				_, _ = m.Write(fmt.Sprintf("pre%d", i))
				m.Step()
				if err := m.Check(); err != nil {
					t.Fatal(err)
				}
			}
			if m.Phase != target {
				t.Fatalf("never reached %s (stuck in %s)", target, m.Phase)
			}

			m.Dst.Down = true
			m.Step()
			if err := m.Check(); err != nil {
				t.Fatal(err)
			}

			if m.Phase != PhaseAborted {
				t.Fatalf("destination died in %s but the move is in %s", target, m.Phase)
			}
			if m.Writer != m.Src.ID {
				t.Fatalf("writer is %q after abort, want the source back", m.Writer)
			}
			if lost := m.Lost(); len(lost) != 0 {
				t.Fatalf("abort lost %d acknowledged writes", len(lost))
			}
			// Writes must resume immediately: an abort is not an outage.
			if _, err := m.Write("after-abort"); err != nil {
				t.Fatalf("source cannot accept writes after an abort: %v", err)
			}
		})
	}
}

// TestSlowDestinationNeverCutsOver: if the destination cannot apply faster than
// the source accepts, the gap never closes. The move must give up rather than
// hold the shard in a pause that never ends -- an unbounded pause is an outage
// with extra steps.
func TestSlowDestinationNeverCutsOver(t *testing.T) {
	m := NewMove(2, "node-1", "node-3", 10)
	m.ShipBatch = 1
	m.LagThreshold = 3
	m.PauseBudget = 4

	// Three writes per tick against a destination that applies one.
	acked, _ := drive(t, m, 300, 3)

	if m.Phase == PhaseDone {
		t.Fatal("a destination that never caught up was nevertheless promoted")
	}
	if m.Phase != PhaseAborted {
		t.Fatalf("phase %s; a move that cannot finish must abort, not hang", m.Phase)
	}
	if acked < 30 {
		t.Fatalf("only %d writes acknowledged; the shard was effectively down", acked)
	}
	if lost := m.Lost(); len(lost) != 0 {
		t.Fatalf("a failed move lost %d acknowledged writes", len(lost))
	}
}

// TestSourceIsFencedAfterCutover. The source may never learn it lost the shard
// -- a long GC pause, a partition, a stopped VM. Safety cannot depend on it
// finding out. The receiver refuses its stale-epoch records.
func TestSourceIsFencedAfterCutover(t *testing.T) {
	m := NewMove(3, "node-1", "node-3", 10)
	m.ShipBatch = 4
	drive(t, m, 200, 1)
	if m.Phase != PhaseDone {
		t.Fatalf("phase %s, want done", m.Phase)
	}

	// The old owner wakes up and replays a write at the epoch it remembers.
	stale := Rec{LSN: m.Dst.Applied() + 1, ID: "zombie"}
	err := m.Dst.accept(stale, m.Epoch-1)
	if !errors.Is(err, core.ErrStaleEpoch) {
		t.Fatalf("destination accepted a stale-epoch write from the old owner: %v", err)
	}
}

// TestCleanupRunsOnlyAfterAuthorityMoves. The source copy is the fallback for
// every failure before the cutover, so dropping it early converts a recoverable
// failure into data loss.
func TestCleanupRunsOnlyAfterAuthorityMoves(t *testing.T) {
	m := NewMove(4, "node-1", "node-3", 10)
	m.ShipBatch = 4
	for i := 0; i < 200; i++ {
		_, _ = m.Write(fmt.Sprintf("w%d", i))
		before := m.Phase
		m.Step()
		if m.Src.dropped && !before.AuthorityMoved() {
			t.Fatalf("source dropped its copy in phase %s, before authority moved", before)
		}
		if m.Phase == PhaseDone {
			return
		}
	}
	t.Fatal("move never completed")
}

// TestRandomizedMoves runs many seeds with faults injected at random ticks and
// asserts the invariants after every single step.
//
// The liveness floors matter more than the safety assertion here: a run where
// every seed aborted at tick 1 would pass every safety check while proving
// nothing at all.
func TestRandomizedMoves(t *testing.T) {
	var done, aborted, totalAcked int

	for seed := 0; seed < 300; seed++ {
		rng := rand.New(rand.NewSource(int64(seed)))
		m := NewMove(core.ShardID(seed%4), "node-1", "node-3", 5+rng.Intn(20))
		m.SnapshotTicks = 1 + rng.Intn(4)
		m.ShipBatch = 2 + rng.Intn(4)
		m.LagThreshold = rng.Intn(4)
		m.PauseBudget = 1 + rng.Intn(6)
		writes := rng.Intn(3)

		for tick := 0; tick < 200; tick++ {
			for w := 0; w < writes; w++ {
				if _, err := m.Write(fmt.Sprintf("s%d-t%d-%d", seed, tick, w)); err == nil {
					totalAcked++
				}
			}
			// Faults: the destination flaps, and occasionally the source dies.
			if !m.Phase.AuthorityMoved() && rng.Intn(100) < 3 {
				m.Dst.Down = true
			} else if m.Dst.Down && rng.Intn(100) < 40 {
				m.Dst.Down = false
			}
			if !m.Phase.AuthorityMoved() && rng.Intn(1000) < 5 {
				m.Src.Down = true
			}

			m.Step()
			if err := m.Check(); err != nil {
				t.Fatalf("seed %d tick %d phase %s: %v", seed, tick, m.Phase, err)
			}
			if m.Phase == PhaseDone || m.Phase == PhaseAborted {
				break
			}
		}
		switch m.Phase {
		case PhaseDone:
			done++
			if lost := m.Lost(); len(lost) != 0 {
				t.Fatalf("seed %d: completed move lost %d acknowledged writes", seed, len(lost))
			}
		case PhaseAborted:
			aborted++
		}
	}

	t.Logf("300 seeds: %d completed, %d aborted, %d writes acknowledged", done, aborted, totalAcked)
	if done < 50 {
		t.Fatalf("only %d moves completed; the faults are so aggressive the test is vacuous", done)
	}
	if aborted < 20 {
		t.Fatalf("only %d moves aborted; the abort paths were barely exercised", aborted)
	}
	if totalAcked < 1000 {
		t.Fatalf("only %d writes acknowledged across 300 seeds", totalAcked)
	}
}
