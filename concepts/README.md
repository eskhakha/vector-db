# Vector database concepts

A reference for the machinery behind a distributed vector database, written for someone who wants to understand *why* each mechanism exists rather than what it is called.

Every file follows the same structure — the problem it solves, how the mechanism works, its knobs, **how real systems do it (including where they were publicly wrong)**, complexity, failure modes, how to test it, and the smallest version that still teaches the lesson.

Two conventions run through all of them:

- **The failure comes before the mechanism.** If a section cannot state what breaks without a mechanism, the mechanism does not belong there.
- **Where systems were wrong is more instructive than where they were right.** Jepsen reports, data-loss issues and documented reversals are cited throughout, because a mechanism's justification is usually a specific historical failure.

Shared notation: `N` vectors, `D` dimensions, `K` results, `S` shards, `RF` replication factor, `M` for HNSW neighbours or PQ subvectors (stated per use), `LSN` log sequence number.

---

## Part I — Search

The domain problem: given a query vector, find its nearest neighbours, fast.

| | | |
|---|---|---|
| 1 | [Vectors, embeddings, and similarity](01-vectors-and-similarity.md) | why semantic closeness becomes geometric closeness, and what cosine and L2 actually measure |
| 2 | [The search problem](02-the-search-problem.md) | the exact answer costs a full scan, so every practical index is a deliberate approximation |
| 3 | [Brute force and kernels](03-brute-force-and-kernels.md) | the only index with no recall loss, and why it is memory-bandwidth-bound rather than compute-bound |
| 4 | [Tree and hash indexes](04-tree-and-hash-indexes.md) | KD-trees and LSH dominated for twenty-five years and lost; how they fail is the fastest route to understanding what replaced them |
| 5 | [IVF and clustering](05-ivf-and-clustering.md) | cluster once, search a few clusters — structurally the same trade-off as choosing a shard count |
| 6 | [Quantization](06-quantization.md) | store fewer bits, lose distance fidelity, buy recall back by rescoring |
| 7 | [Graph indexes and HNSW](07-graph-indexes-hnsw.md) | walk downhill through a proximity graph; the only family that stays fast at high `D` |
| 8 | [Updates and deletes](08-updates-and-deletes.md) | why every production index tombstones instead of deleting, and why nobody has shipped a graph repair |
| 9 | [Choosing an index](09-choosing-an-index.md) | four real answers, decided by RAM budget and required recall rather than by dataset size |
| 10 | [Filtering and hybrid search](10-filtering-and-hybrid-search.md) | real queries have predicates and keywords; both break the clean nearest-neighbour abstraction |

## Part II — Durability

Making a single node's data survive the process dying.

| | | |
|---|---|---|
| 11 | [Storage engines](11-storage-engines.md) | immutable append-only segments, and why vector systems converge on that shape |
| 12 | [Write-ahead logging](12-write-ahead-logging.md) | fsync before apply; every data-loss bug in this domain is a system that acknowledged earlier |
| 13 | [Recovery and compaction](13-recovery-and-compaction.md) | recovery as a pure function of what is on disk, and what snapshots actually buy |
| 23 | [Concurrency: readers and writers](23-concurrency-readers-and-writers.md) | a scan holds the data 20× longer than a write, so the lock decides which one starves — and copy-on-write has a precondition a vector index violates |

## Part III — Distribution

Making the data span machines, and survive one of them failing.

| | | |
|---|---|---|
| 14 | [Sharding and partitioning](14-sharding-and-partitioning.md) | separating *which shard owns a key* from *which node hosts that shard* |
| 15 | [Replication](15-replication.md) | when a write may be called successful, and the conditions under which that promise silently fails |
| 16 | [Consistency models](16-consistency-models.md) | what a client is actually promised, and why per-system labels like CP and AP tell you nothing |
| 17 | [Distributed search](17-distributed-search.md) | why merging per-shard top-K is exact, why approximation breaks that, and why the slowest shard sets the latency |

## Part IV — Failure

What happens when a node dies, and what happens when you are wrong about that.

| | | |
|---|---|---|
| 18 | [Failure detection](18-failure-detection.md) | deciding a node is dead when you fundamentally cannot know, and containing the damage when wrong |
| 19 | [Consensus and Raft](19-consensus-and-raft.md) | the only mechanism that *eliminates* split brain, and why vector databases keep it off the data path |
| 20 | [Rebalancing](20-rebalancing.md) | moving a shard while it is still being written to, and the ordering that separates a pause from data loss |

## Part V — Knowing what your system does

| | | |
|---|---|---|
| 21 | [Observability](21-observability.md) | instrumenting for a failure you cannot reproduce, and the arithmetic that makes most latency dashboards understate the tail by half |
| 22 | [Benchmarking](22-benchmarking.md) | producing numbers that mean something, and the methodology bugs that made a widely used ANN benchmark under-report throughput by 20× |

---

## Reading orders

**If you are building one.** Follow parts II → III → IV in order. Each rung exists because the rung below it has a specific hole: a WAL fixes a crash losing acknowledged writes; replication fixes the disk being gone; failure detection notices the leader died and thereby *creates* split brain; epochs and leases bound it; consensus eliminates it. Skipping a rung means the next one is memorised rather than understood.

**If you want the search side.** Part I stands alone. Read 2, 3 and 9 first for the shape of the problem, then the index families that interest you.

**If you are preparing for a system design interview.** 12, 15, 16, 18 and 19 carry the most weight, and the "how real systems do it" sections are where the answers that distinguish candidates live.

**If you are chasing a performance problem.** 3 (memory-bandwidth-bound scans), 23 (concurrency), 21 (why your latency dashboard understates the tail) and 22 (why the number you are reading may be noise) are the four that stop you optimising the wrong thing.

**If you have thirty minutes.** Read 12 (write-ahead logging), 15 (replication), and 18 (failure detection). Those three contain the argument that everything else in parts III and IV is built on.

## The single most useful idea in the pack

Four mechanisms, in order, each fixing what the previous one leaves open:

1. **Heartbeats** *create* split brain — they promote on suspicion, and suspicion is a timeout gamble.
2. **Epochs** *bound* it — a stale leader's writes are refused by the receiver, so it can be alive and still harmless.
3. **Leases** *bound it in time* — a leader that cannot renew stops serving before its replacement starts.
4. **Consensus** *eliminates* it — a minority cannot commit.

Being able to say which of those four a given system implements, and therefore which failures it is still open to, is most of what "understanding distributed databases" means in practice.
