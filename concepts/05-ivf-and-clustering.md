# 05. IVF and partition-based search

> Cluster the vectors once, then at query time search only the few clusters nearest the query — the first index that beat brute force on real high-dimensional embeddings, and structurally the same trade-off as choosing a shard count and a fan-out width in a distributed system.

## The problem

[Trees and hash tables both failed](04-tree-and-hash-indexes.md) at embedding dimensionality: trees because the pruning bound goes slack, LSH because it hashes without looking at the data. The surviving idea from that era is the one FLANN's hierarchical k-means branch already used — **let the data tell you how to partition it**.

An inverted file index borrows its name and structure from text retrieval. Text IR builds a posting list per term (`"database" -> [4, 19, 77]`) and a query touches only the lists for its terms. IVF does the same with a learned vocabulary of *centroids* instead of words: every vector is assigned to its nearest centroid, each centroid owns an inverted list, and a query scans only lists belonging to centroids near the query.

The gain is arithmetic. Partitioning `N` vectors into `nlist` roughly equal cells and probing `nprobe` of them scans about `N · nprobe / nlist` vectors. At `N = 1,000,000`, `nlist = 4096`, `nprobe = 16`: ~3,900 distance computations instead of 1,000,000 — 256× less work, paid for in recall.

## How it works

### The coarse quantizer: k-means

The partition comes from Lloyd's algorithm: initialise `k` centroids, assign each point to its closest, recompute each centroid as the mean of its assigned points, repeat to convergence, at `O(t · k · N · D)` for `t` iterations [1]. Two properties matter and both are unpleasant: **the optimal clustering is NP-hard** [1], so you get a local optimum; and **it is sensitive to initialisation** [1], so different seeds give different partitions and different recall. (k-means was discovered independently several times — Lloyd 1957, Forgey 1965, Friedman and Rubin 1967, MacQueen — and ranked second among the "top ten most influential data mining algorithms" at ICDM 2006 [2]. Not an exotic tool.)

**k-means++ seeding** is the cheapest quality win in the index: pick the first centre at random, then each subsequent one with probability proportional to its squared distance from the nearest already-chosen centre. Reported `O(log k)`-competitive with the optimal clustering, roughly 70% faster to converge, and reaching SSE values better by 10–1000× [3]. There is no argument for random seeding.

### Voronoi cells

The resulting partition is a **Voronoi tessellation**: cell `Cᵢ` is the region closer to centroid `xᵢ` than to any other, and the closest-neighbour projection onto centroids is the `Lᵖ`-optimal quantizer for that centroid set [4]. Cells are convex polytopes bounded by hyperplanes equidistant between centroid pairs.

```
        C1  .  .          |
      .  .  .  .    C2 .  |  .    C3
     .  ●  .   ------ ● . | .  ●  .
      .  .  .        . .  |  . . .
   ---------------------- |  ----------
              C4          |     C5
           .  ●  .        |   . ● .
                          q   <- query on a boundary: its true
                                 neighbours straddle two cells
```

A query near a boundary is close to points in two cells. Probe only its own and the neighbours across the line are gone. That is the central limitation, below.

### The three phases

**Train.** k-means over a sample learns `nlist` centroids. A real training step with a data requirement — FAISS asks for `30 · nlist` to `256 · nlist` vectors [5]; under-training gives centroids that describe the sample, not the distribution.

**Add.** Per vector: distance to all `nlist` centroids, argmin, append its ID to that list. `O(N · nlist · D)` overall — which is why `nlist` cannot be arbitrarily large.

**Search.**

```
1. dist(q, centroid) for all nlist centroids       -> O(nlist · D)
2. take the nprobe nearest centroids               -> partial sort
3. scan the inverted lists of those cells          -> O(N · nprobe/nlist · D)
4. merge into a global top-K heap
```

