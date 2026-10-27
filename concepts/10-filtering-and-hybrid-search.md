# 10. Filtered and hybrid search

> Real queries are never "find the nearest vectors" — they are "find the nearest vectors *that are in stock, in English, and owned by this tenant*, and also match the exact SKU the user typed." Both halves of that sentence break the clean nearest-neighbour abstraction, and both are solved by the same move: run cheap retrieval twice, then reconcile.

## The problem

An ANN index answers exactly one question: which stored vectors are closest to this one. Two things people always need are outside that contract.

**Metadata predicates.** `WHERE tenant_id = 42 AND price < 100` is trivial for a B-tree and hostile to a proximity graph, because the graph's navigability is a property of the *whole* node set. Filtering removes a query-dependent subset of nodes, and the removal is not known at build time.

**Lexical matching.** Dense embeddings generalise across paraphrase and fail on exact tokens — product SKUs, error codes, surnames, acronyms — where the encoder never learned a precise representation. BM25 does the opposite. Neither subsumes the other, and this is not folklore: BEIR tested 10+ retrieval systems zero-shot across 18 heterogeneous datasets and found that BM25 is a robust baseline, while learned dense and sparse models that outperform it in-domain "often underperform" it once evaluated out of domain [8].

## How it works, part one: filtering

### The three strategies and exactly how each fails

**Pre-filtering.** Compute the set matching the predicate first (usually a roaring bitmap), then search only within it. Recall-correct by construction — it can never miss a qualifying point. Its cost, though, scales with the size of the surviving set rather than with the index's sublinear structure. Qdrant's description: "The mask touches every point, every match becomes a scoring candidate, and a broad filter degrades into brute force" [1]. An unselective predicate turns your ANN index into a linear scan wearing a bitmap.

**Post-filtering.** Run the ANN search for top-`K` (usually over-fetching, `K′ > K`), then apply the predicate to what came back. Fast, trivial to implement, and structurally unable to guarantee `K` results. Qdrant again: "Undershoot and you return too little, overshoot and you drift back toward the brute-force work the index was meant to avoid" [1]. Weaviate's co-founder gives the concrete arithmetic: a filter matching 5% of the data with `k=20` requested yields roughly **one** usable match after the non-matching candidates are dropped [11]. The failure is returning fewer than `K` — sometimes zero — with no error.

**In-filter (filter-during-search).** Evaluate the predicate during graph traversal, skipping ineligible points so the candidate budget is spent only on qualifying ones. Preserves recall far better in principle. Its failure mode is structural: "A strict filter leaves so few eligible points that the paths between them break, and the traversal dead-ends short of the best matches" [1]. Milvus calls the same thing "broken connectivity" — "when many vectors get filtered out, the carefully constructed paths in our graph index get disrupted" [5].

The connectivity mechanism is worth stating precisely, because it is the part that surprises people. Greedy search reaches the query's neighbourhood by walking edges, and the dynamic candidate list it maintains is bounded by `efSearch` [15] ([Graph indexes and HNSW](07-graph-indexes-hnsw.md)). Filtering is equivalent to deleting a query-dependent subset of nodes. Weaviate's explanation: "if C and D are disconnected and thus not evaluated, then the search algorithm cannot find E and F" — the search "cannot follow the graph from an entry point to the target region," and for a restrictive filter uncorrelated with the query there may be **no viable path at all** [2].

```
entry -> A -> B -> [C] -> [D] -> E -> F   (target region)
                    ^      ^
                    filtered out; path severed; E and F unreachable
```

Elastic frames the identical phenomenon from the cost side rather than the correctness side: "filtering documents — thereby reducing the number of candidates — can actually make kNN searches slower," because more comparisons are needed to accumulate enough eligible candidates [6]. Unreachability at high selectivity, extra hops at moderate selectivity — one problem, two symptoms.

### Selectivity is the governing parameter

Every vendor converges on the same variable: the fraction of the corpus the predicate matches.

