# 3. Brute-force search and distance kernels

> The flat index compares the query against every vector, which makes it the only index that is
> always exactly right, the ground-truth oracle for everything else, and — because the distance
> kernel is memory-bandwidth-bound rather than compute-bound — an optimization problem about moving
> bytes rather than about doing less arithmetic.

## The problem

Every approximate index exists to avoid the full scan, but you cannot skip building it, for three
independent reasons:

1. **It is the correctness oracle.** recall@K is defined against the exact top-K, and the only way to
   get the exact top-K is to compute it ([the search problem](02-the-search-problem.md)). Without a
   flat path you cannot tell whether your HNSW graph is at 0.98 recall or 0.6.
2. **It is the right answer at small `N`.** FAISS: for 1,000–10,000 searches use flat, because "the
   index building time will not be amortized by the search time" [1]; and when exact results are
   required only the flat indexes guarantee precision — flat "does not compress the vectors, but does
   not add overhead" [1].
3. **It is the inner loop of every other index.** IVF scans the probed cells; HNSW evaluates
   distances to candidate neighbours. Both run the kernel you write here, so a slow kernel makes
   every index slow and no amount of graph tuning recovers it.

## How it works

### The scan

```
for i in 0..N:
    d[i] = distance(q, V[i])
select K smallest of d
```

`O(N·D)` for the distances, plus selection. Embarrassingly parallel across `i`. Use L2² rather than
L2 internally: the ordering is preserved, so "the k nearest under L2 are the same as the k nearest
under L2², so implementations often compute L2² internally," avoiding a square root per pair [2].

### Why this is a memory problem, not a math problem

Work out the arithmetic intensity — FLOPs performed per byte loaded from memory.

For a dot product over normalized vectors, the per-dimension work is one fused multiply-add:
`acc += q[j] * v[j]`. That is 2 FLOPs. In the naive framing where both operands stream from memory,
you load two `float32` — 8 bytes — per FMA: **0.25 FLOP/byte**. In a real scan the query vector is
tiny and stays resident in registers or L1 across all `N` comparisons, so only the database vector
streams: 4 bytes per FMA, **0.5 FLOP/byte**. Squared L2 does one subtract and one FMA per dimension,
3 FLOPs per 4 bytes, **0.75 FLOP/byte**.

Modern cores retire several FMAs per cycle across wide vector registers while sustaining far less
than one cache line per cycle from DRAM, so the kernel is starved: the ALUs finish before the next
bytes arrive and throughput is set by bandwidth. Elasticsearch's SIMD team states the conclusion from
measurement: "these kernels are often limited by how fast they can load data, more than how fast they
can compute" [3].

This single fact reorganizes the entire optimization strategy:

- **Compression is a speed optimization, not just a memory optimization.** Narrower elements move
  more useful values per memory transaction. Elasticsearch reports that byte-width processing gives
  "a direct 2x advantage" over 16-bit representations "independent of any change in arithmetic
  throughput" [3]. Their int7 kernel, which unlocks the `vpdpbusd` instruction, gave "~6x speedup
  with negligible recall loss" [3]. This is why [quantization](06-quantization.md) makes brute force
  faster and not merely smaller.
- **Doing *more* arithmetic can be a win** if it lets you move fewer bytes. Their bf16 Euclidean
  kernel computes `‖a−b‖² = a·a + b·b − 2(a·b)` — three dot products instead of one subtract-and-square
  loop — specifically to avoid per-element type conversion, giving "1.3–2.4x speedup" on AMD Zen 5 and
  "1.3–1.4x" on Intel Emerald Rapids [3].
- **Hoisting query-independent terms pays.** Precomputing `b·b` out of the per-document loop
  "accounts for 10–20% of the gain" in bulk scoring [3].
- **Binary vectors are the extreme case.** SIMD `popcount(a AND b)` via AVX-512 VPOPCNTDQ gave
  "1.3–2.1x" over AVX2, and AVX2 — which has to synthesize popcount from `vpshufb` byte-shuffle
  lookup tables — is itself "~4x vs. scalar" [3].
- An int8 bulk-scoring trick using the algebraic identity `a·b = (a+128)·b − 128Σb` with `vpsadbw`
  gave "~20% improvement for bulk scoring, but no advantage on single-pair" queries [3] — a useful
  reminder that batch and single-query kernels have different bottlenecks.