Step 1 is brute force over centroids; step 3 is brute force over a subset. IVF is two nested brute-force searches with a learned routing table between them — which is why it is easy to implement correctly and easy to reason about. Formally, encoded vectors are grouped into `|C₁|` inverted lists, one per coarse centroid, and the query is compared only against vectors in the lists the coarse quantizer selects, with the number of centroids probed being the multiprobe parameter [6].

### nprobe is the recall dial

`nprobe = 1` scans one cell. `nprobe = nlist` scans every cell — exact results at *worse* than brute-force cost, because you also paid for the centroid comparison and the list indirection. Everything useful is in between. The curve is steep then flat: the first few probes recover most recall, then each additional probe costs a full `N/nlist` scan for a shrinking return. FAISS's recommended tuning method is not analytic — `nprobe` is "specified at query time (useful for measuring trade-offs between speed and accuracy)" [7], i.e. sweep it and measure.

### The boundary effect: a hard recall ceiling

The most important property of IVF, and the one people miss.

> "Even with large nprobe, if a query's true nearest neighbor resides in a cluster that is not among the searched ones, IVF will not retrieve it." [8]

The same source frames this as structural rather than a tuning problem: a vector near a boundary can be genuinely closer to points in a *neighbouring* cell than to points in its own, so no amount of intra-cell search fixes a wrong coarse assignment — only probing more cells recovers it [8].

Therefore: **for any `nprobe < nlist`, IVF has a recall ceiling strictly below 100%, and that ceiling is a property of the partition, not of search effort.** No better heap or faster kernel moves it. This is qualitatively unlike a graph index, where recall approaches 1 as `efSearch` grows because the graph is connected. If you need recall@10 above ~0.99, IVF alone will fight you.

### Skewed cluster sizes

The text-IR analogy is exact, including its problem: word frequencies are Zipfian, so text inverted indexes are skewed — a few enormous posting lists, a long tail of tiny ones. The same happens on real vector data: "Word frequencies in natural language are different, leading to a skewed index. The proposed methods include different k-means implementations to form a vocabulary and product quantization technique for a better space partitioning" [9].

Plain Lloyd's minimises within-cluster squared error and has **no term for cluster size**. Nothing prevents one cell holding 5% of the dataset and another holding 40 vectors. Query cost is therefore `Σ|list|` over probed cells, not `nprobe · N/nlist`: a query routed to two fat cells does 10× the work, so p50 looks fine and p99 does not.

Balanced alternatives exist. One forces equal-size clusters by ordering points by the benefit of best-over-second-best assignment and assigning greedily to capacity, since optimal balanced assignment is itself NP-hard [10]. Balanced binary clustering produces a shallower, more balanced index tree (height 11) than balanced k-means variants (12), k-means++ (13) and FSCL (16) on the same data, with FSCL "the most unbalanced at 1th-11th levels" [11] — even k-means++ leaves real skew on the table. Cheap mitigation: histogram list sizes after training, retrain if `max/median` exceeds about 5×.

### The residual: the bridge to quantization

IVF's second act is making compression work. Instead of compressing the raw vector, compress the **residual** left after coarse assignment [6]:

```
y  ≈  q1(y)  +  q2( y − q1(y) )
     coarse        fine quantizer on the leftover
```

Within one cell every vector is already near the centroid, so residuals have far smaller dynamic range than raw vectors and a fixed-size codebook resolves them much more finely. See [Quantization](06-quantization.md) for what `q₂` is. Architecturally the coarse partition is doing double duty: it restricts the search *and* normalises the data for the compressor. That is why `IVF...,PQ...` composites dominate billion-scale index strings and bare `PQ` is rare.

### The distributed-systems isomorphism

State this plainly, because it is the most transferable idea in the file.

| IVF | Distributed search |
|---|---|
| `nlist` — number of Voronoi cells | `S` — number of shards |
| centroid assignment | partition function / shard key |
| `nprobe` — cells probed per query | fan-out width — shards queried per request |
| local top-K per cell, merged | local top-K per shard, merged by a coordinator |
| neighbour in an unprobed cell is lost [8] | a match on an unqueried shard is lost |
| skewed list sizes → p99 latency [9] | hot shards → p99 latency |
| `nprobe = nlist` → exact, slowest | fan-out to all shards → complete, slowest |

