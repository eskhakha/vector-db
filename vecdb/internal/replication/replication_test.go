package replication

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
	"github.com/eskhakha/vecdb/internal/wal"
)

const dim = 8

func newShard(t *testing.T, dir string) *store.Shard {
	t.Helper()
	sh, err := store.Open(store.Config{
		Dir: dir, Dim: dim, Metric: core.Cosine,
		Sync: wal.SyncNever, SegmentSize: 1 << 20, Replicated: true,
	})
	if err != nil {
		t.Fatalf("open shard: %v", err)
	}
	return sh
}

func vec(rng *rand.Rand, id string) *core.Vector {
	e := make([]float32, dim)
	for i := range e {
		e[i] = rng.Float32()*2 - 1
	}
	return &core.Vector{ID: id, Embedding: e}
}

// cluster is a leader plus n followers wired over one in-process transport.
type testCluster struct {
	t         *testing.T
	tr        *InProcTransport
	leader    *Leader
	followers map[NodeID]*Follower
	dirs      map[NodeID]string
	rng       *rand.Rand
}

func newCluster(t *testing.T, nFollowers int, cfg Config, faults FaultConfig) *testCluster {
	t.Helper()
	tr := NewInProcTransport(faults)
	ld, err := NewLeader("node-1", newShard(t, t.TempDir()), tr, 1, cfg)
	if err != nil {
		t.Fatal(err)
	}
	c := &testCluster{
		t: t, tr: tr, leader: ld,
		followers: map[NodeID]*Follower{},
		dirs:      map[NodeID]string{},
		rng:       rand.New(rand.NewSource(1)),
	}
	for i := 0; i < nFollowers; i++ {
		id := NodeID(fmt.Sprintf("node-%d", i+2))
		dir := t.TempDir()
		f := NewFollower(id, newShard(t, dir))
		tr.Register(id, f)
		ld.AddReplica(id)
		c.followers[id] = f
		c.dirs[id] = dir
	}
	return c
}

func (c *testCluster) put(id string) (core.LSN, error) {
	return c.leader.Put(context.Background(), vec(c.rng, id))
}

// TestAckedWriteSurvivesLeaderLoss is the acceptance test for this package.
// Kill the leader immediately after an acknowledgement, promote the survivor,
// and assert every acknowledged write is present. Under synchronous
// replication this must pass deterministically.
func TestAckedWriteSurvivesLeaderLoss(t *testing.T) {
	c := newCluster(t, 1, Config{MinISR: 2}, FaultConfig{})
	acked := map[string]core.LSN{}
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("id-%d", i)
		lsn, err := c.put(id)
		if err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
		acked[id] = lsn
	}
	// The leader dies. Not a clean shutdown -- just gone.
	_ = c.leader.Shard().Close()

	var survivor *Follower
	var survivorID NodeID
	for id, f := range c.followers {
		survivor, survivorID = f, id
	}
	newLeader, err := Promote(survivorID, survivor, c.tr, c.leader.Epoch()+1, Config{MinISR: 1})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if newLeader.Epoch() <= c.leader.Epoch() {
		t.Fatal("promotion did not raise the epoch: the old leader is unfenced")
	}
	for id, want := range acked {
		got, err := survivor.Shard().Get(id)
		if err != nil {
			t.Fatalf("acknowledged write %s lost on failover: %v", id, err)
		}
		if got.Version != want {
			t.Fatalf("%s: version %d after failover, acknowledged as %d", id, got.Version, want)
		}
	}
}

