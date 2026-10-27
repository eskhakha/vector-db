// Package index provides the vector index. There is exactly one
// implementation, brute force, and that is a deliberate choice: an exact index
// is the oracle every other component in the system is tested against.
package index

import "github.com/eskhakha/vecdb/internal/core"

// Dot returns the inner product of a and b.
//
// The re-slice below is not decoration. It lets the compiler prove both slices
// have the same length and eliminate the per-iteration bounds check, which is
// typically worth 20-30% on this loop. Verify with:
//
//	go build -gcflags="-d=ssa/check_bce/debug=1" ./internal/index
func Dot(a, b []float32) float32 {
	if len(a) != len(b) {
		panic("index: Dot on unequal lengths")
	}
	b = b[:len(a)]
	var sum float32
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}

// L2Sq returns the squared Euclidean distance between a and b.
func L2Sq(a, b []float32) float32 {
	if len(a) != len(b) {
		panic("index: L2Sq on unequal lengths")
	}
	b = b[:len(a)]
	var sum float32
	for i := range a {
		d := a[i] - b[i]
		sum += d * d
	}
	return sum
}

// CosineScore returns cosine similarity given a precomputed vector norm.
//
// The query norm is intentionally NOT applied: it is identical for every
// candidate in a query and so cannot change the ordering. Callers that need a
// true cosine value in [-1, 1] must divide by |q| themselves.
func CosineScore(q, v []float32, vNorm float32) float32 {
	if vNorm == 0 {
		return 0
	}
	return Dot(q, v) / vNorm
}

// Score computes the ranking score of v against query q under the metric.
func Score(q []float32, v *core.Vector, m core.Metric) float32 {
	switch m {
	case core.Cosine:
		return CosineScore(q, v.Embedding, v.Norm)
	case core.L2Squared:
		return L2Sq(q, v.Embedding)
	default:
		panic("index: unknown metric")
	}
}
