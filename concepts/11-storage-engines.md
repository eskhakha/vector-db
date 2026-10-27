# 11. Storage engines for vector data

> The storage engine decides what a write actually means, where the bytes live between writes, and how long a restart takes — and for vector data almost every serious system converges on the same shape: immutable append-only segments, because a vector is a large opaque blob that nobody ever range-scans.

## The problem

An index answers "which vectors are near this one." It does not answer where the bytes live, what survives a power cut, what happens to the 30% of a collection that has been deleted, or how long a process takes to serve queries after a restart. Those are storage-engine questions, and skipping them leaves you with a cache that has confident documentation.

Vector data puts three pressures on this layer that a relational storage engine was not built for.

**The record is large and opaque.** At `float32`, a 128-`D` vector is 512 bytes and a 1536-`D` vector is 6 KB, before metadata — against a typical B-tree key/value pair of tens of bytes. Prefix compression, key-order locality, packing many records per page: all buy far less when one record is most of a page.

**There is no range scan over a vector.** In a B-tree you sort by key because queries ask for ranges of that key. No query ever asks for "all vectors between `[0.1, 0.4, ...]` and `[0.2, 0.5, ...]`." The access patterns are point lookup by ID, and "give me the vectors the index told me to look at." The on-disk order of raw vectors is therefore free to be *arrival order*, the cheapest order there is. This single observation is why real vector engines separate **vector storage** (append-mostly, segment-organised) from **index** (a graph or partition structure built over it, persisted or rebuilt independently) [1][2].

**Writes arrive as a stream and deletes are cheap lies.** Embeddings are generated in bulk by an upstream pipeline; ingestion is a firehose of appends, not scattered in-place updates. And a delete cannot be executed against an [HNSW graph](07-graph-indexes-hnsw.md) without damaging it, so every system defers it — see [Updates and deletes](08-updates-and-deletes.md).

## How it works

### The four engine shapes

**In-memory map plus index.** A `map[id]*Vector` and an index over it. No durability, no recovery, no compaction, `O(1)` lookup, and the fastest thing you will ever build. Every other shape here is this one plus the machinery to survive a restart.

**Append-only immutable segments.** Writes go into a mutable buffer; when it crosses a threshold it is sealed, written to disk once, and never modified again. Reads consult the buffer plus every sealed segment. Deletes write a tombstone; space comes back only when segments merge. Recovery is "list the sealed segments, replay the log tail." This is the shape essentially every vector engine ships.

**LSM-tree.** The formal, sorted-key version of the same idea. The 1996 paper's motivation was explicit: LSM was designed for workloads where "index inserts are more common than finds," batching small writes in a memory-resident component (`C0`) and cascading them into disk-resident components (`C1`) via a rolling merge "reminiscent of merge sort," specifically to avoid the doubled random I/O a B-tree secondary index pays under a high insert rate [3].

The cost is compaction, and RocksDB documents it precisely. Leveled compaction sizes levels exponentially (a `max_bytes_for_level_multiplier` of 10 is typical); in the recommended dynamic-level-bytes mode, sizing backward from the actual last level "guarantee[s] 90% of data is stored in the last level, 9% data in the second last level," pinning **space amplification at roughly 11%** [4]. The price is stated just as plainly: leveled write amplification is "often larger than 10" [4] — more than ten physical bytes written per logical byte. That pair of numbers is the whole LSM bargain: >10× write amplification bought ~11% space amplification and bounded read amplification.

**B-tree.** In-place page updates, sorted keys, excellent point-and-range reads. It pays write amplification through page splits and — critically — makes torn pages a correctness problem, because a logical WAL record replayed onto a half-written page corrupts further (see [Write-ahead logging](12-write-ahead-logging.md)). Since vector keys are opaque IDs with no range semantics and the payload dwarfs the key, a B+tree over vector data buys nothing it is good at and pays everything it is bad at.

**The convergence.** Append-only segments are structurally LSM-shaped — memory buffer, seal, immutable file, background merge — with the sorted-key part removed because nothing needs it. Systems land here without necessarily calling it an LSM.

