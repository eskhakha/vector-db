// Package rebalance is a SPIKE, not a production component.
//
// It is a pure, deterministic simulation of moving one shard replica from a
// source node to a destination node while the shard keeps serving writes. There
// is no disk, no network, no goroutine: a Move is a state machine that Step()
// advances one tick at a time, so a bug is "seed 7, tick 41" rather than a
// flaky integration test.
//
// # THE FAILURE THIS EXISTS TO FIX
//
// Capacity changes. A node is added, a node is retired, or one node ends up
// leading three of four shards. Without a way to move a shard, the only options
// are to stop the world and copy, or to never change the cluster. The first is
// unacceptable for a database that claims availability; the second is
// unacceptable for one that claims to scale.
//
// Moving data while it is being written to is the whole difficulty. A naive
// copy is wrong the instant it starts, because the source keeps accepting
// writes behind the copy cursor.
//
// THE SHAPE OF THE ANSWER
//
//		snapshot  ->  catch-up  ->  pause  ->  cutover  ->  cleanup
//
//	  - SNAPSHOT bounds the problem: copy everything up to LSN S while writes
//	    continue. This is the long, expensive phase, and it is deliberately done
//	    with no pause at all, because it is the part that takes minutes.
//	  - CATCH-UP shrinks the remaining gap: ship S+1.. as fast as the destination
//	    can take them. The gap shrinks only if the destination applies faster than
//	    the source accepts; if it does not, the move must never cut over.
//	  - PAUSE closes the gap to exactly zero. This is the only moment of
//	    unavailability, and it is short precisely because catch-up did the work.
//	    Writes are refused, not queued and silently dropped.
//	  - CUTOVER flips authority at a NEW EPOCH. The epoch bump is what makes the
//	    old owner harmless if it wakes up believing it still owns the shard.
//	  - CLEANUP drops the source copy. It happens LAST, and only after the new
//	    placement is durable, because the source copy is the only fallback for
//	    every failure before this point.
//
// # WHY EACH ORDERING IS LOAD-BEARING
//
// Cutting over before the gap is zero loses acknowledged writes.
// TestNaiveCutoverLosesAcknowledgedWrites asserts exactly that, on purpose: the
// broken behaviour is the lesson, and the correct path is only meaningful
// against it.
//
// Cleaning up before cutover is durable turns any later failure into data loss,
// because there is nothing to fall back to.
package rebalance

import (
	"fmt"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
)

// Rec is one logged write. Payload is irrelevant to the mechanism, so the
// simulation carries only what the invariants need.
type Rec struct {
	LSN core.LSN
	ID  string
}

// Replica is one node's copy of a shard, modelled as a contiguous log.
//
// Contiguous is not a simplification: a gapless log is what makes "applied up
// to L" a single number instead of a set, and every catch-up protocol in this
// family depends on that.
type Replica struct {
	ID   cluster.NodeID
	log  []Rec
	Down bool
	// epoch is the highest leadership epoch this replica has seen. It refuses
	// anything stamped below it, which is what makes a deposed owner harmless.
	epoch core.Epoch
	// dropped records that cleanup has run. A dropped replica is not merely
	// empty; it must never be treated as a candidate copy again.
	dropped bool
}

// Applied returns the highest LSN this replica holds.
func (r *Replica) Applied() core.LSN {
	if len(r.log) == 0 {
		return 0
	}
	return r.log[len(r.log)-1].LSN
}

// Has reports whether the replica holds a record.
func (r *Replica) Has(lsn core.LSN) bool {
	for _, e := range r.log {
		if e.LSN == lsn {
			return true
		}
	}
	return false
}

// accept appends one record if it is the next one and the epoch is not stale.
//
// The receiver enforces both rules. Safety that depends on the sender behaving
// well is not safety: a paused process wakes up still believing it is the
// owner, and the only thing that saves you is the receiver saying no.
func (r *Replica) accept(e Rec, epoch core.Epoch) error {
	if r.Down {
		return fmt.Errorf("rebalance: %s is down", r.ID)
	}
	if epoch < r.epoch {
		return fmt.Errorf("%w: %s is at epoch %d, record stamped %d",
			core.ErrStaleEpoch, r.ID, r.epoch, epoch)
	}
	if e.LSN != r.Applied()+1 {
		return fmt.Errorf("%w: %s has %d, got %d", core.ErrLSNGap, r.ID, r.Applied(), e.LSN)
	}
	r.log = append(r.log, e)
	return nil
}

