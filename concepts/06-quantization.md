# 06. Quantization and compression

> Vector search is memory-bound before it is compute-bound, so the central lever is storing each vector in fewer bits — and every scheme strikes the same bargain: shrink the vectors, lose distance fidelity, buy the recall back by rescoring a small candidate set against full-precision data.

**Notation:** in this file `M` means **the number of PQ subvectors**. Elsewhere in this pack `M` means HNSW's max neighbours per node per layer. Unrelated.

## The problem

Do the arithmetic first. Raw float32 storage is `N × D × 4` bytes [1]:

| Dataset | Raw float32 |
|---|---|
| 1M × 768 | 3,072,000,000 B ≈ **3.07 GB** |
| 100M × 768 | **307 GB** [2] |
| 1B × 128 | 512,000,000,000 B = **512 GB** |
| 100M × 1,536 | **≈ 614 GB** [1] |
| 1B × 384 | **1,830.4 GB** [3] |

Those are the *vectors alone*. A 100M × 1,536 deployment adds roughly 20 GB of HNSW graph overhead (at HNSW's neighbour parameter 16 — not this file's `M`) for about **634 GB resident**, quoted at roughly **$6,000/month** on managed cloud infrastructure before replicas or query headroom [1].

Why memory and not FLOPs: graph traversal is pointer-chasing, so access is random. "HNSW does not 'use a bit more RAM than IVF.' It assumes the index is resident" [1]; when the working set spills, "latency jumps from milliseconds to seconds" with "no graceful degradation" [1]. There is no partial-credit regime — compression decides whether your index runs on one machine or twenty.

## How it works

### The compression ladder

| Level | Bits/dim | Ratio | Mechanism |
|---|---|---|---|
| float32 | 32 | 1× | baseline |
| float16 / bfloat16 | 16 | 2× [3] | narrower float, no calibration |
| int8 scalar | 8 | 4× [3][4] | per-dimension min/max affine map |
| Product quantization | `M·nbits / D` | 8–64× [5][6] | learned codebooks per subspace |
| Binary | 1 | 32× theoretical [1][2][13] | sign bit, Hamming distance |

**float16 / bfloat16** is the free rung: "bfloat16 vectors show zero quality loss compared to FP32" — 2× reduction with no measured trade-off, recommended universally as a free optimisation [2]; OpenSearch measures FP16 at 2× with recall@100 ≥ 0.95 at 1B × 384 [3]. If you store float32 and have not tried bf16, do that before reading further.

### int8 scalar quantization

Per-dimension affine calibration: take each dimension's min and max across the entire database [4], then

```
start[d] = min over database of x[d]
step[d]  = (max[d] - min[d]) / 255
code[d]  = round( (x[d] - start[d]) / step[d] )    -> uint8, 0..255
recon[d] = start[d] + code[d] * step[d]
```

4× reduction, 32 bits → 8 [3][4]. Two dozen lines, one calibration pass.

Its weakness is stated bluntly by its own vendor documentation: "A major disadvantage of scalar quantization is that it does not take into consideration the *distribution* of values in each dimension" [4]. A dimension clustered near 0.3 with one outlier at 9.0 gets a step sized for the outlier and wastes almost all 256 levels. That observation is the entire motivation for PQ.

Tolerance is **model-dependent**, which is the part people skip: ModernBERT-based models retained 94–98% of float32 quality under int8, while E5-base-v2 dropped to 92% and E5-small-v2 to 87% [2]. There is no universal int8 verdict, only your model's number.

### Product quantization

PQ is k-means applied to *slices*. Split each vector into **`M` subvectors** of `D/M` dimensions (`D` must be divisible by `M`; FAISS enforces it [10]), train an independent k-means codebook of `k* = 2^nbits` centroids per subspace, and encode the vector as its `M` centroid indices.

```
D = 128, M = 8  ->  8 subvectors of 16 dims
per subspace:   k-means -> 256 centroids (nbits = 8)
code = [c1 ... c8], each 0..255            ->  8 bytes
```

