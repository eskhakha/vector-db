# 14. Sharding and partitioning

> Splitting a collection across machines, and the one design decision — separating *which shard owns a key* from *which node hosts that shard* — that makes everything downstream tractable.

## The problem

One machine holds a finite number of vectors and answers a finite number of queries per second. Past that you need more machines, and the moment you have more than one you must answer two questions for every operation: which machine has this data, and which machine should serve this request.

The naive answer is `shard = hash(id) mod N` where `N` is the node count. It is correct and it is fatal. Changing `N` changes the divisor for *every* key, so `mod N` and `mod (N+1)` agree for almost no key: adding a single node forces nearly the entire dataset to move [1]. This is precisely the problem Karger et al. set out to solve in 1997 — prior hash-based distribution schemes required a globally known fixed server count, and any change remapped essentially all objects [2].

For a vector database the cost is worse than for a key-value store. Moving a key means moving a 512-byte to 6-kilobyte payload, and on arrival the destination may have to rebuild an index over it.

## How it works

### The four candidate schemes

**Consistent hashing.** Map both servers and keys onto a ring of hash values; a key belongs to the first server clockwise from its position. Adding or removing a server moves only the keys in that server's arc — an expected `K/n` keys for `K` keys and `n` servers, rather than all of them [2]. The practical defect is load imbalance: with one ring point per server, arc sizes are highly non-uniform and some servers own disproportionately large segments. **Virtual nodes** fix this by giving each physical server many ring points. Dynamo assigns multiple tokens per node specifically to even out load, to spread bulk transfer during a join or leave across many peers instead of one, and to let heterogeneous hardware own a number of virtual nodes proportional to its capacity [3]. Riak calls the same construct a vnode [4].

**Rendezvous (HRW) hashing.** For object `o` and each candidate server `s`, compute a pseudorandom weight `w(o,s)` from the pair, and assign `o` to `argmax_s w(o,s)` [5]. It needs no shared ring structure at all — every client computes the assignment independently from just the server list and the hash function. Its disruption coefficient is provably optimal at `1/m` for `m` servers: removing a server reassigns only the objects that mapped to it, and adding one steals only the objects for which it happens to win the maximum [5]. The cost is `O(m)` work per lookup rather than `O(log m)`, which stops mattering below a few hundred servers.

**Range partitioning.** Sort keys and assign contiguous ranges, splitting and merging as ranges grow. This enables range scans and adapts to skew, at the cost of a range-map service, split/merge machinery, and hot spots on sequential keys. **A vector database has no range queries over the vector itself.** You would pay the entire cost for none of the benefit.

**Fixed logical shards.** Choose `S` once, compute `shard = hash(id) mod S` with `S` much larger than the node count, and assign shards to nodes through a *separate* placement map. Redis Cluster's 16384 hash slots are this pattern, and it is why Redis avoids the `mod N` problem despite using modulo hashing [1].

### The decision, and why it is the important one

Fixed logical shards, because they cleanly separate two questions that consistent hashing tangles together:

```
   "doc-42"                      client-supplied ID
      │
      ▼
   hash("doc-42")                pure function
      │
      ▼  mod S
   ShardID = 1                   NO cluster state. identical on every
      │                          node and every client. never changes.
      ▼
   shard map lookup              versioned cluster state. changes on
      │                          failover and rebalance.
      ▼
   shard-1: {epoch: 7, leader: node-2, replicas: [node-3]}
```

The first question is a pure function requiring no cluster state, identical everywhere, fixed for the life of the cluster. The second is versioned state that changes on every failover.

That separation is what makes rebalancing tractable. Moving data becomes moving *a shard* — an object with an identity, a log, a sequence number and a snapshot. Under consistent hashing you move *key ranges*, which is strictly harder and teaches less about the mechanisms that matter. See [Rebalancing](20-rebalancing.md).

### Choosing S

