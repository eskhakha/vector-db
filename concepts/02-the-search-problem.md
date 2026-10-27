# 2. The search problem: exact, approximate, and recall

> Top-K nearest-neighbour search has an exact answer that costs a full scan, so every practical
> system returns an approximate answer instead — which makes *how wrong you are, at what speed* the
> central quantity the whole stack is engineered around.

## The problem

The query is simple to state. Given a set `V = {v₁ … v_N} ⊂ R^D`, a distance function `d`, a query
vector `q`, and an integer `K`, return the `K` elements of `V` minimizing `d(q, ·)`:

```
topK(q) = argmin_{S ⊆ V, |S| = K}  Σ_{v ∈ S} d(q, v)
```

There is exactly one correct answer (modulo ties). Computing it requires evaluating `d(q, v)` for
every `v`, which is `O(N·D)` — see [brute-force and kernels](03-brute-force-and-kernels.md). At
`N` = 10⁶ and `D` = 768 that is roughly 1.5 GFLOP and ~3 GB of memory traffic **per query**. At
`N` = 10⁹ it is not a system you can build on commodity hardware.

The obvious escape — an index that prunes most of the data the way a B-tree prunes a range scan —
does not work in high dimensions, and that is a proved result rather than an engineering
disappointment. Beyer, Goldstein, Ramakrishnan and Shaft showed that as dimensionality grows the
maximum and minimum distances from a query converge: "as dimensionality increases, all points become
equidistant w.r.t. the query point" [1][2]. Space-partitioning indexes prune by proving a region
cannot contain a better neighbour; when all distances are nearly equal that proof never succeeds and
nothing is pruned. The authors state the consequence: "the ease with which linear scan beats more
complex access methods for high-D nearest neighbor is explained by our theorem" [2].

So the field faced a fork: pay `O(N·D)` per query forever, or give up exactness. It gave up
exactness. Every widely deployed vector index — IVF, HNSW, PQ, DiskANN — is approximate by
construction, and the discipline that replaced "is it correct?" is **recall measurement**.

## How it works

### recall@K

```
recall@K = |{true top-K} ∩ {retrieved top-K}| / K
```

The retrieved set is what the index returned; the true set is the answer a brute-force scan would
have given [3]. ANN-Benchmarks states it operationally as "the fraction of true nearest neighbors
found, on average over all queries" [4]. It is a per-query quantity in `{0, 1/K, 2/K, …, 1}`, and the
number you report is its mean over a query set.

**Ground truth** is the exact top-K, and there is one honest way to get it: run the brute-force
search. Recall@K "requires knowing the exact k nearest neighbors for each query, which serves as the
ground truth for evaluation," and that computation is "only feasible for benchmarking and offline
evaluation, not during production inference" [3]. The architectural consequence: **the exact scan is
never dead code.** It is the oracle. If you cannot run an exact search over your corpus, any recall
number you quote is a guess.

Two details that trip people up:

- **Recall and precision coincide for top-K.** Precision is `|retrieved ∩ true| / |retrieved|` and
  recall is `|retrieved ∩ true| / |true|`. For top-K search both denominators are `K`, so the two
  metrics are numerically identical. Reporting both is not extra information; a benchmark that
  reports "precision and recall" separately for a fixed-K ANN query is padding.
- **recall@K is order-blind.** It "is agnostic to ranking order within the top k. Returning all true
  neighbors in any order yields perfect recall" [3]. If ordering matters to your application, recall
  is not measuring it.

### The recall-vs-QPS Pareto frontier

An ANN index is not one algorithm; it is a family of operating points. Every index exposes at least
one parameter that trades accuracy for speed — HNSW's `ef`, IVF's `nprobe` [5] — and sweeping that
parameter traces a curve. ANN-Benchmarks makes this the primary evaluation frame, plotting recall
against queries per second, with better implementations tracing curves closer to the upper right —
higher recall *and* higher QPS simultaneously [4].

```
QPS
 ^
 |  ****                        each * is one setting of ef / nprobe
 |      ***
 |         **         index A
 |           **
 |             *
 |   ~~~~~~
 |         ~~~~~      index B  (dominated: lower QPS at every recall)
 |              ~~~
 +--------------------------> recall@K
 0.5        0.9   0.99
```

This is why **"our index is 95% accurate" is not a claim.** Any index reaches 95% recall — push
`nprobe` or `ef` high enough and you converge on brute force, which is 100% recall at the QPS of a
full scan. The claim only carries information as a pair: *95% recall at 4,200 QPS on this dataset,
single-threaded, K=10*. Comparisons must be frontier-against-frontier: an index that wins at 0.90
recall routinely loses at 0.99, because the curves cross. Choosing from a single reported number is
choosing based on where the vendor's benchmark happened to sit.