Code size is `M × nbits` bits [6]. The canonical setting is `M = 8`, `k* = 256` → **64 bits = 8 bytes per vector** [6][8]; FAISS still centres PQ on byte-aligned codebooks ("The number of bits `n_bits` must be equal to 8, 12 or 16", "the dimension `d` should be a multiple of `m`" [10]), and the FAISS GPU work confirms the same defaults — typically 8 sub-vectors, each quantized to 256 values [9]. A 128-dim float32 vector is 4,096 bits; PQ at `M=8, nbits=8` is 64, a 64× reduction [5]. The scale number that made PQ famous: **100 million SIFT descriptors need ≈ 51 GB raw; PQ codes at `M=8` need ≈ 800 MB**, roughly 64×, credited with making search over 2 billion vectors on a single machine possible [6][8].

The term nobody mentions is codebook memory, `M × 2^nbits × (D/M)` floats: 128 KB at `M=8, nbits=8, D=128`, but 32 MB at `nbits=16`, which also needs enough data to populate 65,536 centroids per subspace. That is why 8 bits is the default.

### SDC versus ADC

**SDC** quantizes the query too and approximates the distance between the two sets of centroids [6] — both sides approximate, tables query-independent. **ADC** quantizes only the database vector, keeping the query at full precision [6].

The bounds as reported in the paper-review source: SDC applies the triangle inequality twice because both sides are quantized, giving `MSDE_SDC(q) ≤ 2·MSE(q)`, whereas ADC has no query-side error, giving `MSDE_ADC(q) ≤ MSE(q)` — half [6]. (A second summary reports these swapped; see Open questions.) ADC is what every system ships.

**The ADC lookup table, mechanically** — the idea worth the whole file:

```
per query, ONCE:
  build table T of shape M x 256
  T[m][c] = squared distance from the query's m-th subvector
            to centroid c of subspace m
  cost: M * 256 * (D/M) = 256 * D multiply-adds

per candidate with code [c1..cM]:
  dist = T[0][c1] + T[1][c2] + ... + T[M-1][cM]
  cost: M lookups, M-1 adds. No multiplies. No D-dimensional work.
```

At `D = 128, M = 8`: an exact float32 L2² distance is 128 multiply-adds; an ADC estimate is **8 lookups and 7 adds** into an L1-resident 8 × 256 table. PQ's speed is not only that the data is smaller — the distance function itself became cheaper — and the per-query setup (`256 · D`) amortises after a few hundred candidates.

### IVFPQ: quantize the residual

The composite that dominates billion-scale indexes applies PQ not to the raw vector but to the **residual** after coarse quantization [9]:

```
y  ≈  q1(y)  +  q2( y − q1(y) )
     coarse       PQ on the leftover
```

Residual encoding "captures the difference between the original vector and its coarse quantization, allowing better precision with the same code length" [9]. The reason is dynamic range: within one [IVF cell](05-ivf-and-clustering.md) every vector is already near the centroid, so 256 centroids spread over a small residual cloud resolve it far more finely than 256 spread over the whole dataset. Same bits, less error, purely because the coarse partition normalised the input.

The FAISS SIFT1M numbers show what that composition does and does not fix [5]:

| Index | Recall | Search time | Memory |
|---|---|---|---|
| `FlatL2` (exact) | 100% | 8.26 ms | 256 MB |
| `PQ` | 50% | 1.49 ms | 6.5 MB |
| `IVFPQ` (nprobe=48) | 52% | 0.09 ms | 9.2 MB |

97.5% compression, 90× faster than exact — and **half the true neighbours missing**. Read that as the argument for two-stage retrieval below, not as an endorsement of PQ alone.

### OPQ: rotate before you split