The trade is identical in both directions: **partition the space, probe a subset, pay recall for latency.** Anyone who has tuned `nprobe` against a recall target has already done the reasoning [Sharding and partitioning](14-sharding-and-partitioning.md) and [Distributed search](17-distributed-search.md) ask for, and the skew problem is literally the same problem.

The analogy breaks in one place: IVF's cells follow the *data distribution* (k-means), while shards usually follow a *hash of the ID* for uniformity — exactly the trade between locality (nearby vectors co-located, so few probes suffice) and balance (uniform load, no locality, so every query fans out).

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| `nlist` | Number of centroids / lists | `4·sqrt(N)` to `16·sqrt(N)` below 1M [5] | Too small: lists huge, degenerates toward brute force. Too large: centroid scan `O(nlist·D)` dominates, lists too thin to be worth probing, training data insufficient |
| `nprobe` | Cells scanned per query — the recall dial | sweep from 1; 8–64 typical | Too low: recall ceiling too low, silently. Too high: slower than brute force with none of its guarantees |
| Training set size | Centroid quality | `30·nlist` to `256·nlist` vectors [5] | Centroids fit the sample, not the distribution; recall drops in a way that looks like a bug |
| Seeding | k-means initialisation | k-means++ [3] | Random seeding gives run-to-run recall variance and SSE worse by 10–1000× [3] |
| Iterations `t` | Convergence quality | 10–25 | Cost linear in `t` [1]; too few leaves visibly unbalanced lists |
| Coarse quantizer type | How step 1 searches centroids | flat below ~65k centroids; HNSW above [5] | At `nlist = 10⁶` a flat centroid scan is a million distance computations per query |

The `sqrt(N)` scaling is not arbitrary. FAISS derives it as a cost balance: "a typical way to select the number of centroids is to aim at balancing the cost of the assignment to the centroids (`nlist * d` for a plain k-means)" against query cost, "which yields approximately `nlist = C * sqrt(n)`" [7]. Assignment cost grows with `nlist`, per-list scan cost shrinks with it, and the sum minimises near `sqrt(N)`.

## How real systems do it

**FAISS** is the reference implementation, and its index-selection tiering is the most useful single document in this area [5]:

| Dataset size | Recommended index string |
|---|---|
| Few searches (1k–10k) | `"Flat"` — "the index building time will not be amortized by the search time" |
| Exact results required | `"Flat"` or `"IDMap,Flat"` only |
| Below 1M | `"IVFK,..."` where "K is 4\*sqrt(N) to 16\*sqrt(N)", trained on 30·K to 256·K vectors |
| 1M–10M | `"IVF65536_HNSW32,..."`, needing 30·65536 to 256·65536 training vectors |
| 10M–100M | `"IVF262144_HNSW32,..."` |
| 100M–1B | `"IVF1048576_HNSW32,..."` |

Read `_HNSW32` carefully: past 65,536 centroids FAISS stops scanning centroids linearly and puts an **HNSW graph over the centroids** to route queries into cells. IVF does not disappear at scale — it acquires its own index. The trailing `,...` is the per-cell encoder, which memory-constrained tiers escalate `",Flat"` → `OPQM_D,...,PQMx4fsr` → RaBitQ [5]. FAISS's GPU work uses roughly `sqrt(ℓ)` centroids for a list of `ℓ` items and reports 1 billion vectors processed in under 12 hours on 4 Maxwell Titan X GPUs [12].

The FAISS SIFT1M numbers show what IVF buys and what it does not [13]:

| Index | Recall | Search time | Memory |
|---|---|---|---|
| `FlatL2` (exact) | 100% | 8.26 ms | 256 MB |
| `PQ` | 50% | 1.49 ms | 6.5 MB |
| `IVFPQ` (nprobe=48) | 52% | 0.09 ms | 9.2 MB |

