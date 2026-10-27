# 09. Choosing an index

> There are only four real answers — flat, IVF/IVFPQ, HNSW, and a disk-resident graph — and the choice is decided less by dataset size than by whether the index fits in RAM you are willing to pay for, what recall you actually need, and how much build time you can absorb.

## Start by rejecting the question you were asked

"Which index is fastest" has no answer. The VLDB graph-index survey found that "an algorithm's superiority is contingent on the dataset rather than being fixed in its performance," and demonstrated it at an absurd magnitude: NSG is **125× faster than HNSW** at 0.99 recall on the Msong dataset and **80× slower than HNSW** at the same recall target on Crawl [11]. Same two algorithms, same recall, two datasets, a four-order-of-magnitude swing in the verdict.

So the useful question is which constraints bind, and there are eight of them.

| Axis | Question | Why it decides things |
|---|---|---|
| Dataset size `N` | How many vectors, growing how fast? | Sets whether raw storage alone is a problem |
| Memory budget | How much RAM will you pay for? | The actual crossover variable, not `N` |
| Target recall | 100%, 95%, or 90%? | 100% eliminates every approximate method outright |
| Latency SLO | p50 or p99, and what number? | Decides whether disk is admissible at all |
| Build time | How long can indexing take, and how often? | The most commonly ignored cost |
| Update rate | Inserts and deletes per second? | Graphs are hostile to churn |
| Filter usage | Do queries carry metadata predicates? | Changes the answer entirely — see [Filtered and hybrid search](10-filtering-and-hybrid-search.md) |
| Query volume | Thousands of queries, or billions? | Below a threshold, build cost never amortizes |

That last one is the cheapest decision available and almost nobody makes it consciously. FAISS's own guidance: with only **1,000–10,000 searches** expected, use flat, because "the index building time will not be amortized by the search time" [1].

## Build time and memory are first-class costs

Everyone benchmarks query latency. Almost nobody benchmarks the two costs that actually determine whether a deployment is viable.

**Build time.** One million vectors at 1536 dimensions, `m=16`, `ef_construction=200`, took **1 hour 27 minutes** to build single-threaded in pgvector 0.5.1; the parallel build in 0.6.0 did the same job in **9.5 minutes** — with accuracy (0.963) and QPS (~950) unchanged, confirming parallelism changes wall clock and not graph quality [3]. `ef_construction` scales build time roughly linearly, measured on the same dataset serially: **38m 08s at 64, 1h 06m 59s at 100, 1h 27m 45s at 200** [3]. At 10M rows of 1536-D vectors, index build can take hours on a single core and produce tens of gigabytes [13].

**Index memory.** A 1M-row index of typical embeddings "can be 8GB or larger" [4] — substantially above the raw float32 bytes, because of the adjacency lists at every layer. At 100M rows of 1536-D, the raw vectors alone exceed **600 GB** before any index overhead [13]. At billion scale, AWS's estimates for 1B × 128-D with `m=16` and one replica are stark: roughly **1,408 GB for HNSW**, **1,126 GB for IVF**, and **70 GB for IVFPQ** [9].

**Build memory is not serving memory.** DiskANN's peak memory during index *construction* was reported around **1,100 GB** on specialized hardware for the non-merged build path, versus **under 64 GB** for the merged build approach — and its serving footprint is 64 GB [5]. An index you cannot afford to build is not an index you have, and the two numbers can differ by more than an order of magnitude.

## The decision table

| If | Choose | Because |
|---|---|---|
| Exact results required | Flat | Only flat guarantees precision; it "does not compress the vectors, but does not add overhead" [1] |
| `N` under ~10k–100k, or few total queries | Flat | Build cost never amortizes [1]; brute force is linear and cache-friendly ([Brute force and kernels](03-brute-force-and-kernels.md)) |
| Filtered query with a highly selective predicate | Flat over the matching set | The universal correctness backstop; Weaviate measured a crossover near 40,000 objects / 15% of the dataset below which flat beats constrained HNSW [16] |
| `N` < 1M, RAM comfortable, recall ~95% | IVF with `nlist` in `4·√N … 16·√N` | FAISS's own band, with training needing 30·`nlist` to 256·`nlist` vectors [1] |
| Memory is the binding constraint and ~50–60% recall is acceptable | IVFPQ | ~70 GB vs ~1,408 GB at 1B × 128-D, and ~$11/hr vs ~$75/hr in AWS's own cluster [9] |
| Fits in RAM, want the best recall/latency Pareto point | HNSW | Highest measured recall at every latency budget in AWS's billion-scale test [9] |
| Does not fit in affordable RAM, `N` in the hundreds of millions to billions | DiskANN / disk-resident graph | 1B points on one 64 GB machine at >5,000 QPS [5] |
| Heavy insert/delete churn | Reconsider everything | Graph indexes degrade under churn ([Updates and deletes](08-updates-and-deletes.md)) |