```
   write ──► mutable/growing segment (in memory, brute-force searchable)
                       │ threshold: size, count, or age
                       ▼
              sealed segment (immutable on disk)  ──► ANN index built over it
                       │
                       │ background merge: drop tombstoned rows,
                       │ combine graphs, reclaim space
                       ▼
              larger sealed segment
```

### Memory layout: arena versus per-vector allocation

A `[][]float32` of a million 768-`D` vectors is a million separate heap allocations. Each carries allocator overhead, each is a pointer the CPU must chase, and a scan touches a million cache-cold locations in an order the allocator chose. In a garbage-collected runtime every one of those pointers is an object the collector traces on every cycle, so GC pause time scales with vector *count*.

The fix is one contiguous arena — a single `[]float32` of `N × D`, vector `i` at offset `i × D`. One allocation, one pointer for the collector, and a linear scan the hardware prefetcher follows perfectly. It is often a several-fold difference on a brute-force scan for about ten lines of change, and it is a prerequisite for the SIMD kernels in [Brute force and kernels](03-brute-force-and-kernels.md). The cost is that deleting vector `i` leaves a hole mid-arena — which is exactly why arenas pair naturally with tombstones and compaction rather than in-place deletion.

### mmap: the strongest available case, and the strongest rebuttal

Memory-mapping a segment file lets the OS page cache do the caching. No buffer pool to write, no explicit load step, near-instant startup because pages fault in lazily. Qdrant treats this as first-class: vectors live in memory-mapped files, with a `cached` tier that pre-warms the page cache on startup "so the first request is fast," a `cold` tier that skips pre-warming for large collections on slower disks, and a `memmap_threshold` that converts a segment to mmap once it exceeds a size [1].

The case against is the CIDR 2022 paper by Crotty, Leis and Pavlo, which names four problems [5]:

1. **Transactional safety.** "The core issue is that, due to transparent paging, the OS can flush a dirty page to secondary storage at any time, irrespective of whether the writing transaction has committed." [5] This is a direct collision with the log-before-data rule: you cannot order writes you do not control.
2. **I/O stalls.** "Since the OS can transparently evict pages to secondary storage, read-only queries can unknowingly trigger blocking page faults if they attempt to access evicted pages." [5] A page fault is synchronous; the thread stops, and you cannot apply backpressure to something you cannot see.
3. **Error handling.** "Any code that interacts with mmap-backed memory can now produce a SIGBUS that the DBMS must deal with via cumbersome signal handlers." [5] An I/O error surfaces as a signal in the middle of arbitrary code, not as a returned error at a call site.
4. **Page-eviction scaling.** "The OS's page eviction mechanisms cannot scale beyond a few threads for larger-than-memory DBMS workloads on high-bandwidth secondary storage devices." [5]

Their conclusion is unhedged: "We believe that mmap adds too much complexity with no commensurate performance benefit and strongly urge DBMS developers to avoid using mmap as a replacement for a traditional buffer pool" [5]. The paper's landing page states the same in one line: "MMAP is not a suitable replacement for a traditional buffer pool" [6].

The counter-position is LMDB, whose author argues in a direct rebuttal that a fully mmap-based engine addresses every objection while achieving "minimal complexity and superior performance" — i.e. that the paper's problems are implementation footguns rather than properties of mmap [7]. Both sides have a working system behind them. The honest reading is narrower than either: mmap is defensible for **read-mostly data that is immutable once written**, which is exactly the sealed-segment case Qdrant uses it for, and indefensible as a substitute for a buffer pool under a mutating transactional workload.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| Segment seal threshold | Size/count at which a growing segment becomes immutable | Milvus: ~75% of allocated capacity, ~512 MB [2][8] | Too small: segment explosion, per-query fan-out cost. Too large: long index-build stall, slow recovery |
| Segment merge policy | When sealed segments combine | Background, size-triggered | Too lazy: deleted rows stay resident and dead space grows. Too eager: constant I/O competing with queries |
| Level multiplier (LSM) | Size ratio between levels | 10 [4] | Smaller: less write amplification, more read amplification. Larger: the inverse |
| `memmap_threshold` | Segment size above which storage becomes mmap-backed | Qdrant-specific [1] | Too low: page-fault stalls on hot data. Too high: RAM exhaustion |
| `indexing_threshold` | Point count at which a segment gets a real ANN index | Qdrant-specific [9] | Too low: constant index rebuilds. Too high: large brute-force scans on unindexed segments |
| Arena vs per-vector allocation | Memory layout of the vector block | Arena | Per-vector: pointer chasing, GC pressure scaling with `N` |
| Segment count target | Steady-state search speed vs recovery window | Workload-dependent | Qdrant's own framing: "fewer, larger segments give the fastest steady-state search but the longest recovery window" [9] |

