# 07. Graph indexes and HNSW

> Build a sparse graph in which every vector points at a handful of nearby vectors, then answer a query by walking downhill from an arbitrary entry point — the only family of ANN index that stays fast when `D` is in the hundreds, and the only one whose cost is dominated by pointer-chasing rather than arithmetic.

**Notation:** in this file `M` means **HNSW's max neighbours per node per layer**. In [Quantization](06-quantization.md) `M` means the number of PQ subvectors. Unrelated quantities, same letter, because both papers used it.

## The problem

Partitioning indexes — k-d trees, LSH buckets, IVF cells — all rest on the same bet: that carving the space into regions lets you rule out most of it. In high dimensions that bet loses. Yandex researchers state the failure plainly: "due to the curse of dimensionality, for large d the algorithms for exact NNS provide little improvement over a brute-force algorithm," and empirically "methods based on similarity graphs were shown to outperform tree-based and LSH-based techniques according to the public benchmarks" [9]. See [Trees and hash indexes](04-tree-and-hash-indexes.md) for why the geometry collapses; IVF survives by giving up on exactness entirely, at the cost of a hard recall ceiling ([IVF and clustering](05-ivf-and-clustering.md)).

A graph makes a different bet. It never partitions anything. It stores, per vector, a short list of neighbours, and relies on the fact that *local* proximity information composes into *global* navigability. You do not need the geometry to be separable; you need it to be traversable.

## How it works

### Which graph would be correct, and why you cannot have it

Three classical structures define the target.

A **Delaunay graph** connects every pair of points sharing a Voronoi cell boundary. On it, greedy descent — repeatedly step to whichever neighbour is closer to the query — provably reaches the true nearest neighbour. It is also unbuildable: "the worst-case combinatorial complexity of the Voronoi diagram in dimension d grows as Θ(n^⌈d/2⌉)" [3]. At `D = 128` this is not a large number, it is a joke.

A **relative neighbourhood graph (RNG)** links `p` and `q` only when no third point `r` sits inside the lune — the intersection of the two balls of radius `|pq|` centred at `p` and `q`. Out-degree stays low, but the RNG "lacks sufficient edges to guarantee monotonic search paths" [3]: greedy search can stall at a local minimum that is not the answer.

A **k-NN graph** (each vertex linked to its `k` nearest) is cheap to approximate and, in the same author's words, "fully devoid of the nice theoretical properties" of the other two, precisely because of that fixed sparsity [3].

The property you actually want is a **monotonic search network (MSNET)**: for every pair `(s, t)` there exists a path from `s` to `t` along which every hop strictly decreases distance to `t`. Building an exact MSNET is O(n³) with unbounded out-degree [3][4]. Every practical graph index — HNSW, NSG, Vamana — is a bounded-degree heuristic approximation to this, and none of them recovers the guarantee.

A more recent account reframes it: search is two-phase — traverse toward the graph's largest strongly connected component, then converge inside it — and what predicts efficiency is the graph's **clustering coefficient**, not adherence to any one proximity-graph definition [3]. The classical framing tells you what edges to add; this one tells you why heuristic pruning works anyway.

### Greedy routing and the beam

Naive greedy search starts at a random node and hill-climbs to a local minimum. Production implementations instead run best-first search: "a pool of l best candidates is maintained, and the search terminates when all their neighbors are further away than the current worst element in the candidate set" [9]. That pool size is the single knob that trades recall against latency, and in HNSW it is called `ef`.

### NSW: long-range links for free

Malkov et al.'s Navigable Small World graph (2014) is a single flat graph built incrementally: for each new element, greedily search the graph as built so far for its `f` nearest already-inserted neighbours, and connect bidirectionally [2]. The trick is entirely in the ordering. Edges created early, when the graph is small and sparse, span enormous distances in the final dense graph — they become the highway system. The authors state it directly: "the navigable small world feature of the graph is based on keeping long-range links of Delaunay approximation links, created in the beginning of construction" [2]. Empirically the greedy path length grows only logarithmically with dataset size at fixed accuracy [2]. The weakness is that search terminates at a local minimum, which is exactly what HNSW was designed to reduce.

