# 17. Distributed search

> Answering one top-K query from `S` shards at once: why merging per-shard top-K is exact, why it stops being exact the moment the per-shard index is approximate, and why the slowest shard sets the latency.

## The problem

Once the data is partitioned, no single node can answer a query. The dataset lives in `S` disjoint pieces and the answer spans all of them.

That creates three problems at once, and they are usually confused with each other:

1. **Correctness.** Is a merge of per-shard results the same answer as scanning everything? Sometimes yes, sometimes provably not.
2. **Latency.** The query finishes when the *slowest* shard replies, not the average one. Fan-out converts a good median into a bad tail.
3. **Partial failure.** One shard is down or slow. Is a nine-tenths answer acceptable, and if so, does the client know?

Answer these separately. Systems that conflate the second and third — returning incomplete results because a shard was slow, while labelling them complete — are the ones that produce silent recall loss nobody notices for months.

## How it works

```
                        client
                          │  query q, K
                          ▼
                   ┌─────────────┐
                   │ COORDINATOR │  one deadline for the whole query
                   └──────┬──────┘
          ┌───────────────┼───────────────┐
          ▼               ▼               ▼
       shard 0         shard 1         shard 2
     local top-K     local top-K     local top-K
          │               │               │
          └───────────────┼───────────────┘
                          ▼
                 merge S×K candidates
                          ▼
                    global top-K
```

Elasticsearch documents exactly this shape for its kNN API: it finds candidates per shard, computes similarity, selects the top `k` per shard, and merges them into the global top `k` [1].

### Why merging local top-K is exact — when per-shard search is exact

This is the one proof worth being able to reproduce, because the entire architecture rests on it.

Shards partition the dataset **disjointly**: every vector lives on exactly one shard. Suppose vector `v` belongs to the true global top-K. Then fewer than `K` vectors in the whole dataset outrank `v`, so *a fortiori* fewer than `K` vectors **on `v`'s own shard** outrank it. Therefore `v` is in that shard's local top-K, and if per-shard search is exhaustive, that shard returns it.

Every member of the global top-K is thus present in the union of the `S` local top-K lists. Re-selecting the best `K` from those `S × K` candidates is **exact**, not approximate [1][2].

Two conditions are load-bearing and both are easy to break by accident:

- **Disjointness.** If a vector is on two shards it can be counted twice. Deduplicate by ID at the merge, or the top-K can contain the same vector in two positions.
- **Exhaustiveness per shard.** The proof consumes it directly.

### Why it breaks under an approximate index

Once each shard runs HNSW or IVF, the local top-K a shard returns is no longer guaranteed to contain that shard's true top-K. Elasticsearch is explicit that approximate kNN "trades perfect accuracy for speed, so results aren't always the true k closest neighbors" [1].

Now the merged global top-K inherits two errors: the shard-level index's approximation, and the possibility that a shard's true top-K member never reached the merge at all.

The standard mitigation is **over-fetch**: ask each shard for `k' > k` candidates. Elasticsearch's `num_candidates` is exactly this, always ≥ `k`; increasing it improves recall at the cost of latency, decreasing it does the reverse [1].

The important part is the asymmetry with the exact case: **there is no `k'` that provably restores exactness once the per-shard index is approximate** — only diminishing residual error [1]. In the exact case, `k' = k` is provably enough. That is a genuine qualitative difference, and it is why debugging a distributed layer against a brute-force index first is worth doing: the exactness proof gives you an oracle.

### Deterministic tie-breaking

Ties on the score must be broken by a fixed secondary key — vector ID or insertion sequence — so that repeated identical queries against unchanged data return identically ordered results. Without a deterministic secondary key, an unstable sort or a non-deterministic merge order at the coordinator silently reorders equal-scoring candidates between runs. This is standard sorted-merge practice rather than a vector-search-specific result, and no primary source specific to vector top-K merges was located for it.

Ranking by `(score, id)` costs nothing and makes every downstream test reproducible.

### Deadlines, not timeouts

Propagate **one deadline** for the whole query, and have each hop compute its remaining budget, rather than giving each hop a fresh independent timeout. A chain of three hops each with a 1-second timeout is a 3-second query. This is well-established RPC practice; it is not specific to vector search.

### Partial results

Dean and Barroso's "good-enough" principle is the authoritative justification for allowing them: respond once a sufficient number of shards have replied, deliberately sacrificing completeness for latency, on the empirical observation that in large fan-out systems "any individual server holds the optimal result in fewer than 1 in 1,000 queries" [3]. Waiting for the last straggler is rarely worth the latency.

But — and this is the design decision, not the mechanism — the response must **say** it is partial. A `shards_ok / shards_total` field costs one integer and converts a silent recall bug into a visible one.

### The tail is the latency