## How real systems do it

The interesting divergence is not "which tree" — it is **who owns durability**. There are three live answers in production, and the differences are architectural, not cosmetic.

### (a) Local WAL on local disk — Qdrant, Weaviate, Postgres/pgvector

Every write is appended and fsynced to a log on *this node's* disk before the client is acknowledged. Cross-node durability comes afterwards, via replication.

Qdrant runs a **per-shard WAL** that "orders all operations and assigns them a sequential number" before anything touches a segment, and states the boundary plainly: "once a change has been added to the WAL, it will not be lost even if a power loss occurs" [1]. Segments come in `appendable` and `non-appendable` flavours — once indexed, a segment accepts only reads and deletes [1] — which is append/seal/merge under different vocabulary.

Weaviate wrote **its own LSM key-value store in Go**: writes land in a memtable, the sorted memtable flushes to an immutable disk segment at a size threshold, reads check the memtable then walk older segments using Bloom filters, and a background process merges small segments into larger ones [10]. Its HNSW index has a *second*, separate durability mechanism — an append-only commit log per shard — and as of v1.39 automatic snapshots mean that on start it "loads the most recent snapshot and replays only the commit log entries written after it" [10].

pgvector inherits the whole Postgres stack — WAL, crash recovery, streaming replication, MVCC — for free, which is the entire "just use Postgres" argument. The bill arrives at index maintenance: an HNSW index over 1M rows "can be 8GB or larger," and a build over >1M rows takes around 6 minutes even for a simple configuration [11]; at 10M rows of 1536-`D` vectors builds run for hours on a single core [12]. And there is no native horizontal scaling: "vertical scaling is the primary lever; native sharding of vector data across nodes is not supported" [12].

**Trade-off:** lowest write latency of the three (a local fsync, not a network hop). Cost: durable replicated disks on every node, and — per the public issue record — the hard part is not the WAL, it is the rejoin path after a node comes back. See [Replication](15-replication.md) and [Recovery](13-recovery-and-compaction.md).

### (b) Externalised log — Milvus, and its partial reversal

Milvus made the most radical choice: **it has no local WAL at all**. A message queue — Pulsar or Kafka — sits on the write path as the system of record. The Manu paper states the design as "log as data": the system is structured "as a group of log publish/subscribe microservices," writers publish to the log, and every read-side component (data, query and index nodes) is an independent subscriber that "do[es] not need to coordinate" with the others [8]. Compute nodes are stateless. Data nodes consume from the log, buffer into growing segments, and flush to columnar binlog files in object storage [2].

Crucially, Milvus's own **replicas are read-scaling caches, not a durability mechanism**. Loading the same segment on multiple query nodes is explicitly framed as a way to "leverage extra CPU and memory resources" and increase read throughput; the authoritative copy always remains object storage, and a query-node failure just redirects to another node that already has the segment resident [13]. Durability replication is delegated entirely to the log service and the object store's redundancy.

Then came the reversal. Milvus's own engineering blog documents replacing Kafka/Pulsar with **Woodpecker**, a purpose-built log service, because the brokers "required dedicated machines with multiple nodes and careful resource management," imposed topic-count limits that Milvus worked around with an added "VShard" layer which itself "hindered features like log garbage collection," and burned resources persisting ephemeral heartbeat messages that never needed durability [14]. Operating an extra distributed system as "just the WAL" turned out to be too heavy.