// Phase is where a move has got to.
type Phase int

const (
	// PhasePlanned: decided, nothing transferred.
	PhasePlanned Phase = iota
	// PhaseSnapshot: bulk copy up to a fixed LSN, writes still flowing.
	PhaseSnapshot
	// PhaseCatchUp: streaming the tail, writes still flowing.
	PhaseCatchUp
	// PhasePause: writes refused, closing the last records.
	PhasePause
	// PhaseCutover: authority flips at a new epoch.
	PhaseCutover
	// PhaseCleanup: source copy dropped.
	PhaseCleanup
	// PhaseDone: finished.
	PhaseDone
	// PhaseAborted: rolled back; the source is still the owner.
	PhaseAborted
)

// AuthorityMoved reports whether the cutover has committed.
//
// PhaseAborted is numerically the largest constant, so a raw `phase >= cutover`
// comparison silently classifies an abort as a completed cutover. Every
// ordering question therefore goes through these two helpers instead.
func (p Phase) AuthorityMoved() bool {
	return p == PhaseCutover || p == PhaseCleanup || p == PhaseDone
}

// Live reports whether the move is still in progress.
func (p Phase) Live() bool { return p != PhaseDone && p != PhaseAborted }

func (p Phase) String() string {
	return [...]string{"planned", "snapshot", "catch-up", "pause",
		"cutover", "cleanup", "done", "aborted"}[p]
}

// Move is the state machine for relocating one shard.
type Move struct {
	Shard core.ShardID
	Src   *Replica
	Dst   *Replica

	Phase Phase
	// Epoch is the shard's current placement epoch, monotonically increasing.
	Epoch core.Epoch
	// Writer is who may accept writes right now. Empty means nobody: either a
	// deliberate pause, or an abort with no healthy owner.
	Writer cluster.NodeID
	// Reason explains an abort, so the outcome is visible rather than inferred.
	Reason string

	// --- knobs ---

	// SnapshotTicks models how long the bulk copy takes.
	SnapshotTicks int
	// ShipBatch is how many records the destination can apply per tick. If this
	// is not greater than the incoming write rate, the gap never closes and the
	// move must never cut over.
	ShipBatch int
	// LagThreshold is how close the destination must get before the write pause
	// is worth taking. Too low and the pause never starts; too high and the
	// pause is long.
	LagThreshold int
	// PauseBudget bounds the outage. Exceeding it aborts rather than extending
	// the pause, because an unbounded pause is an outage with extra steps.
	PauseBudget int
	// StallBudget is how many catch-up ticks may pass with no improvement in the
	// gap before the move is abandoned. A deadline would be wrong -- a large
	// shard legitimately takes a long time -- so what is measured is PROGRESS,
	// not elapsed time.
	StallBudget int
	// UnsafeCutover skips the requirement that the gap reach zero. It exists so
	// a test can demonstrate the data loss that the pause phase prevents.
	UnsafeCutover bool

	nextLSN    core.LSN
	snapAt     core.LSN
	snapTicks  int
	pauseTicks int
	bestLag    int
	stallTicks int
	// acked is every write the client was told succeeded. The central
	// invariant is that a completed move loses none of these.
	acked []Rec
}

// NewMove sets up a move of shard sh from src to dst with the source already
// holding n records.
func NewMove(sh core.ShardID, src, dst cluster.NodeID, n int) *Move {
	m := &Move{
		Shard:         sh,
		Src:           &Replica{ID: src, epoch: 1},
		Dst:           &Replica{ID: dst, epoch: 1},
		Epoch:         1,
		Writer:        src,
		SnapshotTicks: 3,
		ShipBatch:     4,
		LagThreshold:  2,
		PauseBudget:   5,
		StallBudget:   20,
		nextLSN:       1,
	}
	for i := 0; i < n; i++ {
		if _, err := m.Write(fmt.Sprintf("v%d", i)); err != nil {
			panic(err)
		}
	}
	return m
}

