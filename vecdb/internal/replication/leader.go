package replication

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
)

// ErrNotEnoughReplicas is returned when the in-sync set has shrunk below the
// configured minimum.
//
// This is the single most important error in the package. Kafka's `acks=all`
// silently degrades to `acks=1` when the in-sync set shrinks to the leader
// alone, because "all in-sync replicas" is trivially satisfied by one node --
// a measured fault-injection run lost 194,524 of 500,000 acknowledged
// messages that way. Refusing the write is the fix, and it costs availability
// on purpose.
var ErrNotEnoughReplicas = errors.New("replication: in-sync replica set below minimum")

// Config configures a leader.
type Config struct {
	// MinISR is the smallest in-sync set (including the leader) that may still
	// accept writes. Below this the shard refuses writes rather than
	// acknowledging one that only exists in a single place.
	MinISR int
	// SyncTimeout bounds how long a write waits for a follower before that
	// follower is ejected from the in-sync set.
	//
	// It is a guess, and it is unavoidably a guess: nothing can distinguish a
	// crashed follower from a slow one. Too short and healthy followers flap
	// out under load; too long and every write stalls behind a dead node.
	SyncTimeout time.Duration
	// MaxCatchupBatch bounds how many records are shipped in one catch-up
	// round, so a badly lagging follower cannot produce an unbounded message.
	MaxCatchupBatch int
	// Clock is injectable so lease expiry is testable without sleeping.
	Clock cluster.Clock
}

func (c *Config) setDefaults() {
	if c.MinISR <= 0 {
		c.MinISR = 2
	}
	if c.SyncTimeout <= 0 {
		c.SyncTimeout = 3 * time.Second
	}
	if c.MaxCatchupBatch <= 0 {
		c.MaxCatchupBatch = 512
	}
	if c.Clock == nil {
		c.Clock = cluster.SystemClock{}
	}
}

// replicaState is the leader's view of one follower.
type replicaState struct {
	id      NodeID
	applied core.LSN
	inISR   bool
	lastAck time.Time
}

// Leader owns a shard and replicates its log to followers.
type Leader struct {
	ID    NodeID
	cfg   Config
	shard *store.Shard
	tr    Transport

	clock cluster.Clock

	mu       sync.Mutex
	replicas map[NodeID]*replicaState
	epoch    core.Epoch
	lease    cluster.Lease
	// writeMu serializes client writes so that log order and replication order
	// are the same order. Without it two concurrent writes could be
	// acknowledged out of sequence, and the commit point could advance past a
	// record a follower has not seen.
	writeMu sync.Mutex
}

// NewLeader returns a leader for a shard at the given epoch.
func NewLeader(id NodeID, sh *store.Shard, tr Transport, epoch core.Epoch, cfg Config) (*Leader, error) {
	cfg.setDefaults()
	if err := sh.SetEpoch(epoch); err != nil {
		return nil, err
	}
	return &Leader{
		ID: id, cfg: cfg, shard: sh, tr: tr, epoch: epoch,
		clock:    cfg.Clock,
		replicas: make(map[NodeID]*replicaState),
	}, nil
}

// Shard exposes the leader's replica.
func (l *Leader) Shard() *store.Shard { return l.shard }

// Epoch returns the leadership fence.
func (l *Leader) Epoch() core.Epoch {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch
}

// AddReplica registers a follower, initially in the in-sync set.
func (l *Leader) AddReplica(id NodeID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.replicas[id] = &replicaState{id: id, inISR: true, lastAck: time.Now()}
}