Woodpecker's published numbers are the clearest statement of the object-storage tax anywhere in this corpus: **local filesystem mode reaches 450 MB/s at 1.8 ms latency; S3-backed mode reaches 750 MB/s at 166 ms latency** [14]. Two orders of magnitude of per-write latency, bought with throughput and the removal of an entire subsystem. Woodpecker also offers a three-replica "QuorumBuffer" quorum-write mode to claw latency back for mission-critical workloads — a small consensus protocol bolted onto the log layer [14].

**Trade-off:** genuine stateless compute and a clean separation of durability from serving. Cost: you now operate a distributed log, and ack-in-log is separated in time from flushed-to-storage — a gap that shows up in [Recovery](13-recovery-and-compaction.md) as a recurring class of restart bug.

### (c) Object storage as the log — turbopuffer, LanceDB

The newest generation removes local disk from the durability path entirely. turbopuffer: "every write adds a new file to the WAL directory inside the namespace's prefix. If a write returns successfully, data is guaranteed to be durably written to object storage" [15]. The WAL still exists conceptually; its medium is S3. Concurrent writes to a namespace are batched by group commit into a single entry, committed at roughly one per second per namespace, with indexing asynchronous afterwards [15]. LanceDB reaches the same place differently: immutable fragment writes plus a versioned manifest rather than a log, with concurrency safety resting on S3's native conditional writes, so "LanceDB handles concurrent writers against the same table out-of-the-box — no external commit coordinator is required" [16].

The price is published: **p50 of roughly 165–200 ms to durably commit a write** [15][17], against sub-millisecond-to-low-single-digit-ms for a local fsync. It is traded deliberately, not apologetically — turbopuffer describes its target as "high write throughput, no transactions, and more relaxed latency requirements, especially for writes," while holding a hard sub-100 ms target for search queries [18], and claims 10,000+ vectors/sec ingest [15]. Reads split into cold (p50 ≈ 874 ms for 1M documents, straight from object storage) and warm (p50 ≈ 14 ms from a node-local cache), with queries consistent-hashed to one node per namespace precisely to maximise cache locality [15][17]. The economics are stated outright: memory at "$2+/GB" versus object storage at "$0.02/GB" is why the architecture exists [17].

It is not a free lunch. An independent builder implementing WAL-on-S3 with conditional writes hit a **"zombie WAL" race**, where naively merging local state with remote state on a conflicting metadata update could resurrect an already-deleted WAL file; the fix was to stop merging cached local state at all and always re-fetch fresh remote state and re-apply deltas [19]. That generalises well beyond object storage: local caching plus optimistic merge is a correctness hazard; re-deriving from the source of truth is the safe shape.

### Side by side

| | (a) Local WAL | (b) External log | (c) Object storage |
|---|---|---|---|
| Systems | Qdrant, Weaviate, pgvector [1][10][11] | Milvus (Kafka/Pulsar) [8][14] | turbopuffer, LanceDB, Woodpecker-S3 [14][15][16] |
| Durability point | fsync to local disk | Ack from log service | Object PUT returns |
| Write p50 | sub-ms to low ms | 1.8 ms local-disk Woodpecker [14] | 165–200 ms [15][17] |
| Replicas are | durability + availability | read-scaling caches [13] | irrelevant; storage is the copy |
| Failover means | replica catch-up and reconciliation | resubscribe to the log | start a node, point at the prefix |
| Hard part | rejoin/reconciliation correctness | operating the log; ack-vs-flush gap [14] | metadata races; write latency [19] |
| Cost driver | replicated SSD per node | the broker cluster | object-store request cost |

## Complexity and cost

Storage per vector is `D × 4` bytes at `float32`, plus the ID map, plus index overhead — and the index overhead is not a rounding error: 8 GB for 1M rows of embeddings is a documented pgvector figure [11], well above the raw vectors alone. Compaction is the cost people forget. A leveled LSM writes each logical byte more than ten times over its lifetime [4]; a segment merge in a vector store rewrites every surviving vector *and* rebuilds their graph structure. Read cost scales with segment count, not only with `N`: every unmerged segment is another structure a query must consult.