IVF turned a 1.49 ms scan into 0.09 ms — **16× faster at the same recall**; 50%→52% is noise. That division of labour is the point: IVF buys speed, the encoder costs recall, and recovering the recall is a separate problem ([06](06-quantization.md)).

**pgvector** ships IVF as `ivfflat` and is instructive because it exposes operational failures the FAISS docs do not. With the commonly recommended `lists = sqrt(row_count)` and 1536-dim vectors, `CREATE INDEX USING ivfflat` runs k-means over a sample where "sampling 50,000 vectors alone can consume ~307MB", which with centroid-matrix allocations blows past the default 4MB `work_mem` and OOM-kills real deployments [14]. Separately, `ivfflat.probes` is session-scoped, so under PgBouncer's transaction pooling it silently stops applying — set on one physical connection, next query lands on another [14]. Both are worth internalising: **the training step is a memory event, and the recall dial is per-session state.**

**Versus graph indexes at billion scale**, pure IVF is squeezed from both sides. Below roughly 100M vectors, if the index fits in RAM, HNSW typically delivers the lowest latency [15][16]. Above that the contest is IVF-family-plus-compression against disk-resident graphs: DiskANN reports 1B+ vectors on a single 64GB-RAM machine at "<5 ms query latency with 95%+ recall on the SIFT-1B benchmark" and 5,000 QPS at 95%+ recall@1, sized at roughly 750GB–1TB of NVMe plus 64–128GB of RAM for a PQ cache [15] — its RAM-resident part *is* a quantized cache, and the crossover is framed as a RAM budget rather than a vector count [15][16].

IVF's role today is mostly **a coarse routing layer that makes compression work**, not a standalone index: FAISS never recommends bare IVF at scale, only IVF sandwiched between a graph above and a quantizer below.

## Complexity and cost

With `L = N / nlist` the average list length:

| Phase | Time | Memory |
|---|---|---|
| Train | `O(t · nlist · N_train · D)` [1] | `O(nlist · D)` centroids + the sample (the memory hazard [14]) |
| Add | `O(N · nlist · D)` | `O(N)` IDs + payload |
| Search | `O(nlist · D + nprobe · L · D)` | `O(K)` heap |
| Storage | raw vectors + `O(N)` list entries + `O(nlist · D)` centroids | with `,Flat`, no compression at all |

At `N = 10⁶`, `D = 768`, `nlist = 4096`, `nprobe = 16`: centroid scan `4096 × 768 ≈ 3.1M` multiply-adds, list scan `16 × 244 × 768 ≈ 3.0M` — balanced, as designed [7]. Drop `nlist` to 256 and the list scan becomes `≈ 48M`, 16× worse from one parameter. In a distributed design the inverted lists are the natural unit of placement ([14-sharding-and-partitioning.md](14-sharding-and-partitioning.md)).

## Failure modes

| Failure | What it looks like |
|---|---|
| Recall ceiling mistaken for a bug | Recall plateaus at 0.93 regardless of search-code changes, because the missing neighbours are in unprobed cells [8]. Diagnose by running `nprobe = nlist` against brute force |
| Under-trained centroids | Training on 5,000 vectors for `nlist = 4096` violates the `30·nlist` floor [5] by an order of magnitude. Recall is poor and stays poor; nothing complains |
| Skewed lists → p99 blowup | p50 fine, p99 5–10× worse, because some queries route to fat cells [9]. Invisible without a list-size histogram or per-query candidate counts |
| Training OOM | k-means over a high-dimensional sample is a large allocation — 307MB against a 4MB default in one real case [14] |
| Distribution drift | Centroids are trained once; months of new data pile into whichever cells happen to be nearest, degrading balance and recall with no error signal. IVF needs periodic retraining; graphs do not |
| `nprobe` lost in the plumbing | A session-scoped search parameter silently reverting to default under a connection pooler [14] — recall drops in production, not in staging |
| Deletes | Removing a vector from a list is easy; the centroid it trained is not updated, so heavy deletes slowly invalidate the partition. See [Updates and deletes](08-updates-and-deletes.md) |