PQ's subspace split is arbitrary: dimensions 0–15 become subvector 0 because of how the embedding was laid out, not because they belong together. If variance is unevenly distributed, some subspaces carry most of the information and get the same 256 centroids as subspaces carrying almost none. OPQ learns an orthogonal rotation `R` and encodes `Rx`, chosen to balance variance across subspaces and decorrelate dimensions before splitting [12]. Because `R` is orthogonal it preserves L2 distances exactly, so it is free from a correctness standpoint, and FAISS's memory-constrained guidance recommends `OPQM_D,...,PQMx4fsr` over bare PQ [11]. Honest caveat: no source behind this file gives OPQ's recall improvement at fixed code length or its optimization procedure in verifiable detail [12] — sound mechanism, unmeasured magnitude here.

### Binary quantization

One bit per dimension — the sign relative to a threshold — compared by Hamming distance, which is `XOR` plus `POPCNT` over machine words. 32× memory reduction in theory [1][2][13], with the speed benefit framed as up to 40× faster retrieval [13].

It also breaks predictably. Below **1024 dimensions** "there is not enough information maintained in the binary vector to achieve good results", even though speed and RAM still improve [13]. Measured, all with oversampling and rescoring [13]:

| Model | Dims | Oversampling | Recall |
|---|---|---|---|
| OpenAI text-embedding-3-small | 1536 | 3× | 0.9847 |
| OpenAI text-embedding-ada-002 | 1536 | 4× | 0.98 |
| Mistral Embed | 768 | 3× | 0.9445 |

The deeper explanation is that BQ's success is a property of your *embeddings*, not the method. The coordinate-heterogeneity analysis argues it works when per-dimension variances are unequal — that heterogeneity creates an implicit weighting Hamming distance inherits — and fails on near-isotropic embeddings [17]. It reports off-diagonal covariance terms contributing roughly **30–50% of the ranking-fidelity signal** via a Stein's-lemma argument, and derives a gain `Δ_het(σ) = K·CV²(σ) + O(CV³)` with CV the coefficient of variation of per-coordinate variances [17].

**RaBitQ** puts this on a rigorous footing: a randomized method converting `D`-dimensional float vectors into `D`-bit strings with a sharp theoretical error bound, whose abstract states that prior methods "do not have a theoretical error bound and are observed to fail disastrously on some real-world datasets", and which reports outperforming PQ and its variants on the accuracy-efficiency trade-off by a clear margin [14]. FAISS lists it as the maximum-compression tier [11].

### Matryoshka representations: the orthogonal axis

Everything above cuts **bits per dimension**. Matryoshka Representation Learning cuts **the number of dimensions**, at training time, structuring embeddings so nested prefixes (first 64, 128, 256, ...) are each independently usable without retraining, earliest dimensions carrying the most essential information [15]. Reported: up to 14× smaller embeddings at the same ImageNet-1K accuracy, up to 14× real-world retrieval speed-ups, up to 2% accuracy improvement on long-tail few-shot classification, with no additional inference or deployment cost, across ViT, ResNet, ALIGN and BERT-family models [15]. The two axes compose — truncate, then quantize.

**The caveat that matters:** a follow-up study found Matryoshka-trained models (Qwen3-Embedding) "showed no qualitatively different behavior" from non-Matryoshka models under head-truncation, concluding truncation robustness "appears to be a broader property of recent high-dimensional text embedding models" [16]. Truncating a modern embedding may work whether or not it was trained for it — measure before paying for MRL.

### Two-stage retrieval: search compressed, rescore exact

Every quantized system that reaches acceptable recall does this.

```
1. search the compressed index for C candidates, C = overfetch × K
2. fetch full-precision vectors for those C candidates (RAM or disk)
3. recompute exact distances, re-sort, return top-K
```

Qdrant calls it rescore, Weaviate rescoring [13][18]. The overfetch factor `C/K` is the dial: 3× oversampling gives 0.9445–0.9847 recall depending on the model, 4× gives 0.98 on ada-002, and with `rescore=True` at limit 50 on a 100K-vector index they reached 100% recall in the tested configuration [13].

