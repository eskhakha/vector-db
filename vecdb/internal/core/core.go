// Package core holds the types every other package shares. It imports nothing
// from this module, by design: dependencies point inward.
package core

import (
	"errors"
	"fmt"
	"math"
)

// ShardID identifies a logical partition, in [0, NumShards).
type ShardID uint32

// LSN is a log sequence number: shard-local, strictly increasing, gapless,
// assigned by that shard's leader. It doubles as a record's version, which is
// why there is no separate Version concept.
type LSN uint64

// Epoch fences a leadership term for a shard. It increments on every
// leadership change. A message stamped with a stale epoch is rejected.
//
// This is Elasticsearch's "primary term" under a different name.
type Epoch uint64

// Metric selects the similarity function.
type Metric uint8

const (
	// Cosine ranks by cosine similarity. Higher is better.
	Cosine Metric = iota
	// L2Squared ranks by squared Euclidean distance. Lower is better.
	// Squared, not plain L2: sqrt is monotone so it cannot change the ranking,
	// and skipping it saves a transcendental per candidate.
	L2Squared
)

func (m Metric) String() string {
	switch m {
	case Cosine:
		return "cosine"
	case L2Squared:
		return "l2sq"
	default:
		return fmt.Sprintf("metric(%d)", uint8(m))
	}
}

// HigherIsBetter reports whether a larger score means a closer match.
func (m Metric) HigherIsBetter() bool { return m == Cosine }

// Sentinel errors. Callers inspect with errors.Is.
var (
	ErrNotFound      = errors.New("vecdb: not found")
	ErrDimMismatch   = errors.New("vecdb: dimension mismatch")
	ErrInvalidID     = errors.New("vecdb: invalid id")
	ErrNotLeader     = errors.New("vecdb: not leader for shard")
	ErrStaleEpoch    = errors.New("vecdb: stale epoch")
	ErrShardReadOnly = errors.New("vecdb: shard is read-only")
	ErrLSNGap        = errors.New("vecdb: lsn gap in log")
	ErrCorruptRecord = errors.New("vecdb: corrupt record")
	ErrClosed        = errors.New("vecdb: closed")
)

// MaxIDLen bounds the client-supplied ID. Unbounded IDs are a denial-of-service
// vector against a log reader that must trust its own length prefixes.
const MaxIDLen = 256

// Vector is the unit of storage: one per ID per shard.
type Vector struct {
	ID        string
	Embedding []float32
	// Norm is the precomputed L2 norm. Cosine similarity is
	// dot(q,v) / (|q|*|v|); |q| is constant across a query so it cannot change
	// the ranking and is factored out. Precomputing |v| at insert turns the
	// inner loop into one dot product plus one divide.
	Norm     float32
	Metadata map[string]string
	// Version is the LSN of the write that produced this record. Versions are
	// therefore totally ordered within a shard and directly usable as a
	// replication cursor.
	Version LSN
	// Deleted marks a tombstone. A delete is a write, not a removal: the log
	// must record it so replay reproduces it, and removing outright would let a
	// replayed older insert resurrect the key.
	Deleted   bool
	CreatedAt int64
	UpdatedAt int64
}

// Clone returns a deep copy. The store hands clones to callers so that a
// reader cannot mutate committed state.
func (v *Vector) Clone() *Vector {
	if v == nil {
		return nil
	}
	c := *v
	if v.Embedding != nil {
		c.Embedding = make([]float32, len(v.Embedding))
		copy(c.Embedding, v.Embedding)
	}
	if v.Metadata != nil {
		c.Metadata = make(map[string]string, len(v.Metadata))
		for k, val := range v.Metadata {
			c.Metadata[k] = val
		}
	}
	return &c
}

// Validate checks a record against the collection dimension. It returns a
// wrapped sentinel so callers can branch with errors.Is.
//
// This is deliberately a real check and not an assert: validation that
// protects memory safety must survive an optimized build.
func (v *Vector) Validate(dim int) error {
	if v.ID == "" {
		return fmt.Errorf("%w: empty", ErrInvalidID)
	}
	if len(v.ID) > MaxIDLen {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrInvalidID, len(v.ID), MaxIDLen)
	}
	if !v.Deleted && len(v.Embedding) != dim {
		return fmt.Errorf("%w: got %d, want %d", ErrDimMismatch, len(v.Embedding), dim)
	}
	return nil
}

// ComputeNorm sets Norm from Embedding.
func (v *Vector) ComputeNorm() {
	v.Norm = Norm(v.Embedding)
}

// Norm returns the L2 norm of x.
func Norm(x []float32) float32 {
	var sum float64
	for _, f := range x {
		sum += float64(f) * float64(f)
	}
	return float32(math.Sqrt(sum))
}

// Result is one scored hit.
type Result struct {
	ID    string
	Score float32
}

// Less orders results best-first for the given metric, breaking ties by ID.
//
// The tie-break is not cosmetic. Without a total order, two shards can order
// equal scores differently, which makes distributed results non-deterministic
// and therefore untestable.
func Less(a, b Result, m Metric) bool {
	if a.Score != b.Score {
		if m.HigherIsBetter() {
			return a.Score > b.Score
		}
		return a.Score < b.Score
	}
	return a.ID < b.ID
}