FAISS extends the size band beyond 1M with concrete recipes: `IVF65536_HNSW32` for 1M–10M, `IVF262144_HNSW32` for 10M–100M, and `IVF1048576_HNSW32` for 100M–1B [1]. Note what those strings say — a coarse quantizer that is itself an HNSW graph. The families are not rivals; at scale they compose.

## When each is genuinely correct

**Flat is correct more often than anyone admits.** It is the only thing that returns exact answers, has zero build time, zero tuning, and perfectly predictable latency. FAISS measured SIFT1M flat L2 at **100% recall, 8.26 ms, 256 MB** [15]. If 8 ms meets your SLO — and for a 1M-vector corpus behind an LLM call taking 900 ms, it obviously does — every other index in this file is an unforced complexity purchase. Flat is also the correctness backstop that every filtered-search implementation falls back to, and the ground truth every recall measurement is scored against.

**IVF is correct when memory is comfortable and you want a tunable dial.** It partitions into `nlist` cells and probes `nprobe` of them. FAISS's `nlist ≈ C·√N` rule comes from balancing assignment cost against query cost [2], and the recommended tuning method is empirical: `nprobe` is "specified at query time (useful for measuring trade-offs between speed and accuracy)" [2] — sweep it and measure, do not derive it. IVF's real limitation is structural rather than a matter of tuning: "even with large nprobe, if a query's true nearest neighbor resides in a cluster that is not among the searched ones, IVF will not retrieve it" [14]. A wrong coarse assignment is permanently unrecoverable for any `nprobe < nlist`, which puts a hard recall ceiling below 100% on the whole family. See [IVF and clustering](05-ivf-and-clustering.md).

**IVFPQ is correct when memory dominates and you accept a real recall cost.** The AWS billion-scale numbers are the clearest statement available: HNSW achieved **recall@10 of 0.84–0.99** across its tuning range while IVFPQ reached only **0.17–0.61**, and HNSW was also faster at comparable recall (p50/p90/p99 of **103.7/131.8/151.7 ms** for HNSW optimized-for-recall versus **163/196.5/228.9 ms** for IVFPQ optimized-for-recall) [9]. IVFPQ's sole win was memory — a >20× reduction — and cost. AWS's own conclusion: "there isn't one algorithm or approach that optimises for all the metrics at once" [9]. At the 1M scale the same trade shows up smaller: PQ alone at **50% recall, 1.49 ms, 6.5 MB**, IVFPQ at **52% recall, 0.09 ms, 9.2 MB**, against flat's 100% / 8.26 ms / 256 MB [15]. Quantization is what makes those numbers possible — see [Quantization](06-quantization.md).

**HNSW is correct when the index fits in RAM you are willing to pay for.** That is the whole condition. It has the best measured recall/latency frontier in memory, it exposes `efSearch` as a post-build knob so you can move along that frontier without reindexing [12], and it costs you build time, memory, and a hostile relationship with deletes. Vendor guidance says the same thing without a number: HNSW is appropriate when "your dataset can fit comfortably (and affordably) in memory" [7].

**DiskANN is correct when it does not.** The original result: 1 billion 128-D points (SIFT1B) on a single workstation with **64 GB RAM and an inexpensive SSD**, achieving **>5,000 QPS, mean latency <3 ms, and 95%+ 1-recall@1** on 16 cores — while compression-based competitors at comparable memory "plateau around 50% 1-recall@1" [5]. It stores 5–10× more points per node than DRAM-resident methods at comparable recall by moving the graph to disk [5]. Sizing for a billion-scale 128-D float32 dataset runs roughly **750 GB–1 TB of NVMe** (512 GB for full-precision vectors plus ~224 GB for graph edges) and **64–128 GB of RAM** for the PQ cache [6].

## Where the crossover actually is

There isn't a published one, and you should be suspicious of anyone who quotes you a number.

The most specific figure available is soft: "If your dataset fits in RAM (under ~100M vectors), HNSW typically delivers the lowest latency. Once you're indexing hundreds of millions of vectors, or when RAM cost is a constraint, DiskANN is the more practical choice" [6]. A second vendor comparison says the same thing with no number attached — HNSW when it fits in memory, DiskANN "when storing hundreds of millions or billions of vectors" because "HNSW's memory requirements become cost-prohibitive" [7]. A third declines to quantify at all, stating only directionally that HNSW "scales well for sizable datasets, especially in RAM" [8].

Notice that "~100M vectors" is not a property of the algorithms. It is `N × D × 4 bytes × graph overhead` crossing what a commodity server's RAM costs, for typical dimensionalities of 128–768. Change `D` and the number moves proportionally: at 1536-D the crossover arrives four times sooner than at 384-D. The invariant is the **memory-budget-to-dataset-size ratio**, and 100M is just where that ratio becomes uncomfortable at typical dimensions. Compute it for your own `N` and `D` rather than inheriting someone else's.