```
selectivity ->   0%  ..........  50%  ..........  100%
strategy     pre-filter/flat   the danger zone    post-filter or ignore
                 (small set,   (post-filter        (predicate is nearly
                  scan it)      starves; in-filter  free, index works
                                disconnects)        normally)
```

The middle band is where naive strategies fail and where every serious engineering effort is aimed. And the punchline is that the published thresholds do not agree. Elastic activates extended-neighbourhood traversal when **>10%** of the immediate neighbourhood is filtered out, and a further branching heuristic at **≥40%** [6]. Milvus illustrates its brute-force fallback with the case where "approximately 99% of the data doesn't match the filtering expression" [5]. Those differ by an order of magnitude. There is no universal cutover number; there is a **cost model**, and every system's fixed threshold is a hardcoded approximation of one.

### ACORN and Filtered-DiskANN

Two research answers, attacking opposite ends of the pipeline.

**ACORN** (Patel, Kraft, Guestrin, Zaharia; Stanford; SIGMOD 2024) is a **predicate-agnostic** modification to HNSW-style construction and traversal that emulates "a theoretically ideal, but impractical, hybrid search strategy" via "predicate subgraph traversal," reporting **2–1,000× higher throughput at fixed recall** across the predicate selectivities and types tested [3]. Mechanically, per two independent adopters: it performs **two-hop expansion** — following neighbours-of-neighbours rather than only first-degree edges — so when immediate neighbours are filtered out the traversal can still make progress, without knowing the predicate at build time [2]. That is the fix for the severed-path diagram above: if C and D are filtered, you jump from B directly to E.

**Filtered-DiskANN** (Gollapudi, Karia, Sivashankar, Krishnaswamy et al.; Microsoft Research and Columbia; WWW 2023) attacks build time instead, baking filter labels into graph structure. **FilteredVamana** constructs incrementally, pruning with a filter-aware procedure weighing both geometric proximity and label compatibility; **StitchedVamana** builds separate per-label Vamana graphs, then overlays and re-prunes them to remove redundant edges while preserving cross-label efficiency [4]. Reported: order-of-magnitude-or-greater throughput over IVF, HNSW, NHQ and Milvus baselines on filtered queries; filter specificity as low as **10⁻⁴–10⁻⁶** of the corpus with recall@10 above 90%; and a sponsored-search production deployment reporting **30–80% revenue gains** depending on filter specificity [4].

The trade-off between them is the usual one: ACORN needs no build-time knowledge of predicates and works for arbitrary filters; Filtered-DiskANN needs the label vocabulary up front and rewards you with far better performance on extremely selective filters.

## How it works, part two: hybrid search

### What each retriever gets right

BM25 scores exact term overlap with document-length normalisation. It cannot bridge a vocabulary mismatch — "car" and "automobile" are unrelated tokens — but it will find the SKU, the surname, and the error code every time, on any corpus, with no training. Dense retrieval generalises semantically and degrades out of domain, which BEIR established as the load-bearing empirical result rather than an intuition [8].

**Learned sparse** representations sit between them. SPLADE (Formal, Piwowarski, Clinchant; Naver Labs Europe; SIGIR 2021) learns term-weight expansions over the vocabulary using a masked-language-model head, producing a sparse but *learned* representation that still indexes in an ordinary inverted index [9]. uniCOIL (Lin, Ma; Waterloo, 2021) simplifies to a single scalar weight per term in the COIL/DeepImpact family, also inverted-index-compatible, reported at MRR 0.352 on MS MARCO passage dev in single-stage retrieval without reranking [10]. The appeal is real: you get learned semantics while keeping the operational profile — and the filtering behaviour — of a lexical index.

### Reciprocal Rank Fusion

You now have two ranked lists with incomparable scores. The standard answer is to throw the scores away.

Cormack, Clarke and Büttcher (Waterloo/Google, SIGIR 2009) define:

```
RRFscore(d ∈ D) = Σ_{r ∈ R}  1 / (k + r(d))
```

summing over each ranker `r` in the set of rankers `R`, where `r(d)` is `d`'s rank position in `r`'s list. The constant **k was set to 60** in initial testing and held fixed for the rest of the paper's experiments [7]. Their headline result: RRF beat Condorcet Fuse in **7 of 7 TREC trials** (p ≈ 0.008), with average score improvements of 4–5%; the authors attribute the advantage to RRF exploiting diversity across rankers, where Condorcet's majority-vote logic lets weak collective agreement override one ranker's strong individual signal [7].

Why rank rather than score: BM25 scores are unbounded and term-frequency-driven; cosine similarity is bounded in [−1, 1] with a query-dependent distribution. Adding or weighting them requires per-query or per-corpus normalisation to mean anything, and that normalisation is exactly the thing that silently breaks when your corpus changes. Rank is scale-free by construction. The `k = 60` constant damps the influence of top ranks — the difference between rank 1 and rank 2 is `1/61` vs `1/62`, small — which is what makes the fusion robust to one ranker being confidently wrong.

**Cross-encoder reranking** is the third stage: after fusion, a model that attends jointly over query and document scores the shortlist. It is expensive per document and therefore only viable over the top few dozen, which is precisely why the cheap-and-coarse first stages exist. The full pipeline — lexical retrieval, dense retrieval, RRF, cross-encoder — is consensus engineering practice rather than one paper's finding.

## Parameters and knobs

| Parameter | Controls | Typical value | If you get it wrong |
|---|---|---|---|
| Over-fetch factor `K′/K` | Post-filter headroom | 2–10× | Too low: fewer than `K` results returned. Too high: you are doing brute force again [1] |
| Selectivity cutover | When to fall back to flat | Elastic: 10% and 40% [6]; Milvus illustrates ~99% [5] | A fixed threshold is wrong for some other system's data by design |
| `efSearch` under filtering | Traversal budget | Inflate by roughly `1/selectivity` | Not inflating gives the 90.8% and 39.7% recall collapses below [1] |
| ACORN expansion degree | Two-hop neighbourhood size | `M × M` (Elastic cites 32×32 = 1,024 at `M`=32) [6] | Too aggressive and you scan the neighbourhood you were avoiding |
| RRF `k` | Rank damping | **60** [7] | Small `k` over-weights rank-1 results, reintroducing single-ranker confidence errors |
| Fusion weights | Lexical vs dense influence | Equal, unless measured otherwise | Weighting raw scores instead of ranks is the fragile version |

## How real systems do it

Four systems, four different bets, and the disagreement is the useful part.

**Qdrant explicitly rejects both textbooks.** "Qdrant Does Neither" pre- nor post-filtering [1]; it filters in place during HNSW traversal, adds **Filterable HNSW** (extra indexed edges built with anticipated filter fields in mind) and an ACORN-style algorithm. Its published stress tests are the most useful numbers in this file because they show naive strategies failing on ordinary data: a broad-value filter gave **90.8% recall**, and an AND of two broad values gave **39.7% recall** — and both Filterable HNSW and ACORN "restored both failing cases to 100% recall" [1]. A 39.7% recall regression from adding a `WHERE` clause is not a tuning problem; it is a broken query.

**Elastic ships a measured cost model.** Lucene 10.2 (Feb 2025) landed ACORN-1: the searcher extends to the neighbours-of-neighbours neighbourhood only when **>10%** of the immediate neighbourhood is filtered out, with a further branching heuristic at **≥40%**, and a stopping rule of scoring at least `neighborCount * 1.0/(1.0 - neighborFilterRatio)` vectors. Reported: "up to 5x faster searches with little drop in recall," with diminishing benefit as filters get broader [6]. Before this, Elasticsearch's filtered kNN was over-fetch with a `num_candidates` knob — post-filter semantics with a headroom parameter [6].