### HNSW

HNSW keeps NSW's insertion mechanism and stacks a skip-list-like hierarchy on it.

```
layer 2        (A)------------------(F)          ~ N/M² nodes
                |                    |
layer 1     (A)-(C)---------(E)-----(F)          ~ N/M nodes
             |   |           |       |
layer 0  (A)-(B)-(C)-(D)-(E)-(F)-(G)-(H)         all N nodes

query: enter at top, ef=1 greedy descent -> ef=efSearch beam at layer 0
```

**Level assignment.** Each element draws `level = floor(-ln(unif(0,1)) * mL)` with `mL = 1/ln(M)` [1]. This gives an exponentially decaying distribution: nearly everything lives only at layer 0, a fraction `1/M` reaches layer 1, and so on. A single global **entry point** is maintained at the topmost occupied layer.

**Search** [1]:
1. From the entry point, descend layer by layer down to layer 1 with `ef = 1` — pure greedy, one candidate. This is a cheap zoom-in that costs a logarithmic number of hops and does no real work.
2. At layer 0, run the same graph search with `ef = efSearch`, maintaining a dynamic candidate list (a dual heap of visited-but-unexpanded candidates).
3. Return the top-`K`.

**Insert** [1]:
1. Draw the element's top layer from the `mL` formula.
2. Greedy-descend with `ef = 1` from the global entry point down to one layer above the new element's top layer.
3. From the new element's top layer down to 0: search with `ef = efConstruction`, run neighbour selection on the candidates, add bidirectional edges, and where a neighbour's edge list now exceeds `Mmax` (or `Mmax0` at layer 0), **shrink it using the same selection heuristic** rather than truncating by distance.

That last clause is the part people drop, and it is load-bearing.

### SELECT-NEIGHBORS-HEURISTIC — the thing you must not cut

The obvious neighbour selection is "keep the M nearest candidates." The paper's Algorithm 4 does something else: walk candidates nearest-first and **add an edge only if the candidate is closer to the base element than it is to any already-selected neighbour** [1]. It explicitly prefers diverse directions over raw closeness, and it provably yields the RNG as a subgraph.

Why this matters is a connectivity argument, not a quality argument. On clustered data, a new point's `M` nearest neighbours can all sit inside one dense cluster. Naive selection then spends the entire edge budget inside that cluster and creates no edge leaving it. Do that for every point in the cluster and the region becomes a navigational cul-de-sac: reachable only if the search happens to enter it, and unable to route out. The heuristic deliberately keeps a further neighbour when it is not already "shadowed" by a closer selected one, which is what preserves global connectivity. The paper reports the advantage is strongest on "low-dimensional data, high-recall regimes, and highly clustered datasets" [1].

On uniform random data — the data everyone tests with first — naive selection looks fine, because uniform data has no cluster structure to fragment. This is the single most dangerous property in the whole file.

### Vamana / DiskANN: no hierarchy, α instead

Vamana builds one flat graph [5]:

1. Initialise every vertex with random out-neighbours.
2. Iterate over all points in random order; for each, greedy-search from a fixed start node for candidates, then apply **RobustPrune** to pick out-edges.
3. Do two full passes: the first with `α = 1`, the second with user-chosen `α ≥ 1`.

RobustPrune's rule: having kept edge `(p, p*)`, drop candidate `p′` unless `α · d(p*, p′) ≤ d(p, p′)` [5]. This is the same lune-shadowing test as HNSW's heuristic and as NSG's MRNG approximation — three different relaxations of the same RNG idea. With `α > 1` it keeps *more* long-range edges than strict RNG pruning, which the paper reports "reduces search path length compared to existing methods like HNSW and NSG" [5]. The hierarchy becomes unnecessary because the pruning rule manufactures the highways directly.

