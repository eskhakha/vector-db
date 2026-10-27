package index

import (
	"sort"

	"github.com/eskhakha/vecdb/internal/core"
)

// TopK maintains the best K results seen so far in a bounded binary heap whose
// root is the WORST retained element. Push is O(log K) only when the candidate
// beats the root; otherwise it is a single comparison.
//
// The alternative — collect all N scores then partially sort — allocates an
// N-element slice per query. At N=1M that is 8 MB of allocation and
// initialization before a single distance is computed. This is the classic
// mistake in a from-scratch vector index.
type TopK struct {
	k      int
	metric core.Metric
	h      []core.Result
}

// NewTopK returns a collector for the k best results under metric m.
func NewTopK(k int, m core.Metric) *TopK {
	if k < 0 {
		k = 0
	}
	return &TopK{k: k, metric: m, h: make([]core.Result, 0, k)}
}

// worse reports whether a ranks below b (i.e. a is a worse hit than b).
func (t *TopK) worse(a, b core.Result) bool { return core.Less(b, a, t.metric) }

// Push offers a candidate.
func (t *TopK) Push(r core.Result) {
	if t.k == 0 {
		return
	}
	if len(t.h) < t.k {
		t.h = append(t.h, r)
		t.up(len(t.h) - 1)
		return
	}
	// t.h[0] is the worst retained element. Replace it only if r beats it.
	if t.worse(r, t.h[0]) || r == t.h[0] {
		return
	}
	t.h[0] = r
	t.down(0)
}

// Len returns how many results are currently held.
func (t *TopK) Len() int { return len(t.h) }

// Results returns the retained results in best-first order. It sorts a copy,
// so the collector stays usable afterwards.
func (t *TopK) Results() []core.Result {
	out := make([]core.Result, len(t.h))
	copy(out, t.h)
	sort.Slice(out, func(i, j int) bool { return core.Less(out[i], out[j], t.metric) })
	return out
}

func (t *TopK) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		// Max-heap on "worseness": the worst element bubbles to the root.
		if !t.worse(t.h[i], t.h[parent]) {
			break
		}
		t.h[i], t.h[parent] = t.h[parent], t.h[i]
		i = parent
	}
}

func (t *TopK) down(i int) {
	n := len(t.h)
	for {
		l, r := 2*i+1, 2*i+2
		worst := i
		if l < n && t.worse(t.h[l], t.h[worst]) {
			worst = l
		}
		if r < n && t.worse(t.h[r], t.h[worst]) {
			worst = r
		}
		if worst == i {
			return
		}
		t.h[i], t.h[worst] = t.h[worst], t.h[i]
		i = worst
	}
}

// MergeTopK merges per-shard result lists into a global top-K.
//
// This is exact for exact per-shard search, and the proof is short: if x is in
// the global top-K, at most K-1 items in the entire dataset beat x; a shard is
// a subset of the dataset, so at most K-1 items on x's own shard beat x;
// therefore x is in that shard's local top-K. K per shard is the tight bound.
//
// The proof breaks under approximate per-shard search, which is one reason
// this system keeps the index exact.
func MergeTopK(lists [][]core.Result, k int, m core.Metric) []core.Result {
	t := NewTopK(k, m)
	seen := make(map[string]struct{})
	for _, l := range lists {
		for _, r := range l {
			// An ID lives on exactly one shard. A duplicate across shards means
			// a partitioner or placement bug, so dedupe defensively.
			if _, dup := seen[r.ID]; dup {
				continue
			}
			seen[r.ID] = struct{}{}
			t.Push(r)
		}
	}
	return t.Results()
}