The frontier also has a right-hand wall that tuning cannot climb. IVF has a structural recall
ceiling: "even with large nprobe, if a query's true nearest neighbor resides in a cluster that is not
among the searched ones, IVF will not retrieve it" [6]. A vector near a cell boundary can be genuinely
closer to points in a neighbouring cell than to points in its own, and no amount of intra-cell search
fixes a wrong coarse assignment [6]. See [IVF and clustering](05-ivf-and-clustering.md).

### The evaluation trap

recall@K is a proxy, and it is worth knowing how loose a proxy it is. Dimitropoulos and Mamoulis
argue that "what really matters in ANN search is the quality of the retrieved results and not their
overlap with the true kNN set," and that optimizing for recall@K "forces unnecessary computational
overhead" without proportional real-world benefit [7]. Their evidence is the load-bearing part:
across downstream tasks, "performance indicators (label precision, semantic similarity, BERTScore,
and LLM-graded quality) remain highly stable even when Recall@k drops significantly" [7].

The mechanism is easy to see. If the true 3rd neighbour is at distance 0.412 and your index returned
something at 0.413, recall@10 drops by 0.1 while what a user reads is unchanged: recall@K counts
identity of items, the application cares about distance. They propose **1/Ratio@K** — the inverse
approximation ratio, comparing *distances* of retrieved versus true neighbours — as an alternative
that is "judge-free, hyperparameter-free, and computable from standard ANN benchmark inputs alone,"
reporting that it "closely mirrors this stability, tracking true utility much better" [7].

Keep recall@K, because it is the field's common currency and what every published frontier is plotted
in. But before spending real effort dragging recall from 0.92 to 0.98, measure the downstream task.
It may already be flat.

### When approximation is not acceptable

Approximation is a default, not a law. Three cases where it is the wrong answer:

- **Deduplication and exact-match semantics.** "Is this vector already in the corpus?" is a query
  whose answer must be right. A miss creates a duplicate; a false hit drops data. FAISS's own
  guidance is unambiguous: if exact results are required, only the flat indexes guarantee precision,
  and flat "does not compress the vectors, but does not add overhead" [8].
- **Compliance and audit.** If a result set must be reproducible and defensible — a legal hold, a
  content-moderation decision trail — a probabilistic index that returns different results after a
  rebuild is a liability. Exactness here is a property of the *record*, not of the search quality.
- **Small `N` or few queries.** FAISS says it plainly: for 1,000–10,000 searches, use flat, because
  "the index building time will not be amortized by the search time" [8]. Building an approximate
  index you query a thousand times is a net loss. This is the most common over-engineering mistake in
  the space.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| `K` | Result set size and the denominator of recall@K | 10–100 | Recall@10 and recall@100 are different numbers for the same index; comparing across `K` is meaningless |
| Query-time search effort (`ef_search`, `nprobe`) | Position on the recall/QPS frontier, per query | `nprobe` swept and measured, not derived [5] | Too low: silent recall collapse. Too high: brute-force cost with index overhead on top |
| Build-time effort (`ef_construction`, `nlist`) | The frontier itself — the ceiling you can tune up to | `nlist ≈ C·sqrt(N)` [5][8] | Wrong at build time means a rebuild, not a config change |
| Ground-truth query set size | Confidence interval on your recall estimate | 1,000–10,000 held-out queries | Too few queries and recall differences below a few points are noise |
| Metric | Must match the index's metric exactly | Same as the model's [see 01](01-vectors-and-similarity.md) | Ground truth computed under a different metric makes recall meaningless |

## How real systems do it

The interesting split is **where the recall dial lives**: at query time, or frozen into the index at
build time. Real systems have both, and the difference determines what you can change at 3am.

**FAISS puts the primary dial at query time and tells you to measure it, not compute it.** `nprobe`
is "specified at query time (useful for measuring trade-offs between speed and accuracy)" [5] — sweep
it and observe recall and latency rather than deriving a value analytically [5]. The build-time
choices need care: below 1M vectors, `IVFK` where "K is 4*sqrt(N) to 16*sqrt(N)", requiring "between
30*K and 256*K vectors" for training [8]. Get `nlist` wrong and you rebuild; get `nprobe` wrong and
you change one integer.

**pgvector shows what build-time-only costs you.** Building an HNSW index over 1M vectors at 1536
dimensions with `m=16, ef_construction=200` took "around 1 hour and 27 minutes" single-threaded in
v0.5.1, dropping to 9.5 minutes with parallel builds in v0.6.0 [9]. Build time scales roughly
linearly in `ef_construction` — 38m 08s, 1h 06m 59s, 1h 27m 45s for values 64, 100, 200 on the same
dataset [9]. Crunchy Data's guidance confirms the direction: "the larger the value for
ef_construction the slower the index build," and "further increase of the efConstruction leads to
little extra performance but in exchange of significantly longer construction time" [10]. Notably,
parallelism changed wall-clock build time only — accuracy (0.963) and QPS (~950) were unchanged [9].
A build-time parameter you chose badly is an hour-and-a-half of downtime to fix; a query-time
parameter is a retry.

