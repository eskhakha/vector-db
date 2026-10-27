package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/replication"
	"github.com/eskhakha/vecdb/internal/store"
)

// replicationStory walks the durability contract end to end: a write is
// acknowledged only when it is on every in-sync replica, the in-sync set
// shrinks under partition and writes are refused rather than silently
// degraded, an ejected replica catches up, and a promoted replica loses
// nothing.
func (c *demo) replicationStory(n int) {
	ctx := context.Background()

	openShard := func(name string) *store.Shard {
		sh, err := store.Open(store.Config{
			Dir: filepath.Join(c.dir, name), Dim: c.dim, Metric: core.Cosine,
			Sync: c.sync, SegmentSize: 256 << 10,
			// The write path stops at "durable on the leader"; only the
			// replication layer may advance the commit point.
			Replicated: true,
		})
		if err != nil {
			fatal(c.log, "open "+name, err)
		}
		return sh
	}

	tr := replication.NewInProcTransport(replication.FaultConfig{})
	cfg := replication.Config{MinISR: 2, SyncTimeout: c.syncTimeout}
	leader, err := replication.NewLeader("node-1", openShard("node-1"), tr, 1, cfg)
	if err != nil {
		fatal(c.log, "new leader", err)
	}
	followers := map[replication.NodeID]*replication.Follower{}
	for _, id := range []replication.NodeID{"node-2", "node-3"} {
		f := replication.NewFollower(id, openShard(string(id)))
		tr.Register(id, f)
		leader.AddReplica(id)
		followers[id] = f
	}

	// ---- 1. replicated writes ----
	section("1. Replicated writes (synchronous, to every in-sync replica)")
	acked := map[string]core.LSN{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("vec-%06d", i)
		lsn, err := leader.Put(ctx, c.vec(i))
		if err != nil {
			fatal(c.log, "put", err)
		}
		acked[id] = lsn
	}
	fmt.Printf("  acknowledged %d writes at epoch %d\n", len(acked), leader.Epoch())
	fmt.Printf("  ISR: leader + %v (size %d)\n", leader.ISR(), leader.ISRSize())
	for id, f := range followers {
		fmt.Printf("  %s: applied=%d lag=%d\n", id, f.Shard().AppliedLSN(), leader.Lag(id))
	}
	fmt.Printf("\n  Every one of those returned only after the record was durable on\n")
	fmt.Printf("  the leader AND on every in-sync replica. That is the contract.\n")

	// ---- 2. a replica is partitioned ----
	section("2. A replica is partitioned")
	tr.Partition("node-3", true)
	_, err = leader.Put(ctx, c.vec(n))
	fmt.Printf("  next write -> %v\n", errString(err))
	fmt.Printf("  ISR after ejection: leader + %v (size %d)\n", leader.ISR(), leader.ISRSize())
	fmt.Printf("  MinISR=2 is satisfied, so writes continue on one follower.\n")
	ok := 0
	for i := n + 1; i < n+200; i++ {
		if _, err := leader.Put(ctx, c.vec(i)); err == nil {
			ok++
			acked[fmt.Sprintf("vec-%06d", i)] = 0
		}
	}
	fmt.Printf("  %d further writes acknowledged while node-3 was unreachable\n", ok)

	// ---- 3. both replicas gone ----
	section("3. Both replicas gone: refuse, do not silently degrade")
	tr.Partition("node-2", true)
	_, err = leader.Put(ctx, c.vec(99999))
	fmt.Printf("  write -> %v\n", errString(err))
	fmt.Printf("  ISR: leader only (size %d)\n", leader.ISRSize())
	fmt.Printf("\n  This is the Kafka lesson. With acks=all and an ISR of one, \"all\n")
	fmt.Printf("  in-sync replicas\" is trivially satisfied by the leader alone, and\n")
	fmt.Printf("  acknowledged writes are lost on its death. Refusing costs\n")
	fmt.Printf("  availability on purpose.\n")

	// ---- 4. catch-up ----
	section("4. Heal and catch up")
	for id, f := range followers {
		tr.Partition(id, false)
		before := f.Shard().AppliedLSN()
		if err := leader.Recover(ctx, id, f); err != nil {
			fatal(c.log, "recover "+string(id), err)
		}
		fmt.Printf("  %s: applied %d -> %d (leader %d)\n",
			id, before, f.Shard().AppliedLSN(), leader.Shard().AppliedLSN())
	}
	fmt.Printf("  ISR restored: leader + %v (size %d)\n", leader.ISR(), leader.ISRSize())

	// ---- 5. kill the leader ----
	section("5. Kill the leader, promote a survivor")
	target := leader.Shard().AppliedLSN()
	oldEpoch := leader.Epoch()
	_ = leader.Shard().Close()
	fmt.Printf("  node-1 is gone at applied=%d, epoch=%d\n", target, oldEpoch)

	survivor := followers["node-2"]
	newLeader, err := replication.Promote("node-2", survivor, tr, oldEpoch+1, cfg)
	if err != nil {
		fatal(c.log, "promote", err)
	}
	fmt.Printf("  node-2 promoted at epoch %d (the fence: node-1's messages now\n", newLeader.Epoch())
	fmt.Printf("  carry a lower epoch and are refused, even if it wakes up believing\n")
	fmt.Printf("  it still leads)\n")

	missing := 0
	for id := range acked {
		if _, err := survivor.Shard().Get(id); err != nil {
			missing++
		}
	}
	fmt.Printf("\n  checked %d acknowledged writes on the survivor: %d missing\n", len(acked), missing)
	if missing == 0 {
		fmt.Printf("  PASS: no acknowledged write was lost to the failover.\n")
	} else {
		fmt.Printf("  FAIL: %d acknowledged writes lost.\n", missing)
	}

	// ---- 6. the deposed leader returns ----
	section("6. The deposed leader returns")
	old := replication.NewFollower("node-1", openShard("node-1"))
	tr.Register("node-1", old)
	newLeader.AddReplica("node-1")
	beforeApplied := old.Shard().AppliedLSN()
	if err := newLeader.Recover(ctx, "node-1", old); err != nil {
		fmt.Printf("  recover: %v\n", err)
	}
	fmt.Printf("  node-1 rejoined: applied %d -> %d (new leader %d)\n",
		beforeApplied, old.Shard().AppliedLSN(), newLeader.Shard().AppliedLSN())
	if old.Shard().AppliedLSN() == newLeader.Shard().AppliedLSN() {
		fmt.Printf("  PASS: the old leader converged on the new leader's history.\n")
	}
	fmt.Printf("\n  Had it held records above the new leader's commit point from the\n")
	fmt.Printf("  old epoch, they would have been truncated: never acknowledged,\n")
	fmt.Printf("  so nothing was promised.\n\n")

}