**Weaviate made the fallback adaptive rather than fixed.** From v1.27, "ACORN will automatically switch back to sweeping [brute force] if it detects a high selectivity filter" — detecting at runtime when plain brute force over the matching set is cheaper than graph traversal, instead of trusting a constant [2]. A `flatSearchCutOff` knob exists for direct control, and Weaviate notes the obvious limit: once the filtered set is large, "brute force query performance scales linearly with the dataset size" and becomes the bottleneck [2].

**Milvus layers four options.** Bitset pre-filtering is primary; **iterative filtering** ("retrieve a set of closest matches, apply filters to this batch, then repeat if necessary") reduces the cost of expensive predicate evaluation; full brute-force scanning kicks in at extreme selectivity; and metadata-aware indexing precomputes per-attribute subindexes [5].

The pattern: everyone started with one strategy, measured it failing, and ended up with a runtime decision between at least three. That convergence — from *a strategy* to *a cost model* — is the actual lesson of this section.

## Complexity and cost

- **Pre-filter:** O(`N`) to build the bitmap (or O(matches) with an inverted index) plus O(matches · `D`) if it degenerates to scan. Recall-exact.
- **Post-filter:** unchanged ANN cost at `K′`, plus O(`K′`) predicate evaluations. Returns between 0 and `K` results.
- **In-filter:** ANN cost with an inflated `efSearch`, roughly `1/selectivity` more work, plus one predicate evaluation per visited node — which matters when the predicate is expensive.
- **Two-hop expansion:** up to `M²` candidate edges per hop (1,024 at `M`=32) [6], applied selectively.
- **Hybrid:** two full retrievals in parallel, then O(`K′` log `K′`) fusion, then `R` cross-encoder forward passes. The reranker dominates end-to-end latency and is the reason `R` is 20 and not 200.

## Failure modes

- **Silent recall collapse under an ordinary `WHERE` clause** — 90.8% and 39.7% on real data [1]. The query succeeds; the answers are wrong.
- **Fewer than `K` results, or zero**, from post-filtering a selective predicate [1][11].
- **Latency increasing when the filter narrows.** Counter-intuitive and real [6]; operators tune for the wrong thing because they assume a filter can only help.
- **A fixed cutover threshold tuned on one workload.** Given that published thresholds span 10% to 99% [5][6], a constant that works for your test data is a coincidence.
- **Multi-tenant filtering treated as an afterthought.** `tenant_id = X` is the most selective, most common filter in practice, and it is exactly the case where in-filter traversal disconnects.
- **Score-based fusion drifting.** Raw-score mixing tuned against one corpus silently degrades when the score distributions shift, with no error surface.
- **Testing filters on uniformly random attributes.** Real predicates correlate with the vectors; random ones do not, and the correlated case is much harder.

## How to test it