Dean and Barroso identify the root causes of tail latency: shared resource contention, background daemons, global resource sharing and maintenance, and queueing amplification across layers. With fan-out, overall response time is bounded by the **slowest** shard, not the average [3].

Two mitigations, both measured:

- **Hedged requests.** Send to a preferred replica; if no response after a short delay, send the same request to a second replica and take whichever returns first. Measured: p99.9 latency reduced **from 1,800 ms to 74 ms**, at a cost of **2% additional load** [3].
- **Tied requests.** Send to two replicas simultaneously, staggered by roughly two network-message delays; whichever starts executing first cancels the other. Measured: **16% median reduction, nearly 40% at p99.9**, with latency profiles under high contention comparable to mostly idle systems [3].

The adversarial note: hedging is nearly free *because* hedges are delayed and cancelled. Sending duplicates eagerly, to too many replicas, or without cancellation adds real duplicate work and can make tail latency worse under sustained load. Dean and Barroso's own caveat is that hedging must be applied selectively — only to requests that have already waited past a percentile threshold — to keep the added-load fraction small [3].

### Replica selection

Round-robin routing ignores the information available about replica load and latency, so when one backend degrades, round-robin keeps sending it a fixed share of traffic and injects its slowness into a fixed fraction of all requests [4].

Linkerd's measured comparison — 11 backends, one spiking to 2 s latency for 30 s, 1 s client timeout [4]:

| Algorithm | Behaviour | Effective success rate |
|---|---|---|
| Round robin | degrades above p95 | ~95% |
| Least-loaded (outstanding requests) | stays fast until ~p99 | ~99% |
| Peak EWMA (latency history) | stays fast until ~p99.9 | ~99.9% |

The theory is **power-of-two-choices**: sample two replicas at random and route to the less loaded one. This gets close to full-global-state routing quality at a fraction of the coordination cost, because the probability of picking two simultaneously overloaded replicas is exponentially smaller than picking one [5]. It is the basis nginx, HAProxy and Linkerd all cite for their P2C and least-outstanding-requests modes [4].

**Least-outstanding-requests** is the direct operationalisation, and it is the right default: it reacts to a replica's actual current saturation rather than assuming uniform capacity.

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| `k'` per-shard fetch (`num_candidates`) | recall under an approximate index | `k` exactly if exact; ≥ `k`, often 10× `k`, if approximate | too low: silent recall loss; too high: linear latency cost |
| Query deadline | total latency ceiling | 100 ms – 1 s | fresh per-hop timeouts multiply into a much longer real ceiling |
| Minimum shards required | partial-result policy | all, or a fraction | "all" turns one slow shard into an outage; a fraction without a flag hides recall loss |
| Hedge delay | when to duplicate a request | a high percentile of normal latency | too eager: duplicate load; too late: no tail benefit |
| Replica selection | which copy answers | least-outstanding-requests | round robin routes into a known-degraded replica |
| Tie-break key | reproducibility | `(score, id)` | non-deterministic result ordering between identical runs |

## How real systems do it

**Elasticsearch** documents the per-shard-then-merge mechanic explicitly and exposes `num_candidates` as the over-fetch knob, with the recall/latency trade-off stated in the documentation rather than left to be discovered [1]. It also states plainly that approximate kNN does not always return the true k nearest neighbours [1] — a directness worth imitating.

**Vespa** goes further in a different direction with hybrid HNSW-IF for billion-scale search, splitting the problem between an in-memory graph over centroids and posting lists on disk [6], which changes what "local top-K" even means per node.

**Google's fan-out services** (Dean and Barroso) are the source of the tail-latency framing and the only place the hedging numbers come from measurement rather than assertion [3].

The interesting divergence: Elasticsearch treats over-fetch as a **per-query** parameter the caller tunes, while systems built around a fixed recall target treat it as a **configured** property of the index. The first makes the trade-off visible to whoever is best placed to make it; the second makes it consistent. Both are defensible, and the choice tells you who the system thinks its user is.

## Complexity and cost

For `S` shards, `N` vectors total, `D` dimensions, top-`K`:

| | Exact (brute force per shard) | Approximate per shard |
|---|---|---|
| Per-shard work | `O((N/S) · D)` | `O(k' · log(N/S))` roughly, index-dependent |
| Network | `S` requests, `S · k'` results returned | same |
| Merge | `O(S · k' · log K)` with a heap | same |
| Latency | max over shards, not sum | max over shards |
| Exactness | provable | not restorable at any `k'` |

The merge is negligible. `S · K` for four shards and `K = 10` is 40 candidates — nothing. The cost is entirely in the fan-out and the tail.

Sharding turns a `O(N·D)` scan into `S` parallel `O((N/S)·D)` scans. At 1M vectors and 4 shards, each shard scans 250K. Since brute force is memory-bandwidth-bound (see [Brute force and kernels](03-brute-force-and-kernels.md)), that is a real 4× — up to the point where the tail dominates and adding shards makes things worse, because more shards means more chances that one of them is slow [3].