### SIMD

A SIMD register holds several `float32` lanes — the register width divided by 32 bits, so AVX2's
256-bit registers hold 8, AVX-512's hold 16, and Arm NEON's 128-bit registers hold 4. FMA computes
`acc = acc + a*b` in one instruction with a single rounding, halving the instruction count and
slightly improving accuracy over separate multiply and add.

Four things separate a correct SIMD kernel from a fast one:

**Multiple accumulators.** FMA has a latency of several cycles but a throughput of one or more per
cycle. A single-accumulator loop serializes on the chain `acc → acc → acc` and retires one FMA every
*latency* cycles, wasting most of the machine. Four or eight independent accumulators, summed at the
end, break the chain. This is usually the largest single win in a hand-written kernel and it is
invisible in the source unless you know to look for it.

```
acc0 = acc1 = acc2 = acc3 = 0
for j in steps of 4*LANES:
    acc0 = fma(load(q+j+0*LANES), load(v+j+0*LANES), acc0)
    acc1 = fma(load(q+j+1*LANES), load(v+j+1*LANES), acc1)
    acc2 = fma(load(q+j+2*LANES), load(v+j+2*LANES), acc2)
    acc3 = fma(load(q+j+3*LANES), load(v+j+3*LANES), acc3)
sum = hsum(acc0+acc1+acc2+acc3)
handle scalar tail
```

**Horizontal reduction.** At the end you have a vector of partial sums that must collapse to one
scalar. Horizontal operations are comparatively slow and cross lanes, so do exactly one per distance
computation, at the very end — never inside the loop.

**The scalar tail.** If `D` is not a multiple of `LANES × accumulators`, the remainder needs a scalar
loop or a masked load. This is where correctness bugs live: an off-by-one there gives distances that
are subtly wrong for some `D` and exactly right for others, and it passes every test whose `D`
happens to be 128 or 768. Test `D` = 1, 7, 15, 17, 31, 33. Padding `D` up to a multiple of the lane
count with zeros removes the tail entirely for a few bytes per vector — zeros contribute nothing to a
dot product or a squared difference — which is usually the right trade, since it deletes the buggiest
code path in the kernel.

### Memory layout

Three options, in increasing sophistication:

```
Array of structs (AoS)        Structure of arrays (SoA)       Blocked / tiled
[v0.x v0.y v0.z]              [v0.x v1.x v2.x ...]            [ 64 vectors × D ]
[v1.x v1.y v1.z]              [v0.y v1.y v2.y ...]            [ 64 vectors × D ]
[v2.x v2.y v2.z]              [v0.z v1.z v2.z ...]            [ ...            ]
one vector contiguous          one dimension contiguous        cache-sized panels
```

Arm's own vectorization guidance states the rule: "Data should be consecutive, prefer struct of
arrays instead of arrays of structs," and "try to keep the data as packed as possible, no wasted
elements in the vectors" [4]. AoS wastes lane capacity whenever the per-record width does not divide
evenly into the register width; SoA lets a full-width load pull the same dimension from many records
at once, with no wasted lanes [4]. Their measured result: the SoA version was "up to 2.25x faster"
than the naive AoS version, and about 1.43x faster than a hand-written SIMD version of the AoS code,
under GCC 12 [4].

For a vector database the practical rule is narrower than it looks. Do **not** store `N` separately
heap-allocated vector objects — that is pointer chasing, one cache miss per vector, and it defeats
prefetching before SIMD is relevant. Store one contiguous `N × D` array. Row-major (each vector
contiguous — AoS-shaped, but still fully streamable) is right for one-query-at-a-time scanning, since
each vector is streamed once and never revisited; SoA and blocked layouts win when many queries are
evaluated against the same block, because the block stays in cache across queries. Blocking `N × D`
into cache-sized panels is exactly what BLAS does internally, for the same reason.

### The GEMM formulation

For a *batch* of queries there is a better formulation than any hand-written kernel. Expand:

```
‖x_j − y_i‖² = ‖x_j‖² + ‖y_i‖² − 2⟨x_j, y_i⟩
```