`S` should be a highly composite number several times larger than the maximum node count you will ever run. Two forces:

- **Divisibility.** With `S=4`, `RF=2` and 3 nodes you have 8 shard-replicas over 3 nodes, which cannot divide evenly. One node carries a third less load, permanently.
- **Blast radius.** A shard is `1/S` of your data. When its leader dies, `1/S` of the cluster's traffic fails over at once. At `S=4` that is 25%.

| S | Even over 3 nodes? | Max useful nodes | Shard = % of data |
|---|---|---|---|
| 4 | no (3/3/2) | 4 | 25% |
| 6 | yes (4/4/4) | 6 | 16.7% |
| 12 | yes (8/8/8) | 12 | 8.3% |
| 16 | no (11/11/10) | 16 | 6.25% |

`S` is fixed at creation and changing it requires a dump and reload. That permanent ceiling is the price of never having to move individual keys.

### The shard map

Placement is explicit, versioned cluster state:

```json
{
  "version": 12,
  "num_shards": 4,
  "shards": {
    "0": { "epoch": 1, "leader": "node-1", "replicas": ["node-2"] },
    "1": { "epoch": 1, "leader": "node-2", "replicas": ["node-3"] },
    "3": { "epoch": 3, "leader": "node-1", "replicas": ["node-3"] }
  }
}
```

Leadership is spread round-robin so each node leads roughly `S/N` shards, and a shard's replica is never on its leader's node. Shard 3's epoch of 3 says it has failed over twice.

**The client cache is allowed to be wrong.** Every request carries the caller's map version. A node receiving a request for a shard it does not lead replies with a redirect naming the current leader, epoch and version; the caller updates and retries once. CockroachDB calls this `NotLeaseHolderError`, HBase calls it `NotServingRegionException`. Correcting a stale client at the server is far cheaper than keeping every client perfectly synchronised.

### The vector-specific question: random or semantic partitioning?

Everything above partitions by a hash of the ID, which scatters semantically similar vectors uniformly. Every query must therefore touch every shard — full fan-out.

The alternative is **semantic partitioning**: cluster the vectors and put each cluster on a shard, so a query need only probe the few shards whose centroids are near it. This is [IVF](05-ivf-and-clustering.md) at cluster scale, and the evidence that it works is real. A graph-partitioning approach from Google Research reports up to 25% higher recall than k-means clustering at a fixed small number of shards probed [6]. SPANN builds a billion-scale index on exactly this partition-and-probe principle [7], and Vespa's hybrid HNSW-IF design reaches 90% recall@10 in roughly 50 ms at billion scale while probing only about 128 posting lists [8].

**Default to hash partitioning anyway.** Elasticsearch still defaults to random partitioning with full fan-out [9], and the reasons generalise:

| | Hash / random | Semantic / cluster-based |
|---|---|---|
| Load balance | uniform by construction | follows data distribution; skews with it |
| Query cost | fan-out to all `S` shards | probe a few shards |
| Recall | exact merge is provable | a true neighbour in an unprobed shard is lost |
| Rebalancing | move any shard anywhere | placement is coupled to content |
| Adding data | trivial | may need re-clustering |

Semantic partitioning trades a provable exactness property and operational simplicity for a fan-out reduction. Take that trade when fan-out is your measured bottleneck, not before.

## Parameters and knobs

| Parameter | Controls | Typical | If you get it wrong |
|---|---|---|---|
| `S` (shard count) | partition granularity | 4–16 for a demo; 64–256 in production | Too low: uneven placement, huge blast radius, low node ceiling. Too high: per-shard overhead and more tail exposure on fan-out. Cannot be changed without a reload. |
| Hash function | key → shard | any stable non-cryptographic hash | A per-process-seeded hash puts the same key on different shards on different nodes |
| `RF` | copies per shard | 2–3 | See [Replication](15-replication.md) |
| Virtual nodes per server | ring balance (consistent hashing only) | 100–256 | Too few: hot spots. Too many: a large ring to gossip |