The reason to care is disk. DiskANN keeps **PQ-compressed vectors in RAM** for approximate distance computation during routing, while full-precision vectors and adjacency lists live on **SSD** — roughly 64 GB of in-RAM compressed vectors for billion-point datasets [5]. `BeamSearch` fetches the on-disk neighbourhoods of `W` closest candidates in parallel (typically `W` = 2–8) per round rather than one read per hop, exploiting SSD parallelism [5]. Reported: 2–3× fewer disk hops than competing disk-resident methods, **fewer than 10 round-trips per query**, at only 30–40% SSD load factor [5].

Vamana as published is a static index. Its streaming successor, FreshDiskANN, claims to index over a billion points on a single workstation while supporting "thousands of concurrent real-time inserts, deletes, and searches per second each" at >95% 5-recall@5, at a claimed 5–10× cost reduction versus periodic full rebuilds [17] — the one system here that seriously attempts to escape the rebuild dependency described in [Updates and deletes](08-updates-and-deletes.md).

**NSG** (Fu et al.) targets MRNG, a monotonic RNG variant proven to give "very low search complexity (close to logarithmic time)," and is a tractable approximation to it; it reports the smallest memory footprint among graph methods tested and is deployed at billion-node scale inside Alibaba's search engine [4]. **SPANN** is not a graph at all — centroids in memory, posting lists on disk, hierarchical balanced clustering, query-aware pruning — and it reports 2× DiskANN's speed at 90% recall on equal memory, reaching 90% recall@1 and recall@10 in about one millisecond on 32 GB [6]. Naming it here matters because it is the strongest evidence that graphs are not automatically the right answer at billion scale.

## Parameters and knobs

| Parameter | Controls | Typical value | If you get it wrong |
|---|---|---|---|
| `M` | Bidirectional links per node per layer | 5–48; paper's recommended range [1] | Too low: sparse graph, recall collapses on hard queries. Too high: memory and build time balloon for little recall |
| `Mmax0` | Layer-0 link cap | `2M` [1] | Set to `M` and layer-0 connectivity is too thin for the final beam search to be accurate |
| `mL` | Level-assignment decay | `1/ln(M)` [1] | Too large: too many layers, wasted descent. Too small: degenerates to flat NSW |
| `efConstruction` | Candidate list at build | 200 in the paper's tests [1] | Low: permanently worse graph, no query-time fix. High: build time grows roughly linearly (38m / 67m / 88m at 64 / 100 / 200 on 1M × 1536-D serial) [15] |
| `efSearch` | Candidate list at query | Swept per recall target | The one free knob — changeable after build with no reindex |
| `α` (Vamana) | Pruning relaxation | ≥ 1, second pass [5] | `α = 1` gives RNG-like pruning and longer search paths |

`efSearch` is the only one of these you can change after the fact. Everything else is baked into the graph at build time.

## How real systems do it

The genuinely interesting split is not which graph algorithm — almost everyone ships HNSW — but **when the graph is built and what it is scoped to**. Three systems made three incompatible choices.

**Lucene and Elasticsearch: one immutable HNSW graph per segment.** Lucene segments are immutable, so vectors inherit the text-search architecture wholesale. The consequence is stated bluntly by Elastic: "a query must search every segment's graph because the neighbors of a vector in each partition graph will only comprise a subset of the true nearest neighbors in the single graph" [10]. There is no way to get correct global top-`K` from one segment. Per-query cost therefore scales with **segment count**, not just corpus size — a tax most vendor material omits. Elasticsearch 8.10 parallelised the fan-out to one thread per segment, but each thread still searched blind to the others; a later fix introduced a synchronized global top-n queue plus per-segment local queues so a segment can early-terminate once its candidates are neither locally nor globally competitive, reporting up to 60% faster queries concurrent with indexing (54 ms → 32 ms) and Lucene nightly benchmark throughput nearly doubling (104 → 219 QPS) [10]. Merging compounds it on the write side: combining segments means combining graphs, and the naive approach (keep the largest, reinsert everything else) throws away the smaller graph's structure. Lucene 10.2 instead computes a "join set" of roughly 1/5 of the smaller graph's vertices via a greedy gain heuristic, inserts those normally, then reuses their established neighbour relationships to place the rest — 1.28–1.33× faster indexing and 1.34–1.72× faster force-merges [11]. Graph merging is a distinct, recently-unsolved engineering problem, not a special case of segment merging.