// ISR returns the current in-sync replica IDs, sorted for determinism.
func (l *Leader) ISR() []NodeID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]NodeID, 0, len(l.replicas))
	for id, r := range l.replicas {
		if r.inISR {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ISRSize counts the in-sync set including the leader itself.
func (l *Leader) ISRSize() int { return len(l.ISR()) + 1 }

// Lag returns leader commit position minus a follower's applied position.
func (l *Leader) Lag(id NodeID) core.LSN {
	l.mu.Lock()
	r, ok := l.replicas[id]
	var applied core.LSN
	if ok {
		applied = r.applied
	}
	l.mu.Unlock()
	if !ok {
		return 0
	}
	c := l.shard.CommitLSN()
	if c < applied {
		return 0
	}
	return c - applied
}

// Put writes a vector and returns once it is durable on the leader and on
// every in-sync replica.
//
// That last clause is the entire contract. If it returns nil, the write
// survives the loss of the leader.
func (l *Leader) Put(ctx context.Context, v *core.Vector) (core.LSN, error) {
	return l.write(ctx, func() (core.LSN, error) { return l.shard.Put(ctx, v) })
}

// Delete writes a tombstone with the same guarantee.
func (l *Leader) Delete(ctx context.Context, id string) (core.LSN, error) {
	return l.write(ctx, func() (core.LSN, error) { return l.shard.Delete(ctx, id) })
}

func (l *Leader) write(ctx context.Context, local func() (core.LSN, error)) (core.LSN, error) {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()

	// Refuse before writing anything if the in-sync set is already too small.
	// Appending first and refusing afterwards would leave a durable record the
	// client believes failed -- recoverable, since it is never committed, but
	// needless.
	if n := l.ISRSize(); n < l.cfg.MinISR {
		return 0, fmt.Errorf("%w: have %d, need %d", ErrNotEnoughReplicas, n, l.cfg.MinISR)
	}

	// 1. Durable on the leader. The shard is configured Replicated, so this
	//    does NOT advance the commit point.
	lsn, err := local()
	if err != nil {
		return 0, err
	}

	// 2. Durable on every in-sync replica.
	if err := l.replicate(ctx, lsn); err != nil {
		return 0, err
	}

	// 3. Only now is the record visible to readers.
	l.shard.Commit(lsn)
	return lsn, nil
}

// replicate ships everything up to lsn to the in-sync set and waits for
// acknowledgement, ejecting followers that do not answer in time.
func (l *Leader) replicate(ctx context.Context, lsn core.LSN) error {
	targets := l.ISR()
	if len(targets) == 0 {
		if l.cfg.MinISR > 1 {
			return fmt.Errorf("%w: no in-sync followers", ErrNotEnoughReplicas)
		}
		return nil
	}

	type ack struct {
		id  NodeID
		ok  bool
		err error
	}
	results := make(chan ack, len(targets))
	var wg sync.WaitGroup
	for _, id := range targets {
		wg.Add(1)
		go func(id NodeID) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, l.cfg.SyncTimeout)
			defer cancel()
			err := l.sendUpTo(rctx, id, lsn)
			results <- ack{id: id, ok: err == nil, err: err}
		}(id)
	}
	wg.Wait()
	close(results)

	// Opportunistically try to re-admit ejected replicas. This does NOT gate
	// the write -- an out-of-sync replica has no say in whether a write is
	// acknowledged -- but without it an ejected follower is never contacted
	// again and can only rejoin through an explicit operator action, which is
	// not how a real leader behaves.
	l.reAdmit(ctx, lsn)

	ejected := 0
	for a := range results {
		if a.ok {
			continue
		}
		// Cannot distinguish dead from slow from partitioned. Eject, and make
		// the shrunken in-sync set cluster-visible rather than a private
		// opinion of the leader.
		l.ejectFromISR(a.id)
		ejected++
	}
	if ejected > 0 {
		if n := l.ISRSize(); n < l.cfg.MinISR {
			// The record is durable on the leader but was never committed, so
			// no reader can observe it and the client is correctly told the
			// write failed. Refusing here is what prevents the Kafka failure.
			return fmt.Errorf("%w: have %d after ejection, need %d",
				ErrNotEnoughReplicas, n, l.cfg.MinISR)
		}
	}
	return nil
}

// sendUpTo brings one follower to lsn, retrying through catch-up if it reports
// a gap.
func (l *Leader) sendUpTo(ctx context.Context, id NodeID, lsn core.LSN) error {
	// Bounded: each round either advances the follower or fails. Two rounds
	// suffice in the common case (append, or catch-up then append); the extra
	// allowance covers a snapshot install followed by a tail append.
	for round := 0; round < 4; round++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		l.mu.Lock()
		r, ok := l.replicas[id]
		if !ok {
			l.mu.Unlock()
			return fmt.Errorf("replication: unknown replica %s", id)
		}
		from := r.applied
		epoch := l.epoch
		l.mu.Unlock()

		if from >= lsn {
			// Already caught up on DATA, but possibly stale on the COMMIT
			// POINT. Send an empty append carrying the current commit position.
			//
			// This is not a formality. A follower whose applied state is
			// current but whose commit point is behind will silently omit
			// committed records from reads -- it holds the data and refuses to
			// reveal it. Raft carries the commit index on its heartbeats for
			// exactly this reason.
			resp, err := l.tr.Append(ctx, l.ID, id, &AppendRequest{
				Epoch: epoch, CommitLSN: l.shard.CommitLSN(),
			})
			if err != nil {
				return err
			}
			if resp.Code == store.IngestStaleEpoch {
				return fmt.Errorf("%w: %s has epoch %d, we have %d",
					core.ErrStaleEpoch, id, resp.Epoch, epoch)
			}
			l.noteAck(id, resp.Applied)
			return nil
		}
		recs, ok, err := l.shard.RecordsFrom(from, l.cfg.MaxCatchupBatch)
		if err != nil {
			return err
		}
		if !ok {
			// The leader has truncated past this follower's cursor. Only a
			// snapshot can close the gap.
			if err := l.sendSnapshot(ctx, id, epoch); err != nil {
				return err
			}
			continue
		}
		resp, err := l.tr.Append(ctx, l.ID, id, &AppendRequest{
			Epoch: epoch, Records: recs, CommitLSN: l.shard.CommitLSN(),
		})
		if err != nil {
			return err
		}
		switch resp.Code {
		case store.IngestOK:
			l.noteAck(id, resp.Applied)
			if resp.Applied >= lsn {
				return nil
			}
			// Partial progress: more records remain. Loop.
		case store.IngestStaleEpoch:
			// This node has seen a HIGHER epoch, which means this leader has
			// been deposed and does not know it yet. Stop immediately.
			return fmt.Errorf("%w: %s has epoch %d, we have %d",
				core.ErrStaleEpoch, id, resp.Epoch, epoch)
		case store.IngestNeedCatchup:
			l.noteAck(id, resp.Applied)
			if resp.Applied < from {
				// The follower is further behind than we believed; the next
				// round ships from its real position.
				continue
			}
		default:
			return fmt.Errorf("replication: %s returned %s", id, resp.Code)
		}
	}
	return fmt.Errorf("replication: %s did not converge", id)
}