No controlled head-to-head build-time benchmark across HNSW, IVFPQ and DiskANN at fixed `N` and `D` was located for this file. Absence of that number is itself informative: the industry publishes query latency obsessively and build cost almost never.

## How real systems do it

**FAISS ships the decision as a string.** Its index-factory guidance is a lookup table on `N` — flat below the amortization threshold, `IVF{4–16·√N}` below 1M, then progressively larger `IVF{nlist}_HNSW32` recipes through 1B — with a separate memory axis layered on top: moderate concern gets clustering plus `,Flat` with `nprobe` tuning, high concern gets `OPQ{M}_D,...,PQ{M}x4fsr`, maximum compression gets RaBitQ variants [1]. This is worth studying as a design artifact: FAISS refuses to pick for you and instead makes the axes explicit and composable.

**Vector databases mostly do not let you choose, and that is a product decision.** Qdrant, Weaviate and Milvus all default to HNSW, and their engineering effort goes into the surrounding machinery — segment optimizers, tombstone thresholds, filtering strategies — rather than index selection. The cost surfaces elsewhere: pgvector inherits Postgres's entire durability and replication stack for free, but pays for it with HNSW build operations that run inside `maintenance_work_mem` and, at scale, produce indexes of tens of gigabytes after hours of single-core work [13].

**The systems that do offer a choice, offer it because the memory bill forced them to.** AWS's OpenSearch guidance exists precisely because 1B × 128-D costs $75/hr on HNSW and $11/hr on IVFPQ in their own experimental cluster [9] — that is the entire reason a lower-recall index is on the menu.

**And partition-based designs are not finished.** SPANN — centroids in memory, posting lists on disk, hierarchical balanced clustering, query-aware disk pruning — reports **2× the speed of DiskANN at 90% recall on equal memory**, and 90% recall@1 and recall@10 in around one millisecond using only 32 GB [10]. That is a partition-family method beating a strong graph method outright on the recall/latency/memory frontier, not merely trading recall for memory the way plain IVFPQ does [9]. The three-way tension — HNSW best recall/latency and worst memory, IVFPQ best memory and worst recall, SPANN competitive on all three against DiskANN — is the actual state of the debate. It does not resolve to a winner.

## If you are building to learn, build these in this order

1. **Flat, with a good distance kernel.** It is your ground truth forever. Every recall number in every later milestone is scored against it, and comparing one approximation to another is not a measurement. Teaches: the memory wall, cache behaviour, why `D` matters. ([Brute force and kernels](03-brute-force-and-kernels.md))
2. **IVF with k-means.** Teaches partitioning, the `nlist`/`nprobe` trade, and — most valuably — the boundary effect, because you can construct a query whose true nearest neighbour is unreachable and watch recall cap out below 100% no matter what you tune [14]. ([IVF and clustering](05-ivf-and-clustering.md))
3. **Scalar quantization, then PQ.** Teaches that the index and the storage format are separate decisions, and that rescoring against full-precision vectors is what makes lossy compression usable. ([Quantization](06-quantization.md))
4. **HNSW.** The richest single implementation in the pack, about 400 lines, and the only one where a subtle omission — the neighbour-selection heuristic — produces a graph that passes uniform-random tests and collapses on clustered data. ([Graph indexes and HNSW](07-graph-indexes-hnsw.md))
5. **Tombstones and rebuild-at-threshold on top of HNSW.** About 50 lines, and it turns an abstract weakness into a curve you can plot. ([Updates and deletes](08-updates-and-deletes.md))
6. **Post-filtering with a selectivity-triggered fallback to flat.** Roughly 80 lines and it reproduces the entire industry design. ([Filtered and hybrid search](10-filtering-and-hybrid-search.md))

Stop there. Do not implement DiskANN as a learning exercise — its value is entirely in the SSD I/O engineering, which is a different subject from ANN, and you will learn more by reading the paper's beam-search-with-parallel-reads section than by building it.

Throughout: sweep each index's tunable parameters and keep the Pareto frontier of the recall-versus-QPS trade-off, which is the methodology ANN-Benchmarks established and the only honest way to compare two indexes [20]. A single (recall, latency) point for each is not a comparison; it is two arbitrary configurations. See [Benchmarking](22-benchmarking.md).

## Open questions and contested claims