## How to test it

- **`nprobe = nlist` must equal brute force, exactly.** The strongest invariant IVF has, cheap at small `N`, and it separates "search bug" from "partition limit" definitively. Make it a unit test.
- **Recall must be monotonic in `nprobe`.** Sweep `1, 2, 4, ..., nlist`; a violation means the cell ordering or the merge heap is wrong.
- **Partition invariants.** Every vector in exactly one list, list lengths summing to `N`, and each vector's list equal to the independently recomputed argmin over centroids. Trivial, and they catch the whole add-path bug class.
- **List-size histogram as a regression test.** Record `max/median` after training and alert when it worsens.
- **Boundary-effect property test.** Two tight clusters with a query exactly between them: assert `nprobe=1` misses neighbours from the far cluster and `nprobe=2` finds them. This makes the theoretical ceiling executable and is the best test in this file.
- **Seed stability.** Train twice with different seeds and record the recall delta; a large one means you are not using k-means++ [3] or not running enough iterations.

## Minimal demo version

IVF is the best value-per-line index in this pack — a working version is a few hundred lines and it demonstrates a partition/recall trade you reuse verbatim at the distributed layer.

1. **k-means with k-means++ seeding** [3], fixed iteration count (15 is plenty), over a sample. Plain Lloyd's [1] is correct and fast enough; skip mini-batch and elkan.
2. **Inverted lists** as arrays of vector IDs, plus the centroid matrix.
3. **Search**: full centroid scan, partial-sort to `nprobe`, scan those lists with the *same* distance kernel as your [brute-force path](03-brute-force-and-kernels.md), merge into one top-K heap. Do not write a second kernel.
4. **A recall-vs-nprobe harness** computing ground truth by brute force once, then reporting recall@10, latency and candidate count for `nprobe ∈ {1, 2, 4, 8, 16, 32, 64, nlist}`.

That harness is the deliverable, and two things must be visible in its output: the knee (most recall bought by the first few probes) and the last row (`nprobe = nlist` is exact and *slower than brute force*).

**Cut without losing the lesson:** mini-batch k-means, balanced clustering, residual encoding (that belongs to [06](06-quantization.md)), an HNSW coarse quantizer, incremental retraining, centroid persistence.

**Cannot cut:** the brute-force ground truth (recall without an exact baseline is a number with no meaning — this is the line separating a demo from a lie); the `nprobe` sweep (a single value hides the concept); the boundary-effect demonstration (without it a reader concludes IVF is "approximate but tunable to exact", which is false [8]); and the list-size histogram (one print statement that makes skew [9] real).

For one extra hour: run the harness at `nlist ∈ {64, 256, 1024, 4096}` with a fixed `nprobe/nlist` ratio and watch the centroid-scan term take over. That is the `sqrt(N)` balance [7] appearing in your own measurements.

## Open questions and contested claims

- **The `sqrt(N)` constant.** FAISS gives a 4× band, `4·sqrt(N)` to `16·sqrt(N)` [5], and separately derives `nlist = C · sqrt(n)` from cost balancing without pinning `C` [7]. pgvector's community guidance is plain `sqrt(row_count)` [14] — at or below the bottom of FAISS's band. No one claims a principled value; it depends on `D`, on distance-kernel throughput, and on whether the centroid scan is vectorised.
- **How much balanced clustering is worth.** The evidence that plain and even "balanced" k-means variants leave skew on the table is direct [11], and methods targeting given cluster sizes exist [10]. What is missing everywhere is a measured recall or p99 delta on a vector-search benchmark from switching partitioner. The mechanism is clear; the magnitude is not published in anything cited here.
- **Whether IVF has a future as a standalone index.** Benchmarking consensus is that graph methods outperform tree, LSH and quantization-based indexes generally (see [04](04-tree-and-hash-indexes.md)), and FAISS's own scale guidance wraps IVF in a graph and a quantizer rather than shipping it bare [5]. The counter-position is architectural, not recall-based: IVF's cells are the natural unit of *distribution* and *disk residency* in a way a graph's edges are not, which is why IVF-family layouts persist at billion scale [15][16].

