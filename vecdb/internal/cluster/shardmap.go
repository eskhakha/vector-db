package cluster

import (
	"fmt"
	"sort"
	"time"

	"github.com/eskhakha/vecdb/internal/core"
)

// ShardPlacement is who leads and who follows one shard.
type ShardPlacement struct {
	// Epoch is the leadership fence. It increments on every leadership change
	// and never decreases. A message stamped below the current epoch is
	// refused, which is what stops a deposed leader corrupting the new log.
	Epoch core.Epoch
	// Leader is empty when the shard is read-only because no in-sync replica
	// survived. That state is deliberate: unavailability is chosen over
	// promoting a replica that would silently drop acknowledged writes.
	Leader   NodeID
	Replicas []NodeID
	// ReadOnly records why a shard has no leader, so the condition is visible
	// rather than inferred from an empty field.
	ReadOnly bool
}

// ShardMap is the cluster's authoritative placement state.
//
// It is versioned, and the version is what makes a stale client cheap to
// correct: every request carries the caller's version, and a node that does not
// lead the requested shard replies with the current placement instead of an
// error the client cannot act on.
type ShardMap struct {
	Version uint64
	Shards  map[core.ShardID]ShardPlacement
	Nodes   map[NodeID]NodeState
}

// NewShardMap builds an initial map with round-robin leadership.
//
// Leadership is spread so each node leads roughly S/N shards, balancing write
// load; a shard's replicas never include its leader, since that would make the
// replication factor a lie.
func NewShardMap(numShards int, nodes []NodeID, rf int) (*ShardMap, error) {
	if numShards <= 0 {
		return nil, fmt.Errorf("cluster: numShards must be positive")
	}
	if rf < 1 || rf > len(nodes) {
		return nil, fmt.Errorf("cluster: rf=%d impossible with %d nodes", rf, len(nodes))
	}
	sorted := append([]NodeID(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	m := &ShardMap{
		Version: 1,
		Shards:  make(map[core.ShardID]ShardPlacement, numShards),
		Nodes:   make(map[NodeID]NodeState, len(sorted)),
	}
	for _, n := range sorted {
		m.Nodes[n] = NodeAlive
	}
	for s := 0; s < numShards; s++ {
		leaderIdx := s % len(sorted)
		p := ShardPlacement{Epoch: 1, Leader: sorted[leaderIdx]}
		for r := 1; r < rf; r++ {
			p.Replicas = append(p.Replicas, sorted[(leaderIdx+r)%len(sorted)])
		}
		m.Shards[core.ShardID(s)] = p
	}
	return m, nil
}

// Clone returns a deep copy, so a caller can build a new version without
// mutating the one other goroutines are reading.
func (m *ShardMap) Clone() *ShardMap {
	c := &ShardMap{
		Version: m.Version,
		Shards:  make(map[core.ShardID]ShardPlacement, len(m.Shards)),
		Nodes:   make(map[NodeID]NodeState, len(m.Nodes)),
	}
	for k, v := range m.Shards {
		p := v
		p.Replicas = append([]NodeID(nil), v.Replicas...)
		c.Shards[k] = p
	}
	for k, v := range m.Nodes {
		c.Nodes[k] = v
	}
	return c
}

// ShardsLedBy returns the shards a node leads, sorted.
func (m *ShardMap) ShardsLedBy(n NodeID) []core.ShardID {
	var out []core.ShardID
	for id, p := range m.Shards {
		if p.Leader == n {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Lease is a time-bounded right to act as leader for a shard.
//
// A lease bounds the STALE-READ window, which is the one thing epoch fencing
// cannot close. Fencing stops a deposed leader corrupting the new leader's log,
// because that requires sending a message which is refused. It does not stop
// that node answering a read from its own memory, because reading requires
// talking to nobody. A leader that may only serve strong reads while holding an
// unexpired lease cannot do that past the lease.
//
// The assumption is a bounded clock DRIFT RATE, not synchronised clocks. That
// is much weaker, but it is not nothing, and pretending otherwise is how
// systems acquire holes nobody documented.
type Lease struct {
	Holder  NodeID
	Epoch   core.Epoch
	Expires time.Time
}

// Valid reports whether holder may serve strong reads for epoch at time now.
func (l Lease) Valid(now time.Time, holder NodeID, epoch core.Epoch) bool {
	return l.Holder == holder && l.Epoch == epoch && now.Before(l.Expires)
}

// Remaining returns how long the lease still has to run, or zero.
func (l Lease) Remaining(now time.Time) time.Duration {
	if now.After(l.Expires) {
		return 0
	}
	return l.Expires.Sub(now)
}
