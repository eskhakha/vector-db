package raft

import (
	"fmt"
	"testing"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
)

// applyAll runs every node's state machine forward and returns them.
func applyAll(t *testing.T, s *Sim, sms map[NodeID]*StateMachine) {
	t.Helper()
	for _, n := range s.Nodes() {
		if err := sms[n.ID()].Apply(n); err != nil {
			t.Fatalf("%s: %v", n.ID(), err)
		}
	}
}

func newSMs(s *Sim) map[NodeID]*StateMachine {
	m := map[NodeID]*StateMachine{}
	for _, id := range s.IDs() {
		m[id] = NewStateMachine()
	}
	return m
}

// TestPartitionedControllerCannotPromote is the whole reason consensus is here.
//
// The M8 controller is a single designated node. It cannot detect its own
// isolation, so a partitioned controller will happily promote a replacement for
// a leader that is perfectly healthy on the other side of the partition -- and
// nothing stops it, because it is the only authority.
//
// With the shard map behind Raft, that becomes impossible: a minority cannot
// commit, so a partitioned controller's promotion never takes effect anywhere,
// including on itself.
func TestPartitionedControllerCannotPromote(t *testing.T) {
	s := NewSim(5, 11)
	sms := newSMs(s)
	elect(t, s, 300)

	// Establish an initial placement through the log.
	if !s.Propose((Command{Kind: CmdPromote, Shard: 0, Node: "node-a", Epoch: 1}).Encode()) {
		t.Fatal("initial promote had no leader")
	}
	s.RunFor(40)
	applyAll(t, s, sms)
	for _, id := range s.IDs() {
		if got := sms[id].Shards[0].Leader; got != "node-a" {
			t.Fatalf("%s has leader %q, want node-a", id, got)
		}
	}

	// Isolate the current Raft leader -- the "controller" -- into a minority.
	old := s.AnyLeader().ID()
	var minority, majority []NodeID
	minority = append(minority, old)
	for _, id := range s.IDs() {
		if id != old {
			majority = append(majority, id)
		}
	}
	minority = append(minority, majority[0])
	majority = majority[1:]
	s.Partition(minority)

	// The isolated controller tries to promote. It may append locally; it must
	// never commit, and the change must never reach anyone.
	for i := 0; i < 20; i++ {
		if msgs, ok := s.Node(old).Propose(
			(Command{Kind: CmdPromote, Shard: 0, Node: "node-ZOMBIE", Epoch: 99}).Encode()); ok {
			s.send(msgs)
		}
		s.RunFor(10)
	}
	s.RunFor(300)
	applyAll(t, s, sms)

	for _, id := range s.IDs() {
		if got := sms[id].Shards[0].Leader; got == "node-ZOMBIE" {
			t.Fatalf("%s applied a promotion committed by a partitioned minority", id)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}

	// The majority, meanwhile, can still make a real decision.
	var mLeader *Node
	for i := 0; i < 400 && mLeader == nil; i++ {
		s.Step()
		mLeader = s.LeaderIn(majority)
	}
	if mLeader == nil {
		t.Fatal("the majority side never elected a leader")
	}
	if msgs, ok := mLeader.Propose(
		(Command{Kind: CmdPromote, Shard: 0, Node: "node-b", Epoch: 2}).Encode()); ok {
		s.send(msgs)
	}
	s.RunFor(100)
	applyAll(t, s, sms)
	for _, id := range majority {
		if got := sms[id].Shards[0].Leader; got != "node-b" {
			t.Fatalf("%s has leader %q after the majority promoted node-b", id, got)
		}
	}

	// Heal. The isolated controller's uncommitted entry must be discarded, and
	// it must converge on the majority's decision.
	s.Heal()
	s.RunFor(600)
	applyAll(t, s, sms)
	for _, id := range s.IDs() {
		st := sms[id].Shards[0]
		if st.Leader != "node-b" || st.Epoch != 2 {
			t.Fatalf("%s converged on leader=%q epoch=%d, want node-b/2", id, st.Leader, st.Epoch)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
}

// TestEveryReplicaConvergesOnTheSameMap: the state machine is a pure function
// of the committed log, so applying the same entries in the same order must
// give every node an identical map, with no coordination beyond the log.
func TestEveryReplicaConvergesOnTheSameMap(t *testing.T) {
	s := NewSim(5, 12)
	s.MaxDelay = 3
	s.Reorder = true
	sms := newSMs(s)
	elect(t, s, 300)

	for i := 0; i < 40; i++ {
		cmd := Command{Kind: CmdPromote, Shard: core.ShardID(i % 4),
			Node: cluster.NodeID(fmt.Sprintf("node-%d", i%3)), Epoch: core.Epoch(i + 1)}
		s.Propose(cmd.Encode())
		s.Propose((Command{Kind: CmdSetISR, Shard: core.ShardID(i % 4),
			ISR: []cluster.NodeID{"node-1", "node-2"}}).Encode())
		s.RunFor(6)
	}
	s.RunFor(300)
	applyAll(t, s, sms)

	ref := sms[s.IDs()[0]]
	for _, id := range s.IDs()[1:] {
		got := sms[id]
		if got.Applied != ref.Applied {
			// Divergent APPLIED COUNTS are fine (a node can be behind);
			// divergent CONTENT at the same index is not.
			continue
		}
		for shard, want := range ref.Shards {
			g := got.Shards[shard]
			if g.Leader != want.Leader || g.Epoch != want.Epoch || g.ReadOnly != want.ReadOnly {
				t.Fatalf("shard %d: %s has %+v, %s has %+v",
					shard, s.IDs()[0], want, id, g)
			}
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
}

// TestEpochNeverMovesBackwardsThroughTheLog: the state machine refuses a
// committed command that would lower an epoch. A committed log should never
// contain one; if it does, that is a bug to fail loudly on, not absorb.
func TestEpochNeverMovesBackwardsThroughTheLog(t *testing.T) {
	sm := NewStateMachine()
	n := &Node{Applied: [][]byte{
		(Command{Kind: CmdPromote, Shard: 0, Node: "a", Epoch: 5}).Encode(),
		(Command{Kind: CmdPromote, Shard: 0, Node: "b", Epoch: 3}).Encode(),
	}}
	err := sm.Apply(n)
	if err == nil {
		t.Fatal("a committed promote moved the epoch backwards without complaint")
	}
}

// TestReadOnlyIsReplicated: "no in-sync replica survived, this shard is
// unavailable" is a cluster-wide fact, not a local opinion, so it must be a
// committed log entry like any other decision.
func TestReadOnlyIsReplicated(t *testing.T) {
	s := NewSim(3, 13)
	sms := newSMs(s)
	elect(t, s, 300)
	s.Propose((Command{Kind: CmdPromote, Shard: 2, Node: "node-a", Epoch: 1}).Encode())
	s.RunFor(30)
	s.Propose((Command{Kind: CmdReadOnly, Shard: 2, Epoch: 2}).Encode())
	s.RunFor(60)
	applyAll(t, s, sms)
	for _, id := range s.IDs() {
		st := sms[id].Shards[2]
		if !st.ReadOnly || st.Leader != "" {
			t.Fatalf("%s has %+v, want read-only with no leader", id, st)
		}
	}
}