- **The HNSW-to-DiskANN crossover has no published controlled measurement.** "~100M vectors" appears in two vendor sources [6][7] and is declined by a third [8]. It is a derived heuristic, not a measured threshold, and it moves with `D`.
- **Whether partition or graph wins at billion scale is unresolved.** AWS's benchmark says graphs win decisively on recall [9]; SPANN's paper says a well-engineered partition method beats a strong graph method outright [10]. Both are single-benchmark results.
- **Whether HNSW's hierarchy earns its memory.** "Down with the Hierarchy" (ICML 2025) argues a flat navigable small world graph matches HNSW on high-dimensional data at lower memory, attributing the benefit to hub nodes rather than the layers [17]. If correct, one of the two structural reasons to prefer HNSW over a flat graph disappears.
- **Update rate as a selection axis is under-quantified.** Every source agrees graph indexes suffer under churn, and Weaviate documents implementing updates as delete-plus-reinsert [18] with measured pgvector recall decay past ~10% deletions [19] — but no source gives a writes-per-second threshold at which you should choose a different index family instead of a different maintenance policy.

## Sources

[1] Meta AI Research — "Guidelines to choose an index" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index — accessed 2026
[2] Meta AI Research — "Faiss indexes" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Faiss-indexes — accessed 2026
[3] Supabase Engineering — "pgvector 0.6.0: 30x faster with parallel index builds" — https://supabase.com/blog/pgvector-fast-builds — accessed 2026
[4] Crunchy Data — "HNSW Indexes with Postgres and pgvector" — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector — accessed 2026
[5] Subramanya, S.J., Devvrit, Kadekodi, R., Krishnaswamy, R., & Simhadri, H.V. — "DiskANN: Fast Accurate Billion-point Nearest Neighbor Search on a Single Node" — NeurIPS 2019 — https://proceedings.neurips.cc/paper_files/paper/2019/file/09853c7fb1d3f8ee67a61b6bf4a7f8e6-Paper.pdf — 2019
[6] Couchbase — "What Is DiskANN? Billion-Scale Vector Search Explained" — https://www.couchbase.com/blog/diskann/ — accessed 2026
[7] Vectroid — "HNSW vs DiskANN: comparing the leading ANN algorithms" — https://www.vectroid.com/resources/HNSW-vs-DiskANN-comparing-the-leading-ANN-algorithm — accessed 2026
[8] TigerData — "HNSW vs. DiskANN" — https://www.tigerdata.com/learn/hnsw-vs-diskann — accessed 2026
[9] AWS Big Data Blog — "Choose the k-NN algorithm for your billion-scale use case with OpenSearch" — https://aws.amazon.com/blogs/big-data/choose-the-k-nn-algorithm-for-your-billion-scale-use-case-with-opensearch/ — accessed 2026
[10] Chen, Q., Zhao, B., Wang, H., Li, M., Liu, C., Li, Z., Yang, M., & Wang, J. — "SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search" — NeurIPS 2021 — https://arxiv.org/abs/2111.08566 — 2021
[11] Wang, M., Xu, X., Yue, Q., & Wang, Y. — "A Comprehensive Survey and Experimental Comparison of Graph-Based Approximate Nearest Neighbor Search" — PVLDB Vol. 14 — https://ar5iv.labs.arxiv.org/html/2101.12631 — 2021
[12] Malkov, Y.A., & Yashunin, D.A. — "Efficient and Robust Approximate Nearest Neighbor Search Using Hierarchical Navigable Small World Graphs" — IEEE TPAMI — https://arxiv.org/abs/1603.09320 — 2016/2018
[13] ParadeDB — "pgvector Limitations" — https://www.paradedb.com/learn/postgresql/pgvector-limitations — accessed 2026
[14] VeloDB — "Inverted File Index (IVF) in Vector Similarity Search" — https://www.velodb.io/glossary/inverted-file-index-in-vector-similarity-search — accessed 2026
[15] Briggs, J. — "Product Quantization: Compressing high-dimensional vectors by 97%" — Pinecone Learn — https://www.pinecone.io/learn/series/faiss/product-quantization/ — accessed 2026
[16] Dilocker, E. — "Effects of filtered HNSW searches on Recall and Latency" — Towards Data Science — https://towardsdatascience.com/effects-of-filtered-hnsw-searches-on-recall-and-latency-434becf8041c/ — 2021
[17] "Down with the Hierarchy: The 'H' in HNSW Stands for 'Hubs'" — ICML 2025 — https://arxiv.org/abs/2412.01940 — 2024
[18] Weaviate — "Weaviate, a vector database with ANN Index and CRUD support" — https://weaviate.io/blog/crud-support-in-weaviate — accessed 2026
[19] Perun Engineering — "pgvector in production: five failure patterns" — https://perun.au/insights/pgvector-production/ — accessed 2026
[20] Aumüller, M., Bernhardsson, E., & Faithfull, A. — "ANN-Benchmarks: A Benchmarking Tool for Approximate Nearest Neighbor Algorithms" — Information Systems Vol. 87 — https://github.com/erikbern/ann-benchmarks/blob/main/README.md — 2020