// TestUncommittedWriteIsNotVisible pins the reason commitLSN exists. A record
// that is durable on the leader but not yet replicated must not be readable,
// or a failover could make a previously-read value disappear.
func TestUncommittedWriteIsNotVisible(t *testing.T) {
	c := newCluster(t, 1, Config{MinISR: 2, SyncTimeout: 100 * time.Millisecond}, FaultConfig{})
	if _, err := c.put("committed"); err != nil {
		t.Fatal(err)
	}
	// Partition the follower so the next write cannot be replicated.
	for id := range c.followers {
		c.tr.Partition(id, true)
	}
	_, err := c.put("uncommitted")
	if !errors.Is(err, ErrNotEnoughReplicas) {
		t.Fatalf("write with no reachable replica returned %v, want ErrNotEnoughReplicas", err)
	}
	if _, err := c.leader.Shard().Get("uncommitted"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("a write that was never committed is readable")
	}
	if _, err := c.leader.Shard().Get("committed"); err != nil {
		t.Fatalf("committed write became unreadable: %v", err)
	}
}

// TestMinISRRefusesRatherThanSilentlyDegrading is the Kafka lesson as an
// assertion: with the in-sync set down to the leader alone, the write must be
// refused, not acknowledged into a single copy.
func TestMinISRRefusesRatherThanSilentlyDegrading(t *testing.T) {
	c := newCluster(t, 1, Config{MinISR: 2, SyncTimeout: 100 * time.Millisecond}, FaultConfig{})
	if _, err := c.put("a"); err != nil {
		t.Fatal(err)
	}
	for id := range c.followers {
		c.tr.Partition(id, true)
	}
	// First write ejects the unreachable follower and must fail.
	if _, err := c.put("b"); !errors.Is(err, ErrNotEnoughReplicas) {
		t.Fatalf("first write after partition = %v, want ErrNotEnoughReplicas", err)
	}
	if got := c.leader.ISRSize(); got != 1 {
		t.Fatalf("ISR size = %d after ejection, want 1 (leader only)", got)
	}
	// And every subsequent write must keep failing, not quietly succeed.
	for i := 0; i < 5; i++ {
		if _, err := c.put(fmt.Sprintf("c-%d", i)); !errors.Is(err, ErrNotEnoughReplicas) {
			t.Fatalf("write %d into a one-member ISR = %v, want refusal", i, err)
		}
	}
}

// TestMinISR1AllowsDegradedWrites shows the other side of the trade: an
// operator who sets MinISR=1 has chosen availability, and gets it.
func TestMinISR1AllowsDegradedWrites(t *testing.T) {
	c := newCluster(t, 1, Config{MinISR: 1, SyncTimeout: 50 * time.Millisecond}, FaultConfig{})
	for id := range c.followers {
		c.tr.Partition(id, true)
	}
	if _, err := c.put("a"); err != nil {
		t.Fatalf("MinISR=1 should accept a degraded write: %v", err)
	}
	if _, err := c.leader.Shard().Get("a"); err != nil {
		t.Fatalf("degraded write not readable: %v", err)
	}
}

// TestStaleEpochIsRejected proves the fence. A deposed leader must not be able
// to write into the new leader's log.
func TestStaleEpochIsRejected(t *testing.T) {
	c := newCluster(t, 1, Config{MinISR: 2}, FaultConfig{})
	if _, err := c.put("a"); err != nil {
		t.Fatal(err)
	}
	var f *Follower
	for _, ff := range c.followers {
		f = ff
	}
	// A new leader is elected at a higher epoch.
	if err := f.Shard().SetEpoch(9); err != nil {
		t.Fatal(err)
	}
	// The old leader, still at epoch 1, tries to replicate.
	resp := f.HandleAppend(&AppendRequest{
		Epoch: 1,
		Records: []*wal.Record{{
			LSN: f.Shard().AppliedLSN() + 1, Epoch: 1, Op: wal.OpPut,
			Vec: core.Vector{ID: "zombie", Embedding: make([]float32, dim)},
		}},
	})
	if resp.Code != store.IngestStaleEpoch {
		t.Fatalf("stale-epoch append returned %s, want stale_epoch", resp.Code)
	}
	if _, err := f.Shard().Get("zombie"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("a deposed leader corrupted the follower's log")
	}
}