Weaviate's version is a clean before/after: in v1.18 PQ without rescoring degraded the recall curve significantly at higher compression; in v1.21 with rescoring — search compressed, retrieve full vectors from disk for the candidate bucket, rescore, running the finer search *after* the rough one concludes — the compressed and uncompressed recall/QPS curves became "very close together" [18]. Memory alongside: Sphere 725 MB vs 5,014 MB (~86% reduction), DBPedia 900 MB vs 6,333 MB (~86%), full Sphere at 1B vectors 0.7 TB vs 2.7 TB (~74%), with 6 dimensions per PQ segment for 24:1 compression [18].

Rescoring "increases the time it takes to return a query" [13] and requires full-precision vectors to stay accessible somewhere. So the accurate description of a quantized system is not "we compressed the data" but **"we compressed the searchable index and still store full vectors for the winners."**

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| `M` (PQ subvectors) | Code length and granularity; `D % M == 0` required [10] | 8 [6][9]; or 6–8 dims per segment [18] | Too few: coarse subspaces, recall collapses. Too many: code approaches raw size |
| `nbits` | Centroids per subspace, `k* = 2^nbits` | 8 → 256 [5][6]; FAISS allows 8, 12, 16 [10] | 16 bits needs 65,536 centroids per subspace, far more training data, 256× the codebook memory |
| PQ training set | Codebook quality | tens of thousands of vectors minimum | Under-trained codebooks produce reconstruction error that looks like a search bug |
| int8 calibration range | Per-dimension min/max [4] | full-database min/max | One outlier per dimension wastes the whole 256-level range [4] |
| Binary threshold | Sign cutoff per dimension | 0, or per-dimension median | A badly centred threshold makes bits constant and Hamming distance uninformative |
| Overfetch `C/K` | Rescoring candidate set | 3×–4× [13] | Too low: compression error leaks into results. Too high: approaching brute force |
| Rotation (OPQ) | Variance balance across subspaces [12] | on, for PQ | Helps PQ; may actively hurt binary quantization [17] — see Open questions |

## How real systems do it

**FAISS** treats quantization as a tiered escalation driven by memory pressure: no concern → `",Flat"` inside IVF, tune `nprobe`; moderate → PQ variants; high → `OPQM_D,...,PQMx4fsr`; maximum → RaBitQ [11]. The `x4fsr` suffix is a SIMD 4-bit fast-scan layout with reranking built in — even FAISS's recommendation bundles rescoring into the index string.

**OpenSearch/AWS** publishes the most useful controlled comparison here, at 1B × 384 [3]:

| Method | Memory | Recall@100 |
|---|---|---|
| No quantization | 1,830.4 GB | ≥ 0.99 |
| FP16 scalar | 985.6 GB | ≥ 0.95 |
| Byte (int8) scalar | 563.2 GB | ≥ 0.95 |
| Binary scalar | 193.6 GB | ≥ 0.90 |
| Product quantization | 184.8 GB | > 0.7 |

Read the bottom two rows against each other: binary scalar uses 193.6 GB for ≥0.90 recall, PQ essentially the same 184.8 GB for >0.7. **A simpler scalar method beat PQ at the same memory footprint** in their benchmark [3] — one vendor, one dataset, one dimensionality, but a controlled measurement that contradicts "PQ is strictly better because it is smarter."

**Qdrant** built its story on binary quantization plus rescoring, and is unusually direct about the failure boundary, publishing the sub-1024-dimension degradation and per-model recall differences rather than only the 40× headline [13].

**Weaviate** is the case of a system that shipped PQ, found the recall unacceptable, and fixed it architecturally rather than by tuning — v1.18 PQ alone degraded badly at high compression, v1.21 added a distinct rescoring phase and the curves converged [18]. The lesson generalises: a compressed index without a rescoring path is an incomplete design, and one major system shipped that version before correcting it.

**Vespa** publishes the model-dependence most vendors elide: bf16 free everywhere, int8 costing 2–6% on ModernBERT-based models but 8–13% on E5 variants, binary retaining 98% on one model (0.670 vs 0.685) and 92% on another (0.602 vs 0.651) [2]. Their posture — bf16 universally, everything else measured per model — is the correct default.

