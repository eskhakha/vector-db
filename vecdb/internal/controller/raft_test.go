package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/raft"
)

// This file is the payoff for building Raft, and it is the reason the M8
// controller was kept rather than deleted.
//
// A single controller cannot detect its own isolation. From inside a partition,
// "I cannot reach node-1" and "node-1 is dead" are the same observation, so a
// partitioned controller promotes a replacement for a leader that is perfectly
// healthy on the other side -- and nothing stops it, because it IS the
// authority. Epoch fencing does not help: the controller issues the epochs.
//
// The two tests below are a pair. The first asserts that behaviour, on purpose.
// The second shows the same scenario under a Raft-backed store, where the
// decision never commits and therefore never happens anywhere, including on the
// controller that made it.

// ---------- shared fixtures ----------

const (
	dataLeader   = cluster.NodeID("node-1")
	dataReplica  = cluster.NodeID("node-2")
	dataObserver = cluster.NodeID("node-3")
)

func dataNodes() []cluster.NodeID {
	return []cluster.NodeID{dataLeader, dataReplica, dataObserver}
}

func testConfig() Config {
	return Config{
		Detector: cluster.DetectorConfig{
			SuspectAfter: 1500 * time.Millisecond,
			DeadAfter:    5 * time.Second,
		},
		LeaseDuration: 2 * time.Second,
	}
}

// beat reports that a node is alive, and (for the leader) who is in sync.
// Promotion eligibility comes from the LAST heartbeat of the node that dies --
// by failover time it is gone and cannot be asked.
func beat(c *Controller, n cluster.NodeID, isLeader bool, applied core.LSN, epoch core.Epoch) {
	r := ShardReport{Epoch: epoch, Applied: applied, IsLeader: isLeader}
	if isLeader {
		r.ISR = []cluster.NodeID{dataReplica}
	}
	c.Heartbeat(Heartbeat{Node: n, Shards: map[core.ShardID]ShardReport{0: r}})
}

// killLeaderAndTick stops heartbeating the data-plane leader, advances past the
// failure threshold, and returns the events the controller produced.
func killLeaderAndTick(c *Controller, clk *cluster.ManualClock) []Event {
	var events []Event
	for i := 0; i < 8; i++ {
		clk.Advance(time.Second)
		beat(c, dataReplica, false, 100, 1)
		events = append(events, c.Tick()...)
	}
	return events
}

// ---------- the broken version, asserted on purpose ----------