// TestIngestIsIdempotent: replaying the same records must not double-apply.
// Retries and duplicated replication messages both depend on this.
func TestIngestIsIdempotent(t *testing.T) {
	sh := newShard(t, t.TempDir())
	defer sh.Close()
	f := NewFollower("f", sh)
	recs := []*wal.Record{
		{LSN: 1, Epoch: 1, Op: wal.OpPut, Vec: core.Vector{ID: "a", Embedding: make([]float32, dim)}},
		{LSN: 2, Epoch: 1, Op: wal.OpPut, Vec: core.Vector{ID: "b", Embedding: make([]float32, dim)}},
	}
	for i := 0; i < 3; i++ {
		resp := f.HandleAppend(&AppendRequest{Epoch: 1, Records: recs, CommitLSN: 2})
		if resp.Code != store.IngestOK {
			t.Fatalf("round %d: %s", i, resp.Code)
		}
		if resp.Applied != 2 {
			t.Fatalf("round %d: applied = %d, want 2", i, resp.Applied)
		}
	}
	if sh.Len() != 2 {
		t.Fatalf("replaying records three times produced %d vectors, want 2", sh.Len())
	}
}

// TestGapIsRefused: a follower must not apply a record that would leave a hole.
func TestGapIsRefused(t *testing.T) {
	sh := newShard(t, t.TempDir())
	defer sh.Close()
	f := NewFollower("f", sh)
	resp := f.HandleAppend(&AppendRequest{Epoch: 1, Records: []*wal.Record{
		{LSN: 5, Epoch: 1, Op: wal.OpPut, Vec: core.Vector{ID: "x", Embedding: make([]float32, dim)}},
	}})
	if resp.Code != store.IngestNeedCatchup {
		t.Fatalf("gapped append returned %s, want need_catchup", resp.Code)
	}
	if resp.Applied != 0 {
		t.Fatalf("applied = %d, want 0", resp.Applied)
	}
}