**IVF demonstrates why neither dial is sufficient alone.** `nprobe` moves you along the frontier, but
the frontier's ceiling was set when you chose `nlist` and ran k-means, because a vector assigned to
the wrong coarse cell is unreachable for any `nprobe < nlist` [6]. OpenSearch's own HNSW guide
confirms only the qualitative direction — denser graph gives better recall and lower throughput —
and explicitly defers index-size and build-time measurement [11].

**Per-query is the right design, because recall requirements are per-query, not per-index.** In one
collection, an autocomplete request wants an answer in 5 ms and does not care about the 9th
neighbour, while a nightly deduplication pass wants near-exact results and has hours. A dial frozen
at build time forces two indexes over the same data — double the memory and write amplification — to
serve two latency classes; a query-time dial serves both from one structure. It also makes the system
honest under load: shedding recall is a graceful degradation path a load shedder can trigger, which
matters when [distributed search](17-distributed-search.md) must return partial results under a
deadline. Design the API so the caller passes the search-effort parameter (or a named profile mapping
to one) and the response reports the effort actually used.

## Complexity and cost

| Operation | Cost |
|---|---|
| Exact top-K, one query | `O(N·D)` distance work + `O(N log K)` selection |
| Ground truth for a benchmark of `Q` queries | `Q × O(N·D)` — this is the expensive part of evaluating an index, often more than building it |
| Approximate top-K | Index-dependent; the point of the frontier is that it is *tunable*, not fixed |
| Recall computation itself | `O(Q·K)` given ground truth — free |

The FAISS paper gives the scale of an exact batch: 10⁴ queries × 10⁶ database vectors × 128
dimensions is "1.28 Tflop, which runs in less than one second on current GPUs" [12]. That is the
budget for one ground-truth run at small scale. At `N` = 10⁹ it is a job, not a step — which is why
public benchmarks ship precomputed ground truth files.

## Failure modes

- **Recall measured against the wrong ground truth.** Ground truth computed with a different metric,
  a different normalization, or a stale snapshot of the corpus. Produces confident, wrong numbers.
  This is the most common benchmarking bug and it always flatters the index.
- **Ties at the boundary.** If the K-th and (K+1)-th neighbours are equidistant, the "true" top-K is
  ambiguous and recall drops for a correct implementation. Break ties deterministically by ID in both
  the oracle and the index, or exclude tied queries from the query set.
- **Recall reported without QPS.** Or without `K`, dataset, thread count, and hardware. Unusable.
- **Tuning on the evaluation set.** Sweeping `ef` against the same queries you report recall on
  overfits the dial. Hold out a separate query set.
- **Deletes not reflected in ground truth.** After deletes, the oracle must exclude tombstoned
  vectors too, or the index is penalized for being correct. See
  [updates and deletes](08-updates-and-deletes.md).
- **Averaging recall over queries with different `K`.** Truncated result sets (fewer than `K` matches
  exist after filtering) make the denominator wrong. See
  [filtering and hybrid search](10-filtering-and-hybrid-search.md).

## How to test it

- **Oracle equivalence at the limit.** Turn the search-effort parameter to its maximum. Recall must
  approach 1.0. If an index plateaus at 0.97 with `nprobe = nlist`, you have a bug, not a trade-off.
- **Monotonicity property.** Recall must be non-decreasing in search effort, and QPS non-increasing.
  Sweep the parameter and assert both. A non-monotone recall curve almost always means the candidate
  set or the result heap is being truncated somewhere unexpected.
- **Self-query recall.** Query with vectors that are in the corpus. Every one must return itself as
  rank 1. This should hold at *any* search effort for a correct graph index and is a much sharper
  test than aggregate recall.
- **Differential test against brute force.** For a small corpus (`N` = 10⁴), compute exact top-K and
  assert the approximate result set matches at the effort level where you claim 1.0 recall. Small
  enough to run in CI on every commit.
- **Recall regression gate.** Pin a fixed dataset, query set, `K`, and search-effort value; fail CI
  if mean recall drops more than a threshold. This catches accidental changes to distance kernels,
  tie-breaking, and graph construction that no unit test will notice.
- **Downstream stability check.** If you have a downstream task, measure it across the recall sweep.
  The finding that downstream quality "remain[s] highly stable even when Recall@k drops
  significantly" [7] is worth verifying on your own data before you spend a quarter chasing recall.

## Minimal demo version

Build these three things and you have genuinely exercised the concept:

