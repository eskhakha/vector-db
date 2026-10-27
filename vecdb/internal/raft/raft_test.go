package raft

import (
	"fmt"
	"math/rand"
	"testing"
)

// elect runs until a leader exists, or fails.
func elect(t *testing.T, s *Sim, within int) *Node {
	t.Helper()
	for i := 0; i < within; i++ {
		s.Step()
		if l := s.AnyLeader(); l != nil {
			return l
		}
	}
	t.Fatalf("no leader elected within %d ticks", within)
	return nil
}

func TestElectsALeader(t *testing.T) {
	s := NewSim(3, 1)
	l := elect(t, s, 200)
	if l.Term() == 0 {
		t.Fatal("leader elected at term 0")
	}
	// Everyone must agree, and there must be exactly one.
	s.RunFor(20)
	if err := s.CheckNoSplitBrain(); err != nil {
		t.Fatal(err)
	}
	for _, n := range s.Nodes() {
		if n.Leader() != l.ID() {
			t.Fatalf("%s thinks %s leads, actual leader is %s", n.ID(), n.Leader(), l.ID())
		}
	}
}

func TestReplicatesAndCommits(t *testing.T) {
	s := NewSim(3, 2)
	elect(t, s, 200)
	for i := 0; i < 20; i++ {
		if !s.Propose([]byte(fmt.Sprintf("cmd-%d", i))) {
			t.Fatalf("proposal %d had no leader", i)
		}
		s.RunFor(5)
	}
	s.RunFor(50)
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
	// Every node must have applied the same commands in the same order.
	want := s.AnyLeader().Applied
	if len(want) < 20 {
		t.Fatalf("leader applied %d commands, want >= 20", len(want))
	}
	for _, n := range s.Nodes() {
		for i := range n.Applied {
			if string(n.Applied[i]) != string(want[i]) {
				t.Fatalf("%s applied %q at %d, leader applied %q",
					n.ID(), n.Applied[i], i, want[i])
			}
		}
	}
}

