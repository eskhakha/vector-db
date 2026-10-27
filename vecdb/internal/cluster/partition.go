// Package cluster holds partitioning and the shard map: the state that says
// which shard a key belongs to, and which node hosts that shard.
//
// Keeping those two questions separate is the whole design. The first is a pure
// function requiring no cluster state, identical on every node and every client,
// and it never changes for the life of the cluster. The second is versioned
// cluster state that changes on failover and rebalance. Tangle them and moving
// data means moving key ranges; separate them and it means moving a shard, an
// object with an identity, a log and a snapshot.
package cluster

import (
	"fmt"
	"hash/fnv"

	"github.com/eskhakha/vecdb/internal/core"
)

// Partitioner maps a vector ID to a logical shard.
type Partitioner struct {
	numShards uint32
}

// NewPartitioner returns a partitioner over n logical shards.
//
// n is fixed for the life of the cluster. That is a real, permanent ceiling --
// changing it later requires a dump and reload -- and it is the price of never
// having to move individual keys.
func NewPartitioner(n int) (*Partitioner, error) {
	if n <= 0 {
		return nil, fmt.Errorf("cluster: numShards must be positive, got %d", n)
	}
	return &Partitioner{numShards: uint32(n)}, nil
}

// NumShards returns the fixed shard count.
func (p *Partitioner) NumShards() int { return int(p.numShards) }

// Shard returns the shard that owns id.
//
// FNV-1a is used rather than Go's hash/maphash. maphash is deliberately seeded
// per process, so the same key would land on different shards on different
// nodes -- a catastrophic and very confusing bug. The requirement here is not
// speed or cryptographic strength; it is that the function be STABLE across
// processes, machines and releases.
func (p *Partitioner) Shard(id string) core.ShardID {
	h := fnv.New64a()
	_, _ = h.Write([]byte(id))
	return core.ShardID(h.Sum64() % uint64(p.numShards))
}
