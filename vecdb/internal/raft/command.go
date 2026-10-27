package raft

import (
	"encoding/json"
	"fmt"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
)

// This file is what makes Raft useful here rather than an exercise: the
// replicated log carries CONTROL-PLANE commands -- who leads which shard --
// and nothing else.
//
// Point writes do not go through consensus. They go through leader/follower
// replication with an in-sync set, which is what Qdrant and Weaviate both
// independently converged on. Routing every vector through a majority round
// trip would buy a guarantee that the epoch fence and a synchronous in-sync set
// already provide, at the cost of a quorum on the hot path.
//
// What consensus buys is the one thing a single controller cannot do: a
// controller cannot detect its own isolation, so a partitioned one will promote
// a replacement for a leader that is perfectly healthy on the other side. A
// Raft-replicated map cannot, because a minority cannot commit.

// CmdKind identifies a control-plane command.
type CmdKind string

const (
	// CmdPromote installs a new leader for a shard at a new epoch.
	CmdPromote CmdKind = "promote"
	// CmdReadOnly marks a shard unavailable because no in-sync replica survived.
	CmdReadOnly CmdKind = "read_only"
	// CmdSetISR records the in-sync set, so promotion eligibility is durable
	// cluster state rather than a dead leader's private memory.
	CmdSetISR CmdKind = "set_isr"
)

// Command is one entry in the control-plane log.
type Command struct {
	Kind  CmdKind          `json:"kind"`
	Shard core.ShardID     `json:"shard"`
	Node  cluster.NodeID   `json:"node,omitempty"`
	Epoch core.Epoch       `json:"epoch,omitempty"`
	ISR   []cluster.NodeID `json:"isr,omitempty"`
	// Replicas is the shard's replica set after this command. Placement is
	// cluster state like any other: two controllers that disagree about who
	// holds a copy will make different promotion decisions later, so it goes
	// through the log rather than living in one controller's memory.
	Replicas []cluster.NodeID `json:"replicas,omitempty"`
}

// Encode serializes a command for the log.
func (c Command) Encode() []byte {
	b, err := json.Marshal(c)
	if err != nil {
		// Command is a closed struct of plain types; a marshal failure here
		// would be a programming error, not a runtime condition.
		panic(fmt.Sprintf("raft: encoding command: %v", err))
	}
	return b
}

// DecodeCommand parses a log entry.
func DecodeCommand(b []byte) (Command, error) {
	var c Command
	err := json.Unmarshal(b, &c)
	return c, err
}

// ShardState is the replicated state machine's view of one shard.
type ShardState struct {
	Leader   cluster.NodeID
	Epoch    core.Epoch
	ISR      []cluster.NodeID
	Replicas []cluster.NodeID
	ReadOnly bool
}

// StateMachine applies control-plane commands in commit order.
//
// It is a pure function of the committed log, which is the property that makes
// every replica converge: apply the same entries in the same order and you get
// the same map, with no coordination beyond the log itself.
type StateMachine struct {
	Shards map[core.ShardID]ShardState
	// Applied counts entries consumed, so the caller knows where it is.
	Applied int
}

// NewStateMachine returns an empty machine.
func NewStateMachine() *StateMachine {
	return &StateMachine{Shards: map[core.ShardID]ShardState{}}
}

// Apply consumes every entry the node has committed but not yet applied.
func (sm *StateMachine) Apply(n *Node) error {
	for sm.Applied < len(n.Applied) {
		raw := n.Applied[sm.Applied]
		sm.Applied++
		c, err := DecodeCommand(raw)
		if err != nil {
			return fmt.Errorf("raft: undecodable committed entry %d: %w", sm.Applied, err)
		}
		st := sm.Shards[c.Shard]
		switch c.Kind {
		case CmdPromote:
			// Refuse to move an epoch backwards even from the log. A committed
			// log should never contain such a command; if it does, that is a
			// bug worth failing loudly on rather than silently absorbing.
			if c.Epoch < st.Epoch {
				return fmt.Errorf("raft: committed promote moves shard %d epoch %d -> %d backwards",
					c.Shard, st.Epoch, c.Epoch)
			}
			st.Leader, st.Epoch, st.ReadOnly = c.Node, c.Epoch, false
			if c.Replicas != nil {
				st.Replicas = append([]cluster.NodeID(nil), c.Replicas...)
			}
		case CmdReadOnly:
			st.Leader, st.ReadOnly = "", true
			if c.Replicas != nil {
				st.Replicas = append([]cluster.NodeID(nil), c.Replicas...)
			}
			if c.Epoch > st.Epoch {
				st.Epoch = c.Epoch
			}
		case CmdSetISR:
			st.ISR = append([]cluster.NodeID(nil), c.ISR...)
		default:
			return fmt.Errorf("raft: unknown command kind %q", c.Kind)
		}
		sm.Shards[c.Shard] = st
	}
	return nil
}