1. **An exact brute-force path that stays in the product**, callable as `search_exact()`. Not a test
   fixture — a real code path on real storage, so the oracle sees exactly the data the index sees,
   including deletes and filters.
2. **A recall harness.** 10⁴ random vectors, 10³ held-out queries, `K` = 10. Compute ground truth
   once, cache it, and report mean recall@10 plus QPS as a pair. This is under 100 lines.
3. **One tunable index with a query-time effort parameter**, and a script that sweeps it and prints
   the frontier as a table. Even four rows makes the trade-off real in a way no amount of reading
   does.

You can cut: 1M-vector scale (10⁴ shows the same shape), latency percentiles, multi-threading,
real embeddings, and every metric other than recall@K.

You cannot cut: **the exact oracle, and reporting recall and QPS together.** An approximate index
with no way to measure its error is not a demonstration of approximate search — it is a search
function that returns whatever it returns, and the entire intellectual content of this part of the
stack is the measured gap between it and the exact answer. Equally, a demo that prints recall without
the QPS it was measured at reproduces the exact mistake this file exists to warn about. Print them on
one line, always.

## Open questions and contested claims

**Is recall@K the right objective at all?** Dimitropoulos and Mamoulis say no: optimizing for it
"forces unnecessary computational overhead," while downstream indicators stay stable as recall@K
drops significantly [7], and their 1/Ratio@K is distance-based rather than set-overlap-based [7]. The
counterargument is practical, not theoretical: recall@K is what ANN-Benchmarks plots [4] and what
makes results comparable across a decade of papers, so switching unilaterally makes your numbers
incomparable with everyone else's. The defensible position is to report recall@K for comparability
and track a distance-based or downstream metric for decisions.

**Whether the classical high-dimensional pessimism applies to real embeddings.** Beyer et al. predict
that linear scan should beat structured access methods in high dimensions [1][2], yet HNSW and IVF
demonstrably work on 768- and 1536-dimensional embeddings. Lakshman, Munyampirwa, Shun and Coleman
name this paradox directly — classical theory "predicts instability in high dimensions, yet modern
vector databases function effectively" — and propose *stability*, defined as "a slight perturbation
to a query should not dramatically change the list of near neighbors to that query," as the property
that actually explains when ANN works [13]. They extend the framework to multi-vector, filtered, and
sparse search, noting that in filtered search large penalties for non-matching filters can *induce*
stability even when unfiltered search is unstable [13]. If this holds, the right question when an
index underperforms is not "is `D` too high?" but "is this query distribution stable?" — and that
is a measurable property of your data, not a fixed property of the algorithm.

## Sources

[1] Kevin Beyer, Jonathan Goldstein, Raghu Ramakrishnan, Uri Shaft — When Is Nearest Neighbor Meaningful? — https://minds.wisc.edu/items/4c666f43-b2e4-40d1-bba3-e34072be1576/full — UW-Madison TR1377, ICDT 1999
[2] Kevin Beyer — "When Is Nearest Neighbor Meaningful?" presentation of the theorem and its consequences — https://slidetodoc.com/when-is-nearest-neighbor-meaningful-by-kevin-beyer/
[3] Inference Systems — Recall@k: Definition, Formula & Use in Vector Search — https://inferensys.com/glossary/vector-database-infrastructure/vector-indexing-algorithms/recallk
[4] Erik Bernhardsson et al. — ANN-Benchmarks — https://ann-benchmarks.com/
[5] Meta AI Research — Faiss indexes (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Faiss-indexes
[6] VeloDB — Inverted File Index (IVF) in Vector Similarity Search — https://www.velodb.io/glossary/inverted-file-index-in-vector-similarity-search
[7] Dimitris Dimitropoulos, Nikos Mamoulis — ANN Search: Recall What Matters — https://arxiv.org/abs/2606.04522 — arXiv:2606.04522, 2026
[8] Meta AI Research — Guidelines to choose an index (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index
[9] Supabase Engineering — pgvector 0.6.0: 30x faster with parallel index builds — https://supabase.com/blog/pgvector-fast-builds
[10] Crunchy Data — HNSW Indexes with Postgres and pgvector — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector
[11] OpenSearch Project — A practical guide to selecting HNSW hyperparameters — https://opensearch.org/blog/a-practical-guide-to-selecting-hnsw-hyperparameters/
[12] Jeff Johnson, Matthijs Douze, Hervé Jégou — Billion-scale similarity search with GPUs — https://arxiv.org/pdf/1702.08734 — arXiv:1702.08734, 2017
[13] Vihan Lakshman, Blaise Munyampirwa, Julian Shun, Benjamin Coleman — Breaking the Curse of Dimensionality: On the Stability of Modern Vector Retrieval — https://arxiv.org/html/2512.12458 — arXiv:2512.12458v2, 2025