**Vespa: one real-time mutable HNSW, no segments.** Vespa supports CRUD on vectors "in true real time," with multithreaded graph-link updates during insert [12] — explicitly the opposite choice, taken to avoid exactly the fan-out and merge costs above. The price is concurrency control on a live graph under concurrent readers, plus in-place memory reclamation, neither of which its documentation details. Its parameters carry Vespa names rather than the paper's: `max-links-per-node` (≈ `M`), `neighbors-to-explore-at-insert` (≈ `efConstruction`), and query-time `hnsw.exploreAdditionalHits` (≈ `efSearch`) [12].

**Milvus: build once, on seal.** Growing segments are searched with a lightweight brute-force-oriented structure, because building a real index on a mutating segment is impractical; the real index only exists for sealed segments [14]. When a segment seals, a dedicated index node loads its binlog from object storage, builds the index, serialises it, and writes it **back to object storage**, so query nodes load a prebuilt file rather than rebuilding [13]. A single query therefore mixes approximate search over sealed segments with exact scan over the small growing one — a hybrid-precision design that is easy to miss.

Per-segment immutable, mutable in place, or build-on-seal: that is the real architectural question in ANN persistence, and it decides your merge strategy, your delete strategy ([Updates and deletes](08-updates-and-deletes.md)), and your recovery story ([Recovery and compaction](13-recovery-and-compaction.md)) all at once.

## Complexity and cost

The paper claims **O(log N)** search and **O(N log N)** construction, attributed to constant per-layer degree plus a logarithmic layer count [1]. Both are empirical average-case claims from the original paper, not proven worst-case bounds — see contested claims below.

Memory splits in two. Raw vectors cost `4·D` bytes each in float32. Edges cost roughly `2·M·4` bytes per node at layer 0 plus smaller upper-layer contributions, which across the recommended `M` = 5–48 range works out to roughly **48–384 bytes per object for links alone** [1]. At billion scale this dominates the conversation: AWS estimates 1B × 128-D with `m=16` and one replica at roughly **1,408 GB for HNSW** versus **1,126 GB for IVF** and **70 GB for IVFPQ** [16].

Build time is the cost people underestimate: 1M vectors at 1536-D, `m=16`, `efConstruction=200`, took **1 hour 27 minutes** single-threaded in pgvector 0.5.1, and **9.5 minutes** with parallel builds in 0.6.0 at unchanged accuracy and QPS [15]. See [Choosing an index](09-choosing-an-index.md).

## Failure modes

- **Clustered data plus naive neighbour selection.** The graph fragments; recall collapses on some clusters and looks fine on others; uniform-random test data never shows it.
- **Local minima.** Greedy search terminates at a node with no closer neighbour, which may not be near the answer. `efSearch` buys you out probabilistically, never absolutely.
- **Multi-graph fan-out.** Under a segment model, skipping the "search every segment" requirement silently returns wrong top-`K` [10].
- **Non-determinism.** A Milvus user could not obtain reproducible HNSW results after seeding Python's `random`, NumPy, PyTorch, TensorFlow and CUDA determinism flags, while the FLAT baseline reproduced fine in the same environment; filed July 2023 with no captured root-cause [18]. Treat HNSW as non-reproducible run-to-run unless proven otherwise.
- **Memory cliff.** HNSW assumes residency; when the working set spills there is no graceful degradation.

## How to test it