## Failure modes

- **Segment explosion.** Seal threshold too low, merges behind ingest, every query fanning out across hundreds of structures. Latency degrades smoothly and confusingly.
- **Dead space that never comes back.** Tombstones accumulate; nothing reclaims until a merge; disk grows on a collection whose logical size is flat.
- **GC pause scaling with vector count.** Per-vector allocation in a managed runtime makes pause time a function of `N`.
- **mmap page-fault stalls under memory pressure.** The query thread blocks on I/O neither the profiler nor the query planner can see [5].
- **Index build blocking ingest.** Sealing triggers a build; if it runs on the write path, ingest stops for minutes.
- **Restart longer than the availability budget.** Recovery time is a design output, not an accident — Qdrant frames segment size explicitly as steady-state search speed traded against recovery window [9].

## How to test it

- **Round-trip property test.** Write `n` random vectors with metadata, seal, restart from disk, read every ID back, assert byte-exact equality. Run at `n` = 0, 1, and one past the seal threshold.
- **Kill during seal.** `SIGKILL` mid-segment-write, restart, assert the partial segment is discarded and the data recovers from the log. Repeat at randomised offsets.
- **Space reclamation.** Insert `N`, delete half, assert on-disk bytes do *not* drop; force a merge; assert they do. If nothing fails before the merge, you implemented deletion, not tombstones — and that has different bugs.
- **Segment-count sweep.** Query latency at 1, 10, 100 segments with total `N` fixed. That curve is the read-amplification tax made visible.
- **Layout benchmark.** The same brute-force scan over `[][]float32` and over one arena. Report both numbers.

## Minimal demo version

**An in-memory map, a local WAL, and a full snapshot.** That is the whole engine, and it is the right one.

```
map[uint64]int          id -> arena slot
[]float32               contiguous arena, N × D
[]byte or bitset        tombstones
wal.log                 append-only, checksummed  (see 12)
snapshot.bin            full state, atomically renamed (see 13)
```

Write path: append to WAL, fsync, apply to the map and arena, return. Restart: load the newest snapshot, replay the WAL tail. Segmentation can be a single "seal everything on snapshot" operation. Roughly 300 lines including the WAL.

**Say it plainly: building an LSM tree here teaches you LSM trees, not vector databases.** It is the classic premature-complexity trap in this project. The sorted-key machinery — comparators, SSTable block indexes, Bloom filters, leveled compaction policy, tiered-vs-leveled tuning — exists to serve range scans over sorted keys, and there are no range scans over vectors. You will spend weeks on it and learn nothing about vector storage that the segment model does not teach in an afternoon. If you want the LSM lesson, read RocksDB's own numbers: >10× write amplification for ~11% space amplification [4] is the entire idea, and you now have it.

**What can be cut without losing the lesson:** compaction policy (one merge trigger is enough), Bloom filters (no sorted keys to skip), mmap (an in-memory arena teaches the layout lesson better), columnar encoding, multi-level anything.

**What cannot be cut without the demo becoming a lie:** tombstones with deferred reclamation — if delete frees memory immediately you have hidden the single most important storage property of vector systems; the contiguous arena, because per-vector allocation misrepresents scan cost by a large factor; and a restart path that actually reads from disk, because an engine that only recovers from its own live memory is not a storage engine.

**For one extra hour:** implement segment sealing, insert enough to produce 20 segments, and plot query latency against segment count. That graph is why compaction exists.

## Open questions and contested claims