**Where they genuinely differ:** OpenSearch treats quantization as a storage-layer concern bolted onto an unmodified HNSW graph, accounting for `m` and `ef_construction` on top of each method but making no claim that quantization changes graph structure [3]. Whether that separation is right is an open question below.

## Complexity and cost

| Operation | Cost |
|---|---|
| int8 calibration | one pass `O(N·D)`; stores `2D` floats |
| int8 distance | `O(D)` integer ops, 4× less memory traffic than float32 |
| PQ training | `M` independent k-means runs, `O(t · 2^nbits · N_train · D/M)` each |
| PQ encoding | `O(N · 2^nbits · D)` |
| ADC table build | `O(2^nbits · D)` per query — `256·D` multiply-adds at nbits=8 |
| ADC per candidate | `M` lookups + `M−1` adds; independent of `D` |
| PQ storage | `N · M · nbits / 8` bytes + `M · 2^nbits · (D/M) · 4` codebook bytes |
| Binary distance | `D/64` XOR+POPCNT word operations |
| Rescoring | `O(C · D)` plus `C` random reads if full vectors are on disk |

The disk-read term usually dominates a rescoring stage, not the arithmetic.

## Failure modes

| Failure | What it looks like |
|---|---|
| Compressed recall accepted as final | 50% recall from bare PQ [5] shipped with no rescoring stage: plausible results, wrong half the time |
| int8 calibrated on a sample missing the tails | Later vectors clamp at the range boundary, silently |
| Codebooks trained on the wrong distribution | Train on one corpus, index another; reconstruction error explodes with no error signal |
| Binary below 1024 dimensions | Documented information loss [13]; a quiet recall drop, not an exception |
| Rotation applied blindly | Random rotation helps PQ/OPQ and is reported to *destroy* the coordinate heterogeneity BQ depends on [17] — one shared preprocessing step improving one encoder and breaking another |
| Forgetting the full vectors exist | The compressed index is 6.5 MB; the full vectors needed for rescoring are still 256 MB [5]. Budget both |
| Codebook memory at high `nbits` | 16-bit codebooks are 256× the 8-bit ones and need proportionally more training data [10] |
| Distribution drift | Codebooks and calibration ranges are learned once, and degrade quietly exactly as [IVF centroids](05-ivf-and-clustering.md) do |

## How to test it

- **ADC differential test.** For random query/vector pairs, the table-lookup distance must equal the distance to the reconstructed vector, to floating-point tolerance. The highest-value test here — the `M`-lookup path is where indexing bugs hide, and they produce plausible-looking wrong rankings.
- **Reconstruction error as a unit test.** Encode then decode; assert mean L2 reconstruction error below a threshold on a held-out set. Catches codebook bugs that recall tests misattribute to search.
- **Rank correlation, not just recall.** Spearman correlation between exact and approximate distances over a candidate set: recall@10 can hold flat while ranking quality rots.
- **Overfetch sweep.** Recall@K against `C/K ∈ {1, 2, 3, 4, 8}` gives your operating point; a flat curve means compression is not the bottleneck.
- **Codebook occupancy and determinism.** No PQ centroid owning zero vectors or a wildly disproportionate share; identical codes on re-encode and identical codebooks for a fixed seed — without the latter, recall regressions are unbisectable.
- **Per-model tolerance in CI.** Given the 87%–98% spread under int8 [2], treat "which models survive quantization" as a measured property that changes when you change embedders.

## Minimal demo version

Build these three, in order. Well under a day.

**1. int8 scalar quantization — about 40 lines.** Per-dimension min/max calibration, encode, decode, distance over codes [4]. Plot recall@10 and memory against the float32 baseline. The cheapest way to make the memory/recall trade visible, and it works immediately.

**2. Product quantization — the one to build if you build only one.** Reuse the k-means from [IVF](05-ivf-and-clustering.md); PQ is that k-means run `M` times on slices. What earns the effort is **the ADC lookup table** — an `M × 256` table built once per query, then `M` lookups and `M−1` adds per candidate instead of `D` multiply-adds [5][6]. That is the transferable idea, and implementing it once is how you understand why PQ is fast rather than merely small. Instrument it: print the per-query table-build cost and the per-candidate cost, and show where the table pays for itself.

