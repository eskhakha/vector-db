# 04. Tree and hash indexes, and why they lost

> Space-partitioning trees and locality-sensitive hashing were the two dominant answers to nearest-neighbour search for twenty-five years; both lost to graph indexes on real embedding data, and understanding exactly how they fail is the fastest route to understanding what a modern vector index is solving.

## The problem

[Brute force](03-brute-force-and-kernels.md) computes `N` distances per query. The obvious fix is the one that works everywhere else in databases: build a tree, descend in `O(log N)`, touch a fraction of the data. Every classical spatial index is a version of that bet.

The bet pays off at `D = 3`. It does not pay off at `D = 768`, and not by a constant factor — it collapses structurally. Everything in files [05](05-ivf-and-clustering.md) through [07](07-graph-indexes-hnsw.md) exists because of that collapse.

## How it works

### kd-trees

Bentley's kd-tree partitions recursively: each node owns a subset, the root owns everything, terminal buckets hold small mutually exclusive subsets [1][2]. Each internal node picks one **discriminator** dimension and a split value. The classic rule cycles dimensions by depth — for level `L` (root at 0) and `D` dimensions, the discriminating key index is `L mod D + 1` [1].

Construction is `O(D·N·log₂N)` naively; presorting each dimension once and reusing that order at every split drops it to `O(N·log₂N)`, independent of `D`, and removes insertion-order sensitivity [3].

Search is **branch-and-bound** over a priority queue of the best `K` candidates so far [1][2]:

```
descend to the leaf on the query's side of each hyperplane, collect candidates
then unwind; at each node on the way back up:

  d_plane = |q[discriminator] - split_value|   # query-to-hyperplane distance
  r       = distance to current K-th best

  if d_plane >= r:  skip the far subtree          <-- the entire point of the tree
  else:             backtrack into it too
```

If the ball of radius `r` around the query does not cross the plane, nothing on the far side can win. When that fires often you touch `O(log N)` nodes; when it never fires you touch all `N` and paid for a tree.

### Ball trees and bounding volumes

Swap axis-aligned splits for bounding **spheres**: leaves are points, internal nodes are the sphere enclosing their children, built bottom-up by greedily picking centres, assigning to the nearest, and bounding each group [4]. Pruning becomes `d(q, centre) − radius >= r`. The greedy centre-selection step is quadratic, so it scales to about `10⁵` elements per call and larger inputs must be pre-chunked [4].

R-trees generalise this to disk pages: non-leaf nodes hold `(pointer, MBR)` pairs, fanout between `m = M/2` and `M`, all leaves at equal depth [5][6]. Their weakness is **MBR overlap** — insertion optimises locally, internal rectangles accumulate overlap, and one query descends several branches [5][6]. The SR-tree names the high-dimensional failure of both shapes at once: rectangles degrade because "the difference between the length of the edge of the rectangle and the diagonal becomes too large", spheres because their volume grows much faster, so it stores both bounds and prunes on the intersection [7]. It beat SS-tree and R*-tree — and still lost to linear scan past a point.

### Randomised and rotated forests, Annoy, FLANN