// TestCatchUpByLogShipping: an ejected follower rejoins by replaying the gap
// from the leader's log.
func TestCatchUpByLogShipping(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 1, Config{MinISR: 1, SyncTimeout: 50 * time.Millisecond}, FaultConfig{})
	var fid NodeID
	var f *Follower
	for id, ff := range c.followers {
		fid, f = id, ff
	}
	if _, err := c.put("before"); err != nil {
		t.Fatal(err)
	}
	// Follower goes away; the leader keeps accepting because MinISR=1.
	c.tr.Partition(fid, true)
	for i := 0; i < 50; i++ {
		if _, err := c.put(fmt.Sprintf("during-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if c.leader.ISRSize() != 1 {
		t.Fatalf("follower was not ejected; ISR size = %d", c.leader.ISRSize())
	}
	// It comes back.
	c.tr.Partition(fid, false)
	if err := c.leader.Recover(ctx, fid, f); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got, want := f.Shard().AppliedLSN(), c.leader.Shard().AppliedLSN(); got != want {
		t.Fatalf("after catch-up follower applied %d, leader %d", got, want)
	}
	if c.leader.ISRSize() != 2 {
		t.Fatalf("follower did not rejoin the ISR; size = %d", c.leader.ISRSize())
	}
	for i := 0; i < 50; i++ {
		if _, err := f.Shard().Get(fmt.Sprintf("during-%d", i)); err != nil {
			t.Fatalf("record missed during the outage was not replayed: %v", err)
		}
	}
}

// TestCatchUpBySnapshot forces the other path: the leader has truncated past
// the follower's cursor, so only a full snapshot can close the gap. This is the
// path that rots undetected if nothing exercises it.
func TestCatchUpBySnapshot(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 1, Config{MinISR: 1, SyncTimeout: 50 * time.Millisecond}, FaultConfig{})
	var fid NodeID
	var f *Follower
	for id, ff := range c.followers {
		fid, f = id, ff
	}
	c.tr.Partition(fid, true)
	for i := 0; i < 60; i++ {
		if _, err := c.put(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Snapshot and truncate the whole log, deliberately crossing the
	// follower's cursor. In production this is the bug; here it is the setup.
	ld := c.leader.Shard()
	ld.Commit(ld.AppliedLSN())
	if _, err := ld.Snapshot(ld.AppliedLSN()); err != nil {
		t.Fatal(err)
	}
	c.tr.Partition(fid, false)
	if err := c.leader.Recover(ctx, fid, f); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got, want := f.Shard().AppliedLSN(), ld.AppliedLSN(); got != want {
		t.Fatalf("after snapshot transfer follower applied %d, leader %d", got, want)
	}
	if got, want := f.Shard().Len(), ld.Len(); got != want {
		t.Fatalf("after snapshot transfer follower holds %d vectors, leader %d", got, want)
	}
	for i := 0; i < 60; i++ {
		if _, err := f.Shard().Get(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatalf("id-%d missing after snapshot transfer: %v", i, err)
		}
	}
}

// TestDivergentTailIsTruncated is the returning-leader case: a node holding
// records above the new leader's position, written under an older epoch. They
// were never acknowledged, so they must go.
func TestDivergentTailIsTruncated(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 1, Config{MinISR: 1, SyncTimeout: 50 * time.Millisecond}, FaultConfig{})
	var fid NodeID
	var f *Follower
	for id, ff := range c.followers {
		fid, f = id, ff
	}
	for i := 0; i < 10; i++ {
		if _, err := c.put(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Give the follower extra records the leader never had, as if it had been
	// a leader that crashed after its own fsync but before replicating.
	base := f.Shard().AppliedLSN()
	var extra []*wal.Record
	for i := 1; i <= 5; i++ {
		extra = append(extra, &wal.Record{
			LSN: base + core.LSN(i), Epoch: 1, Op: wal.OpPut,
			Vec: core.Vector{ID: fmt.Sprintf("ghost-%d", i), Embedding: make([]float32, dim)},
		})
	}
	if code, _ := f.Shard().Ingest(1, extra); code != store.IngestOK {
		t.Fatalf("seeding divergent tail: %s", code)
	}
	// Model the node having been a LEADER that applied and committed these
	// locally before dying -- which is the only way they could exist. Note
	// that the leader committing locally is not the same as acknowledging to a
	// client: acknowledgement also requires in-sync replication, which by
	// construction never happened for these.
	f.Shard().Commit(f.Shard().AppliedLSN())
	if f.Shard().AppliedLSN() <= c.leader.Shard().AppliedLSN() {
		t.Fatal("test setup failed to create a divergent tail")
	}
	if _, err := f.Shard().Get("ghost-1"); err != nil {
		t.Fatalf("ghost record not present before recovery: %v", err)
	}

	if err := c.leader.Recover(ctx, fid, f); err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got, want := f.Shard().AppliedLSN(), c.leader.Shard().AppliedLSN(); got != want {
		t.Fatalf("after truncation follower applied %d, leader %d", got, want)
	}
	for i := 1; i <= 5; i++ {
		if _, err := f.Shard().Get(fmt.Sprintf("ghost-%d", i)); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("ghost-%d survived truncation; the histories have diverged", i)
		}
	}
	for i := 0; i < 10; i++ {
		if _, err := f.Shard().Get(fmt.Sprintf("id-%d", i)); err != nil {
			t.Fatalf("truncation destroyed a committed record id-%d: %v", i, err)
		}
	}
}

// TestChaosNoAckedWriteIsLost is the test that finds real bugs: partition and
// heal replicas at random under sustained writes, and assert continuously that
// every acknowledged write is still readable and that no replica diverges.
func TestChaosNoAckedWriteIsLost(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t, 2, Config{MinISR: 2, SyncTimeout: 50 * time.Millisecond},
		FaultConfig{DropRate: 0.05, Seed: 7})
	ids := make([]NodeID, 0, len(c.followers))
	for id := range c.followers {
		ids = append(ids, id)
	}

	acked := map[string]core.LSN{}
	rng := rand.New(rand.NewSource(99))
	refused := 0
	for i := 0; i < 400; i++ {
		// Randomly isolate or heal one follower.
		if rng.Intn(10) == 0 {
			victim := ids[rng.Intn(len(ids))]
			c.tr.Partition(victim, rng.Intn(2) == 0)
		}
		// Heal everything periodically so the run makes real progress.
		// This must happen regardless of whether the last write succeeded --
		// otherwise one refusal wedges the run and the test proves nothing.
		if i%25 == 0 {
			for _, fid := range ids {
				c.tr.Partition(fid, false)
				_ = c.leader.Recover(ctx, fid, c.followers[fid])
			}
		}
		id := fmt.Sprintf("id-%d", i)
		lsn, err := c.put(id)
		if err != nil {
			// A refused write is a correct outcome: the system chose
			// unavailability over acknowledging into too few copies.
			refused++
			if _, gerr := c.leader.Shard().Get(id); !errors.Is(gerr, core.ErrNotFound) {
				t.Fatalf("%s: write was refused but is readable", id)
			}
			continue
		}
		acked[id] = lsn
	}
	// Every acknowledged write must still be readable on the leader.
	for id, want := range acked {
		got, err := c.leader.Shard().Get(id)
		if err != nil {
			t.Fatalf("acknowledged write %s lost: %v", id, err)
		}
		if got.Version != want {
			t.Fatalf("%s: version %d, acknowledged as %d", id, got.Version, want)
		}
	}
	// Heal, recover, and assert the replicas converged on identical state.
	for _, fid := range ids {
		c.tr.Partition(fid, false)
		if err := c.leader.Recover(ctx, fid, c.followers[fid]); err != nil {
			t.Fatalf("final recover of %s: %v", fid, err)
		}
	}
	leaderApplied := c.leader.Shard().AppliedLSN()
	for _, fid := range ids {
		f := c.followers[fid]
		if got := f.Shard().AppliedLSN(); got != leaderApplied {
			t.Fatalf("%s applied %d, leader %d: replicas did not converge", fid, got, leaderApplied)
		}
		for id := range acked {
			if _, err := f.Shard().Get(id); err != nil {
				t.Fatalf("%s is missing acknowledged write %s after convergence", fid, id)
			}
		}
	}
	sent, dropped := c.tr.Stats()
	t.Logf("acked=%d refused=%d messages=%d dropped=%d", len(acked), refused, sent, dropped)
	if dropped == 0 {
		t.Fatal("fault injection never fired; this run proved nothing")
	}
	// Guard against a vacuous pass. A run where almost everything was refused
	// exercises the refusal path and nothing else.
	if len(acked) < 100 {
		t.Fatalf("only %d of 400 writes were acknowledged; this run proved little", len(acked))
	}
	if refused == 0 {
		t.Fatal("no write was ever refused; the MinISR path was not exercised")
	}
}

func BenchmarkReplicatedPut(b *testing.B) {
	for _, nf := range []int{0, 1, 2} {
		b.Run(fmt.Sprintf("followers=%d", nf), func(b *testing.B) {
			tr := NewInProcTransport(FaultConfig{})
			sh, err := store.Open(store.Config{
				Dir: b.TempDir(), Dim: dim, Metric: core.Cosine,
				Sync: wal.SyncNever, SegmentSize: 1 << 30, Replicated: true,
			})
			if err != nil {
				b.Fatal(err)
			}
			defer sh.Close()
			ld, err := NewLeader("l", sh, tr, 1, Config{MinISR: 1})
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < nf; i++ {
				id := NodeID(fmt.Sprintf("f%d", i))
				fs, err := store.Open(store.Config{
					Dir: b.TempDir(), Dim: dim, Metric: core.Cosine,
					Sync: wal.SyncNever, SegmentSize: 1 << 30, Replicated: true,
				})
				if err != nil {
					b.Fatal(err)
				}
				defer fs.Close()
				tr.Register(id, NewFollower(id, fs))
				ld.AddReplica(id)
			}
			rng := rand.New(rand.NewSource(1))
			ctx := context.Background()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ld.Put(ctx, vec(rng, fmt.Sprintf("id-%d", i))); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
