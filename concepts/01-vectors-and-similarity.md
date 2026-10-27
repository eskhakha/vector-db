# 1. Vectors, embeddings, and similarity

> An embedding maps an item into `R^D` so that semantic closeness becomes geometric closeness; every
> query a vector database answers is ultimately a distance computation, so the choice of distance
> function is the first correctness decision in the system.

## The problem

A database storing text, images, or audio cannot answer "find things that mean roughly this" with
an equality predicate. The field's trick is to push the hard part into a model: a trained encoder
produces a dense `float32` vector per item such that items humans consider similar land near each
other. The database then never reasons about meaning at all — it does arithmetic on coordinates.

This is a *load-bearing assumption*, not a theorem. Nothing guarantees that geometric proximity
tracks the semantics you care about, and there are documented cases where it does not (see
[Open questions](#open-questions-and-contested-claims)). A vector database is a faithful executor of
whatever geometry the model handed it; if that geometry is wrong, it returns wrong answers extremely
fast and with perfect internal consistency.

## How it works

### Dimensionality

`D` is a property of the model, not the database, and it has drifted upward. The word-embedding era
settled around 300 dimensions as "enough to compress the nuance of a textual dataset" [1]. BERT and
GPT-2 use 768 — BERT's chosen so it divides evenly into 12 attention heads at 64 dimensions each, and
CLIP reuses 768 for symmetry between its image and text towers; MiniLM dropped to 384 to make
sentence-level inference cheaper [1]. OpenAI's `text-embedding-3-small` defaults to 1536,
`text-embedding-3-large` to 3072, and legacy `text-embedding-ada-002` to 1536 [2]. 2026-era models
span "768 to 4096+", with Qwen-3 reaching 4096 [1].

`D` is not free: it sets storage (`N · D · 4` bytes at `float32`), scan cost, and index memory.
Matryoshka Representation Learning front-loads the important information so a vector can be truncated
— to 64 dimensions in the extreme — and still retain most retrieval signal, with roughly 50% of
dimensions droppable in some cases without materially hurting retrieval [1]. OpenAI exposes this as a
`dimensions` parameter on `text-embedding-3-*` [2]. Truncation is the cheapest dimensionality
reduction available; evaluate it before reaching for [quantization](06-quantization.md).

### The metrics

Three functions cover essentially all production usage [3]:

| Name | Formula | Meaning |
|---|---|---|
| Euclidean / L2 | `L2(a,b) = sqrt( Σᵢ (aᵢ − bᵢ)² )` | distance between the vector endpoints [3] |
| Cosine similarity | `cos(a,b) = (aᵀb) / (‖a‖·‖b‖)` | cosine of the angle between them [3] |
| Dot / inner product | `a·b = Σᵢ aᵢbᵢ = ‖a‖‖b‖cos θ` | cosine scaled by both magnitudes [3] |

Note the direction convention: L2 is a *distance* (smaller is better), cosine and dot are
*similarities* (larger is better). Getting this backwards is the single most common bug in a
first implementation, and it does not crash — it silently returns the worst matches.

Dot product incorporates magnitude as well as direction, which makes it sensitive to phenomena like
item popularity in learned embeddings, where a vector's norm correlates with how often the item
appeared in training [3].

### The identity that makes the metric choice mostly disappear

Expand the squared Euclidean distance:

```
‖a − b‖² = ‖a‖² + ‖b‖² − 2(a·b)
```

If both vectors are L2-normalized so `‖a‖ = ‖b‖ = 1`, this collapses to:

```
‖a − b‖² = 2 − 2(a·b)
```

`a·b` is exactly the cosine similarity when both norms are 1. So squared L2 distance is a strictly
decreasing linear function of cosine similarity — **the two induce the identical ranking**. Google's
ML Crash Course states the same equivalence formally: when `‖a‖ = ‖b‖ = 1`, all three measures "are
equivalent because they are proportional to cos(θₐᵦ)" [3]. OpenAI states it for their own API:
their embeddings are normalized to length 1, so "cosine similarity can be computed slightly faster
using just a dot product" and "cosine similarity and Euclidean distance will result in the identical
rankings" [2].

This means: **normalize once at write time, and the metric debate is over.** Store unit vectors,
compute dot products, rank descending. You get cosine semantics at the cost of a bare inner product,
and you can still recover L2 ordering for free from the same score.

### Why L2² and not L2

Square root is strictly monotonic on non-negative reals, so the `K` nearest under L2 are exactly the
`K` nearest under L2²; implementations compute L2² internally and skip one `sqrt` per pair [4]. The
consequence: any *threshold* supplied in L2 units must be squared before it meets the kernel, and any
distance returned to a client must be un-squared. Mixing the two spaces gives a filter that is
silently wrong for every radius except 1.0.

### MIPS is not a metric space

Maximum inner product search — top-K by raw `a·b` on *unnormalized* vectors — is not a metric
nearest-neighbour problem, and this is a structural fact, not a tuning detail. Two of the metric
axioms fail:

- **Self-similarity is not extremal.** `a·a = ‖a‖²`, but if `‖b‖ > ‖a‖` and the angle is small,
  `a·b` can exceed `a·a`. A point is not its own best match.
- **No triangle inequality.** There is no bound of the form `d(a,c) ≤ d(a,b) + d(b,c)` to exploit.

Every classical metric index — ball trees, VP-trees, and the pruning logic in
[tree and hash indexes](04-tree-and-hash-indexes.md) — prunes by proving a region cannot contain a
better neighbour, and that proof *is* the triangle inequality. Remove it and pruning is unsound, not
merely weak. So MIPS is handled either by *reducing* it to a metric problem (append a dimension
encoding the norm, turning inner-product ranking into distance ranking) or by using an index family
such as [graph indexes](07-graph-indexes-hnsw.md) whose correctness argument is empirical rather than
metric. If your engine advertises inner product, know which of the two you rely on.

### Choosing a metric

Use what the embedding model was trained with. That is the whole rule. A model trained with a cosine
objective has no calibrated norms; a model trained with an inner-product objective encodes
information in the norms that cosine will throw away. The secondary rule: "use L2 when vector
magnitude is meaningful... avoid L2 when you care only about direction (e.g. most text embeddings)"
[4].

### The curse of dimensionality

**Distance concentration.** Beyer, Goldstein, Ramakrishnan and Shaft proved that under broad
conditions — loosely, i.i.d. dimensions — the maximum and minimum distances from a query converge as
dimensionality grows: "as dimensionality increases, all points become equidistant w.r.t. the query
point" [5][6]. The formal condition is that the variance of the distance distribution, relative to
its mean, approaches zero [6]. Their stated consequence shaped the field: "the ease with which linear
scan beats more complex access methods for high-D nearest neighbor is explained by our theorem" [6].
When distances lose contrast, a space-partitioning index cannot exclude any partition and degenerates
to a full scan with extra pointer chasing.

**Rate of degradation differs by norm.** Aggarwal, Hinneburg and Keim showed that the gap
`Dmax − Dmin` for the Lk norm scales as `d^(1/k − 1/2)`: L1 *diverges* (contrast is preserved), L2
stays bounded, and higher-order norms collapse fastest [7]. They recommend L1 over L2 for high
dimensional data, and go further to propose fractional norms `Lk` with `k < 1`, reporting k-means
accuracy of "99% for the fractional distance metric with f = 0.3 versus 89% for the Euclidean
metric" on UCI datasets, and that "L0.1 distance metric is most robust to the presence of noise" [7].

**Hubness is a separate pathology.** Radovanović, Nanopoulos and Ivanović define *k-occurrence* as
the number of times a point appears in other points' k-NN lists. As dimensionality rises this
distribution "becomes considerably skewed... causing the emergence of hubs, that is, points with very
high k-occurrences" [8] — "an inherent property of data distributions in high-dimensional vector
space" [8], not an artifact of one dataset. In retrieval it shows up as a handful of vectors
appearing in results for many unrelated queries. This is *not* distance concentration and is not
fixed by fixing contrast: concentration says all distances become similar, hubness says the
near-neighbour relation becomes asymmetric.

**Intrinsic vs ambient dimensionality** is the standard rebuttal. The concentration theorems assume
uncorrelated coordinates; real embeddings lie near a manifold whose intrinsic dimensionality is far
below the stored `D` of 768 or 1536, which is why the pessimistic bounds are not fully realized.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| Metric | Ranking function | Whatever the model trained with — usually cosine for text [2] | Silently degraded relevance; nothing errors |
| Normalize at write | Whether cosine reduces to a dot product | On, for text embeddings | Cosine costs an extra divide per pair, or is computed wrong if you forget the denominator |
| `D` | Storage, scan cost, index memory | 384–3072 depending on model [1][2] | Oversized `D` multiplies every cost in the system linearly |
| Matryoshka truncation dim | Lossy dimensionality reduction at the source | 50% of full has been reported as viable in some cases [1] | Too aggressive and recall drops with no error signal |
| Stored norm per vector | Whether cosine needs a runtime `sqrt` | 4 bytes/vector | Recomputing norms in the inner loop; a stale norm after an update returns wrong scores |

## How real systems do it

Three vantage points that made genuinely different choices:

**FAISS (library).** Reduces everything to two primitives — L2 and inner product — naming its exact
index accordingly (`FlatL2`) [9]. Higher-level metrics are the caller's problem, normally solved by
normalizing at ingest. Its batch kernel is built directly on the expansion above, where "the two
first terms can be precomputed in one pass over the matrices X and Y" so that "the bottleneck is to
evaluate ⟨x_j,y_i⟩, equivalent to the matrix multiplication XY^⊤" [10]. The metric identity is not a
formality here; it is what lets exact search inherit BLAS. See
[brute-force and kernels](03-brute-force-and-kernels.md).

**OpenAI (model provider).** Sidesteps the decision entirely by shipping pre-normalized embeddings
and telling users "we recommend cosine similarity" while noting "the choice of distance function
typically doesn't matter much" [2]. That claim is true *for their vectors* precisely because they are
already unit length — it is not a universal statement, and it stops being true the moment you mix in
embeddings from a model that does not normalize.

**Elasticsearch (search engine kernels).** Treats the metric as a memory-traffic problem. Hoisting
query-independent terms out of the per-document loop — precomputing `b·b`, exactly the `‖y_i‖²` term
above — "accounts for 10–20% of the gain" in bulk scoring, and computing bf16 Euclidean distance via
the three-dot-product decomposition rather than by type conversion gives a 1.3–2.4x speedup on AMD
Zen 5 and 1.3–1.4x on Intel Emerald Rapids [11].

The practical takeaway shared by all three: **store a precomputed per-vector norm.** With `‖y‖`
cached, cosine against an arbitrary query is one dot product plus one divide, with no `sqrt` and no
second pass over the vector. With vectors normalized at write time you do not even need the divide.
Four bytes per vector buys you the entire metric abstraction.

## Complexity and cost

- One distance: `O(D)` time, `O(1)` extra space. Roughly `2D` FLOPs (one FMA per dimension).
- Storage: `N · D · 4` bytes at `float32`, plus 4 bytes per vector if you cache the norm.
- Normalization at write: one `O(D)` pass, amortized to zero against the read path.
- L2 vs L2²: saves one `sqrt` per candidate. Immaterial for a single query, measurable when you are
  evaluating `N` candidates per query in a scan.

## Failure modes

- **Sign/direction confusion.** Sorting a similarity ascending or a distance descending. Returns the
  `K` *worst* matches with no error. Catch it with a test that asserts a vector is its own top-1.
- **Mixed normalization.** Normalization living in application code on one write path and not
  another. Unnormalized vectors get systematically higher or lower dot-product scores regardless of
  relevance.
- **Stale cached norms.** An update rewrites the vector but not the cached `‖y‖`. Scores for that one
  vector are wrong forever. See [updates and deletes](08-updates-and-deletes.md).
- **Cross-model contamination.** Vectors from two models in one collection. The geometry is
  incomparable and results are noise. Store a model identifier per vector; reject mismatched queries.
- **Threshold in the wrong space.** A user-supplied L2 radius compared against an L2² score.
- **Hubs dominating results.** A few vectors appearing across unrelated queries [8]. Detect by
  histogramming k-occurrence over your query log.

## How to test it

- **Self-match property.** Every vector's own top-1 under a metric-space metric is itself, at
  distance 0. Catches sign errors, indexing offsets, and layout bugs in one line. It does **not**
  hold for raw MIPS — that failure is the definition of MIPS not being a metric.
- **Metric equivalence property.** Over random normalized vectors, ranking by cosine and ranking by
  L2² must produce identical orderings. Any divergence is a bug in one kernel. Likewise assert L2 and
  L2² give identical top-K sets and differ only in reported values.
- **Differential test against a naive kernel.** A three-line scalar loop is the oracle for every
  optimized kernel: distances within a float tolerance, top-K sets exact when distances are well
  separated.
- **Triangle inequality property test.** Assert `d(a,c) ≤ d(a,b) + d(b,c)` for random triples under
  L2, then assert it *fails* under raw inner product. That failing test is the documentation for why
  metric-tree indexes are off the table.
- **Norm invariance.** Scale a vector by a positive constant: cosine ranking unchanged, dot-product
  ranking changed. If neither changes, you are normalizing somewhere you did not intend.

## Minimal demo version

1. Store `float32` vectors in one contiguous `N × D` array plus a parallel array of cached norms.
2. Normalize at write time. Support exactly one metric — cosine, implemented as a dot product over
   normalized vectors — and one internal representation, L2².
3. Use `D = 128`: large enough that dimensionality effects are visible, small enough that a full scan
   over a million vectors is fast. Random unit vectors are adequate for correctness and performance
   work.
4. Write the self-match, metric-equivalence, and triangle-inequality property tests.

You can cut: multiple metrics, per-collection metric configuration, MIPS support, `float16`/`int8`
storage, and Matryoshka truncation. None of these change what you learn.

You cannot cut: **normalization discipline and the cached norm.** If normalization is inconsistent,
every downstream number you measure — recall, latency, index quality — is measuring the bug rather
than the algorithm, and you will spend days blaming the index. And you cannot cut the exact
brute-force kernel, because it is the ground truth against which every approximate index in the
system is scored ([the search problem](02-the-search-problem.md)).

One honest demonstration worth building: generate random vectors at `D` = 2, 10, 100, 1000 and plot
`Dmax/Dmin` for random queries. Watching that ratio walk toward 1 [5][6] is the fastest way to
internalize why the rest of this folder exists.

## Open questions and contested claims

**Aggarwal's L1/fractional-norm recommendation is not what anyone ships.** The 2001 result is not
disputed — `d^(1/k − 1/2)` is a derivation — and the numbers are concrete: 99% versus 89% clustering
accuracy for f = 0.3 against Euclidean [7]. Yet no mainstream vector database defaults to L1, let
alone L0.3. Two defensible readings: the result concerns i.i.d. synthetic and UCI-scale data and does
not transfer to embedding manifolds; or the ecosystem is locked in, because embedding models are
themselves trained with cosine and L2 objectives, so a better query-time metric would be mismatched
with the geometry the model produced. Either way the gap is real and unresolved.

**Cosine similarity of learned embeddings can be arbitrary.** Steck, Ekanadham and Kallus (Netflix)
showed that for regularized matrix-factorization-style linear models with objective
`min_{A,B} ‖X − XAB^⊤‖²_F + λ‖AB^⊤‖²_F`, if `ÂB̂^⊤` is a solution then so are `ÂD` and `B̂D⁻¹` for
**any** diagonal `D` [12]. The dot product `ÂD·(B̂D⁻¹)^⊤ = ÂB̂^⊤` is invariant under that rescaling;
cosine, which divides by each vector's own norm, is not, and can be driven to arbitrary values by the
choice of `D`. In one full-rank worked example item-item similarities "collapse to identity matrix I:
all distinct items appear orthogonal" [12]. The alternative regularization `λ(‖XA‖²_F + ‖B‖²_F)` does
not exhibit the degeneracy [12]. Their conclusion: "cosine similarity can in fact yield arbitrary
results. We find that the underlying reason is not cosine similarity itself" [12] — "the learned
embeddings have a degree of freedom that can render arbitrary cosine-similarities even though their
dot-products are well-defined" [12]. Empirically cosine "can work better but sometimes also worse
than the unnormalized dot-product" [12], and they recommend normalizing *during training* so the
model controls scale, rather than applying cosine post hoc [12]. This is a direct challenge to "just
normalize and use cosine," and the strongest argument in the literature for treating metric choice as
a property of the model rather than a database configuration option.

**Whether the curse of dimensionality applies to real embeddings at all.** Lakshman, Munyampirwa,
Shun and Coleman name the paradox directly: classical theory predicts instability in high dimensions,
"yet modern vector databases function effectively" [13]. Their reconciliation is *stability* — "a
slight perturbation to a query should not dramatically change the list of near neighbors to that
query" — as the explanatory property rather than intrinsic dimensionality or index structure [13].
They extend it to multi-vector, filtered (where large penalties for non-matching filters can *induce*
stability even when unfiltered search is unstable), and sparse search [13]. If this holds, "curse of
dimensionality" is the wrong diagnosis for an underperforming index; instability of the specific
query distribution is the right one.

## Sources

[1] Vicki Boykis — How big are our embeddings now and why? — https://vickiboykis.com/2025/09/01/how-big-are-our-embeddings-now-and-why/ — 2025
[2] OpenAI — Vector embeddings (API docs) — https://developers.openai.com/api/docs/guides/embeddings
[3] Google for Developers — Measuring similarity from embeddings, Machine Learning Crash Course — https://developers.google.com/machine-learning/clustering/dnn-clustering/supervised-similarity
[4] About Vector Database — Euclidean Distance (L2) explained — https://aboutvectordatabase.com/learn/euclidean-distance-l2-explained/
[5] Kevin Beyer, Jonathan Goldstein, Raghu Ramakrishnan, Uri Shaft — When Is Nearest Neighbor Meaningful? — https://minds.wisc.edu/items/4c666f43-b2e4-40d1-bba3-e34072be1576/full — UW-Madison TR1377, ICDT 1999
[6] Kevin Beyer — "When Is Nearest Neighbor Meaningful?" presentation of the theorem and its consequences — https://slidetodoc.com/when-is-nearest-neighbor-meaningful-by-kevin-beyer/
[7] Charu C. Aggarwal, Alexander Hinneburg, Daniel A. Keim — On the Surprising Behavior of Distance Metrics in High Dimensional Space — https://bib.dbvis.de/uploadedFiles/155.pdf — ICDT 2001
[8] Miloš Radovanović, Alexandros Nanopoulos, Mirjana Ivanović — Hubs in Space: Popular Nearest Neighbors in High-Dimensional Data — https://jmlr.org/papers/v11/radovanovic10a.html — JMLR 11, 2010
[9] James Briggs / Pinecone — Product Quantization: Compressing high-dimensional vectors by 97% — https://www.pinecone.io/learn/series/faiss/product-quantization/
[10] Jeff Johnson, Matthijs Douze, Hervé Jégou — Billion-scale similarity search with GPUs — https://arxiv.org/pdf/1702.08734 — arXiv:1702.08734, 2017
[11] Elastic Search Labs — SIMD vector search: Elasticsearch simdvec's 6x speedup — https://www.elastic.co/search-labs/blog/vector-search-simd-optimization-simdvec
[12] Harald Steck, Chaitanya Ekanadham, Nathan Kallus — Is Cosine-Similarity of Embeddings Really About Similarity? — https://arxiv.org/abs/2403.05440 — WWW 2024 Companion Proceedings
[13] Vihan Lakshman, Blaise Munyampirwa, Julian Shun, Benjamin Coleman — Breaking the Curse of Dimensionality: On the Stability of Modern Vector Retrieval — https://arxiv.org/html/2512.12458 — arXiv:2512.12458v2, 2025