FAISS builds its exact search on this directly: "the two first terms can be precomputed in one pass
over the matrices X and Y," so "the bottleneck is to evaluate ⟨x_j, y_i⟩, equivalent to the matrix
multiplication XY^⊤" [5]. The pairwise distance computation for a batch of queries collapses almost
entirely into one dense GEMM call, which inherits decades of BLAS and GPU matmul tuning — blocking,
register tiling, prefetch schedules — that you are not going to reproduce by hand.

The numbers show what that buys: the partial distance matrix for 10⁴ queries × 10⁶ database vectors
× 128 dimensions is "1.28 Tflop, which runs in less than one second on current GPUs" [5]; the same
machinery processed "95 million images from the YFCC100M dataset in 35 minutes" at above 0.8
accuracy, and "1 billion vectors in less than 12 hours on 4 Maxwell Titan X GPUs" — "8.5× faster"
than the prior GPU state of the art on SIFT1B with superior accuracy [5].

The caveat: this is a *batch* technique. It needs many queries to fill the GEMM and materializes a
`Q × N` distance block, so you tile it. For a single low-latency query the streaming kernel wins,
because GEMM setup and materialization are not amortized over one row. Ground-truth generation is the
ideal GEMM workload; online serving is not.

### Top-K selection

Once you have distances you must select the `K` smallest. Three approaches, with measured
differences [6]:

| Approach | Time | Space | When it wins |
|---|---|---|---|
| Bounded max-heap of size `K` | `O(N log K)` [6] | `O(K)` [6] | Streaming: distances consumed as computed, never materialized |
| Quickselect | `O(N)` average, `O(N²)` worst [6] | `O(N)` [6] | All `N` distances already in an array (e.g. after a GEMM) |
| Full sort then slice | `O(N log N)` | `O(N)` | Never, for this purpose |

Lemire's direct comparison, selecting the smallest 128 of 1,408 random integers: quickselect ran at
roughly 45,000 ops/sec against roughly 37,000 ops/sec for the heap — about 20% faster — with both
"about an order of magnitude faster than the naive sort/slice approach" [6]. His own reading of that
gap is deflationary: "for all practical purposes, 20% faster is negligible in this case" [6]. And the
advantage flips with scale: as `n` grows, "QuickSelect will become increasingly limited by
potentially expensive cache misses," whereas the heap is more predictable, and "QuickSelect still has
the worst case quadratic-time scenario" [6].

For a streaming scan the bounded max-heap is the correct default, and the reason is memory, not the
20%: it needs `O(K)` space and one pass, while quickselect needs the full `N`-element distance array
materialized up front. For small `K` (say `K ≤ 32`) a sorted bounded array with an "is this better
than the current worst?" guard is even better — that guard rejects the overwhelming majority of
candidates in one comparison against a register-resident value, and insertion cost is paid only on
the rare improvement. Selection then never dominates; the distance kernel does, which is where you
want the time.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| Accumulator count | Whether FMA latency or throughput bounds the loop | 4–8 | With 1, the loop runs at FMA *latency* and wastes most of the machine |
| Block/tile size | Cache residency during multi-query scans | Sized to fit L1/L2 | Too large: spills to DRAM. Too small: loop overhead dominates |
| Element type | Bytes moved per comparison | `float32` baseline; `int8`/`int7` for speed [3] | Quantizing without measuring recall trades accuracy you did not agree to |
| `D` padding | Whether a scalar tail exists | Pad to a multiple of the lane count | Unpadded `D` needs tail code, which is where the bugs are |
| Selection structure | Selection overhead and memory | Bounded heap for streaming; quickselect after GEMM [6] | Materializing `N` distances per query is a scaling bug, not a slow path |
| Thread count | Parallelism across the `N` split | Cores | Changes float summation order and therefore exact tie ordering |

## How real systems do it

**FAISS chose the GEMM route and organized its architecture around it.** The squared-L2 expansion is
not an implementation detail there; it is the reason exact search rides on BLAS rather than a bespoke
kernel [5]. Its own SIFT1M benchmark: `FlatL2` gives 100% recall in 8.26 ms using 256 MB, PQ 50%
recall in 1.49 ms using 6.5 MB, IVFPQ 52% recall in 0.09 ms using 9.2 MB [7]. Two lessons. Exact
search at 1M vectors is genuinely usable — 8.26 ms disqualifies few applications, which is why the
"use flat" guidance [1] is not conservatism. And the 92× speedup from `FlatL2` to `IVFPQ` cost recall
falling from 100% to 52% [7]: the frontier trade from
[the search problem](02-the-search-problem.md), and why the flat number must stay measurable.