func (l *Leader) sendSnapshot(ctx context.Context, id NodeID, epoch core.Epoch) error {
	data, lsn, err := l.shard.SnapshotBytes()
	if err != nil {
		return err
	}
	resp, err := l.tr.Snapshot(ctx, l.ID, id, &SnapshotRequest{Epoch: epoch, LSN: lsn, Data: data})
	if err != nil {
		return err
	}
	if resp.Code != store.IngestOK {
		return fmt.Errorf("replication: snapshot to %s returned %s", id, resp.Code)
	}
	l.noteAck(id, resp.Applied)
	return nil
}

// reAdmit makes one best-effort attempt to bring ejected replicas forward.
// Failures are ignored: a replica that is still unreachable simply stays out.
func (l *Leader) reAdmit(ctx context.Context, lsn core.LSN) {
	l.mu.Lock()
	var out []NodeID
	for id, r := range l.replicas {
		if !r.inISR {
			out = append(out, id)
		}
	}
	l.mu.Unlock()
	if len(out) == 0 {
		return
	}
	var wg sync.WaitGroup
	for _, id := range out {
		wg.Add(1)
		go func(id NodeID) {
			defer wg.Done()
			rctx, cancel := context.WithTimeout(ctx, l.cfg.SyncTimeout)
			defer cancel()
			_ = l.sendUpTo(rctx, id, lsn)
		}(id)
	}
	wg.Wait()
}

func (l *Leader) noteAck(id NodeID, applied core.LSN) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, ok := l.replicas[id]
	if !ok {
		return
	}
	if applied > r.applied {
		r.applied = applied
	}
	r.lastAck = time.Now()
	if !r.inISR && r.applied >= l.shard.AppliedLSN() {
		// Caught up. Rejoining is safe only at the leader's current position;
		// admitting a lagging replica would let it be promoted later while
		// missing writes.
		r.inISR = true
	}
}

func (l *Leader) ejectFromISR(id NodeID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.replicas[id]; ok {
		r.inISR = false
	}
}

// Recover brings an ejected or restarted follower back into the in-sync set.
//
// It handles all three cases the log can present: a small gap fills from the
// log, a large gap needs a snapshot, and a follower holding records ABOVE the
// leader's position is a returning leader whose divergent tail must go.
func (l *Leader) Recover(ctx context.Context, id NodeID, f *Follower) error {
	target := l.shard.AppliedLSN()

	// Divergent tail: the follower holds records this leader never committed,
	// written under an older epoch. They were never acknowledged to a client,
	// so discarding them violates nothing -- and keeping them would leave two
	// permanently different histories.
	if f.Shard().AppliedLSN() > target {
		if err := f.Shard().TruncateDivergentTail(target); err != nil {
			return err
		}
	}
	l.mu.Lock()
	if r, ok := l.replicas[id]; ok {
		r.applied = f.Shard().AppliedLSN()
	} else {
		l.replicas[id] = &replicaState{id: id, applied: f.Shard().AppliedLSN()}
	}
	l.mu.Unlock()

	if err := l.sendUpTo(ctx, id, target); err != nil {
		return err
	}
	l.noteAck(id, f.Shard().AppliedLSN())
	return nil
}

// Promote makes a follower's shard the new leader at a strictly higher epoch.
//
// The epoch bump is the fence: from this moment the old leader's replication
// messages carry a lower epoch and are refused, so it cannot corrupt this
// node's log even if it still believes it leads.
func Promote(id NodeID, f *Follower, tr Transport, newEpoch core.Epoch, cfg Config) (*Leader, error) {
	sh := f.Shard()
	if newEpoch <= sh.Epoch() {
		return nil, fmt.Errorf("%w: promoting to %d but already at %d",
			core.ErrStaleEpoch, newEpoch, sh.Epoch())
	}
	// Everything this replica holds was in-sync at the moment of failure, so
	// it is all committed: the old leader could not have acknowledged anything
	// above it without this replica having acknowledged first.
	sh.Commit(sh.AppliedLSN())
	return NewLeader(id, sh, tr, newEpoch, cfg)
}