**3. Binary quantization — about 20 lines as an add-on.** Sign bit, `XOR` + `POPCNT`. Its demo value is precisely that it is *bad*: run it at `D = 256` without rescoring, watch recall fall off a cliff, add a 4× overfetch rescoring pass, watch it come back [13]. Nothing demonstrates the necessity of two-stage retrieval so cheaply.

**Cut without losing the lesson:** OPQ's learned rotation (state the mechanism, skip the optimisation), IVFPQ residual encoding (do plain PQ, mention the framing), SIMD fast-scan layouts, `nbits ∈ {12, 16}`, RaBitQ, MRL, disk-resident full vectors.

**Cannot cut:** the float32 baseline running alongside, since every compression number is a ratio against something; the rescoring stage at least once, because a demo reporting compressed-index recall as final recall teaches the wrong model; *measured* rather than computed memory, including codebooks and retained full vectors, since the gap between theoretical and realised ratio is itself the lesson; and recall measured against exact brute-force ground truth, because comparing one approximate method to another is not a measurement.

## Open questions and contested claims

- **Scalar quantization beating PQ at equal memory.** OpenSearch's 1B × 384 benchmark: binary scalar at 193.6 GB / ≥0.90 recall against PQ at 184.8 GB / >0.7 [3], contradicting the assumption that learned codebooks always dominate per-dimension scaling. Caveats: one vendor, one dataset, one dimensionality — 384, *below* where binary is supposed to degrade [13], making the result more surprising, not less — and PQ's configuration is not fully specified. Do not generalise it; do not ignore it.
- **Binary compression: 32× or 7×?** The theoretical bit ratio is 32× and three sources state it [1][2][13]. But Qdrant's own end-to-end example — 100K OpenAI 1536-dim embeddings, 900 MB to 128 MB — is roughly **7×**, because of metadata, index overhead and the full-precision copies kept for rescoring [13]. Both numbers appear in the same source, unreconciled. Assume the realised ratio in any rescoring system is far below the headline, and measure it.
- **Rotation helps PQ and may hurt binary quantization.** The "rotation paradox": random orthogonal rotation uniformizes per-dimension variances — what OPQ-style preprocessing wants — and simultaneously destroys the heterogeneity BQ depends on [17]. There is no single "decorrelate then quantize" pipeline right for both encoders.
- **SDC/ADC bounds are reported inconsistently.** One paper review gives `MSDE_SDC ≤ 2·MSE(q)` and `MSDE_ADC ≤ MSE(q)`, reasoning that SDC applies the triangle inequality twice [6]; another summary of the same primary paper states them the other way round [7]. Neither is a first-hand reading [8]. Not in dispute: ADC is what production systems use, because leaving the query unquantized removes an entire error source.
- **Does quantization interact with graph structure, or only storage?** OpenSearch treats them as orthogonal [3]; a recent preprint argues binary quantization should reshape graph topology decisions [20]. Only one side is verified here — the weakest-supported claim in the file, worth chasing alongside [graph indexes](07-graph-indexes-hnsw.md).
- **Two-stage retrieval versus brute force is unquantified end-to-end.** Rescoring recovers recall [13][18] and costs latency [13], but nothing behind this file compares total cost — latency and memory, including retained full vectors — against simply scanning float32. For datasets that fit in RAM uncompressed, the honest answer may be that brute force wins; see [Choosing an index](09-choosing-an-index.md). ScaNN's anisotropic quantization loss [19] and the inverted multi-index / additive quantization line [21] are likewise relevant and unexamined here.

## Sources

