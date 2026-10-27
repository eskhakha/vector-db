// Package replication implements leader/follower replication with a
// synchronous in-sync replica set.
//
// The transport is an interface with an in-process implementation, which is
// what makes this testable. Dropping a message, delaying it, or partitioning
// two nodes becomes a function call against a seeded RNG rather than a
// container orchestration problem, and every failure reproduces exactly from
// its seed. That is how FoundationDB and TigerBeetle test this class of code,
// and it is worth far more here than real sockets would be.
package replication

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
	"github.com/eskhakha/vecdb/internal/wal"
)

// NodeID names a node in the cluster.
//
// An alias, not a distinct type: the controller and the replication layer must
// be talking about the same nodes, and two structurally identical ID types that
// need converting at every boundary is how they quietly stop being the same.
type NodeID = cluster.NodeID

// ErrUnreachable is returned when a message cannot be delivered. Note what it
// does NOT tell you: whether the peer is dead, slow, or merely partitioned.
// That ambiguity is inherent, not a limitation of this implementation.
var ErrUnreachable = errors.New("replication: peer unreachable")

// AppendRequest ships a contiguous run of log records to a follower.
type AppendRequest struct {
	Shard core.ShardID
	// Epoch is the leadership fence. A follower refuses anything below the
	// highest epoch it has seen, which is what stops a deposed leader from
	// corrupting the current leader's log.
	Epoch     core.Epoch
	Records   []*wal.Record
	CommitLSN core.LSN
}

// AppendResponse reports what the follower did.
type AppendResponse struct {
	Code    store.IngestCode
	Applied core.LSN
	Epoch   core.Epoch
}

// SnapshotRequest ships a full state image to a follower whose gap is too
// large to fill from the leader's log.
type SnapshotRequest struct {
	Shard core.ShardID
	Epoch core.Epoch
	LSN   core.LSN
	Data  []byte
}

// Transport delivers replication messages.
//
// `from` is part of the signature because a partition isolates a node in BOTH
// directions. A transport that only knows the recipient cannot model a
// partitioned sender, and would let an isolated leader keep replicating -- the
// single most important case the whole failure-handling design exists for.
type Transport interface {
	Append(ctx context.Context, from, to NodeID, req *AppendRequest) (*AppendResponse, error)
	Snapshot(ctx context.Context, from, to NodeID, req *SnapshotRequest) (*AppendResponse, error)
}

// Handler is the follower side of the protocol.
type Handler interface {
	HandleAppend(req *AppendRequest) *AppendResponse
	HandleSnapshot(req *SnapshotRequest) *AppendResponse
}

// FaultConfig controls injected failures. The zero value injects nothing.
type FaultConfig struct {
	// DropRate is the probability in [0,1] that a message is silently lost.
	DropRate float64
	// Latency is added to every delivery.
	Latency time.Duration
	// Seed makes injection reproducible.
	Seed int64
}

// InProcTransport delivers messages by direct call, with optional faults.
type InProcTransport struct {
	mu         sync.Mutex
	handlers   map[NodeID]Handler
	partitions map[NodeID]bool
	faults     FaultConfig
	rng        *rand.Rand
	// Counters make the injected faults observable, so a test can assert that
	// it actually exercised the path it meant to.
	Sent    int
	Dropped int
}

// NewInProcTransport returns a transport with no peers registered.
func NewInProcTransport(f FaultConfig) *InProcTransport {
	return &InProcTransport{
		handlers:   make(map[NodeID]Handler),
		partitions: make(map[NodeID]bool),
		faults:     f,
		rng:        rand.New(rand.NewSource(f.Seed)),
	}
}

// Register adds a follower endpoint.
func (t *InProcTransport) Register(id NodeID, h Handler) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.handlers[id] = h
}

// Partition isolates or restores a node. An isolated node is still running and
// still believes everything it believed a moment ago; it simply cannot be
// reached. That is the case that makes failure detection undecidable.
func (t *InProcTransport) Partition(id NodeID, isolated bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.partitions[id] = isolated
}

// Stats returns delivery counters.
func (t *InProcTransport) Stats() (sent, dropped int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Sent, t.Dropped
}

func (t *InProcTransport) route(from, to NodeID) (Handler, error) {
	t.mu.Lock()
	t.Sent++
	if t.partitions[to] || t.partitions[from] {
		t.Dropped++
		side := to
		if t.partitions[from] {
			side = from
		}
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s is partitioned", ErrUnreachable, side)
	}
	if t.faults.DropRate > 0 && t.rng.Float64() < t.faults.DropRate {
		t.Dropped++
		t.mu.Unlock()
		return nil, fmt.Errorf("%w: %s (dropped)", ErrUnreachable, to)
	}
	h, ok := t.handlers[to]
	t.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: %s unknown", ErrUnreachable, to)
	}
	return h, nil
}

func (t *InProcTransport) delay(ctx context.Context) error {
	if t.faults.Latency <= 0 {
		return ctx.Err()
	}
	select {
	case <-time.After(t.faults.Latency):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Append delivers an append request.
func (t *InProcTransport) Append(ctx context.Context, from, to NodeID, req *AppendRequest) (*AppendResponse, error) {
	h, err := t.route(from, to)
	if err != nil {
		return nil, err
	}
	if err := t.delay(ctx); err != nil {
		return nil, err
	}
	return h.HandleAppend(req), nil
}

// Snapshot delivers a snapshot request.
func (t *InProcTransport) Snapshot(ctx context.Context, from, to NodeID, req *SnapshotRequest) (*AppendResponse, error) {
	h, err := t.route(from, to)
	if err != nil {
		return nil, err
	}
	if err := t.delay(ctx); err != nil {
		return nil, err
	}
	return h.HandleSnapshot(req), nil
}