## Failure modes

- **One shard down.** Either the query fails or it returns partial results. Silently returning partial results labelled complete is the failure mode to design against.
- **One shard slow.** Worse than down, because there is no error to react to. This is what hedging and least-outstanding-requests target.
- **Duplicate responses.** A retried request whose original also completes. Deduplicate by ID at the merge.
- **Recall loss that looks like a data bug.** An approximate per-shard index with `k' = k` loses recall invisibly. Users report "search got worse", nothing is in the logs.
- **Fan-out amplification.** With enough shards, the probability that *some* shard is in its slow tail approaches one. At `S` shards each with p99 latency `L`, the query's p99 is much worse than `L` [3].
- **Non-deterministic ordering.** Equal-scoring results reorder between identical runs, so every downstream test is flaky for reasons unrelated to the change under test.

## How to test it

- **Differential test against a single-shard oracle.** Build the same dataset once as `S` shards and once as one shard. For exact per-shard search, assert the results are **byte-identical**, not merely similar. This is the strongest test in the whole distributed layer and it exists only because the exactness proof holds.
- **Property test on the merge.** Generate random per-shard result lists, merge, and assert the output equals sorting all candidates and taking the first `K`, with ties broken by ID.
- **Kill a shard mid-query** and assert the declared policy: either an error, or partial results *with the partial flag set*. Assert the flag, not just the results.
- **Delay a shard past the deadline** and assert the query returns at the deadline rather than at the delay. A test that only kills shards never catches the slow-shard path, which is the more common production failure.
- **Recall measurement under an approximate index**, sweeping `k'`. Without this the over-fetch parameter is a guess. Note that random synthetic vectors are pathologically uniform in high dimensions, so recall numbers from them do not transfer to real embeddings — this is fine for testing an exact index, and not fine for tuning an approximate one.

## Minimal demo version

A coordinator with three parts: hash the query to nothing (searches hit all shards), fan out concurrently with a single shared deadline, and merge with a bounded heap keyed on `(score, id)`.

Make the **partial-result policy explicit and visible**: return `results, shardsOK, shardsTotal`. Then close one shard and run the same query. Watching `4/4` become `3/4` with a plausible-looking but wrong answer is the entire lesson, and it takes one line of demo output.

**What you can cut:** hedged requests, tied requests, EWMA replica selection, over-fetch tuning, and any approximate index. All of them are refinements on a mechanism that already works.

**What you cannot cut without the demo becoming a lie:**

- **A shard that is unavailable must actually fail its reads.** If a closed shard quietly returns stale results, the partial-result path never fires and the demo proves nothing. This is a real bug that a green test suite can miss, because the tests assert what you thought to assert.
- **The deterministic tie-break.** Without it the differential test against the single-shard oracle is flaky, and you will "fix" it by weakening the assertion.
- **One deadline, propagated.** Per-hop timeouts make the latency story wrong in a way that is invisible at four shards and disastrous at forty.

## Open questions and contested claims

**Is partial results the right default?** Dean and Barroso's evidence supports it for large fan-out web search, where an individual server holds the optimal result in fewer than 1 in 1,000 queries [3]. That statistic is about *web search over thousands of leaves*. Whether it transfers to a four-shard vector index where each shard holds a quarter of the data is not established — with `S = 4`, a missing shard is missing 25% of the corpus, and the "good-enough" argument is much weaker. Treat the default as workload-dependent, and note that the widely-cited justification comes from a very different regime.

**Does hedging help or hurt?** The measured wins are large and come with an explicit caveat about selective application [3]. The load-amplification critique is practitioner consensus rather than a documented incident; a stronger version of this claim would need a published post-mortem where hedging measurably worsened tail latency, and none was located.

## Sources

[1] Elastic — "kNN search in Elasticsearch" — https://www.elastic.co/docs/solutions/search/vector/knn
[2] Cao, Wang — "Efficient Top-K Query Calculation in Distributed Networks" — PODC 2004 — https://crypto.stanford.edu/~cao/topk.pdf
[3] Dean, Barroso — "The Tail at Scale" — Communications of the ACM 56(2), 2013 — https://cacm.acm.org/research/the-tail-at-scale/
[4] Linkerd — "Beyond Round Robin: Load Balancing for Latency" — 2016 — https://linkerd.io/2016/03/16/beyond-round-robin-load-balancing-for-latency/
[5] Mitzenmacher — "The Power of Two Choices in Randomized Load Balancing" — IEEE TPDS, 2001
[6] Bergum — "Billion-scale vector search using hybrid HNSW-IF" — Vespa.ai blog, 2022 — https://blog.vespa.ai/vespa-hybrid-billion-scale-vector-search/