// Write appends a record on whoever currently holds write authority.
//
// A refusal is a successful outcome, not a failure of the design: during the
// pause the honest answer to a client is "not now", and an honest refusal is
// always preferable to an acknowledgement the system cannot keep.
func (m *Move) Write(id string) (core.LSN, error) {
	if m.Writer == "" {
		return 0, core.ErrShardReadOnly
	}
	w := m.Src
	if m.Writer == m.Dst.ID {
		w = m.Dst
	}
	if w.Down {
		return 0, fmt.Errorf("rebalance: %s is down", w.ID)
	}
	e := Rec{LSN: m.nextLSN, ID: id}
	if err := w.accept(e, m.Epoch); err != nil {
		return 0, err
	}
	m.nextLSN++
	// The client is told "yes" here. Everything after this point must preserve
	// this record, through any failure and through the move itself.
	m.acked = append(m.acked, e)
	return e.LSN, nil
}

// Lag is how many records the destination is behind the source.
func (m *Move) Lag() int {
	d := int(m.Src.Applied()) - int(m.Dst.Applied())
	if d < 0 {
		return 0
	}
	return d
}

// ship moves up to ShipBatch records from source to destination.
func (m *Move) ship() {
	for i := 0; i < m.ShipBatch; i++ {
		next := m.Dst.Applied() + 1
		if next > m.Src.Applied() {
			return
		}
		if err := m.Dst.accept(m.Src.log[next-1], m.Epoch); err != nil {
			return
		}
	}
}

// Abort rolls the move back. The source keeps authority and its data.
//
// Abort is safe at every phase before the cutover commits, and that is not an
// accident: the source is left untouched right up to the moment authority
// moves. This is why cleanup runs last.
func (m *Move) Abort(reason string) {
	if m.Phase.AuthorityMoved() {
		panic("rebalance: cannot abort after authority has moved")
	}
	m.Phase, m.Reason = PhaseAborted, reason
	// The destination's partial copy is garbage, not a head start. Keeping it
	// invites a later bug that treats it as a valid replica.
	m.Dst.log = nil
	if m.Src.Down {
		// No healthy owner: read-only is the honest state, not a silent
		// promotion of a copy that may be missing acknowledged writes.
		m.Writer = ""
		return
	}
	m.Writer = m.Src.ID
}

// Step advances the move by one tick.
func (m *Move) Step() {
	switch m.Phase {
	case PhasePlanned:
		if m.Dst.Down {
			// Nothing has been transferred, so abandoning costs nothing. A move
			// is always optional; the cluster is correct without it.
			m.Abort("destination unavailable before the move started")
			return
		}
		// The snapshot's LSN is fixed HERE, when the phase starts -- not when
		// the transfer finishes. That is the whole point: the copy is of a
		// consistent past state, and everything written while it is in flight
		// is deliberately left for catch-up to carry.
		m.Phase, m.snapTicks, m.snapAt = PhaseSnapshot, m.SnapshotTicks, m.Src.Applied()

	case PhaseSnapshot:
		if m.Dst.Down {
			m.Abort("destination failed during snapshot")
			return
		}
		if m.snapTicks--; m.snapTicks > 0 {
			return
		}
		// The snapshot lands atomically at the LSN it was taken at. A partially
		// applied snapshot is never a valid state to serve or to resume from.
		m.Dst.log = append([]Rec(nil), m.Src.log[:m.snapAt]...)
		m.Phase, m.bestLag, m.stallTicks = PhaseCatchUp, m.Lag()+1, 0

	case PhaseCatchUp:
		if m.Dst.Down {
			m.Abort("destination failed during catch-up")
			return
		}
		m.ship()
		if m.UnsafeCutover {
			// The bug, made explicit: authority moves while the destination is
			// still behind, so everything above its cursor is gone.
			m.Phase = PhaseCutover
			return
		}
		// Abandon a move that is not converging. Holding resources for a
		// transfer that will never finish is worse than not starting it: the
		// destination accumulates a copy it will never use, and the source
		// cannot truncate its log past the destination's cursor.
		if m.Lag() < m.bestLag {
			m.bestLag, m.stallTicks = m.Lag(), 0
		} else if m.stallTicks++; m.stallTicks > m.StallBudget {
			m.Abort("destination is not converging; the gap stopped shrinking")
			return
		}
		if m.Lag() <= m.LagThreshold {
			// Stop accepting writes. The gap is now small enough that the
			// outage this causes is bounded and short.
			m.Phase, m.Writer, m.pauseTicks = PhasePause, "", 0
		}

	case PhasePause:
		if m.Dst.Down {
			m.Abort("destination failed during the write pause")
			return
		}
		m.ship()
		if m.Lag() == 0 {
			m.Phase = PhaseCutover
			return
		}
		if m.pauseTicks++; m.pauseTicks > m.PauseBudget {
			// An unbounded pause is an outage. Give the shard back to the
			// source and try again later.
			m.Abort("pause budget exceeded before the destination caught up")
		}

	case PhaseCutover:
		// The one irreversible step. The epoch bump is the fence: from here the
		// source's writes are stamped below the destination's epoch and are
		// refused by the receiver, even if the source never learns it lost.
		m.Epoch++
		m.Dst.epoch = m.Epoch
		m.Writer = m.Dst.ID
		m.Phase = PhaseCleanup

	case PhaseCleanup:
		// Only now is the fallback expendable.
		m.Src.log, m.Src.dropped = nil, true
		m.Phase = PhaseDone
	}
}

