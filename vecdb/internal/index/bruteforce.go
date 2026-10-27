package index

import (
	"context"

	"github.com/eskhakha/vecdb/internal/core"
)

// Index is the interface the store consumes. It has one implementation today.
// The interface exists because Search must be cancellable and because the
// store should not care how candidates are produced.
type Index interface {
	Add(v *core.Vector)
	Remove(id string)
	Search(ctx context.Context, q []float32, k int) ([]core.Result, error)
	Len() int
	// Snapshot returns an immutable view of the candidates, for a reader that
	// will scan without holding the writer's lock. The returned slice must
	// never be mutated by the index afterwards -- that promise is what makes
	// lock-free reads safe, and it is the whole contract.
	Snapshot() []*core.Vector
}

// BruteForce is an exact index: it scans every live vector.
//
// Cost is O(N*D) per query and it is memory-bandwidth-bound, not
// compute-bound: the loop does one multiply-add per two loaded float32s, so at
// N=1e6, D=128 a query streams 512 MB. That is why compression speeds up
// brute force by moving fewer bytes, not by doing less arithmetic.
//
// BruteForce is not safe for concurrent use; the owning shard serializes access.
type BruteForce struct {
	dim    int
	metric core.Metric
	// ids and vecs are parallel slices. Storing them separately keeps the scan
	// loop touching only float data.
	ids  []string
	vecs []*core.Vector
	pos  map[string]int
}

var _ Index = (*BruteForce)(nil)

// NewBruteForce returns an empty exact index.
func NewBruteForce(dim int, m core.Metric) *BruteForce {
	return &BruteForce{dim: dim, metric: m, pos: make(map[string]int)}
}

// Add inserts or replaces a vector. The caller retains ownership of v.
func (b *BruteForce) Add(v *core.Vector) {
	if i, ok := b.pos[v.ID]; ok {
		b.vecs[i] = v
		return
	}
	b.pos[v.ID] = len(b.vecs)
	b.ids = append(b.ids, v.ID)
	b.vecs = append(b.vecs, v)
}

// Remove deletes a vector by ID. It is a no-op if the ID is absent.
//
// Removal swaps the last element into the hole rather than shifting, so it is
// O(1). That reorders the scan, which is exactly why the total order in
// core.Less breaks ties by ID: without it, results would depend on insertion
// and deletion history.
func (b *BruteForce) Remove(id string) {
	i, ok := b.pos[id]
	if !ok {
		return
	}
	last := len(b.vecs) - 1
	if i != last {
		b.vecs[i] = b.vecs[last]
		b.ids[i] = b.ids[last]
		b.pos[b.ids[i]] = i
	}
	b.vecs[last] = nil
	b.vecs = b.vecs[:last]
	b.ids = b.ids[:last]
	delete(b.pos, id)
}

// Len returns the number of live vectors.
func (b *BruteForce) Len() int { return len(b.vecs) }

// Snapshot copies the candidate slice so a reader can scan it with no lock.
//
// Only the POINTERS are copied, not the vectors -- so this is O(N) pointer
// writes (~100 us at N=100k), not O(N*D). That cost is paid by the writer, and
// it is the trade copy-on-write makes: the writer does bounded extra work so
// the reader does none.
//
// The elements must be treated as immutable from here. Add() replaces a slot
// in place, which would be visible to a reader already scanning an older
// snapshot -- benign for a value that is only read, and the reason this returns
// a copy of the slice rather than the slice itself.
func (b *BruteForce) Snapshot() []*core.Vector {
	out := make([]*core.Vector, len(b.vecs))
	copy(out, b.vecs)
	return out
}

// SearchSnapshot scans a snapshot with no lock held. It is the same loop as
// Search, which calls it, so there is no second implementation to diverge.
func SearchSnapshot(ctx context.Context, vecs []*core.Vector, q []float32, k, dim int, m core.Metric) ([]core.Result, error) {
	if len(q) != dim {
		return nil, core.ErrDimMismatch
	}
	if k <= 0 || len(vecs) == 0 {
		return nil, nil
	}
	t := NewTopK(k, m)
	for i, v := range vecs {
		if i%cancelCheckStride == 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
		}
		if v.Deleted {
			continue
		}
		t.Push(core.Result{ID: v.ID, Score: Score(q, v, m)})
	}
	return t.Results(), nil
}

// cancelCheckStride bounds how often the scan checks for cancellation. Checking
// every iteration would cost more than the distance computation; never checking
// would make a long scan uncancellable.
const cancelCheckStride = 4096

// Search returns the k best matches for q, or ctx.Err() if cancelled.
func (b *BruteForce) Search(ctx context.Context, q []float32, k int) ([]core.Result, error) {
	return SearchSnapshot(ctx, b.vecs, q, k, b.dim, b.metric)
}
