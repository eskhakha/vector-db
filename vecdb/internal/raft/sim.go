package raft

import (
	"fmt"
	"math/rand"
	"sort"
)

// Sim runs an entire Raft cluster inside one goroutine against a virtual clock
// and a simulated network.
//
// This is deterministic simulation testing, the technique FoundationDB
// pioneered and TigerBeetle popularised. Everything nondeterministic -- time,
// message delivery order, drops, partitions, election-timeout randomization --
// comes from a single seeded RNG. A failing run therefore reproduces exactly
// from its seed, which is the difference between "consensus is flaky sometimes"
// and "here is the schedule that breaks it".
//
// It is the only practical way to find Raft bugs. Antithesis found real safety
// and liveness defects in every major Raft library they pointed this technique
// at, which is worth knowing before assuming a hand-rolled one is correct.
type Sim struct {
	nodes map[NodeID]*Node
	ids   []NodeID
	rng   *rand.Rand

	// inflight holds undelivered messages with the tick they arrive on.
	inflight []timed
	tick     int

	// partitions[a][b] true means a cannot send to b.
	partitions map[NodeID]map[NodeID]bool
	// down nodes neither tick nor receive; they model a crashed process.
	down map[NodeID]bool

	// Knobs.
	DropRate  float64
	MaxDelay  int
	Reorder   bool
	Delivered int
	Dropped   int

	// Trace records what happened, so a failure can be read back.
	Trace []string
}

type timed struct {
	at  int
	seq int
	msg Message
}

// NewSim builds a cluster of n nodes.
func NewSim(n int, seed int64) *Sim {
	rng := rand.New(rand.NewSource(seed))
	var ids []NodeID
	for i := 0; i < n; i++ {
		ids = append(ids, NodeID(fmt.Sprintf("n%d", i+1)))
	}
	s := &Sim{
		nodes: map[NodeID]*Node{}, ids: ids, rng: rng,
		partitions: map[NodeID]map[NodeID]bool{}, down: map[NodeID]bool{},
		MaxDelay: 1,
	}
	for _, id := range ids {
		s.nodes[id] = New(Config{
			ID: id, Peers: ids,
			ElectionTick: 10, HeartbeatTick: 2,
			// Each node gets its own RNG stream, derived from the run seed, so
			// election timeouts differ between nodes but the whole run stays
			// reproducible.
			Rand:        rand.New(rand.NewSource(rng.Int63())),
			CheckQuorum: true,
		})
	}
	return s
}

// Nodes returns the cluster, sorted by ID.
func (s *Sim) Nodes() []*Node {
	out := make([]*Node, 0, len(s.ids))
	for _, id := range s.ids {
		out = append(out, s.nodes[id])
	}
	return out
}

// Node returns one node.
func (s *Sim) Node(id NodeID) *Node { return s.nodes[id] }

// IDs returns node IDs in order.
func (s *Sim) IDs() []NodeID { return s.ids }

// Partition splits the cluster into two groups that cannot talk to each other.
// Within each group communication is unaffected, which is what makes a
// partition different from nodes simply being down.
func (s *Sim) Partition(groupA []NodeID) {
	inA := map[NodeID]bool{}
	for _, id := range groupA {
		inA[id] = true
	}
	s.partitions = map[NodeID]map[NodeID]bool{}
	for _, a := range s.ids {
		s.partitions[a] = map[NodeID]bool{}
		for _, b := range s.ids {
			if inA[a] != inA[b] {
				s.partitions[a][b] = true
			}
		}
	}
	s.tracef("partition %v | rest", groupA)
}

// Heal removes all partitions.
func (s *Sim) Heal() {
	s.partitions = map[NodeID]map[NodeID]bool{}
	s.tracef("heal")
}

// Crash stops a node entirely: it does not tick, send or receive.
func (s *Sim) Crash(id NodeID) {
	s.down[id] = true
	s.tracef("crash %s", id)
}

// Restart brings a crashed node back.
//
// Its log survives, because a Raft log is persistent state -- that is the
// entire point. Its volatile state (role, votes) does not.
func (s *Sim) Restart(id NodeID) {
	s.down[id] = false
	n := s.nodes[id]
	n.state = Follower
	n.leader = ""
	n.votes = map[NodeID]bool{}
	n.resetTimeout()
	s.tracef("restart %s", id)
}

func (s *Sim) blocked(from, to NodeID) bool {
	if s.down[from] || s.down[to] {
		return true
	}
	m, ok := s.partitions[from]
	return ok && m[to]
}

// Send injects messages into the simulated network. Exported so a component
// outside this package can use the simulation as its transport.
func (s *Sim) Send(msgs []Message) { s.send(msgs) }

// ProposeOn submits a command to one SPECIFIC node, which is what a test needs
// to model a partitioned controller: the interesting case is a node that
// believes it may decide and is wrong.
func (s *Sim) ProposeOn(id NodeID, data []byte) bool {
	n := s.nodes[id]
	if n == nil {
		return false
	}
	msgs, ok := n.Propose(data)
	if ok {
		s.send(msgs)
	}
	return ok
}

func (s *Sim) send(msgs []Message) {
	for _, m := range msgs {
		if s.blocked(m.From, m.To) {
			s.Dropped++
			continue
		}
		if s.DropRate > 0 && s.rng.Float64() < s.DropRate {
			s.Dropped++
			continue
		}
		delay := 1
		if s.MaxDelay > 1 {
			delay = 1 + s.rng.Intn(s.MaxDelay)
		}
		s.inflight = append(s.inflight, timed{at: s.tick + delay, seq: len(s.inflight), msg: m})
	}
}