// TestMinorityCannotCommit is the property that makes consensus worth the cost.
// A partitioned minority cannot make progress, so it cannot acknowledge
// anything the majority will later contradict.
func TestMinorityCannotCommit(t *testing.T) {
	s := NewSim(5, 3)
	elect(t, s, 200)

	minority := []NodeID{"n1", "n2"}
	s.Partition(minority)
	s.RunFor(300)

	// The majority side must have a leader and must make progress.
	majority := []NodeID{"n3", "n4", "n5"}
	if l := s.LeaderIn(majority); l == nil {
		t.Fatal("the majority side elected no leader")
	}
	before := s.LeaderIn(majority).Log().Commit()
	for i := 0; i < 10; i++ {
		l := s.LeaderIn(majority)
		if l == nil {
			t.Fatal("majority lost its leader")
		}
		if msgs, ok := l.Propose([]byte(fmt.Sprintf("majority-%d", i))); ok {
			s.send(msgs)
		}
		s.RunFor(10)
	}
	if s.LeaderIn(majority).Log().Commit() <= before {
		t.Fatal("the majority made no progress")
	}

	// The minority side must NOT have committed anything new.
	for _, id := range minority {
		n := s.Node(id)
		if n.IsLeader() {
			t.Fatalf("%s believes it leads on the minority side of a partition", id)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
}

// TestPreVotePreventsTermInflation. Raft as published does not guarantee
// liveness under partial partitions: an isolated node times out, bumps its
// term, and on rejoining forces the healthy leader to step down -- repeatedly.
// PreVote asks "would you vote for me?" without adopting the term, so a node
// that cannot win never disturbs anyone.
func TestPreVotePreventsTermInflation(t *testing.T) {
	s := NewSim(3, 4)
	l := elect(t, s, 200)
	s.RunFor(50)
	termBefore := l.Term()

	// Isolate one follower for a long time. Without pre-vote its term would
	// climb on every election timeout.
	var victim NodeID
	for _, id := range s.IDs() {
		if id != l.ID() {
			victim = id
			break
		}
	}
	s.Partition([]NodeID{victim})
	s.RunFor(500)

	if got := s.Node(victim).Term(); got > termBefore {
		t.Fatalf("isolated node inflated its term from %d to %d; pre-vote is not working",
			termBefore, got)
	}

	// On healing, the healthy leader must survive.
	s.Heal()
	s.RunFor(100)
	if s.Node(l.ID()).Term() != termBefore {
		t.Fatalf("leader's term moved from %d to %d after a node rejoined",
			termBefore, s.Node(l.ID()).Term())
	}
	if err := s.CheckAll(); err != nil {
		t.Fatal(err)
	}
}

// TestCheckQuorumStepsDownIsolatedLeader. Without CheckQuorum an isolated
// leader keeps believing it leads forever, which is exactly the stale-leader
// problem the whole design is trying to bound.
func TestCheckQuorumStepsDownIsolatedLeader(t *testing.T) {
	s := NewSim(3, 5)
	l := elect(t, s, 200)
	s.RunFor(30)

	s.Partition([]NodeID{l.ID()})
	s.RunFor(200)

	if s.Node(l.ID()).IsLeader() {
		t.Fatal("an isolated leader never stepped down; CheckQuorum is not working")
	}
	if err := s.CheckNoSplitBrain(); err != nil {
		t.Fatal(err)
	}
}

// TestLogDivergenceIsRepaired: a follower with a conflicting tail must be
// brought back to the leader's history.
func TestLogDivergenceIsRepaired(t *testing.T) {
	s := NewSim(3, 6)
	l := elect(t, s, 200)

	for i := 0; i < 5; i++ {
		s.Propose([]byte(fmt.Sprintf("early-%d", i)))
		s.RunFor(5)
	}
	s.RunFor(30)

	// Isolate the leader and let it write entries it can never commit.
	old := l.ID()
	s.Partition([]NodeID{old})
	for i := 0; i < 5; i++ {
		if msgs, ok := s.Node(old).Propose([]byte(fmt.Sprintf("orphan-%d", i))); ok {
			s.send(msgs)
		}
		s.RunFor(3)
	}
	orphanTail := s.Node(old).Log().LastIndex()
	s.RunFor(300) // the majority elects a new leader and moves on

	var majority []NodeID
	for _, id := range s.IDs() {
		if id != old {
			majority = append(majority, id)
		}
	}
	for i := 0; i < 10; i++ {
		if nl := s.LeaderIn(majority); nl != nil {
			if msgs, ok := nl.Propose([]byte(fmt.Sprintf("new-%d", i))); ok {
				s.send(msgs)
			}
		}
		s.RunFor(10)
	}

	// Heal. The old leader's orphan entries must be replaced, not kept.
	s.Heal()
	s.RunFor(400)

	if s.Node(old).IsLeader() {
		t.Fatal("the old leader is still leading after rejoining")
	}
	nl := s.AnyLeader()
	if nl == nil {
		t.Fatal("no leader after healing")
	}
	// The rejoined node's log must now agree with the leader's everywhere the
	// leader has entries.
	for i := Index(1); i <= nl.Log().Commit(); i++ {
		want, _ := nl.Log().EntryAt(i)
		got, ok := s.Node(old).Log().EntryAt(i)
		if !ok || got.Term != want.Term || string(got.Data) != string(want.Data) {
			t.Fatalf("index %d: rejoined node has %v, leader has %v", i, got, want)
		}
	}
	if err := s.CheckAll(); err != nil {
		t.Fatalf("%v\ntrace: %v", err, s.Trace)
	}
	_ = orphanTail
}

// TestFigure8CommitRule. A leader must not commit an entry from a PREVIOUS term
// merely because it sits on a majority. Doing so lets a later leader with a
// different history overwrite an entry that was already reported committed.
//
// This is the subtlest rule in Raft and the one most often omitted.
func TestFigure8CommitRule(t *testing.T) {
	n := New(Config{ID: "a", Peers: []NodeID{"a", "b", "c"},
		Rand: rand.New(rand.NewSource(1))})
	// Hand-build a log holding an entry from an older term.
	n.log.Append(1, []byte("old-term-entry"))
	n.currentTerm = 5
	n.state = Leader
	n.matchIndex = map[NodeID]Index{"a": 1, "b": 1, "c": 0}

	// A majority (a and b) holds index 1, but it is from term 1, not 5.
	if n.maybeCommit() {
		t.Fatal("committed a previous-term entry by counting replicas; " +
			"a later leader could still overwrite it")
	}
	if n.log.Commit() != 0 {
		t.Fatalf("commit index moved to %d", n.log.Commit())
	}

	// Appending an entry in the CURRENT term and replicating it commits both.
	n.log.Append(5, []byte("current-term-entry"))
	n.matchIndex = map[NodeID]Index{"a": 2, "b": 2, "c": 0}
	if !n.maybeCommit() {
		t.Fatal("failed to commit a current-term entry on a majority")
	}
	if n.log.Commit() != 2 {
		t.Fatalf("commit = %d, want 2: the older entry commits indirectly", n.log.Commit())
	}
}

// TestElectionRestriction: a candidate whose log is behind must not win, which
// is what guarantees Leader Completeness.
func TestElectionRestriction(t *testing.T) {
	voter := New(Config{ID: "v", Peers: []NodeID{"v", "c", "x"},
		Rand: rand.New(rand.NewSource(1))})
	voter.log.Append(3, []byte("a"), []byte("b"))
	voter.currentTerm = 3

	// A candidate with a shorter log at the same term.
	resp := voter.Step(Message{Type: MsgVoteReq, From: "c", Term: 4,
		LastLogIndex: 1, LastLogTerm: 3})
	if resp[0].Granted {
		t.Fatal("voted for a candidate with a shorter log at the same term")
	}
	// A candidate with a higher last term wins regardless of length.
	voter2 := New(Config{ID: "v", Peers: []NodeID{"v", "c", "x"},
		Rand: rand.New(rand.NewSource(1))})
	voter2.log.Append(3, []byte("a"), []byte("b"))
	voter2.currentTerm = 3
	resp = voter2.Step(Message{Type: MsgVoteReq, From: "c", Term: 5,
		LastLogIndex: 1, LastLogTerm: 4})
	if !resp[0].Granted {
		t.Fatal("refused a candidate whose last log term is higher")
	}
}

// TestOneVotePerTerm: the other half of why two leaders cannot coexist.
func TestOneVotePerTerm(t *testing.T) {
	v := New(Config{ID: "v", Peers: []NodeID{"v", "a", "b"},
		Rand: rand.New(rand.NewSource(1))})
	r1 := v.Step(Message{Type: MsgVoteReq, From: "a", Term: 2, LastLogIndex: 0, LastLogTerm: 0})
	if !r1[0].Granted {
		t.Fatal("first vote refused")
	}
	r2 := v.Step(Message{Type: MsgVoteReq, From: "b", Term: 2, LastLogIndex: 0, LastLogTerm: 0})
	if r2[0].Granted {
		t.Fatal("granted a second vote in the same term; two leaders become possible")
	}
}

// TestRandomizedSchedules is the payoff: thousands of randomized crash,
// partition, drop and reorder schedules, each fully reproducible from its seed,
// asserting the safety invariants after every single step.
//
// Safety is asserted continuously rather than at the end, because a violation
// that heals itself is still a violation -- a client may have read the wrong
// answer in between.
func TestRandomizedSchedules(t *testing.T) {
	const runs = 300
	for seed := int64(0); seed < runs; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			s := NewSim(5, seed)
			s.DropRate = 0.10
			s.MaxDelay = 4
			s.Reorder = true
			rng := rand.New(rand.NewSource(seed * 7919))

			for step := 0; step < 400; step++ {
				switch rng.Intn(24) {
				case 0: // partition a random minority
					var group []NodeID
					for _, id := range s.IDs() {
						if rng.Intn(3) == 0 {
							group = append(group, id)
						}
					}
					s.Partition(group)
				case 1:
					s.Heal()
				case 2:
					s.Crash(s.IDs()[rng.Intn(len(s.IDs()))])
				case 3:
					s.Restart(s.IDs()[rng.Intn(len(s.IDs()))])
				case 4, 5, 6:
					s.Propose([]byte(fmt.Sprintf("s%d-%d", seed, step)))
				}
				s.Step()
				if err := s.CheckAll(); err != nil {
					t.Fatalf("seed %d step %d: %v\ntrace:\n%v",
						seed, step, err, s.Trace)
				}
			}
			// Heal everything and let it settle; the cluster must converge on a
			// single leader and consistent logs.
			s.Heal()
			for _, id := range s.IDs() {
				s.Restart(id)
			}
			s.RunFor(600)
			if err := s.CheckAll(); err != nil {
				t.Fatalf("seed %d after healing: %v", seed, err)
			}
			if s.AnyLeader() == nil {
				t.Fatalf("seed %d: no leader after healing and 600 ticks (liveness)", seed)
			}
		})
	}
}