## Sources

[1] Zhang, S., Huang, J., Xiao, R., Du, X., Gong, P., & Lin, X. — "Toward more efficient locality-sensitive hashing via constructing novel hash function cluster" — https://onlinelibrary.wiley.com/ai/10.1002/cpe.6355 — 2021
[2] Jin, X., Kim, S., Han, J., Cao, L., & Yin, Z. — "A general framework for efficient clustering of large datasets based on activity detection" — https://onlinelibrary.wiley.com/ai/10.1002/sam.10097 — 2010
[3] Belhaouari, S. B., Ahmed, S., Mansour, S., & Milovanović, G. — "Optimized K-Means Algorithm" — https://onlinelibrary.wiley.com/ai/10.1155/2014/506480 — 2014
[4] Bally, V., Pagès, G., & Printems, J. — "A Quantization Tree Method for Pricing and Hedging Multidimensional American Options" — https://onlinelibrary.wiley.com/ai/10.1111/j.0960-1627.2005.00213.x — 2005
[5] Meta AI Research — "Guidelines to choose an index" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index — accessed 2026
[6] Ly, N. Q., Do, T. K., Nguyen, B. X., & Fernández, J. C. — "Large-Scale Coarse-to-Fine Object Retrieval Ontology and Deep Local Multitask Learning" — https://onlinelibrary.wiley.com/ai/10.1155/2019/1483294 — 2019
[7] Meta AI Research — "Faiss indexes" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Faiss-indexes — accessed 2026
[8] VeloDB — "Inverted File Index (IVF) in Vector Similarity Search" — https://www.velodb.io/glossary/inverted-file-index-in-vector-similarity-search — accessed 2026
[9] Protasov, S., Khan, A. M., & Cheong, S. A. — "Using Proximity Graph Cut for Fast and Robust Instance-Based Classification in Large Datasets" — https://onlinelibrary.wiley.com/ai/10.1155/2021/2011738 — 2021
[10] Dirgová Luptáková, I., Šimon, M., Huraj, L., Pospíchal, J., & Simon, D. — "Neural Gas Clustering Adapted for Given Size of Clusters" — https://onlinelibrary.wiley.com/ai/10.1155/2016/9324793 — 2016
[11] Liang, H., Zhang, X., Cheng, H., Wei, Q., & Yan, Z. — "Secure and Efficient Image Retrieval over Encrypted Cloud Data" — https://onlinelibrary.wiley.com/ai/10.1155/2018/7915393 — 2018
[12] Johnson, J., Douze, M., & Jégou, H. — "Billion-scale similarity search with GPUs" — https://ar5iv.labs.arxiv.org/html/1702.08734 — 2017
[13] Briggs, J. — "Product Quantization: Compressing high-dimensional vectors by 97%" — Pinecone Learn — https://www.pinecone.io/learn/series/faiss/product-quantization/ — accessed 2026
[14] Perun Engineering — "pgvector in production: five failure patterns" — https://perun.au/insights/pgvector-production/ — accessed 2026
[15] Couchbase — "What Is DiskANN? Billion-Scale Vector Search Explained" (summarising Subramanya et al., NeurIPS 2019) — https://www.couchbase.com/blog/diskann/ — accessed 2026
[16] Vectroid — "HNSW vs DiskANN: comparing the leading ANN algorithms" — https://www.vectroid.com/resources/HNSW-vs-DiskANN-comparing-the-leading-ANN-algorithm — accessed 2026