- **mmap.** Crotty/Leis/Pavlo urge DBMS developers to avoid it outright [5][6]; LMDB's author argues every objection is an implementation artefact and that a well-built mmap engine has "minimal complexity and superior performance" [7]. Both ship. The narrow case — read-mostly, immutable-once-written segments — is where both camps' evidence overlaps, and it is exactly what Qdrant does [1].
- **Is a WAL even necessary?** Phil Eaton argues no: "A database does not need a write-ahead log (WAL) to achieve durability. A database can write its long-term data structure durably to disk before returning to a client" [20]. Nobody does, purely on I/O cost — "it's cheaper to write only these small messages representing the client request to disk" [20]. For a vector store the argument has extra edge, because an ANN index is reconstructible from raw vectors: log only vectors, rebuild on recovery, trade recovery time for a smaller log. Whether that trade is good depends entirely on build time; see [Recovery](13-recovery-and-compaction.md).
- **Whether object storage becomes the default.** Milvus moved *away* from external brokers and *toward* object storage in the same redesign [14]; turbopuffer and LanceDB started there [15][16]. The 166 ms write latency is the open question — fine for a batch-embedded corpus, disqualifying for anything user-interactive on the write path.

## Sources

[1] Qdrant — "Storage" — https://qdrant.tech/documentation/concepts/storage/ — accessed 2026
[2] Milvus/Zilliz — "Data Insertion and Data Persistence in a Vector Database" (Deep Dive 4) — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md — accessed 2026
[3] O'Neil, P., Cheng, E., Gawlick, D., & O'Neil, E. — "The Log-Structured Merge-Tree (LSM-Tree)" — Acta Informatica 33(4) — https://www.cs.umb.edu/~poneil/lsmtree.pdf — 1996
[4] RocksDB team (Meta) — "Leveled Compaction" (RocksDB wiki) — https://github.com/facebook/rocksdb/wiki/Leveled-Compaction — accessed 2026
[5] Crotty, A., Leis, V., & Pavlo, A. — "Are You Sure You Want to Use MMAP in Your DBMS?" — CIDR 2022 — https://www.pdl.cmu.edu/ftp/Database/p13-crotty.pdf — 2022
[6] CMU Database Group — "Are You Sure You Want to Use MMAP in Your Database Management System?" (paper page) — https://db.cs.cmu.edu/mmap-cidr2022/ — 2022
[7] Symas (Howard Chu) — "Are You Sure You Want to Use MMAP in Your DBMS?" (rebuttal) — https://www.symas.com/post/are-you-sure-you-want-to-use-mmap-in-your-dbms — accessed 2026
[8] Guo, R. et al. (Zilliz) — "Manu: A Cloud Native Vector Database Management System" — VLDB 2022 — https://arxiv.org/pdf/2206.13843 — 2022
[9] Qdrant — "Configure Qdrant's Optimizer for Predictable Search Latency" — https://qdrant.tech/articles/tuning-qdrant-optimizer/ — accessed 2026
[10] Weaviate — "Storage" — https://docs.weaviate.io/weaviate/concepts/storage — accessed 2026
[11] Crunchy Data — "HNSW Indexes with Postgres and pgvector" — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector — accessed 2026
[12] ParadeDB — "pgvector Limitations" — https://www.paradedb.com/learn/postgresql/pgvector-limitations — accessed 2026
[13] Milvus — "In-Memory Replica" — https://milvus.io/docs/v2.5.x/replica.md — accessed 2026
[14] Milvus/Zilliz — "We Replaced Kafka/Pulsar with a Woodpecker for Milvus" — https://milvus.io/blog/we-replaced-kafka-pulsar-with-a-woodpecker-for-milvus.md — accessed 2026
[15] turbopuffer — "Architecture" — https://turbopuffer.com/docs/architecture — accessed 2026
[16] LanceDB — "Storage Architecture in LanceDB" — https://docs.lancedb.com/storage — accessed 2026
[17] turbopuffer — "turbopuffer: fast search on object storage" — https://turbopuffer.com/blog/turbopuffer — accessed 2026
[18] turbopuffer — "Tradeoffs" — https://turbopuffer.com/docs/tradeoffs — accessed 2026
[19] Janthe, K. — "What I learned building a vector database on object storage" — https://blog.karanjanthe.me/posts/vecpuff/ — accessed 2026
[20] Eaton, P. — "A write-ahead log is not a universal part of durability" — https://notes.eatonphil.com/2024-07-01-a-write-ahead-log-is-not-a-universal-part-of-durability.html — 2024