The standard rescue (Silpa-Anan and Hartley, the basis of FLANN's randomised forest) combines three tricks [8]: build `m` structurally different trees so their search paths are largely independent; cap total nodes visited at `n`, split as ~`n/m` per tree against one shared queue; **rotate by PCA first** so hyperplanes are perpendicular to directions of maximum variance rather than raw axes. Each helps, and they help more together [8].

**Annoy** productionised this with random projection splits instead of axis-aligned ones, plus many trees, explicitly to fight the curse of dimensionality — with memory as the acknowledged cost, since "each tree consumes memory proportional to dataset size" [10]. Deployments report the parameterisation directly: `n_trees=50`, Manhattan distance, `search_k` at default, results re-ranked afterwards by a second metric [11][12]. Its other decision is operational: the built index is an immutable **mmap'd** file, so many processes share one page-cached copy, startup is a mmap rather than a load, and an index larger than RAM degrades through the page cache instead of failing. `n_trees` is the build-time recall/memory dial; `search_k` the query-time recall/latency dial.

**FLANN** (Muja and Lowe) added automatic algorithm selection between randomised kd-trees and hierarchical k-means trees, because "while kD trees and hierarchical k-means trees on average yield similar results, their relative performance depends on the data and type of search query" — building sample structures with various parameters on a data subset and adopting the winner's parameters, paying construction time for the choice [13]. Both support best-bin-first search with a hard cap on points evaluated, plus a `(1+ε)` variant pruning any cell farther than `dist(q,p)/(1+ε)` for `p` the current K-th best [13].

### The collapse

Four framings of one fact.

| Framing | Evidence |
|---|---|
| **Threshold** | "In the exact matching case, linear search is the best method for data with more than 20 dimensions... Systems based on kD trees and k-means trees support guaranteed exact search, but at best outperform linear search by only a fraction" [13]. On real non-uniform image data the crossover is measured at roughly **12 dimensions** [14] |
| **Scaling shape** | Linear scan grows *linearly* in `D`; R*-tree and SR-tree search time grows *exponentially* [14]. A purpose-built non-tree method in the same study eroded steadily: 81% faster than scan at `D=3`, 77% at `D=12`, 70% at `D=48` [14]. kd-tree cost on 2-D/3-D grids already scales as `O(N^p)`, `p` between 2 and 3, exponent growing in `D` [15] |
| **Mechanism** | "In high dimensional space, the kd-tree will visit nearly all of the elements in the database. As a consequence, its performance is not even better than brute-force search when considering the cost of indexing" [16] — `d_plane >= r` stops firing because nearly every cell intersects the query ball |
| **Why the bound goes slack** | Per-dimension tightness does not survive aggregation: the probability a point lies within 3 standard deviations in *every* dimension is 97.33% at `D=10`, 76.31% at `D=100`, 6.696% at `D=1000` [17]. A 10-bin-per-axis grid has `10²` cells at `D=2` and `10¹⁰⁰` at `D=100`, and "in order for these cells not to be already empty on average, we need at least as many objects as cells" [17]. A root-to-leaf path over `10⁹` points uses only `log₂(10⁹) ≈ 30` splits, so on 768-dim embeddings "the search procedure will not account [for] the majority of dimensions" [10] |

Weber, Schek and Blott's VA-file was the canonical response — accept the scan, but scan a compressed approximation first. Downstream benchmarking reports its own failure: "the VA-File is not fit to search for nonuniformly and practical data sets since data skew weakens the effect of filtering by approximation file", while SR-tree handles skew better but suffers as "large volume of entries in nonleaf nodes causes higher search cost" with dimension [18]. Both degrade, by different mechanisms. The VA-file's idea — scan everything, in compressed form — came back and won, as [quantization](06-quantization.md).

### Locality-sensitive hashing

LSH abandons partitioning for randomised collision. A family `H` over a space with distance `d` is **`(R, cR, P₁, P₂)`-sensitive** (`c > 1`, `P₁ > P₂`) if for any `p, q` [19][20][21]:

```
d(p,q) <= R    =>  Pr[h(p) = h(q)] >= P1
d(p,q) >= cR   =>  Pr[h(p) = h(q)] <= P2
```

The whole theory is that gap, amplified.

**Random-hyperplane LSH (Charikar, 2002)**, for cosine: draw `w` from a standard Gaussian, `h(x) = sign(wᵀx)` [20]. The collision probability is exactly the angle:

```
Pr[h(x) = h(y)] = 1 - θ(x,y)/π      θ = arccos( xᵀy / (||x||·||y||) )
```

[20]. No parameter tuning beyond the projections, and one matrix multiply [22].

**p-stable LSH (Datar, Immorlica, Indyk, Mirrokni, 2004)**, for L2: `D` is `p`-stable if for i.i.d. `X₁..Xₙ ~ D` and any `v`, `Σvᵢ Xᵢ` has the distribution of `(Σ|vᵢ|^p)^{1/p}·X` [23][24]. Gaussian is 2-stable (L2), Cauchy 1-stable (L1). The hash projects and quantises into buckets of width `w` with random offset `b ~ Uniform[0, w]`:

```
h_{a,b}(v) = floor( (a·v + b) / w )
```

[23][24].

**AND/OR amplification.** Concatenate `k` functions into a compound key `g(x) = (h₁(x),...,h_k(x))` (AND), then build `L` independent tables (OR):

```
per-table collision:  p^k          overall recall:  1 - (1 - p^k)^L
```

At `k = 10`: a similar pair at `p = 0.7` collides in one table with probability `0.7¹⁰ ≈ 0.3`; a dissimilar pair at `p = 0.2` with `0.2¹⁰ ≈ 10⁻⁸`. AND widens the gap and crushes absolute recall; OR restores it. At `k = 10, L = 100` the similar pair collides with probability **0.94**, the dissimilar pair about **10⁻⁵** [20] — at a cost of 100 hash tables.

Indyk and Motwani proved sublinear query time for this scheme [22]; the standard statement of the bound is `O(n^ρ)` candidate lookups with `ρ = log P₁ / log P₂ < 1`, and `ρ` is what every later LSH paper tried to push down. **Multi-probe LSH** attacks `L` directly by probing *adjacent* buckets instead of adding tables [25], justified provably rather than heuristically: under a p-stable family, points at smaller true distance land in buckets *closer* to the query's bucket, not merely equal to it [24].

### Why LSH lost

1. **Memory blow-up.** `L` tables store `O(L·N)` entries — at `L = 100` the ID set exists a hundred times. Tree forests share the structure ("each tree consumes memory proportional to dataset size" [10]), and the theoretically better variants are worse: LSB-forest and Beyond-LSH are flagged for "high memory costs" despite improved worst-case guarantees [25].
2. **Constants.** `O(n^ρ)` is asymptotic; the `(k, L)` settings that push `ρ` low enough to matter cost more in hashing and memory than the scan they replace.
3. **Data-obliviousness — the deep one.** `a` is drawn from a Gaussian and `b` uniformly, with no reference to the dataset [20][23][24]. The hash is identical whether your embeddings occupy a thin manifold or fill the cube. IVF's coarse quantizer is *trained* by k-means [26][27] and a graph builds edges from the actual point set; both are data-aware in a way LSH structurally is not.

The verdict reads as settled background in the graph literature: "ANNBenchmark and many experiments show that graph-based methods outperform other types of ANNS methods", naming tree-based, LSH-based and quantization-based indexes as the losing categories [28]. A robotics system chooses HNSW because it "outperforms other contemporary approaches, such as tree-based BoW (Muja & Lowe, 2014), product quantization (Jégou et al., 2011), and locality sensitive hashing (Andoni & Razenshteyn, 2015)" [29]. A clone-detection system reports the same, deciding on HNSW's `O(log N)` search and `O(N log N)` build, and finding "hnswlib's implementation was more performant than Faiss under identical parameters" [30].

With numbers, on ResNet50 embeddings [31]:

| Method | Precision@10 | Recall@10 | Latency |
|---|---|---|---|
| Brute-force KNN | 88.7% | 90.1% | 142.7 ms |
| Annoy | 90.2% | 91.5% | 15.6 ms |
| HNSW | 93.8% | 94.5% | 8.2 ms |

Annoy beats brute force 9× on latency and loses to HNSW on *every* axis at once. That is the shape of the whole story.

### What these are still good for

| Use | Why these still win |
|---|---|
| Low intrinsic dimension | The curse is about intrinsic dimension, not array length. Geospatial, 3-D point clouds, small tabular features: kd-trees and ball trees are exact and fast |
| Provable guarantees | LSH gives a `(R, cR, P₁, P₂)` statement about failure probability. HNSW gives a recall number on your dataset and nothing else |
| Streaming and sketching | SimHash bits are cheap, mergeable and order-independent — near-duplicate detection at crawl scale, not top-K |
| As components | Random-hyperplane bits reappear as [binary quantization](06-quantization.md); rotate-before-splitting reappears as OPQ. The primitives survived; the indexes did not |
| Versus plain trees | An LSH+R-tree hybrid reports "better effectiveness and efficiency than R-Tree on high-dimensional datasets" for approximate KNN on uncertain data [25]. The loss was specifically to graphs |

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| kd-tree `leaf_size` | Points per bucket before scanning | 16–64 | Too small: pointer-chasing, no cache locality. Too large: brute force with overhead |
| Split rule | Median vs midpoint vs max-variance dim | Median on max-variance dim | Cycling blindly [1] splits near-constant dimensions and wastes levels |
| `n_trees` (Annoy) | Independent structures; build time, memory | 50 [11] | Memory is `O(n_trees · N)` [10]; too few caps recall regardless of `search_k` |
| `search_k` (Annoy) | Nodes inspected per query across trees | default, or `n_trees · K` | The recall dial; too low returns wrong neighbours silently |
| LSH `k` | Selectivity of one bucket | 10–20 [20] | Too high: `p^k` collapses. Too low: buckets become scans |
| LSH `L` | Recall via OR-amplification | 10–100 [20] | Memory and build linear in `L` — why LSH is memory-hostile |
| p-stable `w` | Bucket width on the projection [23] | data-scale dependent | Too small: neighbours split across buckets. Too large: everything collides |
| Multi-probe count | Buckets probed per table | 5–20 | Buys recall at fixed `L` [25]; the only reason plain LSH memory is tolerable |

## How real systems do it

**Essentially nobody ships a kd-tree or an LSH table as a primary vector index today.** That is the lesson, and the older literature does not read that way, so state it flatly.

- **FAISS** never recommended either. Its guidance routes to `Flat` for exact results or few queries ("the index building time will not be amortized by the search time"), to `IVFK` with `K` between `4·sqrt(N)` and `16·sqrt(N)` below 1M vectors, and to `IVF..._HNSW32` composites above [32]. `IndexLSH` exists in the codebase and is not in the guidance path. The one tree-shaped structure surviving at scale is an HNSW graph used as a *coarse quantizer* routing into IVF cells — routing, not answering.
- **Annoy** is the one tree-family index with continuing production use, and it survives on operational properties rather than recall: immutable mmap'd file, shared across processes, near-zero startup, graceful page-cache degradation past RAM. It is static — adding data means rebuilding — which suits a recommendation workload with nightly rebuilds and many serving processes per box. Where recall dominates, HNSW leads on speed *and* every quality metric [31].
- **scikit-learn** ships `KDTree` and `BallTree`, documents them for low dimensions, and has `algorithm='auto'` fall back to brute force as `D` grows — correct behaviour, and an implicit admission of [13][14].
- **FLANN** is the high-water mark of the tree approach: it automated the choice between randomised kd-trees and hierarchical k-means, and conceded in the same paper that past 20 dimensions exact tree search loses to linear scan [13]. The hierarchical k-means branch is the half that survived, as [IVF](05-ivf-and-clustering.md).
- **LSH persists** in deduplication and sketching (SimHash for near-duplicate pages, MinHash for Jaccard similarity) and where worst-case guarantees are required — not as a top-K index in any major vector database's default path.

The arc is the lesson: exact partitioning failed on geometry, data-oblivious hashing failed on memory and constants, and what won was approximate *data-dependent* structure — clustering the data ([05](05-ivf-and-clustering.md)) or connecting it ([07](07-graph-indexes-hnsw.md)).

## Complexity and cost

| | Build | Memory | Query (low `D`) | Query (high `D`) |
|---|---|---|---|---|
| kd-tree | `O(D·N·log N)`, or `O(N log N)` presorted [3] | `O(N)` + nodes | `O(log N)` nodes | → `O(N)`, worse than brute force once index cost counts [16] |
| Ball tree | `O(N log N)` chunked; greedy centre step quadratic [4] | `O(N)` | `O(log N)` | same collapse [16] |
| Forest / Annoy | `n_trees × O(N log N)` | `O(n_trees · N)` [10] | — | capped by `search_k`; recall degrades with `D` |
| LSH | `O(L·k·N·D)` hashing | `O(L·N)` [10][25] | — | `O(n^ρ)` candidates + exact distance on each |

LSH query cost has three terms people forget: `L·k` projections at `O(D)` each, `L` table lookups, and a full-precision distance on every candidate. The third usually dominates.

## Failure modes

| Failure | What it looks like |
|---|---|
| Silent recall collapse | A kd-tree at `D=128` returns an answer, no error, usually wrong. Nothing signals that pruning stopped working |
| Pruning that never fires | `visited_nodes → N` while still paying traversal overhead — strictly worse than the scan it replaced [16] |
| Splits on near-constant dimensions | Depth-cycled discriminators [1] happily split a dimension with no variance |
| Order-sensitive, unbalanced builds | Presorted construction fixes cost and order-sensitivity [3]; incremental insertion does not |
| Rebuild-only updates | Annoy and most forests are immutable; any insert is a rebuild — see [Updates and deletes](08-updates-and-deletes.md) |
| LSH bucket skew | Real data clusters, buckets do not come out uniform, and one fat bucket turns a lookup into a partial scan |
| `w` on the wrong scale | Bucket width lives in the units of `a·v`; change the model's norm distribution and every tuned `w` is wrong |
| Forest memory surprise | `n_trees=50` over 1M×768 vectors can outweigh the vectors themselves [10] |

## How to test it

- **Differential test against brute force.** For an exact tree, top-K must equal the linear scan's top-K exactly, including ties. Property-test over random `N`, `D`, `K`, and duplicate-heavy inputs.
- **Assert the pruning rate.** Count `visited_nodes` and `pruned_branches` per query. A refactor that breaks the bound still returns correct answers via backtracking, just slowly — only the counter catches it.
- **Bound boundary cases.** Queries exactly on a splitting hyperplane, duplicate points, all-identical vectors, `K > N`. The `>=` versus `>` in the pruning test is an off-by-one that only appears on ties.
- **LSH: verify the collision curve.** Hash pairs at known angles a few thousand times, compare the empirical rate to `1 − θ/π` [20]. Subtly non-Gaussian projections show up here and nowhere else. Then measure recall over a `(k, L)` grid against `1 − (1 − p^k)^L` [20] — systematic shortfall means bucket skew, not a formula bug.
- **Sweep `D` in CI.** Assert a pruning-rate floor at `D=4` and that the benchmark still terminates at `D=64`.

## Minimal demo version

Build a kd-tree — not to use it, but to **measure it failing**. About 200 lines, and the resulting curve is the empirical motivation for every other index in this pack.

1. Median-split construction over a flat `float32` array with a leaf-size cutoff (a partition-based selection is fine; presorting [3] is optional at demo scale).
2. Branch-and-bound top-K with the hyperplane test above, backed by a size-`K` max-heap.
3. **Two counters incremented on every query: `visited_nodes` and `pruned_branches`.** These are the deliverable; everything else is scaffolding.
4. A harness generating `N = 10,000` random vectors, sweeping `D ∈ {4, 8, 12, 16, 24, 32}`, running 1,000 queries at each, printing visited/N, pruned fraction, tree time and scan time side by side.

The shape you want: `visited/N → 1`, `pruned_frac → 0`, tree time crossing scan time in the low tens of dimensions — consistent with the 12-to-20-dimension crossovers in the literature [13][14].

**Cut without losing the lesson:** rebalancing, deletion, persistence, the `(1+ε)` variant, ball trees (same collapse, more code), the forest (you already have the curve), LSH as an index. For the LSH lesson cheaply, implement `sign(wᵀx)` in ~20 lines and verify the `1 − θ/π` collision curve empirically [20]; that plot teaches what LSH is, and you can then honestly skip the `L`-table machinery.

**Cannot cut:** the counters, the `D` sweep, and the brute-force baseline running alongside. A kd-tree without instrumentation is a slower brute force with extra code; a demo that shows it "working" at `D=8` and stops teaches the opposite of the truth; and you cannot show a crossover with one curve.

## Open questions and contested claims

- **Do randomised forests help, or just explore more?** Silpa-Anan and Hartley report rotation plus multiple trees improving search and improving further combined [8]. A later study reports the opposite on its data: the robustness gain "can be simply achieved by a standard kd-tree using a larger number of leaf nodes for exploration", with plain kd-tree beating the forest [9]. Both single-corpus. The honest reading: a forest buys *independence of failure modes*, which matters only when one tree's splits are pathological for your data.
- **Where the crossover sits.** 20 dimensions per FLANN's authors [13]; about 12 on real non-uniform image data [14]. They disagree by nearly 2× and both are right — the threshold tracks intrinsic dimension, not array length. Measure it; do not quote a constant.
- **What breaks the VA-file.** Downstream benchmarking attributes its degradation to data skew weakening the approximation filter, and SR-tree's to nonleaf-node entry volume — different mechanisms, with the proposed alternative validated only to 64 dimensions [18]. What happens to any of these families at 768 is unanswered in these sources.

## Sources

[1] Nayak, A. K., & Mohanty, K. B. — "Capacity value estimation of random solar power through associative searching in a k-d tree" — https://onlinelibrary.wiley.com/ai/10.1002/2050-7038.12037 — 2019
[2] Nayak, A. K., & Mohanty, K. B. — "Capacity value of wind power using kd tree and nearest neighbor search algorithm" — https://onlinelibrary.wiley.com/ai/10.1002/jnm.2469 — 2018
[3] Cao, Y., et al. — "A New Method to Construct the KD Tree Based on Presorted Results" — https://onlinelibrary.wiley.com/ai/10.1155/2020/8883945 — 2020
[4] Olechnovič, K., & Venclovas, Č. — "Voronota: A fast and reliable tool for computing the vertices of the Voronoi diagram of atomic balls" — https://onlinelibrary.wiley.com/ai/10.1002/jcc.23538 — 2014
[5] Zhang, C., & Plewczynski, D. — "Research on Literature Clustering Algorithm for Massive Scientific and Technical Literature Query Service" — https://onlinelibrary.wiley.com/ai/10.1155/2022/3392489 — 2022
[6] Kakkar, D., Lewis, B., & Guan, W. — "Interactive analysis of big geospatial data with high-performance computing" — https://onlinelibrary.wiley.com/ai/10.1111/tgis.12955 — 2022
[7] Katayama, N., & Satoh, S. — "SR-tree: An index structure for nearest-neighbor searching of high-dimensional point data" — https://onlinelibrary.wiley.com/ai/10.1002/(SICI)1520-684X(19980615)29:6%3C59::AID-SCJ6%3E3.0.CO;2-K — 1999
[8] Vijendra, S., Laxman, S., & Abiyev, R. H. — "Symmetry Based Automatic Evolution of Clusters: A New Approach to Data Clustering" — https://onlinelibrary.wiley.com/ai/10.1155/2015/796276 — 2015
[9] Jin, X., Kim, S., Han, J., Cao, L., & Yin, Z. — "A general framework for efficient clustering of large datasets based on activity detection" — https://onlinelibrary.wiley.com/ai/10.1002/sam.10097 — 2010
[10] Protasov, S., Khan, A. M., & Cheong, S. A. — "Using Proximity Graph Cut for Fast and Robust Instance-Based Classification in Large Datasets" — https://onlinelibrary.wiley.com/ai/10.1155/2021/2011738 — 2021
[11] Capecchi, A., Awale, M., Probst, D., & Reymond, J. — "PubChem and ChEMBL beyond Lipinski" — https://onlinelibrary.wiley.com/ai/10.1002/minf.201900016 — 2019
[12] Awale, M., Sirockin, F., Stiefl, N., & Reymond, J. — "Medicinal Chemistry Aware Database GDBMedChem" — https://onlinelibrary.wiley.com/ai/10.1002/minf.201900031 — 2019
[13] Lakemond, R., Fookes, C., Sridharan, S., Ghita, O., & Mattoccia, S. — "Fast Exact Nearest Neighbour Matching in High Dimensions Using d-D Sort" — https://onlinelibrary.wiley.com/ai/10.1155/2013/405680 — 2013
[14] Al Aghbari, Z., & Al-Hamadi, A. — "Efficient KNN search by linear projection of image clusters" — https://onlinelibrary.wiley.com/ai/10.1002/int.20496 — 2011
[15] Yuan, J., & Chen, W. — "A dose distribution evaluation technique using the k-d tree for nearest neighbor searching" — https://onlinelibrary.wiley.com/ai/10.1118/1.3480964 — 2010
[16] Zhang, H., Dong, Y., & Xu, D. — "Accelerating exact nearest neighbor search in high dimensional Euclidean space via block vectors" — https://onlinelibrary.wiley.com/ai/10.1002/int.22692 — 2021
[17] Zimek, A., Schubert, E., & Kriegel, H. — "A survey on unsupervised outlier detection in high-dimensional numerical data" — https://onlinelibrary.wiley.com/ai/10.1002/sam.11161 — 2012
[18] Sakurai, Y., Yoshikawa, M., Uemura, S., & Kojima, H. — "An indexing technique using relative approximation for high-dimensional data" — https://onlinelibrary.wiley.com/ai/10.1002/scj.1218 — 2003
[19] Min, J., Ng, R. T., Shim, K., & Wandowski, T. — "Efficient Aggregation Processing in the Presence of Duplicately Detected Objects in WSNs" — https://onlinelibrary.wiley.com/ai/10.1155/2019/1496208 — 2019
[20] Wang, L., Liu, K., Li, S., & Tang, H. — "A Fast and Memory-Efficient Spectral Library Search Algorithm Using Locality-Sensitive Hashing" — https://onlinelibrary.wiley.com/ai/10.1002/pmic.202000002 — 2020
[21] Chen, Z., & Liu, X. C. — "Response to discussion on 'Roadway asset inspection sampling using high-dimensional clustering and locality-sensitivity hashing'" — https://onlinelibrary.wiley.com/ai/10.1111/mice.12443 — 2019
[22] Zhang, S., Huang, J., Xiao, R., Du, X., Gong, P., & Lin, X. — "Toward more efficient locality-sensitive hashing via constructing novel hash function cluster" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.6355 — 2021
[23] Gao, F., et al. — "A Verifiable and Efficient Multi-Keyword Fuzzy Rank Search Scheme Over Encrypted Data With Privacy-Preserving" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.70149 — 2025
[24] Zhou, W., Han, J., Zhang, Z., Dai, J., & Xu, Z. — "HDKV: supporting efficient high-dimensional similarity search in key-value stores" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.2863 — 2012
[25] M, V., Kesarkar, A. P., & Lopez, D. — "High-performance implementation of a two-bit geohash coding technique for nearest neighbor search" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.6029 — 2020
[26] Ly, N. Q., Do, T. K., Nguyen, B. X., & Fernández, J. C. — "Large-Scale Coarse-to-Fine Object Retrieval Ontology and Deep Local Multitask Learning" — https://onlinelibrary.wiley.com/ai/10.1155/2019/1483294 — 2019
[27] Ai, L., et al. — "Accumulative Quantization for Approximate Nearest Neighbor Search" — https://onlinelibrary.wiley.com/ai/10.1155/2022/4364252 — 2022
[28] Xu, X., Li, C., Wang, Y., & Xia, Y. — "Multi-attribute approximate nearest neighbor search based on navigable small world graph" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.5970 — 2020
[29] An, S., Zhu, H., Wei, D., Tsintotas, K. A., & Gasteratos, A. — "Fast and incremental loop closure detection with deep features and proximity graphs" — https://onlinelibrary.wiley.com/ai/10.1002/rob.22060 — 2022
[30] Ahmed, G. A., et al. — "Nearest-neighbor, BERT-based, scalable clone detection" — https://onlinelibrary.wiley.com/ai/10.1002/spe.3355 — 2024
[31] Chakravarthi, G. K., Kumar, R., Addagarla, S. K., Sachan, S., & Dutta, D. — "VisCom-Res: An Efficient Visual Complementary E-Commerce Product Recommendations" — https://onlinelibrary.wiley.com/ai/10.1002/itl2.70212 — 2026
[32] Meta AI Research — "Guidelines to choose an index" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index — accessed 2026