**One hazard worth naming.** The hash must be stable across processes, machines and releases. Go's `hash/maphash` is deliberately seeded per process; using it here would place the same key on different shards on different nodes, which is catastrophic and very confusing to debug. The requirement is not speed or cryptographic strength — it is determinism.

## How real systems do it

**Redis Cluster** hashes into 16384 fixed slots and maps slots to nodes as a separate migratable layer [1] — the fixed-logical-shard pattern, arrived at from the opposite direction.

**Dynamo and Cassandra** use consistent hashing with virtual nodes, and Dynamo's paper is explicit that virtual nodes exist to solve load imbalance, transfer parallelism and hardware heterogeneity together [3]. Riak exposes the same idea as vnodes [4].

**Elasticsearch** fixes the shard count at index creation, partitions randomly, and fans every k-NN query out to all shards [9]. That a mature search engine still does the simple thing is the strongest available argument for defaulting to it.

**Milvus** separates the partition from placement more aggressively than most: data is partitioned into segments that live in object storage, and compute nodes subscribe to what they need rather than owning it [10].

**Vespa and SPANN** are the counter-examples, and they are counter-examples on purpose: both are billion-scale designs where full fan-out is genuinely unaffordable, so both accept content-coupled placement to buy a large reduction in shards probed [7][8].

The honest summary is that the field has not converged. Systems optimising for operational simplicity partition randomly; systems optimising for billion-scale fan-out partition semantically. Which one is right depends entirely on whether fan-out is your bottleneck.

## Complexity and cost

For `N` vectors, `S` shards, `D` dimensions, `K` results:

| | Cost |
|---|---|
| Routing a write | `O(1)` hash, one node contacted |
| Fan-out search | `S` requests of ~`4D` bytes, `S` responses of ~`24K` bytes — network is negligible |
| Per-shard scan | `O((N/S)·D)` — this dominates |
| Merge | `O(S·K·log K)` — negligible |
| Search latency | `max` over shards, not sum, since they run in parallel |

The interesting consequence: **more shards means less work per shard but more tail exposure**, because you wait for the slowest of `S`. That trade is why `S` is worth benchmarking rather than reasoning about. See [Distributed search](17-distributed-search.md).

## Failure modes

- **A per-process-seeded hash.** The same key lands on different shards on different nodes. Data appears to vanish and reappear depending on which node answers.
- **Storing the shard ID on the record.** It can then disagree with the partition function. Derive it, never store it.
- **`S` chosen too small.** A permanent ceiling on cluster size and a 25%-of-traffic blast radius on every failover.
- **Skewed IDs.** Hash partitioning assumes IDs are not adversarial. Sequential or heavily prefixed IDs still distribute fine under a good hash; deliberately colliding ones do not.
- **Semantic partitioning meeting a distribution shift.** Clusters learned on last month's data mis-route this month's queries, and recall degrades silently because nothing errors.
- **Assuming an ID appears on exactly one shard, without checking.** If a duplicate ID ever surfaces across two shards, that is a partitioner or placement bug and the merge should log it loudly rather than silently deduplicating.

## How to test it

- **Determinism.** The same ID maps to the same shard, every call, every process. Trivial to write and it catches the maphash class of bug immediately.
- **Distribution.** Hash a large synthetic ID set and run a chi-square test against uniform. With `S=8` and 7 degrees of freedom, a statistic above roughly 24.3 is a red flag at the 0.999 level. This is a sanity check, not a proof.
- **The one that matters: global top-K over `S` shards must be byte-identical to a single shard holding all the same data**, for several `S`, both metrics, and several `K`. If this fails, nothing built above the partitioner can be trusted.
- **Parameterise every test over `S`.** Run at `S ∈ {1, 2, 4, 8}`. `S=1` is the degenerate case that must still work.
- **Rebalance simulation.** Given a map, compute how many shards move when a node is added. Under fixed logical shards this should be roughly `S/N`, never all of them.

