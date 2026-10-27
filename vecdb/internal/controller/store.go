package controller

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/raft"
)

// ErrNotDecider means this controller may not change cluster state right now.
//
// This error is the entire point of putting a Store between the controller and
// the shard map. A single controller can never return it, because it is the
// authority and cannot detect its own isolation. A Raft-backed controller
// returns it whenever it is not the leader of a live majority -- which is
// exactly the case where a lone controller would have promoted a replacement
// for a leader that was perfectly healthy on the other side of a partition.
var ErrNotDecider = errors.New("controller: not permitted to decide (no majority)")

// Store is where a controller's decisions become cluster state.
//
// The controller stays a pure decision-maker: it reads a map, works out what
// should change, and hands the changes here. Whether those changes are allowed
// to take effect is a separate question with two different answers, which is
// why it is a separate type.
type Store interface {
	// Map returns the current placement.
	Map() *cluster.ShardMap
	// Propose submits a set of changes. A nil error means they were accepted;
	// ErrNotDecider means this controller is not entitled to decide and NOTHING
	// was changed.
	Propose(cmds []raft.Command) error
	// SetNodeState records a liveness observation. Node liveness is a LOCAL
	// observation, not a cluster-wide fact -- two controllers can legitimately
	// disagree about whether a node is reachable, and forcing agreement through
	// the log would turn every network blip into a cluster-wide decision. So
	// this never goes through consensus.
	SetNodeState(n cluster.NodeID, st cluster.NodeState)
	// Mode names the implementation, for logs and demo output.
	Mode() string
}

// ---------- local store: the single-controller behaviour ----------

// LocalStore keeps the shard map in this process's memory and applies every
// decision immediately.
//
// It never refuses. That is not a bug in the implementation; it is the property
// a single controller has, and the reason a single controller is unsafe: it
// cannot detect its own isolation, so from inside a partition "I cannot reach
// the other nodes" and "the other nodes are dead" are the same observation.
type LocalStore struct {
	mu sync.Mutex
	m  *cluster.ShardMap
}

// NewLocalStore wraps a map.
func NewLocalStore(m *cluster.ShardMap) *LocalStore { return &LocalStore{m: m} }

// Mode identifies this store.
func (s *LocalStore) Mode() string { return "local" }

// Map returns a copy of the placement.
func (s *LocalStore) Map() *cluster.ShardMap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m.Clone()
}

// Propose applies every command unconditionally.
func (s *LocalStore) Propose(cmds []raft.Command) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cmds {
		applyToMap(s.m, c)
	}
	return nil
}

// SetNodeState records a liveness observation.
func (s *LocalStore) SetNodeState(n cluster.NodeID, st cluster.NodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.Nodes[n] = st
}

// ---------- raft store: decisions must commit ----------

// RaftTransport carries Raft messages. In tests and in the demo this is the
// deterministic simulation; in a deployed system it would be an RPC client.
type RaftTransport interface {
	Send(msgs []raft.Message)
}

// RaftStore backs the shard map with a replicated log.
//
// The map is no longer a variable this controller owns. It is the state machine
// derived from the committed log, which is a pure function of that log, so
// every replica converges on the same map with no coordination beyond the log
// itself.
type RaftStore struct {
	mu    sync.Mutex
	node  *raft.Node
	sm    *raft.StateMachine
	tr    RaftTransport
	nodes map[cluster.NodeID]cluster.NodeState
	// version increments on every applied command so callers can tell the map
	// moved without diffing it.
	version uint64
}

// NewRaftStore builds a store over a Raft node.
func NewRaftStore(n *raft.Node, tr RaftTransport, nodes []cluster.NodeID) *RaftStore {
	s := &RaftStore{
		node: n, sm: raft.NewStateMachine(), tr: tr,
		nodes: make(map[cluster.NodeID]cluster.NodeState, len(nodes)),
	}
	for _, id := range nodes {
		s.nodes[id] = cluster.NodeAlive
	}
	return s
}

// Mode identifies this store.
func (s *RaftStore) Mode() string { return "raft" }

// Refresh applies everything the node has committed since the last call.
//
// Commit is asynchronous: Propose returning nil means "accepted for
// replication", not "in effect". The map moves when the entry commits, which
// is the honest shape of the guarantee and worth not hiding behind a
// synchronous-looking API.
func (s *RaftStore) Refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := s.sm.Applied
	if err := s.sm.Apply(s.node); err != nil {
		// A committed log that does not decode, or that moves an epoch
		// backwards, is a bug worth failing loudly on rather than absorbing.
		panic(fmt.Sprintf("controller: applying committed control-plane log: %v", err))
	}
	s.version += uint64(s.sm.Applied - before)
}

// Map returns the placement derived from the committed log.
func (s *RaftStore) Map() *cluster.ShardMap {
	s.Refresh()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := &cluster.ShardMap{
		Version: s.version,
		Shards:  make(map[core.ShardID]cluster.ShardPlacement, len(s.sm.Shards)),
		Nodes:   make(map[cluster.NodeID]cluster.NodeState, len(s.nodes)),
	}
	for id, st := range s.sm.Shards {
		m.Shards[id] = cluster.ShardPlacement{
			Epoch:    st.Epoch,
			Leader:   st.Leader,
			Replicas: append([]cluster.NodeID(nil), st.Replicas...),
			ReadOnly: st.ReadOnly,
		}
	}
	for id, st := range s.nodes {
		m.Nodes[id] = st
	}
	return m
}

// Propose submits decisions to the log.
//
// It refuses unless this node is the Raft leader. That check is necessary but
// not sufficient on its own -- a deposed leader can still believe it leads --
// and the sufficient part is that the entries will simply never commit from a
// minority. Both matter: the check makes the refusal fast and visible, the
// commit rule makes it correct.
func (s *RaftStore) Propose(cmds []raft.Command) error {
	if !s.node.IsLeader() {
		return fmt.Errorf("%w: %s is %s", ErrNotDecider, s.node.ID(), s.node.State())
	}
	for _, c := range cmds {
		msgs, ok := s.node.Propose(c.Encode())
		if !ok {
			return ErrNotDecider
		}
		s.tr.Send(msgs)
	}
	return nil
}

// SetNodeState records a local liveness observation.
func (s *RaftStore) SetNodeState(n cluster.NodeID, st cluster.NodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[n] = st
}

// Node exposes the underlying Raft node, for tests and metrics.
func (s *RaftStore) Node() *raft.Node { return s.node }

// ---------- shared application logic ----------

// applyToMap is the same transformation the Raft state machine performs,
// applied directly. Keeping one function means the two stores cannot drift
// into disagreeing about what a command means.
func applyToMap(m *cluster.ShardMap, c raft.Command) {
	p := m.Shards[c.Shard]
	switch c.Kind {
	case raft.CmdPromote:
		p.Leader, p.Epoch, p.ReadOnly = c.Node, c.Epoch, false
		if c.Replicas != nil {
			p.Replicas = append([]cluster.NodeID(nil), c.Replicas...)
			sort.Slice(p.Replicas, func(i, j int) bool { return p.Replicas[i] < p.Replicas[j] })
		}
	case raft.CmdReadOnly:
		p.Leader, p.ReadOnly = "", true
		if c.Epoch > p.Epoch {
			p.Epoch = c.Epoch
		}
		if c.Replicas != nil {
			p.Replicas = append([]cluster.NodeID(nil), c.Replicas...)
		}
	case raft.CmdSetISR:
		// The in-sync set is carried for promotion eligibility. The local map
		// type has no field for it, so nothing to do here.
		return
	}
	m.Shards[c.Shard] = p
	m.Version++
}