- **Recall against exact brute force**, at several `efSearch` values. Anything else compares one approximation to another. See [Benchmarking](22-benchmarking.md).
- **Connectivity as a property test**: BFS over the layer-0 graph from any node must reach every node. A fragmented graph fails this immediately and cheaply.
- **The clustered-data differential test.** Build twice — with the neighbour heuristic and with naive nearest-`M` — on (a) uniform random vectors and (b) tightly clustered Gaussian blobs. Expect near-identical recall on (a) and a clear gap on (b). No gap on (b) means your clusters are not tight enough, not that the heuristic is unnecessary.
- **Degree invariant**: no node exceeds `Mmax` (or `Mmax0` at layer 0) after any insert, including the neighbour-shrinking path.
- **Layer distribution**: the empirical level histogram must match the `mL` geometric decay. A wrong `mL` silently yields a graph that works, just slower.

## Minimal demo version

HNSW is roughly 400 lines and is the single most instructive index to build from scratch. Everything you need is in the paper's five algorithms, and the payoff is that the whole recall/latency/memory trade-off becomes something you can feel by changing a constant.

**Cut freely:** persistence — build in memory at startup; deletes — a separate exercise in [Updates and deletes](08-updates-and-deletes.md); concurrency — a single-threaded build is correct and removes an entire class of bug; the upper layers, if you want to test the flat-graph claim; SIMD distance kernels ([Brute force and kernels](03-brute-force-and-kernels.md)); cache-friendly memory layout.

**Cannot cut:** the SELECT-NEIGHBORS-HEURISTIC, including in the neighbour-shrinking path. A demo with naive nearest-`M` selection passes every test you write against uniform random data and collapses on clustered data — teaching the wrong lesson twice: that the heuristic is a detail, and that your benchmark meant something. Also keep the `ef=1` descent as a phase distinct from the layer-0 beam, since collapsing them hides where the hierarchy earns its keep, and a brute-force reference to measure recall against.

## Open questions and contested claims

- **The hierarchy may be unnecessary.** "Down with the Hierarchy: The 'H' in HNSW Stands for 'Hubs'" (arXiv, Dec 2024; ICML 2025) argues that a flat navigable small world graph "retains all of the benefits of HNSW on high-dimensional datasets, with latency and recall performance essentially identical," using less memory since upper layers vanish [8]. Its explanation is the **hub highway hypothesis**: naturally-occurring hub nodes in the layer-0 graph already act as the long-range shortcut structure the hierarchy was thought to supply. If it holds, it retroactively validates Vamana's flat design [5] as correct rather than merely SSD-driven. Author list and exact parity numbers are not established here — take the qualitative claim, not a figure.
- **There is no universal winner across datasets.** The VLDB graph survey found that "an algorithm's superiority is contingent on the dataset rather than being fixed in its performance": NSG is **125× faster than HNSW** at 0.99 recall on Msong and **80× slower** than HNSW at the same recall on Crawl [7]. The same survey found higher measured graph quality does not imply better search performance (HNSW and DPG performed similarly on GIST1M at 99.2% vs 63.3% graph quality), and — most damning — that when every algorithm is decomposed into a shared seven-component pipeline, published gains often trace to a different component than the original paper credited [7]. Distrust any paper's own account of *why* its method wins, including the ones cited above.
- **No worst-case guarantees exist.** No source behind this file provides a worst-case or adversarial-input recall bound for HNSW or any relative. Every complexity figure in this file — including O(log N) — is an empirical average-case claim. Papers targeting this gap exist and are recent; the honest statement today is that graph ANN indexes have no known worst-case guarantees.
- **Whether partition-based designs beat graphs at billion scale depends entirely on which partition method.** AWS found HNSW beating IVFPQ decisively on recall (0.84–0.99 vs 0.17–0.61 recall@10) and on latency at comparable recall, with IVFPQ winning only on memory (~70 GB vs ~1,408 GB) [16]; SPANN reports beating DiskANN outright on the recall/latency/memory frontier [6]. Both are true. See [Choosing an index](09-choosing-an-index.md). NGT's quantized-graph variant likewise claims higher performance than HNSW, ScaNN and Annoy in most benchmarked scenarios, with no head-to-head figure captured here [19].

## Sources