// Check asserts every safety invariant. Call it after EVERY step: an invariant
// checked only at the end tells you a run failed, not where.
func (m *Move) Check() error {
	// I1. Write authority is single-valued. Two writers means two divergent
	// logs claiming the same LSNs, which no later merge can repair.
	if m.Writer != "" && m.Writer != m.Src.ID && m.Writer != m.Dst.ID {
		return fmt.Errorf("shard %d: writer %q is neither source nor destination", m.Shard, m.Writer)
	}

	// I2. Before authority moves, the destination is never ahead of the source.
	// Ahead would mean it holds a record the owner never acknowledged.
	if !m.Phase.AuthorityMoved() && m.Dst.Applied() > m.Src.Applied() {
		return fmt.Errorf("shard %d: destination at %d is ahead of source at %d in phase %s",
			m.Shard, m.Dst.Applied(), m.Src.Applied(), m.Phase)
	}

	// I3. The source may only write while it holds authority.
	if (m.Phase == PhaseCleanup || m.Phase == PhaseDone) && m.Writer == m.Src.ID {
		return fmt.Errorf("shard %d: source still holds write authority after cutover", m.Shard)
	}

	// I4. Once the move is done, the destination holds every acknowledged
	// write. This is the invariant the whole protocol exists to preserve.
	if m.Phase == PhaseDone {
		for _, e := range m.acked {
			if !m.Dst.Has(e.LSN) {
				return fmt.Errorf("shard %d: acknowledged write %d (%s) is missing after the move",
					m.Shard, e.LSN, e.ID)
			}
		}
	}

	// I5. An abort leaves the source with everything it ever acknowledged.
	if m.Phase == PhaseAborted && !m.Src.Down {
		for _, e := range m.acked {
			if !m.Src.Has(e.LSN) {
				return fmt.Errorf("shard %d: acknowledged write %d lost by an abort", m.Shard, e.LSN)
			}
		}
	}

	// I6. A dropped replica is never the writer.
	if m.Src.dropped && m.Writer == m.Src.ID {
		return fmt.Errorf("shard %d: source was cleaned up but still holds authority", m.Shard)
	}
	return nil
}

// Acked returns every write the client was told succeeded.
func (m *Move) Acked() []Rec { return append([]Rec(nil), m.acked...) }

// Lost returns acknowledged writes the current owner no longer holds. It should
// always be empty; a test that calls it is asking "did we lose data".
func (m *Move) Lost() []Rec {
	owner := m.Src
	if m.Writer == m.Dst.ID || m.Phase.AuthorityMoved() {
		owner = m.Dst
	}
	var out []Rec
	for _, e := range m.acked {
		if !owner.Has(e.LSN) {
			out = append(out, e)
		}
	}
	return out
}