## Minimal demo version

The partitioner is genuinely about thirty lines: a stable hash, a modulo, and a constructor that rejects `S <= 0`. The shard map can be a JSON file. Do not build consistent hashing — it solves a problem (moving keys) that fixed logical shards define out of existence, and building it would obscure the separation that is the actual lesson.

**What you must not cut:**

- **A stable hash.** Using a randomly seeded one produces a demo that works on one node and corrupts itself on three.
- **`S` as a config value, with tests parameterised over it.** Hard-coding `S=4` makes the eventual `S=12` change a refactor instead of a config edit, and hides the tail-latency trade entirely.
- **The chi-square distribution check.** Four lines, and it is the only thing standing between you and a subtly bad hash.
- **The exactness test against a single-shard oracle.** This is the whole point of having sharded at all.

Skip semantic partitioning. It is worth understanding and not worth building at demo scale, because with 20,000 synthetic vectors full fan-out costs nothing and the recall risk buys you no visible win.

## Open questions and contested claims

**Does semantic partitioning actually help?** The evidence for it is real and recent [6][7][8], but it is drawn from billion-scale settings where full fan-out is genuinely prohibitive. Elasticsearch's continued use of random partitioning with full fan-out [9] is the strongest counter-evidence at ordinary scale. No source found gives a dataset size at which the crossover happens; the honest position is that semantic partitioning is a specialist optimisation for a specific bottleneck rather than a general improvement.

**How large should `S` be?** The composite-number and blast-radius arguments are sound but produce a range, not a number. There is no principled way to pick within it short of measuring your own fan-out tail latency at two values.

## Sources

[1] Redis Ltd — "Redis Scalability: Clustering, Sharding, and Hash Slots" — Redis documentation, accessed 2026
[2] Karger, Lehman, Leighton, Levine, Lewin, Panigrahy — "Consistent Hashing and Random Trees: Distributed Caching Protocols for Relieving Hot Spots on the World Wide Web" — MIT, STOC 1997
[3] DeCandia, Hastorun, Jampani, Kakulapati, Lakshman, Pilchin, Sivasubramanian, Vosshall, Vogels — "Dynamo: Amazon's Highly Available Key-value Store" — Amazon, SOSP 2007 — https://www.cs.cornell.edu/courses/cs5414/2017fa/papers/dynamo.pdf
[4] Basho — "Vnodes" — Riak KV documentation — https://docs.riak.com/riak/kv/latest/learn/concepts/vnodes/index.html
[5] Thaler, Ravishankar — "A Name-Based Mapping Scheme for Rendezvous" — IEEE/ACM Transactions on Networking 6(1), Feb 1998 — https://www.microsoft.com/en-us/research/wp-content/uploads/2017/02/HRW98.pdf
[6] Gottesbüren, Dhulipala, Jayaram, Łącki — "Unleashing Graph Partitioning for Large-Scale Nearest Neighbor Search" — Google Research, PVLDB 18(6), 2025 — https://www.vldb.org/pvldb/vol18/p1649-gottesbueren.pdf
[7] Chen, Zhao, Wang, Li, Liu, Li, Yang, Wang — "SPANN: Highly-efficient Billion-scale Approximate Nearest Neighbor Search" — Microsoft Research, NeurIPS 2021 — https://arxiv.org/abs/2111.08566
[8] Bergum — "Billion-scale vector search using hybrid HNSW-IF" — Vespa.ai blog, 2022 — https://blog.vespa.ai/vespa-hybrid-billion-scale-vector-search/
[9] Elastic — "kNN search in Elasticsearch" — Elastic documentation, accessed 2026 — https://www.elastic.co/docs/solutions/search/vector/knn
[10] Zilliz — Milvus architecture overview — https://milvus.io/docs/architecture_overview.md