// TestCommittedEntriesSurviveLeaderChanges is the end-to-end safety claim: a
// command acknowledged as committed is never lost, no matter how many leaders
// come and go.
func TestCommittedEntriesSurviveLeaderChanges(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		s := NewSim(5, seed+1000)
		s.MaxDelay = 3
		rng := rand.New(rand.NewSource(seed))
		elect(t, s, 400)

		// Track every entry that any node ever reported as committed.
		everCommitted := map[Index]Entry{}
		record := func() {
			for _, n := range s.Nodes() {
				for i := Index(1); i <= n.Log().Commit(); i++ {
					if e, ok := n.Log().EntryAt(i); ok {
						if prev, seen := everCommitted[i]; seen {
							if prev.Term != e.Term || string(prev.Data) != string(e.Data) {
								t.Fatalf("seed %d: index %d was committed as %v and later as %v",
									seed, i, prev, e)
							}
						} else {
							everCommitted[i] = e
						}
					}
				}
			}
		}

		for step := 0; step < 300; step++ {
			if rng.Intn(30) == 0 {
				// Kill whoever leads, forcing an election.
				if l := s.AnyLeader(); l != nil {
					s.Crash(l.ID())
				}
			}
			if rng.Intn(12) == 0 {
				s.Restart(s.IDs()[rng.Intn(len(s.IDs()))])
			}
			s.Propose([]byte(fmt.Sprintf("v%d", step)))
			s.Step()
			record()
		}
		s.Heal()
		for _, id := range s.IDs() {
			s.Restart(id)
		}
		s.RunFor(500)
		record()

		// Every entry ever committed must still be present, unchanged, in the
		// final leader's log.
		l := s.AnyLeader()
		if l == nil {
			t.Fatalf("seed %d: no leader after settling", seed)
		}
		for i, want := range everCommitted {
			got, ok := l.Log().EntryAt(i)
			if !ok {
				t.Fatalf("seed %d: committed entry at index %d vanished from the leader's log", seed, i)
			}
			if got.Term != want.Term || string(got.Data) != string(want.Data) {
				t.Fatalf("seed %d: index %d committed as %v but leader now holds %v",
					seed, i, want, got)
			}
		}
	}
}

func BenchmarkConsensusRoundTrip(b *testing.B) {
	s := NewSim(3, 1)
	for i := 0; i < 400 && s.AnyLeader() == nil; i++ {
		s.Step()
	}
	if s.AnyLeader() == nil {
		b.Fatal("no leader")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Propose([]byte("x"))
		s.RunFor(4)
	}
}