[1] Malkov, Y.A., & Yashunin, D.A. — "Efficient and Robust Approximate Nearest Neighbor Search Using Hierarchical Navigable Small World Graphs" — IEEE TPAMI — https://arxiv.org/abs/1603.09320 — 2016/2018
[2] Malkov, Y., Ponomarenko, A., Logvinov, A., & Krylov, V. — "Approximate Nearest Neighbor Algorithm Based on Navigable Small World Graphs" — Information Systems Vol. 45 — https://publications.hse.ru/pubs/share/folder/x5p6h7thif/128296059.pdf — 2014
[3] Wang, H., & Wang, Z. — "A Note on Graph-Based Nearest Neighbor Search" — https://arxiv.org/pdf/2012.11083 — 2020
[4] Fu, C., Xiang, C., Wang, C., & Cai, D. — "Fast Approximate Nearest Neighbor Search With The Navigating Spreading-out Graph" (NSG) — PVLDB — https://arxiv.org/html/1707.00143v10 — 2017/2019
[5] Subramanya, S.J., Devvrit, Kadekodi, R., Krishnaswamy, R., & Simhadri, H.V. — "DiskANN: Fast Accurate Billion-point Nearest Neighbor Search on a Single Node" — NeurIPS 2019 — https://suhasjs.github.io/files/diskann_neurips19.pdf — 2019
[6] Chen, Q., Zhao, B., Wang, H., Li, M., Liu, C., Li, Z., Yang, M., & Wang, J. — "SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search" — NeurIPS 2021 — https://arxiv.org/abs/2111.08566 — 2021
[7] Wang, M., Xu, X., Yue, Q., & Wang, Y. — "A Comprehensive Survey and Experimental Comparison of Graph-Based Approximate Nearest Neighbor Search" — PVLDB Vol. 14 — https://ar5iv.labs.arxiv.org/html/2101.12631 — 2021
[8] "Down with the Hierarchy: The 'H' in HNSW Stands for 'Hubs'" — ICML 2025 — https://arxiv.org/abs/2412.01940 — 2024
[9] Prokhorenkova, L., & Baranchuk, D. — "Graph-based nearest neighbor search" — Yandex Research — https://research.yandex.com/blog/graph-based-nearest-neighbor-search — 2022
[10] Elasticsearch Labs — "Lucene vector search: Speeding up multi-graph vector search" — https://www.elastic.co/search-labs/blog/multi-graph-vector-search — accessed 2026
[11] Elasticsearch Labs — "HNSW graphs: Speeding up merging" — https://www.elastic.co/search-labs/blog/hnsw-graphs-speed-up-merging — accessed 2026
[12] Vespa — "Approximate nearest neighbor search using HNSW index" — https://docs.vespa.ai/en/querying/approximate-nn-hnsw.html — accessed 2026
[13] Milvus — "Data Processing" — https://milvus.io/docs/data_processing.md — accessed 2026
[14] Milvus/Zilliz — "Data Insertion and Data Persistence in a Vector Database (Deep Dive 4)" — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md — accessed 2026
[15] Supabase Engineering — "pgvector 0.6.0: 30x faster with parallel index builds" — https://supabase.com/blog/pgvector-fast-builds — accessed 2026
[16] AWS Big Data Blog — "Choose the k-NN algorithm for your billion-scale use case with OpenSearch" — https://aws.amazon.com/blogs/big-data/choose-the-k-nn-algorithm-for-your-billion-scale-use-case-with-opensearch/ — accessed 2026
[17] Singh, A., Subramanya, S.J., Krishnaswamy, R., & Simhadri, H.V. — "FreshDiskANN: A Fast and Accurate Graph-Based ANN Index for Streaming Similarity Search" — https://arxiv.org/abs/2105.09613 — 2021
[18] milvus-io/milvus GitHub issue #25908 — "Milvus HNSW no reproducibility or How to set random_seed for HNSW" — https://github.com/milvus-io/milvus/issues/25908 — 2023
[19] Iwasaki, M. — "Fusion of graph-based indexing and product quantization for ANN search" (NGT) — https://medium.com/@masajiro.iwasaki/fusion-of-graph-based-indexing-and-product-quantization-for-ann-search-7d1f0336d0d0 — 2023