// TestIsolatedLocalControllerPromotesAnyway pins the failure a single
// controller has. This test passing is not a bug report -- it is the
// motivation. If it ever fails, the contrast the next test draws has quietly
// disappeared and the Raft store is no longer buying anything demonstrable.
func TestIsolatedLocalControllerPromotesAnyway(t *testing.T) {
	m, err := cluster.NewShardMap(1, dataNodes(), 2)
	if err != nil {
		t.Fatal(err)
	}
	// Force a known placement so the test does not depend on round-robin.
	m.Shards[0] = cluster.ShardPlacement{
		Epoch: 1, Leader: dataLeader, Replicas: []cluster.NodeID{dataReplica},
	}

	clk := cluster.NewManualClock()
	var promoted []cluster.NodeID
	c, err := New(m, testConfig(), clk, func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
		promoted = append(promoted, to)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	beat(c, dataLeader, true, 100, 1)
	beat(c, dataReplica, false, 100, 1)
	c.Tick()

	// The controller is now isolated from node-1. It cannot tell that apart
	// from node-1 being dead, so it acts.
	events := killLeaderAndTick(c, clk)

	if !hasKind(events, "promoted") {
		t.Fatalf("a lone controller did not promote; the motivation for consensus "+
			"is gone and the contrast test proves nothing: %v", events)
	}
	if len(promoted) == 0 || promoted[0] != dataReplica {
		t.Fatalf("promoted %v, want node-2", promoted)
	}
	if p := c.Map().Shards[0]; p.Leader != dataReplica || p.Epoch != 2 {
		t.Fatalf("map has leader=%s epoch=%d, want node-2/2", p.Leader, p.Epoch)
	}
	t.Log("a single controller promoted a replacement while isolated, " +
		"with no way to know the old leader was healthy")
}

// ---------- the fix ----------

// raftFixture builds a Raft group with a store per node and seeds the initial
// placement through the committed log.
type raftFixture struct {
	sim    *raft.Sim
	stores map[raft.NodeID]*RaftStore
}

func newRaftFixture(t *testing.T, n int, seed int64) *raftFixture {
	t.Helper()
	s := raft.NewSim(n, seed)
	f := &raftFixture{sim: s, stores: map[raft.NodeID]*RaftStore{}}
	for _, id := range s.IDs() {
		f.stores[id] = NewRaftStore(s.Node(id), s, dataNodes())
	}

	// Elect, then seed the placement through the log so every store agrees on
	// where the shard starts.
	var leader *raft.Node
	for i := 0; i < 400 && leader == nil; i++ {
		s.Step()
		leader = s.AnyLeader()
	}
	if leader == nil {
		t.Fatal("no raft leader elected")
	}
	seed0 := raft.Command{
		Kind: raft.CmdPromote, Shard: 0, Node: dataLeader, Epoch: 1,
		Replicas: []cluster.NodeID{dataReplica},
	}
	if err := f.stores[leader.ID()].Propose([]raft.Command{seed0}); err != nil {
		t.Fatalf("seeding placement: %v", err)
	}
	s.RunFor(60)
	for id, st := range f.stores {
		if p := st.Map().Shards[0]; p.Leader != dataLeader {
			t.Fatalf("%s did not see the seeded placement: %+v", id, p)
		}
	}
	return f
}

// TestPartitionedControllerCannotFailOver is the whole reason consensus is here.
//
// The controller on the minority side sees exactly what the lone controller in
// the previous test saw, decides exactly the same thing, and is refused --
// because a minority cannot commit. Nothing changes on any node, including the
// one that decided.
func TestPartitionedControllerCannotFailOver(t *testing.T) {
	f := newRaftFixture(t, 5, 21)
	s := f.sim

	// Isolate the current raft leader with one companion: a 2-of-5 minority.
	old := s.AnyLeader().ID()
	var minority, majority []raft.NodeID
	minority = append(minority, old)
	for _, id := range s.IDs() {
		if id != old {
			majority = append(majority, id)
		}
	}
	minority = append(minority, majority[0])
	majority = majority[1:]
	s.Partition(minority)

	// The controller on the isolated side. It still believes it leads, because
	// it has not yet learned otherwise -- which is exactly the dangerous state.
	clk := cluster.NewManualClock()
	var promoted []cluster.NodeID
	isolated, err := NewWithStore(f.stores[old], testConfig(), clk,
		func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
			promoted = append(promoted, to)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	beat(isolated, dataLeader, true, 100, 1)
	beat(isolated, dataReplica, false, 100, 1)
	isolated.Tick()

	// It stops hearing from node-1 and tries to fail over, repeatedly.
	var events []Event
	for i := 0; i < 12; i++ {
		clk.Advance(time.Second)
		beat(isolated, dataReplica, false, 100, 1)
		events = append(events, isolated.Tick()...)
		s.RunFor(10)
	}

	if !hasKind(events, "node_dead") {
		t.Fatal("the isolated controller never even detected the leader as dead; " +
			"it is not being put in the situation the test is about")
	}
	if !hasKind(events, "not_decider") {
		t.Fatalf("the isolated controller was never refused: %v", events)
	}
	if hasKind(events, "promoted") || len(promoted) != 0 {
		t.Fatalf("a minority controller promoted %v", promoted)
	}

	// Nothing moved, on any node -- including the one that decided.
	for id, st := range f.stores {
		p := st.Map().Shards[0]
		if p.Leader != dataLeader || p.Epoch != 1 {
			t.Fatalf("%s has leader=%s epoch=%d after a minority decision", id, p.Leader, p.Epoch)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}

	// ---- the majority side can still make a real decision ----

	var mLeader *raft.Node
	for i := 0; i < 600 && mLeader == nil; i++ {
		s.Step()
		mLeader = s.LeaderIn(majority)
	}
	if mLeader == nil {
		t.Fatal("the majority never elected a raft leader")
	}

	clk2 := cluster.NewManualClock()
	var promoted2 []cluster.NodeID
	healthy, err := NewWithStore(f.stores[mLeader.ID()], testConfig(), clk2,
		func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
			promoted2 = append(promoted2, to)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	beat(healthy, dataLeader, true, 100, 1)
	beat(healthy, dataReplica, false, 100, 1)
	healthy.Tick()

	var events2 []Event
	for i := 0; i < 12; i++ {
		clk2.Advance(time.Second)
		beat(healthy, dataReplica, false, 100, 1)
		events2 = append(events2, healthy.Tick()...)
		s.RunFor(10)
	}
	if !hasKind(events2, "promoted") {
		t.Fatalf("the majority controller could not fail over: %v", events2)
	}
	if len(promoted2) == 0 || promoted2[0] != dataReplica {
		t.Fatalf("majority promoted %v, want node-2", promoted2)
	}

	// ---- heal: the isolated controller converges on the majority's decision ----

	s.Heal()
	s.RunFor(600)
	for id, st := range f.stores {
		p := st.Map().Shards[0]
		if p.Leader != dataReplica || p.Epoch != 2 {
			t.Fatalf("%s converged on leader=%s epoch=%d, want node-2/2", id, p.Leader, p.Epoch)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
}

// TestRaftControllerInstallsTheEpochOnApplyNotOnDecide.
//
// Commit is asynchronous, and the ordering matters: installing the new epoch on
// the replica at the moment the controller DECIDES would fence the old leader
// on the strength of a decision that may never commit. That is the same bug the
// Raft store exists to prevent, wearing a different hat.
func TestRaftControllerInstallsTheEpochOnApplyNotOnDecide(t *testing.T) {
	f := newRaftFixture(t, 3, 22)
	s := f.sim
	leaderID := s.AnyLeader().ID()

	clk := cluster.NewManualClock()
	type call struct {
		to    cluster.NodeID
		epoch core.Epoch
	}
	var calls []call
	// Record the committed state at the moment promote() is invoked.
	var seenAtCall []core.Epoch
	store := f.stores[leaderID]

	c, err := NewWithStore(store, testConfig(), clk,
		func(_ core.ShardID, to cluster.NodeID, e core.Epoch) error {
			calls = append(calls, call{to, e})
			seenAtCall = append(seenAtCall, store.Map().Shards[0].Epoch)
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	beat(c, dataLeader, true, 100, 1)
	beat(c, dataReplica, false, 100, 1)
	c.Tick()

	for i := 0; i < 12 && len(calls) == 0; i++ {
		clk.Advance(time.Second)
		beat(c, dataReplica, false, 100, 1)
		c.Tick()
		s.RunFor(10)
	}

	if len(calls) == 0 {
		t.Fatal("no promotion happened at all")
	}
	if calls[0].to != dataReplica || calls[0].epoch != 2 {
		t.Fatalf("promoted %s at epoch %d, want node-2 at 2", calls[0].to, calls[0].epoch)
	}
	// The decision was already committed when the epoch was installed.
	if seenAtCall[0] != 2 {
		t.Fatalf("promote() ran while the committed epoch was still %d; "+
			"the epoch was installed before the decision committed", seenAtCall[0])
	}
}

// TestPromotionFailureGoesReadOnlyThroughTheLog: if installing the new epoch
// fails, the shard must become unavailable as a COMMITTED fact, not as one
// controller's private opinion -- otherwise another controller reading the log
// still believes the shard has a healthy leader.
func TestPromotionFailureGoesReadOnlyThroughTheLog(t *testing.T) {
	f := newRaftFixture(t, 3, 23)
	s := f.sim
	leaderID := s.AnyLeader().ID()

	clk := cluster.NewManualClock()
	c, err := NewWithStore(f.stores[leaderID], testConfig(), clk,
		func(core.ShardID, cluster.NodeID, core.Epoch) error {
			return errors.New("replica refused the epoch")
		})
	if err != nil {
		t.Fatal(err)
	}
	beat(c, dataLeader, true, 100, 1)
	beat(c, dataReplica, false, 100, 1)
	c.Tick()

	var events []Event
	for i := 0; i < 15; i++ {
		clk.Advance(time.Second)
		beat(c, dataReplica, false, 100, 1)
		events = append(events, c.Tick()...)
		s.RunFor(10)
	}

	if !hasKind(events, "read_only") {
		t.Fatalf("a failed promotion did not make the shard read-only: %v", events)
	}
	s.RunFor(120)
	for id, st := range f.stores {
		p := st.Map().Shards[0]
		if !p.ReadOnly || p.Leader != "" {
			t.Fatalf("%s has %+v, want read-only with no leader", id, p)
		}
	}
}