**Elasticsearch fought for the kernel instead.** Rather than treating brute force as the baseline to
escape, they invested in width: int7 with `vpdpbusd` for "~6x speedup with negligible recall loss,"
bf16 for "1.3–2.4x," binary popcount for "1.3–2.1x" over AVX2 [3]. FAISS reduces brute force to a
library call and moves on; a search engine running the same kernel inside every filtered and
re-ranked query treats kernel bytes-per-second as a product metric. Both are right for their setting,
and what makes them diverge is what fraction of total query time the kernel represents.

**Arm's guidance targets the layer beneath both**, reporting the layout gain independent of
algorithm: 2.25x from SoA over naive AoS, and 1.43x over hand-written SIMD on the AoS layout [4].
That second number is the one to internalize — hand-vectorizing a bad layout lost to auto-vectorizing
a good one. Fix the layout before writing intrinsics.

## Complexity and cost

| Quantity | Cost |
|---|---|
| Time, one query | `O(N·D)` distances + `O(N log K)` selection with a bounded heap |
| Time, batch of `Q` queries via GEMM | `O(Q·N·D)`, but at BLAS efficiency rather than kernel efficiency [5] |
| Memory, index | `N · D · 4` bytes at `float32`, plus optional 4 bytes/vector for cached norms. Zero graph or codebook overhead |
| Memory, per query | `O(K)` with a bounded heap; `O(N)` if you materialize distances |
| Bytes moved per query | `N · D · 4` — this, not FLOPs, is the quantity that predicts latency [3] |
| Build time | Zero. There is no index to build |
| Update cost | `O(D)` append. No structure to repair — see [updates and deletes](08-updates-and-deletes.md) |

Two properties fall out of "no index" that matter more than they first appear: writes are trivially
cheap and immediately visible, and there is no build step to invalidate. A flat index has no
concept of a stale graph, no rebuild schedule, and no [compaction](13-recovery-and-compaction.md)
interaction. At small `N` this operational simplicity is worth more than the latency difference.

Latency grows linearly in both `N` and `D`, with no knee and no cliff, which makes brute force the
easiest component in the system to capacity-plan. Estimate it as
`(N · D · 4 bytes) / achievable_bandwidth`; if your measured latency is far above that, the problem
is layout, allocation, or accumulator dependency chains — not arithmetic.

## Failure modes

- **Allocating an `N`-element scratch array per query.** The most common performance bug here. It
  turns an `O(K)`-memory streaming scan into `O(N)` allocation, adds an allocator round-trip and a
  page-fault storm per query, and destroys the cache locality the kernel just worked for. Allocate
  the heap once per query at size `K`, or reuse a per-thread buffer.
- **Forgetting that distances are squared.** A radius filter, a score threshold, or a
  distance-to-similarity conversion applied to an L2² value is wrong for every radius except 1.0 [2].
  It usually passes the unit test, because the test picked 1.0.
- **Float accumulation order making results non-reproducible.** Floating-point addition is not
  associative, so splitting the scan across 4 threads versus 8 produces slightly different sums,
  reordering near-ties and changing the returned set at the `K`-th position. Not wrong, but not
  reproducible either, which breaks recall regression gates and any exact-output assertion. Break
  ties on a deterministic key (vector ID) rather than distance alone, and fix the reduction tree
  shape if you need bit-exact reproducibility.
- **Scalar tail bugs.** Wrong for some `D`, right for others. See the SIMD section above.
- **Pointer-chasing layout.** `N` separately allocated vectors. One cache miss per comparison; SIMD
  cannot help.
- **Recomputing `‖v‖` inside the loop.** A query-independent term that belongs outside it — worth
  10–20% in bulk scoring by Elasticsearch's measurement [3].
- **Benchmarking with a warm query that is in the dataset.** The early-exit and cache behaviour is
  unrepresentative, and the result is always suspiciously fast.

## How to test it

- **Differential test against a naive scalar reference.** Write the dumbest possible triple-loop
  implementation and keep it forever. Assert the optimized kernel agrees within a float tolerance,
  and that top-K sets match exactly when distances are well separated. Highest-value test here.