[1] datarekha — "The vector-search memory wall: why HNSW eats RAM, and how quantization cuts the bill 32x" — https://datarekha.com/blog/vector-search-memory-wall/ — accessed 2026
[2] Vespa.ai — "Embedding Tradeoffs, Quantified" — https://blog.vespa.ai/embedding-tradeoffs-quantified/ — accessed 2026
[3] AWS Big Data Blog — "Cost Optimized Vector Database: Introduction to Amazon OpenSearch Service quantization techniques" — https://aws.amazon.com/blogs/big-data/cost-optimized-vector-database-introduction-to-amazon-opensearch-service-quantization-techniques/ — accessed 2026
[4] Zilliz Learn — "Scalar Quantization and Product Quantization" — https://zilliz.com/learn/scalar-quantization-and-product-quantization — accessed 2026
[5] Briggs, J. — "Product Quantization: Compressing high-dimensional vectors by 97%" — Pinecone Learn — https://www.pinecone.io/learn/series/faiss/product-quantization/ — accessed 2026
[6] r3gardless.dev — "Product Quantization for Nearest Neighbor Search: Paper Review" — https://r3gardless.dev/en/blog/2026-05-07-product-quantization-for-nearest-neighbor-search-paper-review/ — 2026
[7] Stutz, D. — "Product Quantization for Nearest Neighbor Search, Jégou/Douze/Schmid" (paper notes) — https://davidstutz.de/product-quantization-for-nearest-neighbor-search-jegou-douze-schmid/ — accessed 2026
[8] Jégou, H., Douze, M., & Schmid, C. — "Product Quantization for Nearest Neighbor Search" — IEEE TPAMI — https://inria.hal.science/inria-00514462 — 2011
[9] Johnson, J., Douze, M., & Jégou, H. — "Billion-scale similarity search with GPUs" — https://ar5iv.labs.arxiv.org/html/1702.08734 — 2017
[10] Meta AI Research — "Faiss indexes" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Faiss-indexes — accessed 2026
[11] Meta AI Research — "Guidelines to choose an index" (FAISS wiki) — https://github.com/facebookresearch/faiss/wiki/Guidelines-to-choose-an-index — accessed 2026
[12] Ge, T., He, K., Ke, Q., & Sun, J. — "Optimized Product Quantization" — IEEE TPAMI — https://people.csail.mit.edu/kaiming/publications/pami13opq.pdf — 2013
[13] Qdrant — "Binary Quantization: 40x Faster Vector Search" — https://qdrant.tech/articles/binary-quantization/ — accessed 2026
[14] Gao, J., & Long, C. — "RaBitQ: Quantizing High-Dimensional Vectors with a Theoretical Error Bound for Approximate Nearest Neighbor Search" — SIGMOD 2024 — https://arxiv.org/abs/2405.12497 — 2024
[15] Kusupati, A., Bhatt, G., Rege, A., et al. — "Matryoshka Representation Learning" — NeurIPS 2022 — https://arxiv.org/abs/2205.13147 — 2022
[16] "When Is 0.1% Enough? Analyzing the Combined Effects of Dimensionality Reduction and Quantization on Text Embedding Compression" — https://arxiv.org/html/2606.01074v1 — 2026
[17] "Coordinate Heterogeneity Governs Binary Quantization: From InfoNCE to Recall" — https://arxiv.org/html/2605.17524v1 — 2026
[18] Weaviate — "How to Reduce Memory Requirements by up to 90%+ using Product Quantization" — https://weaviate.io/blog/pq-rescoring — accessed 2026
[19] Guo, R., Sun, P., Lindgren, E., Geng, Q., Simcha, D., Chern, F., & Kumar, S. — "Accelerating Large-Scale Inference with Anisotropic Vector Quantization" (ScaNN) — ICML 2020 — https://arxiv.org/abs/1908.10396 — 2020
[20] "QuIVer: Rethinking ANN Graph Topology via Training-Free Binary Quantization" — https://arxiv.org/html/2605.02171v2 — 2026
[21] Babenko, A., & Lempitsky, V. — "The Inverted Multi-Index" — IEEE TPAMI — http://sites.skoltech.ru/app/data/uploads/sites/25/2014/12/TPAMI14.pdf — 2014