- **The selectivity sweep.** Fix a query set, vary the predicate from 0.1% to 99% selectivity, and plot recall@10 and p95 latency for each strategy against exact filtered brute force. This single chart contains the entire concept, including the non-monotonic latency curve.
- **Correlated versus uncorrelated predicates.** Test both a filter aligned with the vector distribution (one cluster's worth of labels) and one uncorrelated with it. The uncorrelated case is where paths sever.
- **Result-count invariant:** post-filtering must never silently return fewer than `K` when at least `K` matching vectors exist. Assert it, and watch it fail.
- **Ground truth is filtered brute force**, not unfiltered ANN results with the predicate applied. Getting this wrong makes post-filtering look correct.
- **Fusion property test:** RRF must be permutation-invariant across input rankers and must be unchanged by monotonic rescaling of either ranker's scores. The second property is the entire argument for using it; test it.
- **Rank-fusion differential:** compare RRF against normalised score mixing on the same candidate lists, then shift one retriever's score scale and re-run. The score-mixed version moves; RRF does not.

## Minimal demo version

Post-filtering plus a selectivity-triggered fallback to brute force is roughly 80 lines and reproduces the entire industry design — including the reason everyone eventually needs a cost model.

```
search_filtered(q, K, pred):
    n_match = cardinality(pred)             # from an inverted index or a scan
    if n_match / N < FLAT_CUTOFF:           # very selective -> scan the matches
        return brute_force(q, K, over=matching_ids(pred))
    results = ann_search(q, K * OVERFETCH)  # broad -> post-filter
    hits = [r for r in results if pred(r)]
    if len(hits) < K:                        # honest failure, then fall back
        return brute_force(q, K, over=matching_ids(pred))
    return hits[:K]
```

RRF is 15 lines:

```
fuse(lists, k=60):
    score = defaultdict(float)
    for lst in lists:
        for rank, doc in enumerate(lst, start=1):
            score[doc] += 1.0 / (k + rank)
    return sorted(score, key=score.get, reverse=True)
```

**Cut freely:** the in-filter traversal path — post-filter plus fallback covers the same trade-off space and is a tenth of the work; two-hop ACORN expansion; Filterable-HNSW-style build-time edges; the cross-encoder reranker (a stub that reverses the list proves the plumbing); learned sparse retrieval — plain BM25 is enough to demonstrate the complementarity; and any predicate language beyond equality on a single attribute.

**Cannot cut:** the selectivity measurement and the fallback branch. A demo that always post-filters is not a filtering demo, it is a demonstration of one failure mode, and it will look fine right up until you test a selective predicate. Also keep the filtered-brute-force ground truth, because with the wrong baseline every strategy scores 100%. And keep the result-count assertion — the moment you see it fire, you have understood post-filtering.

## Open questions and contested claims

- **The widely-cited Weaviate "~15% selectivity threshold" could not be verified verbatim.** What Weaviate's own material describes is an adaptive runtime switch from v1.27 plus a `flatSearchCutOff` config knob, not a fixed constant [2]. A separate Weaviate-authored article does report a crossover near 40,000 objects / 15% of the dataset below which flat search beat constrained HNSW traversal in their tests [11], which is plausibly the origin of the folk number — but it is one measurement on one dataset, not a documented product threshold.
- **No universal cutover number exists, and the published ones differ by 10×** — Elastic's 10%/40% [6] against Milvus's illustrative ~99% [5]. Any source quoting a single industry-wide threshold is reporting one vendor's heuristic.
- **Whether filter-aware graph construction beats predicate-agnostic traversal** is unresolved. Filtered-DiskANN bakes labels into the graph and reports order-of-magnitude gains at extreme specificity [4]; ACORN needs no build-time knowledge and reports 2–1,000× at fixed recall [3]. They were not evaluated head to head under controlled conditions here.
- **Filtered-ANN benchmarks may be measuring the wrong difficulty.** A hardness-controlled benchmark generator has been proposed on the argument that existing filtered-ANN benchmarks do not control for query hardness, making reported results hard to compare [16]; its publication venue and date are inconsistent across the record, so treat the direction as informative and the provenance as unsettled.
- **Filtered search is recent enough to still be a competition track.** The NeurIPS 2023 Big ANN competition departed from prior editions by adding four tracks — Filtered Search, Out-of-Distribution Data, Sparse Vectors, and Streaming ANNS — precisely because the earlier static, dense, filter-free format under-tested what production workloads do [12]. A dedicated VLDB survey of filtered vector search now exists [14], and vendor engineering writeups on ACORN's remaining limitations continue to appear [13] — this is an open area, not a solved one.
- **No controlled experiment quantifying raw-score-mixing degradation versus RRF** was located. The normalisation-fragility argument is consensus reasoning; the 7/7 Condorcet result [7] is the only measured fusion comparison behind this file.

## Sources

[1] Couzon, D. — "Vector Search Filtering Strategies: Definitions and Failure Modes" — Qdrant blog — https://qdrant.tech/blog/pre-filtering-vs-post-filtering/ — 2026
[2] Rodriguez, A., Trengrove, J., & Hwang, J.-P. — "How we speed up filtered vector search with ACORN" — Weaviate blog — https://weaviate.io/blog/speed-up-filtered-vector-search — 2024
[3] Patel, L., Kraft, P., Guestrin, C., & Zaharia, M. — "ACORN: Performant and Predicate-Agnostic Search Over Vector Embeddings and Structured Data" — SIGMOD 2024 — https://arxiv.org/abs/2403.04871 — 2024
[4] Gollapudi, S., Karia, N., Sivashankar, V., Krishnaswamy, R., et al. — "Filtered-DiskANN: Graph Algorithms for Approximate Nearest Neighbor Search with Filters" — WWW 2023 — https://harsha-simhadri.org/pubs/Filtered-DiskANN23.pdf — 2023
[5] Gao, C., & Xu, P. — "Vector Search in the Real World: How to Filter Efficiently Without Killing Recall" — Milvus blog — https://milvus.io/blog/how-to-filter-efficiently-without-killing-recall.md — 2025
[6] Trent, B. — "Filtered HNSW & kNN search in Lucene and Elasticsearch" — Elasticsearch Labs — https://www.elastic.co/search-labs/blog/filtered-hnsw-knn-search — 2025
[7] Cormack, G.V., Clarke, C.L.A., & Büttcher, S. — "Reciprocal rank fusion outperforms Condorcet and individual rank learning methods" — SIGIR 2009 — https://cormack.uwaterloo.ca/cormacksigir09-rrf.pdf — 2009
[8] Thakur, N., Reimers, N., Rücklé, A., Srivastava, A., & Gurevych, I. — "BEIR: A Heterogeneous Benchmark for Zero-shot Evaluation of Information Retrieval Models" — NeurIPS 2021 Datasets & Benchmarks — https://arxiv.org/abs/2104.08663 — 2021
[9] Formal, T., Piwowarski, B., & Clinchant, S. — "SPLADE: Sparse Lexical and Expansion Model for First Stage Ranking" — SIGIR 2021 — https://dl.acm.org/doi/10.1145/3404835.3463098 — 2021
[10] Lin, J., & Ma, X. — "A Few Brief Notes on DeepImpact, COIL, and a Conceptual Framework for Information Retrieval Techniques that Explicitly Model Term Weight" (uniCOIL) — https://arxiv.org/abs/2106.14807 — 2021
[11] Dilocker, E. — "Effects of filtered HNSW searches on Recall and Latency" — Towards Data Science — https://towardsdatascience.com/effects-of-filtered-hnsw-searches-on-recall-and-latency-434becf8041c/ — 2021
[12] Simhadri, H.V., Aumüller, M., Douze, M., et al. — "Results of the Big ANN: NeurIPS'23 competition" — https://arxiv.org/abs/2409.17424 — 2024
[13] Qdrant — "Filtered Vector Search: What ACORN Fixes, and What Fixes ACORN" — https://qdrant.tech/articles/filtered-vector-search-acorn/ — accessed 2026
[14] Caminal et al. — "Filtered Vector Search: State-of-the-Art and Research Opportunities" — PVLDB Vol. 18 — https://vldb.org/pvldb/vol18/p5488-caminal.pdf — accessed 2026
[15] Malkov, Y.A., & Yashunin, D.A. — "Efficient and Robust Approximate Nearest Neighbor Search Using Hierarchical Navigable Small World Graphs" — IEEE TPAMI — https://arxiv.org/abs/1603.09320 — 2016/2018
[16] Lim, M., Kim, D., Kim, M., & Do, J. — "Revisiting Filtered ANN Benchmarks: A Hardness-Controlled Benchmark Generator for Realistic Evaluation" — https://arxiv.org/abs/2606.14193 — publication date unsettled