- **Property test over `D`.** Run the kernel for every `D` in 1..64 plus a few large odd values.
  Catches every scalar-tail and alignment bug in one test.
- **Self-match.** Every vector's top-1 is itself at distance 0. Catches sign errors, stride errors,
  and off-by-one indexing.
- **Permutation invariance.** Shuffle the dataset and re-query. The returned *set* must be identical
  and the distances must match. Catches accidental dependence on insertion order.
- **Top-K selection invariants.** Property test the heap directly against a sort-and-slice reference
  on random arrays, including all-equal inputs, `N < K`, `K = 1`, and `K = N`. `N < K` is the case
  that reaches production untested.
- **Thread-count determinism test.** Run the same query at 1, 2, and 8 threads; assert identical
  result sets. Failure means a tie-breaking bug, and this is how you find out before a recall
  regression gate starts flapping in CI for no reason.
- **Allocation assertion.** Assert zero heap allocations in the steady-state query path. Catches the
  per-query scratch array immediately.

## Minimal demo version

**Brute force is the minimal demo index.** Do not build HNSW first; build this, and build it exactly.

1. One contiguous `float32` slice of length `N × D`, row-major, plus parallel ID and norm arrays. No
   per-vector allocations.
2. A plain scalar dot-product loop over normalized vectors, returning L2². Do not hand-vectorize
   initially: compilers auto-vectorize this shape well, and a clean scalar loop is the reference
   implementation you differentially test everything against for the rest of the project.
3. A bounded max-heap of size `K`, allocated once per query, with the "better than current worst?"
   guard before any heap operation.
4. Split the scan across threads by contiguous ranges of `i`, merging per-thread heaps at the end.
   Then write the thread-count determinism test, watch it fail, and add ID tie-breaking.

You can cut without losing anything: SIMD intrinsics, the GEMM path, blocked layouts, `int8` kernels,
multiple metrics, and anything GPU. Each is a constant-factor optimization on a component whose
asymptotics you have already demonstrated. For the memory-bandwidth lesson empirically, the cheapest
honest experiment is measuring latency at `D` = 128 and `D` = 768 and confirming it scales with bytes
moved and nothing else.

You **cannot** cut exactness, and this is the one place in the system with no acceptable shortcut.
The flat scan is the oracle: recall for every approximate index, correctness for every
[shard](14-sharding-and-partitioning.md) merge, the reference for every
[replica](15-replication.md) consistency check, the expected output for every
[recovery](13-recovery-and-compaction.md) test. If it is subtly wrong — a tail bug, a squared-distance
confusion, a tie-ordering nondeterminism — every recall number the project produces is measuring that
bug, and every downstream conclusion is a lie told confidently. It is also the one component simple
enough to make provably correct. Do that first.

Two things that look optional and are not: cache the ground truth the oracle produces (recomputing
`Q × O(N·D)` per test run quietly becomes the slowest thing in CI), and keep the flat path reachable
in the running system rather than only in tests, so it sees the same deletes, filters, and storage
state the real index sees.

## Sources

[1] Meta AI Research — Guidelines to choose an index (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index
[2] About Vector Database — Euclidean Distance (L2) explained — https://aboutvectordatabase.com/learn/euclidean-distance-l2-explained/
[3] Elastic Search Labs — SIMD vector search: Elasticsearch simdvec's 6x speedup — https://www.elastic.co/search-labs/blog/vector-search-simd-optimization-simdvec
[4] Arm — Optimize SIMD code with vectorization-friendly data layout: Structure of arrays — https://learn.arm.com/learning-paths/cross-platform/vectorization-friendly-data-layout/a-more-complex-problem-revisited/
[5] Jeff Johnson, Matthijs Douze, Hervé Jégou — Billion-scale similarity search with GPUs — https://arxiv.org/pdf/1702.08734 — arXiv:1702.08734, 2017
[6] Daniel Lemire — QuickSelect versus binary heap for top-k queries — https://lemire.me/blog/2017/06/14/quickselect-versus-binary-heap-for-top-k-queries/ — 2017
[7] James Briggs / Pinecone — Product Quantization: Compressing high-dimensional vectors by 97% — https://www.pinecone.io/learn/series/faiss/product-quantization/