// Step advances the simulation by one tick: deliver due messages, then tick
// every live node.
func (s *Sim) Step() {
	s.tick++

	// Collect messages due now.
	var due []timed
	var rest []timed
	for _, t := range s.inflight {
		if t.at <= s.tick {
			due = append(due, t)
		} else {
			rest = append(rest, t)
		}
	}
	s.inflight = rest

	// Deliver in a deterministic order. Reorder shuffles that order using the
	// run's RNG, which is still reproducible -- reordering is a real network
	// behaviour and a protocol that only works in-order is not correct.
	sort.Slice(due, func(i, j int) bool { return due[i].seq < due[j].seq })
	if s.Reorder && len(due) > 1 {
		s.rng.Shuffle(len(due), func(i, j int) { due[i], due[j] = due[j], due[i] })
	}

	for _, t := range due {
		if s.down[t.msg.To] {
			s.Dropped++
			continue
		}
		s.Delivered++
		s.send(s.nodes[t.msg.To].Step(t.msg))
	}
	for _, id := range s.ids {
		if s.down[id] {
			continue
		}
		s.send(s.nodes[id].Tick())
	}
}

// RunFor advances n ticks.
func (s *Sim) RunFor(n int) {
	for i := 0; i < n; i++ {
		s.Step()
	}
}

// Leaders returns every node currently believing it is leader, with its term.
//
// The safety property this exists to check: there must never be two leaders in
// the SAME term. Two leaders in different terms is normal and transient -- a
// deposed leader that has not yet learned it was replaced is still a leader in
// its own mind, which is precisely why fencing matters.
func (s *Sim) Leaders() map[Term][]NodeID {
	out := map[Term][]NodeID{}
	for _, id := range s.ids {
		n := s.nodes[id]
		if n.IsLeader() && !s.down[id] {
			out[n.currentTerm] = append(out[n.currentTerm], id)
		}
	}
	return out
}

// LeaderIn returns the single leader among the given nodes, if there is one.
func (s *Sim) LeaderIn(group []NodeID) *Node {
	for _, id := range group {
		if n := s.nodes[id]; n.IsLeader() && !s.down[id] {
			return n
		}
	}
	return nil
}

// AnyLeader returns some current leader, or nil.
func (s *Sim) AnyLeader() *Node { return s.LeaderIn(s.ids) }

// Propose submits a command to the current leader. It reports whether there was
// one to submit to.
func (s *Sim) Propose(data []byte) bool {
	l := s.AnyLeader()
	if l == nil {
		return false
	}
	msgs, ok := l.Propose(data)
	if !ok {
		return false
	}
	s.send(msgs)
	return true
}

// CheckNoSplitBrain asserts the core safety property and returns an error
// describing the violation.
func (s *Sim) CheckNoSplitBrain() error {
	for term, ids := range s.Leaders() {
		if len(ids) > 1 {
			return fmt.Errorf("SPLIT BRAIN: term %d has leaders %v", term, ids)
		}
	}
	return nil
}

// CheckLogsConsistent asserts the Log Matching property across all nodes: if
// two logs hold an entry at the same index with the same term, every preceding
// entry must be identical.
func (s *Sim) CheckLogsConsistent() error {
	for i, a := range s.ids {
		for _, b := range s.ids[i+1:] {
			la, lb := s.nodes[a].log, s.nodes[b].log
			max := la.LastIndex()
			if lb.LastIndex() < max {
				max = lb.LastIndex()
			}
			for idx := Index(1); idx <= max; idx++ {
				ta, _ := la.TermAt(idx)
				tb, _ := lb.TermAt(idx)
				if ta != tb {
					continue // divergent tail; fine as long as neither committed it
				}
				ea, _ := la.EntryAt(idx)
				eb, _ := lb.EntryAt(idx)
				if string(ea.Data) != string(eb.Data) {
					return fmt.Errorf("LOG MISMATCH: %s and %s differ at index %d, same term %d",
						a, b, idx, ta)
				}
			}
		}
	}
	return nil
}

// CheckCommittedNotLost asserts that every entry any node has COMMITTED is
// present, with the same data, in every node's log at that index -- or at least
// is never contradicted by a committed entry elsewhere.
//
// This is the State Machine Safety property: if a node has applied an entry at
// a given index, no other node may ever apply a different entry at that index.
func (s *Sim) CheckCommittedNotLost() error {
	committed := map[Index]Entry{}
	for _, id := range s.ids {
		n := s.nodes[id]
		for i := Index(1); i <= n.log.Commit(); i++ {
			e, ok := n.log.EntryAt(i)
			if !ok {
				return fmt.Errorf("%s claims commit %d but has no entry there", id, n.log.Commit())
			}
			if prev, seen := committed[i]; seen {
				if prev.Term != e.Term || string(prev.Data) != string(e.Data) {
					return fmt.Errorf("STATE MACHINE SAFETY: index %d committed as %v by one node and %v by %s",
						i, prev, e, id)
				}
				continue
			}
			committed[i] = e
		}
	}
	return nil
}

// CheckAll runs every invariant.
func (s *Sim) CheckAll() error {
	for _, f := range []func() error{s.CheckNoSplitBrain, s.CheckLogsConsistent, s.CheckCommittedNotLost} {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sim) tracef(format string, args ...any) {
	s.Trace = append(s.Trace, fmt.Sprintf("t=%d "+format, append([]any{s.tick}, args...)...))
}

// Tick returns the current logical time.
func (s *Sim) Tick() int { return s.tick }
