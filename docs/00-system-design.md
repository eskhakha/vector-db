# vecdb — Phase 0: System Design Specification

**Status:** Draft for approval. No implementation code has been written.
**Scope:** The whole system, end to end. Phase 1 begins only after you approve or amend this.
**Audience:** You. Written to be argued with, not admired.

---

## 0. How to read this document

Sections 1–5 fix the contract (what we promise, what we assume, what we refuse to promise).
Sections 6–12 fix the mechanisms (sharding, replication, WAL, storage, index, query, failure handling).
Sections 13–16 cover what comes later (Raft, rebalancing, observability, performance).
Section 17–19 cover testing, repo layout, and the milestone roadmap.
Section 20 lists the decisions **I need from you** before Phase 1.
Section 21 is the honest list of what this system will never do.

Because you said you're learning Go alongside the systems material, there is a **Go Notes** box wherever a design decision has a specific idiomatic-Go consequence. Appendix A collects the Go idioms this project depends on.

A design doc that hides its own weaknesses is worthless, so every major section ends with **Weaknesses**.

---

## 1. Requirements

### 1.1 Functional requirements (must have)

| # | Requirement | Notes |
|---|---|---|
| F1 | `Insert(id, vector, metadata)` | Creates or overwrites. Upsert semantics. |
| F2 | `Get(id)` | Returns vector + metadata + version, or not-found. |
| F3 | `Delete(id)` | Logical delete (tombstone), later reclaimed. |
| F4 | `Search(query_vector, k, filter?)` | Returns global top-K by similarity. |
| F5 | Metadata storage | Small key→value map per vector; filterable later. |
| F6 | Durability across process restart | An acknowledged write survives `kill -9` + restart. |
| F7 | Sharding | Vectors partitioned deterministically across shards. |
| F8 | Replication | Each shard has RF replicas on distinct nodes. |
| F9 | Distributed query | Scatter to shards, gather, merge to global top-K. |
| F10 | Request routing | Client can hit any node; that node coordinates. |
| F11 | Failure detection | Dead nodes detected and removed from routing. |
| F12 | Replica recovery | A restarted/stale replica catches up automatically. |
| F13 | Crash recovery | Node rebuilds exact state from snapshot + WAL. |

### 1.2 Non-functional requirements (must have)

| # | Requirement | Concrete target |
|---|---|---|
| N1 | **Durability** | No acknowledged write is lost given: single node crash, or the loss of any one node in the cluster. Not: simultaneous loss of all replicas of a shard. |
| N2 | **Consistency** | Per-vector-ID linearizability for writes and strong reads (see §3). Search is *not* snapshot-consistent across shards. |
| N3 | **Availability** | Reads and writes for a shard survive the loss of one of its replicas, after failover completes. Bounded unavailability window during failover is acceptable and must be measured. |
| N4 | **Latency** | Targets to *measure*, not to claim: `Get` p99 < 5 ms locally; `Search` p99 to be established by benchmark at 10K/100K/1M. No number here is a promise until §16 measures it. |
| N5 | **Fault tolerance** | The failures in §4 are handled with defined, tested behavior. |
| N6 | **Recoverability** | Recovery time from WAL is measured and bounded by snapshot frequency. |
| N7 | **Observability** | Every request has a request ID; metrics in §15 are exported; logs are structured. |
| N8 | **Testability** | Every layer has unit tests; the cluster has integration + fault-injection tests. `go test -race ./...` is green. |
| N9 | **Operability** | `docker compose up` produces a working 3-node cluster with no manual steps. |

### 1.3 Explicitly *nice to have* (not requirements)

These are deliberately deferred. If a design choice makes one of them impossible forever, that's a bug in the design; if it merely makes one of them harder, that's acceptable.

- Metadata **filtered** search (pre-filter or post-filter) — design must leave room, implementation deferred.
- Hybrid / sparse-dense search.
- Multi-tenancy, collections/namespaces.
- Authentication, authorization, TLS.
- Multi-datacenter replication.
- Online schema/dimension change.
- Compression / quantization (PQ, scalar quantization).
- Backup & restore tooling.
- A web UI.

### 1.4 Anti-requirements

We are **not** building a general database. No SQL, no joins, no multi-key transactions, no secondary indexes beyond the vector index, no cross-shard atomicity.

---

## 2. Explicit assumptions

Every assumption is labeled:
**[Given]** = from your brief. **[Derived]** = follows from a Given. **[Arbitrary]** = I picked it; challenge it freely.

| # | Assumption | Kind | Value / statement |
|---|---|---|---|
| A1 | Vector dimensionality | [Given] | 128–768, fixed per collection at cluster start. Not changeable at runtime. |
| A2 | Element type | [Arbitrary] | `float32`. Rationale: half the memory of float64, and the precision loss is far below embedding noise. |
| A3 | Dataset size | [Given] | Up to ~1M vectors total. |
| A4 | Nodes | [Given] | 3, all on one physical machine via Docker Compose. |
| A5 | Logical shards | [Given] | 4. **I want to change this — see §6.4.** |
| A6 | Replication factor | [Given] | 2 pre-Raft. **Must become 3 at the Raft milestone — see §13.1.** |
| A7 | Expected QPS | [Arbitrary] | Design point: 1,000 writes/s and 100 searches/s per node. Chosen so single-threaded brute force is plausibly the bottleneck, which is pedagogically the interesting case. |
| A8 | Network model | [Arbitrary] | Asynchronous, unreliable: messages may be delayed, reordered across connections, or dropped. No bound on message delay. We do **not** assume synchrony for correctness; we assume it only for *liveness* (failure detection uses timeouts, so a slow node may be wrongly suspected — this is inherent, see §12). |
| A9 | Failure model | [Arbitrary] | Crash-stop and crash-recovery. Fail-fast processes. **Non-Byzantine**: nodes never lie, never send corrupted-but-well-formed data, never act maliciously. |
| A10 | Storage model | [Arbitrary] | Local POSIX filesystem, one directory per node. `fsync` is honest (the disk actually persists). Docker volumes. Disk does not silently corrupt data — but we still checksum the WAL, because *torn writes* (partial writes at crash) are real and are not corruption. |
| A11 | Clocks | [Arbitrary] | Wall clocks are **not** trusted for ordering or correctness. Used only for metrics, logs, and human-readable timestamps. Ordering comes from monotonically increasing sequence numbers assigned by a shard leader. |
| A12 | Consistency requirement | [Arbitrary] | See §3. Strong per key, eventual across shards for search. |
| A13 | Query pattern | [Arbitrary] | Read-heavy on search, write pattern is bulk-load then incremental. No range scans. No "list all". |
| A14 | Metadata size | [Arbitrary] | ≤ 1 KB per vector, flat `map[string]string`. Keeps the payload dominated by the vector. |
| A15 | Client model | [Arbitrary] | Clients are trusted, in-cluster, and use gRPC. No rate limiting, no quotas. |
| A16 | Single writer per ID | [Arbitrary] | Concurrent writes to the *same* ID are allowed but the winner is "last write to reach the leader", not "highest client timestamp". No conflict resolution logic (no LWW on wall clock, no vector clocks) is needed, because a single leader per shard totally orders writes to that key. |
| A17 | Development host | [Observed] | Windows. **See §18.4** — this has real consequences for `fsync`/rename semantics; I recommend all durability testing runs inside Linux containers. |

### Memory budget check (this is the assumption that bites first)

```
1M vectors × 128 dims × 4 B = 512 MB   (raw vectors only)
1M vectors × 768 dims × 4 B = 3.07 GB  (raw vectors only)
```

Plus per-vector overhead: ID string (~32 B), metadata map (~200 B realistic), Go map bucket + pointer overhead (~100 B), version/timestamps (24 B) ≈ **~350 B/vector ≈ 350 MB at 1M**.

With S=4 shards and RF=2, there are 8 shard-replicas spread over 3 nodes → each node holds ~2.67 shards ≈ 0.67 of the dataset:

| dim | dataset in RAM | per node (RF=2, 3 nodes) |
|---|---|---|
| 128 | ~860 MB | ~575 MB |
| 768 | ~3.4 GB | ~2.3 GB |

**Conclusion:** 1M × 768 across three containers on one laptop needs ~7 GB of RAM just for the vectors. That is the practical ceiling.
**Recommendation:** benchmark the full ladder (10K/100K/1M) at **d=128**, and benchmark d=768 only up to 100K. Label all headline numbers with the dimension. I'll bake this into §16.

**Weaknesses of §2:** A7 (QPS) is a guess with no workload behind it. A10 (honest fsync) is false on many consumer SSDs with write caches; we accept this because we're not shipping to production.

---

## 3. Consistency model

### 3.1 The four candidates

**Strong consistency (linearizability).** Every operation appears to take effect at a single instant between its invocation and response; all clients see one total order. *Cost:* every write needs a quorum round-trip and a fenced leader; reads must go to the leader or a lease-holder. *Value here:* it's the model that forces you to actually understand leaders, fencing, and quorums. This is the whole point of the project.

**Eventual consistency.** Replicas converge if writes stop. *Cost:* you need conflict resolution (LWW with clock skew hazards, or CRDTs, or vector clocks) and clients can read arbitrarily stale data. *Value here:* teaches anti-entropy and read repair, but lets you avoid ever confronting split-brain properly — which is the most valuable thing to confront.

**Read-after-write (read-your-writes).** A client that wrote X will not subsequently read a version older than X. Other clients may lag. *Cost:* session tracking (a token carrying the last-seen sequence number). *Value here:* a genuinely useful middle ground, and cheap to add once you have sequence numbers, which you need anyway.

**Session consistency.** Read-your-writes + monotonic reads + monotonic writes, scoped to one client session. Superset of the above.

### 3.2 The choice

> **Per-key linearizability, with an explicit per-request consistency knob, and explicitly *no* cross-shard consistency.**

Precisely, the client-visible guarantees:

**G1 — Write linearizability per vector ID.** All writes to a given ID are totally ordered by that ID's shard leader and assigned strictly increasing sequence numbers. Once a write is acknowledged, it is durable on the leader and on all in-sync replicas (see §7). There is exactly one order; no conflicts, no merges, no last-writer-wins guesswork.

**G2 — Strong reads.** `Get(id, consistency=STRONG)` is served by the shard leader from committed state. Combined with G1: if `Insert` returned OK at time *t*, every `STRONG` `Get` starting after *t* observes that version or a newer one. This gives read-your-writes for free, and monotonic reads for free.

**G3 — Bounded-staleness reads (opt-in).** `Get(id, consistency=EVENTUAL)` may be served by any replica and may be stale. The response carries the replica's `applied_lsn`, so the client can *observe* how stale. Not enabled by default.

**G4 — Search is eventually consistent and not atomic across shards.** A `Search` fans out to S shards. Each shard answers from its own committed state at its own instant. There is no cross-shard snapshot. Therefore:
- A vector inserted "just now" may or may not appear.
- Two concurrent searches may disagree.
- A search may observe shard 0 at a state *after* a write and shard 1 at a state *before* it.

This is not a defect being papered over. Elasticsearch, Milvus and Qdrant all behave this way, because a cross-shard consistent snapshot requires either a global clock (Spanner/TrueTime), a global sequencer (a scalability bottleneck), or MVCC with a distributed timestamp oracle. **Say it in the docs, don't hide it.**

**G5 — No cross-key atomicity.** Two inserts are two independent operations even in the same request. Partial application is possible and visible.

**G6 — No guarantee for un-acknowledged writes.** If a write times out or errors, its outcome is *undefined*: it may have been applied, may not have been. Clients must retry, and retries must be **idempotent**. Since `Insert` is an upsert keyed by ID, retrying is safe by construction. This is why I chose upsert semantics over insert-or-fail — see §5.

### 3.3 Why this is the right choice for *learning*

Eventual consistency lets you skip the hard parts. Strong per-key consistency forces you to answer, with code:

- Who is allowed to accept a write? (leader)
- How does a leader know it's still the leader? (fencing / epoch — §12.4)
- What happens if two nodes both think they lead shard 2? (split-brain — §13.2)
- When is a write "committed"? (§7.3)
- Can a promoted replica have lost committed writes? (§7.5 — the answer pre-Raft is *yes*, and confronting that is what motivates Raft)

That last question is the pedagogical spine of the whole project.

**Weaknesses of §3:** Pre-Raft, G1 and G2 are only *as strong as the failover mechanism*. §12.4 explains the residual split-brain window and §13 explains why only consensus closes it. Being honest about this now prevents you from believing a false guarantee for eight milestones.

---

## 4. Failure model

### 4.1 Handled

| Failure | Definition | Behavior (detail in §12) |
|---|---|---|
| **Process crash** | `kill -9`, panic, OOM. | Restart → recover from snapshot + WAL replay. No acknowledged write lost. |
| **Node crash** | Whole container dies. | Failure detector marks node dead after `T_fail`; shards it led are failed over; shards it followed become under-replicated. |
| **Replica crash** | A follower dies. | Leader marks it out-of-sync, keeps serving (see §7.4 for the availability/durability trade-off this forces). |
| **Network timeout** | RPC exceeds deadline. | Caller cancels via `context`, records the failure, retries on another replica if the op is idempotent. |
| **Network partition** | Node group A cannot reach group B. | Pre-Raft: detected as node failure; **split-brain is possible** and is documented, fenced by epochs, not eliminated. Post-Raft: minority side cannot commit; correctness preserved, availability lost on the minority side. |
| **Slow node** | Alive but exceeding latency budget. | Search: excluded via per-shard deadline; result flagged partial. Writes: leader marks a slow follower out-of-sync after `T_sync`. Explicitly: **we cannot distinguish "slow" from "dead"** (this is the FLP-flavoured reality, A8). |
| **Restarted node** | Node returns with stale on-disk state. | Rejoins as follower, catches up (§9): WAL-tail catch-up if the gap is small, snapshot transfer if not. |
| **Lost connection** | TCP drops, node alive. | gRPC reconnects with backoff; in-flight RPCs fail with `Unavailable` and are retried. |
| **Stale replica** | Follower's `applied_lsn` far behind leader. | Excluded from `EVENTUAL` reads beyond a staleness bound; excluded from the in-sync set; triggers catch-up. |
| **Partial write to disk** | Crash mid-`write()`; last WAL record truncated. | WAL reader detects short/invalid record, truncates the file to the last good offset, continues. §8.5. |
| **Corrupted WAL entry** | Bit flip inside a record. | CRC32C mismatch → treated as end-of-log, file truncated there. Data after the corruption is *discarded, not skipped* — skipping would create a hole and break LSN contiguity. §8.5. |
| **Duplicate / retried RPC** | Client retries a write that actually succeeded. | Idempotent by construction (upsert by ID). Search dedupes by `(shard, request_id)`. |

### 4.2 NOT handled (explicit non-goals)

- **Byzantine faults.** Nodes are trusted. No signatures, no verification of peer claims.
- **Silent disk corruption of the snapshot / data files.** Only the WAL is checksummed in v1. (Adding snapshot checksums is a 20-line improvement; listed in §21.)
- **Correlated failure of all replicas of a shard.** RF=2 and both replicas die → that shard is unavailable. Data is not lost (it's on disk), but service is.
- **Disk full.** We will detect the write error and fail the request, but there is no reserve-space strategy, no read-only degraded mode.
- **Clock skew / clock jumps.** Correctness never depends on wall clocks (A11), so this is handled by construction, not by mechanism.
- **Malicious or buggy clients.** No input fuzzing hardening beyond basic validation (dimension check, size limits).
- **Data loss from operator error.** No backups, no PITR.
- **Cascading overload.** No admission control, no circuit breakers, no load shedding. A thundering herd will simply make everything slow.

---

## 5. Data model

### 5.1 The stored record

```go
// package core

// Vector is the unit of storage. One per ID per shard.
type Vector struct {
    ID        string            // 1
    Embedding []float32         // 2
    Norm      float32           // 3
    Metadata  map[string]string // 4
    Version   uint64            // 5
    Deleted   bool              // 6
    CreatedAt int64             // 7
    UpdatedAt int64             // 8
}
```

**1. `ID string`** — client-supplied, opaque, the only thing the partition function sees. String (not int) because real embeddings are keyed by document IDs, UUIDs, URLs. *Constraint:* non-empty, ≤ 256 bytes, no interpretation of structure.

**2. `Embedding []float32`** — the vector. Length must equal the cluster's configured dimension; a mismatch is a client error, not a silent pad/truncate.

**3. `Norm float32`** — the precomputed L2 norm ‖v‖. This is the single most valuable denormalization in the system. Cosine similarity is `dot(q,v) / (‖q‖·‖v‖)`; ‖q‖ is constant across a query so it can be factored out of the ranking, and ‖v‖ precomputed at insert turns the inner loop into one fused multiply-add pass with a single division at the end. Storing the norm rather than storing a pre-normalized vector means we keep the original vector losslessly and can still serve Euclidean distance.

**4. `Metadata map[string]string`** — flat, ≤1 KB (A14). Flat rather than nested JSON because filtering on flat equality is trivial and nested filtering is a query-language rabbit hole we're not entering.

**5. `Version uint64`** — the leader-assigned sequence number of the write that produced this record. It is **the shard's LSN**, not a per-key counter. Consequences: versions are globally ordered within a shard, comparable across keys, and directly usable as a replication cursor. This one decision removes the need for separate "version" and "LSN" concepts.

**6. `Deleted bool`** — tombstone flag. A delete is a *write* of a tombstone, not a removal, because (a) the WAL must record it so replay reproduces it, (b) a follower must learn about it, (c) removing immediately would let a replayed older insert resurrect the key. Tombstones are reclaimed at snapshot time (§9.4).

**7/8. `CreatedAt`, `UpdatedAt int64`** — Unix nanoseconds, **observability only**. Never used for ordering or conflict resolution (A11). I nearly cut them; they stay because "when did this row last change" is the first question you ask when debugging replication lag, and it costs 16 bytes.

**Rejected fields:**
- `ShardID` — derivable from `ID` via the partition function. Storing it means it can disagree with the function, which is a bug waiting to happen. Derive, don't store.
- `Checksum` per vector — the WAL record already has one; a second one at the record level is redundant until we add durable segment files.
- Wall-clock `Timestamp` for LWW — not needed (A16), and actively harmful (clock skew).

### 5.2 Types that are *not* stored with the vector

```go
// ShardID identifies a logical partition. 0..NumShards-1.
type ShardID uint32

// LSN is a log sequence number: a shard-local, strictly increasing,
// gapless counter assigned by that shard's leader.
type LSN uint64

// Epoch fences a leadership term for a shard. Increments on every
// leadership change. A message stamped with a stale epoch is rejected.
type Epoch uint64

// ReplicaState is a follower's status as tracked by the leader.
type ReplicaState int
const (
    ReplicaUnknown  ReplicaState = iota
    ReplicaCatchup               // receiving snapshot or replaying WAL tail
    ReplicaInSync                // applied_lsn >= commit_lsn - tolerance
    ReplicaLagging               // alive but behind the tolerance
    ReplicaDead                  // failure detector gave up
)
```

**Why `Epoch` exists from day one:** without a fencing token, a leader that was declared dead (because it was merely slow or partitioned) can wake up and keep accepting writes. Every replication RPC carries `(shard, epoch, lsn)`. A follower rejects any append whose epoch is lower than the highest it has seen. This does not *prevent* split-brain — two leaders can still both accept client writes for a window — but it prevents the stale leader from *corrupting the follower's log*. That distinction matters and is exactly what §13 will improve on.

### 5.3 Wire format

Protobuf over gRPC. Sketch (final `.proto` lands in M0/M4):

```protobuf
message VectorRecord {
  string id = 1;
  repeated float embedding = 2 [packed = true];
  float norm = 3;
  map<string, string> metadata = 4;
  uint64 version = 5;
  bool deleted = 6;
  int64 created_at = 7;
  int64 updated_at = 8;
}

message SearchRequest {
  repeated float query = 1 [packed = true];
  uint32 k = 2;
  Metric metric = 3;             // COSINE | L2
  bool allow_partial = 4;        // default false
  Consistency consistency = 5;   // STRONG | EVENTUAL
  string request_id = 6;
}

message ScoredID { string id = 1; float score = 2; ShardId shard = 3; }

message SearchResponse {
  repeated ScoredID results = 1;
  bool partial = 2;
  repeated uint32 failed_shards = 3;
  string request_id = 4;
}
```

> **Go Note — `[packed = true]`.** For `repeated float`, packed encoding writes the values as one length-delimited blob instead of one tag per element. For a 768-dim vector that's the difference between ~3 KB and ~4.6 KB on the wire, and it lets the decoder allocate the slice once. Proto3 packs numeric repeated fields by default, but state it explicitly so the intent survives a proto2 copy-paste.

> **Go Note — `[]float32` and the garbage collector.** Every vector is a separate heap allocation with a slice header (24 B) pointing at it. At 1M vectors that's 1M pointers the GC must scan on every cycle. This *will* show up as GC pressure at the 1M mark. The fix — and we will do it as a measured optimization in M13, not before — is to store all embeddings in one big `[]float32` arena per shard and give each vector an `(offset, length)`. Premature now; note it so the benchmark result is not a surprise.

**Weaknesses of §5:** `map[string]string` metadata allocates a map per vector, which is heavy (~200+ B even when empty-ish). If metadata turns out to be mostly absent, a `nil` map plus a side table would be cheaper. Deferred until measured.

---

## 6. Sharding design

### 6.1 The four candidates

**`hash(id) % N` with N = number of *nodes*.** Simplest. Fatal flaw: changing N remaps almost every key, so adding a node requires moving ~all data. Rejected.

**Consistent hashing.** Nodes and keys are placed on a ring; a key belongs to the next node clockwise. Adding a node moves only ~1/N of keys. Cost: uneven load with few nodes (needs virtual nodes to fix), and the ring must be agreed on by everyone. Used by Cassandra, DynamoDB, Riak.

**Range partitioning.** Keys sorted; contiguous ranges assigned to shards; ranges split/merge dynamically. Enables range scans and adapts to skew. Cost: needs a range-map service, split/merge machinery, and hotspots on sequential keys. Used by HBase, CockroachDB, Bigtable, TiKV. **We have no range queries (A13), so we'd pay all the cost for none of the benefit.**

**Fixed logical shards ("virtual nodes" / fixed partition count).** Choose S once, `shard = hash(id) mod S`, with S ≫ number of nodes. Shards are then *assigned* to nodes by a separate placement map. Adding a node moves whole shards; the partition function never changes. Used by Elasticsearch, Kafka, Redis Cluster (16384 slots), Milvus.

### 6.2 The choice

> **Fixed logical shards.** `shard = xxhash64(id) mod S`, S fixed at cluster creation.

**Why:** it cleanly separates two concerns that consistent hashing tangles together —
1. *Which shard does this key belong to?* → a pure function, deterministic, requires no cluster state, identical on every node and every client. Never changes for the life of the cluster.
2. *Which node hosts that shard?* → cluster state, changes on failover and rebalance, must be distributed and versioned.

That separation is what makes rebalancing (§14) tractable: moving data means moving *a shard*, an object with an identity, a WAL, an LSN, and a snapshot. Under consistent hashing you'd be moving *key ranges*, which is strictly harder and teaches less about the thing we actually care about (leaders, logs, and recovery).

**Hash function:** `xxhash64` (`github.com/cespare/xxhash/v2`) — fast, well-distributed, non-cryptographic, and **stable across versions and platforms**. That last property is the requirement: if the hash changes, every key moves. Do *not* use Go's built-in `hash/maphash` (deliberately randomized per process — it would place the same key on different shards on different nodes, which would be a catastrophic and very confusing bug). Do not use `fnv` (weaker distribution).

### 6.3 The routing chain

```
   "doc-42"                      client-supplied ID
      │
      ▼
   xxhash64("doc-42") = 0x9e3779b97f4a7c15
      │
      ▼  mod S (=4)
   ShardID = 1                   pure function, no cluster state
      │
      ▼  ShardMap lookup (versioned cluster state)
   shard-1: { epoch: 7, leader: "node-2", replicas: ["node-3"] }
      │
      ├── write  ────────────► node-2 (leader only)
      └── read   ────────────► node-2 (STRONG) | node-2|node-3 (EVENTUAL)
```

### 6.4 Choosing S — I want to change your number

Your brief says S=4, N=3 nodes, RF=2. That means **8 shard-replicas across 3 nodes**, which cannot divide evenly: the placement is 3/3/2. One node carries 33% less data and load than the others, permanently. With S=4 you also can never usefully grow past 4 nodes, and a single shard is 25% of your data — so one failover moves a quarter of the cluster's traffic at once.

The general rule: **S should be a highly composite number, several times larger than the maximum node count you ever intend.**

| S | 8→ replicas at RF=2 | Even over 3 nodes? | Max useful nodes | Shard = % of data |
|---|---|---|---|---|
| 4 | 8 | ✗ (3/3/2) | 4 | 25% |
| 6 | 12 | ✓ (4/4/4) | 6 | 16.7% |
| **12** | **24** | **✓ (8/8/8)** | **12** | **8.3%** |
| 16 | 32 | ✗ (11/11/10) | 16 | 6.25% |

> **Recommendation:** Keep **S = 4 for milestones M1–M9.** Four shards means four log files, four leaders, four sets of trace output — you can hold the whole cluster in your head while debugging replication and failover, which is exactly when you need to. Then **switch to S = 12 at M10 (rebalancing)**, where uneven placement becomes the thing you're actually studying. Make `S` a config value from day one, and write the tests parameterized over S so the switch is a config change, not a refactor.

### 6.5 Shard map

The shard map is the cluster's authoritative placement state. It is **versioned** — every change bumps a monotonic `version`, and every shard has its own `epoch` that bumps on leadership change only.

```json
{
  "version": 12,
  "num_shards": 4,
  "dim": 128,
  "metric": "cosine",
  "nodes": {
    "node-1": { "addr": "node-1:7001", "state": "alive" },
    "node-2": { "addr": "node-2:7001", "state": "alive" },
    "node-3": { "addr": "node-3:7001", "state": "alive" }
  },
  "shards": {
    "0": { "epoch": 1, "leader": "node-1", "replicas": ["node-2"] },
    "1": { "epoch": 1, "leader": "node-2", "replicas": ["node-3"] },
    "2": { "epoch": 1, "leader": "node-3", "replicas": ["node-1"] },
    "3": { "epoch": 3, "leader": "node-1", "replicas": ["node-3"] }
  }
}
```

**Placement rationale:** leadership is spread round-robin so each node leads roughly S/N shards (write load balance), and a shard's replica is never on its leader's node (that would defeat the point). Shard 3 shows an epoch of 3 — it has failed over twice.

**How the map is distributed, per phase:**
- **M5–M6:** static JSON file, baked into the compose config. No failover. Deliberately boring.
- **M8:** a **controller** role (one designated node) owns the map, gossips it with heartbeats, and performs failover. This is a single point of failure and *we will treat that as a feature* — it makes the need for consensus visceral.
- **M11:** the map itself becomes Raft-replicated (a "meta" Raft group), and each shard becomes its own Raft group. Standard multi-raft.

**Stale map handling:** every RPC carries the caller's `shard_map_version`. A node that receives a request for a shard it does not lead responds `FAILED_PRECONDITION` with `NotLeader{current_leader, epoch, map_version}`. The caller updates and retries once. This is the same mechanism CockroachDB calls a `NotLeaseHolderError` and HBase calls `NotServingRegionException` — the client cache is allowed to be wrong, and is corrected by the server.

### 6.6 Answering your four questions

**Why this placement?** Round-robin leadership balances write load; distinct-node replica placement is the minimum requirement for RF to mean anything.

**What happens when a node dies?** For each shard it *led*: the controller waits for the failure detector (§12.2), picks the most-caught-up surviving replica, **bumps the shard epoch**, installs it as leader, publishes a new map version. For each shard it *followed*: the shard is now under-replicated (RF effectively 1); the controller schedules re-replication to a third node if one exists — with N=3 and RF=2 there is always exactly one candidate.

**How is the shard reassigned?** Controller decision, epoch bump, map version bump, push to all nodes. Not by peer agreement — pre-Raft, peer agreement is exactly what we don't have.

**How do we avoid data loss?** Pre-Raft: synchronous replication (§7) means an acknowledged write is on both replicas before the client hears OK, so promoting the survivor loses nothing — **provided** the survivor was in-sync at the moment of failure. If the follower had been marked out-of-sync and the leader kept accepting writes (§7.4), promoting it *does* lose writes. **This is a real, unavoidable hole in the pre-Raft design and it is the single strongest argument for §13.** The mitigation until then: never promote a replica that was not `ReplicaInSync`; if none exists, the shard goes **read-only and alerts**, rather than silently losing data. Availability sacrificed for durability — the right trade for a database.

**How do we rebalance?** §14.

**Weaknesses of §6:** fixed S is a permanent ceiling; changing it later requires a full re-partition (dump and reload). The controller is a SPOF from M8 to M11. And with N=3/RF=2, a second node failure during a failover window loses shard availability entirely.

---

## 7. Replication design

### 7.1 Roles and terms

- **Leader** — the single node accepting writes for a shard in a given epoch. Assigns LSNs. Owns the WAL.
- **Follower / replica** — applies the leader's log. Serves `EVENTUAL` reads and search. Never accepts client writes.
- **Replication log** — *the WAL itself.* There is no second log. The follower is fed WAL records from `follower.applied_lsn + 1` onward. This is the DynamoDB/Postgres model, not the "separate replication stream" model, and it means one durability mechanism serves both crash recovery and replication.
- **`commit_lsn`** — the highest LSN the leader considers durable per the replication policy. Reads only see records ≤ `commit_lsn`.
- **`applied_lsn`** (per replica) — the highest LSN that replica has fsynced *and* applied to its in-memory state.
- **Replica lag** — `leader.commit_lsn − follower.applied_lsn`. Exported as a metric (§15).

### 7.2 Synchronous vs asynchronous

**Synchronous.** Client → leader (WAL append + fsync + apply) → replicate → follower (append + fsync + apply) → ack → leader acks client.
- Durability: an acked write survives the loss of the leader. Failover is lossless.
- Latency: client pays `max(leader_fsync, network_rtt + follower_fsync)` ≈ leader fsync + ~0.5–2 ms locally.
- Availability: **a dead or slow follower blocks writes** unless you have a rule for ejecting it — and that rule is where the durability hole lives (§7.4).

**Asynchronous.** Client → leader (append + fsync + apply) → ack client; replication happens in the background.
- Durability: acked writes **can be lost** on failover. The window is the replication lag.
- Latency: only leader fsync.
- Availability: follower health doesn't affect the write path.

### 7.3 The choice

> **Synchronous replication to all in-sync replicas.** With RF=2 that means: the write is acked only after both the leader and the in-sync follower have it durable.

**Why:**
1. It is the only choice consistent with §3 (G1: acked ⇒ durable ⇒ survives failover). Async replication silently contradicts a "strong consistency" claim, and a design that quietly breaks its own guarantee is worse than one that's honestly weak.
2. With RF=2 and a local network, the cost is one extra RTT (~0.2 ms in Docker) plus one follower fsync. That's a real cost but a bounded one.
3. It sets up the Raft transition naturally: sync-to-all-in-sync-replicas is one step away from sync-to-a-majority. Going async first and then to Raft means throwing work away.
4. Failure-injection tests become *meaningful*: kill the leader after an ack and assert the data is on the survivor. Under async that test is expected to fail sometimes, which teaches nothing.

**We will add async as an opt-in per-request durability level (`durability: LEADER_ONLY | ALL_REPLICAS`) at M13**, and benchmark the two side by side. That comparison is worth having; making async the default is not.

### 7.4 The in-sync set — where the honesty lives

If a follower dies, do we (a) block writes forever, or (b) eject it and keep going?

Blocking forever means one dead node takes down half the cluster's shards. Ejecting means the shard is now running with one durable copy, and if the leader then dies, promoting the ejected follower loses writes.

> **Choice:** the leader maintains an **in-sync replica set (ISR)**, exactly Kafka's model. A follower is ejected from the ISR if it fails to ack within `T_sync` (default **3 s**, arbitrary) or is declared dead. Writes require acks from *all current ISR members*. Ejection **bumps the shard epoch and is published in the shard map**, so the fact that the shard is running under-replicated is *cluster-visible state*, not a leader's private opinion.
>
> **Hard rule:** a node that is not in the ISR at the time of the leader's failure is **never** promoted. If the ISR is empty apart from the dead leader, the shard becomes **read-only** and raises an alert. We choose unavailability over silent data loss.

This is precisely the trade-off Kafka's `min.insync.replicas` exposes, and pre-Raft we have the same weakness Kafka had before KRaft: the ISR itself is state that needs consensus to be safe. Ours lives in the controller. §13 fixes it.

### 7.5 Write path, precisely ordered

```
CLIENT                LEADER (shard 1, epoch 7)          FOLLOWER
  │
  │─── Insert(id,vec) ──►│
  │                      │ 1. validate (dim, id, size)
  │                      │ 2. am I leader for shard(id) in the
  │                      │    current map? else NotLeader ──────► client retries
  │                      │ 3. lsn = atomic ++nextLSN
  │                      │ 4. build WALRecord{lsn, epoch, op, payload}
  │                      │ 5. wal.Append(rec)      (buffered write)
  │                      │ 6. wal.Sync()           ◄── DURABLE ON LEADER
  │                      │ 7. apply to memtable + index
  │                      │
  │                      │─── Replicate{epoch=7, from=lsn} ──►│ a. epoch >= mine? else reject
  │                      │                                     │ b. lsn == applied+1? else request catchup
  │                      │                                     │ c. wal.Append + wal.Sync  ◄── DURABLE
  │                      │                                     │ d. apply to memtable + index
  │                      │◄────────── Ack{applied_lsn} ────────│
  │                      │
  │                      │ 8. all ISR acked → commit_lsn = lsn
  │◄──── OK{version=lsn}─│ 9. ack client
```

**Why fsync before apply (step 6 before 7)?** If we applied first and crashed before fsync, the in-memory state would have contained a write that the log does not, and recovery would silently lose it — but only *sometimes*, depending on whether a read observed it first. Write-ahead means the log is always at least as new as the state. That is the entire meaning of "write-ahead logging."

**Why apply on the leader before replicating (7 before the RPC)?** It isn't required for correctness, but it means the leader can serve a `STRONG` read the instant the write is durable locally, and it keeps the apply path identical on leader and follower (same function, same order), which halves the surface area for divergence bugs.

**Why is `commit_lsn` advanced only after acks (step 8)?** Because §3-G2 says a `STRONG` read reflects acknowledged writes. If reads observed uncommitted records, a failover could make a previously-read value disappear — a lost-update-visible-then-gone anomaly. Reads are served from state, filtered by `version <= commit_lsn`.

> **Go Note — clean concurrency for this path.** `nextLSN` is owned by the leader's single-writer goroutine, not by an atomic scattered across callers. The idiomatic shape is: a `writeLoop` goroutine owns all mutable log state and receives `writeRequest{rec, replyCh}` on a channel; callers block on `select { case resp := <-replyCh: ; case <-ctx.Done(): }`. This gives you (a) a total order for free, (b) natural **group commit** later — batch everything in the channel buffer into one `Sync()` — and (c) a single owner of mutable state, which is the Go answer to "who holds the lock?" See Appendix A.

### 7.6 Follower catch-up

A follower that is behind by `gap = leader.commit_lsn − follower.applied_lsn`:

- `gap == 0` → in sync, stream new records.
- `0 < gap ≤ WAL_tail_available` → **log shipping**: leader streams WAL records from `applied_lsn+1`. Cheap.
- `gap > WAL_tail_available` (leader already truncated past that point) → **snapshot transfer**: leader sends its latest snapshot (which carries `snapshot_lsn`), follower installs it wholesale, then log-ships from `snapshot_lsn+1`.

This is why snapshots record their LSN, and why WAL truncation must never delete records below the slowest ISR member's `applied_lsn` without first ejecting it. Detail in §9.

**Weaknesses of §7:** ISR membership is controller state and therefore only as reliable as the controller. `T_sync = 3 s` is arbitrary and untuned. Sync replication doubles write latency's sensitivity to fsync — group commit (M13) is the mitigation and is not free.

---

## 8. WAL design

### 8.1 Requirements for the log

1. Append-only. Never rewrite a committed record.
2. Every record self-describing and self-verifying (length + checksum).
3. Detect and survive a **torn tail** (partial record at crash) without human intervention.
4. Support sequential replay from an arbitrary LSN (for recovery *and* replication — same code path).
5. Support truncation of the prefix (after snapshot) without invalidating LSNs.

### 8.2 Record format

```
  0        4        8       12               20                  20+len
  ┌────────┬────────┬────────┬────────────────┬──────────────────────┐
  │ MAGIC  │ LEN    │ CRC32C │ LSN            │ PAYLOAD (protobuf)   │
  │ 4 B    │ 4 B u32│ 4 B    │ 8 B u64 LE     │ LEN bytes            │
  └────────┴────────┴────────┴────────────────┴──────────────────────┘
   0x56454331          ^ CRC covers LSN ‖ PAYLOAD (not the header)
   ("VEC1")
```

Header is 20 bytes. `LEN` is bounded by `maxRecordSize` (default 4 MiB, arbitrary) so a garbage length can never cause a huge allocation — a classic parser-hardening rule that matters even for trusted input, because the "input" here is a *torn file*.

Payload (protobuf, versioned independently of the frame):

```protobuf
message WALRecord {
  uint64 lsn        = 1;
  uint64 epoch      = 2;    // leadership fence at time of write
  OpType op         = 3;    // PUT | DELETE | SNAPSHOT_MARK | NOOP
  string vector_id  = 4;
  VectorRecord data = 5;    // absent for DELETE
  int64 wall_time   = 6;    // observability only
}
```

**Checksum choice: CRC32-Castagnoli**, not IEEE. Go's `hash/crc32` uses SSE 4.2 / ARMv8 hardware instructions for Castagnoli, giving multi-GB/s; the IEEE polynomial falls back to table lookup. Same 4 bytes, ~10× the speed. `crc32.MakeTable(crc32.Castagnoli)` once, package-level, reused.

**Why the CRC covers LSN+payload but not MAGIC/LEN:** MAGIC and LEN must be validated *before* we're willing to read LEN bytes; they're checked structurally (magic matches, len ≤ max, len ≤ bytes remaining). The CRC then validates the content we were told to read.

**Why MAGIC per record, not just per file:** it makes resynchronization possible in a future recovery tool, and it makes a torn header detectable immediately rather than as a wild length.

### 8.3 Segments

The WAL is a directory of segments: `wal/000000000000000001.log`, rolled at `segmentSize` (default 64 MiB, arbitrary). Filename encodes the **first LSN** in the segment, zero-padded so lexical order == numeric order.

Why segments rather than one file: truncating a prefix becomes `os.Remove` of whole files (cheap, atomic) instead of rewriting a large file. Replication can seek to a segment by name. And a corrupt segment loses bounded data.

### 8.4 Durability policy

```go
type SyncPolicy int
const (
    SyncAlways   SyncPolicy = iota // fsync every record. Default.
    SyncInterval                   // fsync every N ms. Bounded loss window.
    SyncNever                      // OS page cache only. Benchmarks/tests only.
)
```

**Default `SyncAlways`.** This is the honest default for a system claiming durability, and it makes the fsync cost *visible* in every benchmark, which is the point. `SyncInterval` will be offered at M13 with the loss window documented in the same sentence as the throughput number.

**Group commit** (M13): when many writes queue at the single writer goroutine, append all of them, then call `Sync()` **once**, then ack all of them. Amortizes fsync across the batch — often 10–50× on write throughput. It is the single highest-leverage write optimization and it falls out of the single-writer design in §7.5 almost for free.

> **Go Note — three things people get wrong about fsync.**
> 1. `f.Write()` does not persist. `f.Sync()` does. `bufio.Writer.Flush()` only moves bytes from Go's buffer to the OS — it is *not* durability.
> 2. Creating a **new file** (a new segment) is not durable until the **parent directory** is fsynced: `dir, _ := os.Open(walDir); dir.Sync()`. Skip this and a crash can leave you with a segment that has data but no directory entry. Almost every from-scratch WAL gets this wrong.
> 3. `f.Sync()` returning an error is **not retryable** on Linux — the kernel may have already dropped the dirty pages. The only correct response is to fail the write and stop accepting new ones for that shard. (This is the "fsyncgate" lesson from PostgreSQL, 2018.)

### 8.5 Recovery and the torn tail

```go
// Pseudocode for the reader loop, run at startup and for replication.
for {
    hdr, err := readFull(f, 20)
    if err == io.EOF || err == io.ErrUnexpectedEOF { truncateHere(); break }
    if magic(hdr) != MAGIC                        { truncateHere(); break }
    ln := length(hdr)
    if ln == 0 || ln > maxRecordSize              { truncateHere(); break }
    payload, err := readFull(f, ln)
    if err != nil                                 { truncateHere(); break }
    if crc32c(lsn(hdr), payload) != crc(hdr)      { truncateHere(); break }
    if lsn(hdr) != expectedLSN                    { return ErrLSNGap }   // corruption, not a torn tail
    apply(decode(payload)); expectedLSN++
}
```

**Why truncate rather than skip.** If a record fails validation, everything after it in that file is untrustworthy — we cannot know whether the following bytes are a valid record or the middle of a half-written one that happens to start with the magic. Skipping could produce an LSN *hole*, which breaks the gapless-LSN invariant that replication and `commit_lsn` depend on. Truncating to the last known-good offset restores the invariant: the log is a contiguous prefix of what the leader intended. The cost is losing the tail — which is exactly the set of writes that were never acknowledged, because acknowledgment requires a completed fsync.

`truncateHere()` **physically** calls `f.Truncate(lastGoodOffset)` and then fsyncs the file, so subsequent appends don't land after garbage.

**An `ErrLSNGap` is different.** A valid, checksummed record with the wrong LSN means real corruption or a mis-assembled directory, not a torn write. That is a hard failure: refuse to start, log loudly. Never "repair" silently.

### 8.6 Crash-point analysis (your seven cases)

Let the write be `W` with LSN 100, and let the state before it be consistent at LSN 99.

**1. Crash before WAL append.** Nothing on disk, nothing in memory, client got no ack (or got a timeout). Recovery → state at LSN 99. **Correct.** Client's retry re-issues the upsert and gets a fresh LSN. This is why upsert-by-ID (A16/G6) is not a convenience — it's a correctness requirement.

**2. Crash during WAL append (torn record).** Partial bytes on disk. No fsync completed ⇒ no ack was sent. Recovery truncates the partial record → state at LSN 99. **Correct, no ack was violated.** *This is the case the CRC and the truncating reader exist for.*

**3. Crash after WAL append + fsync, before apply.** Record 100 is on disk; memory never saw it; the client had **not** been acked (ack is step 9, after replication). Recovery replays 100 → state at LSN 100. Client's retry writes it again at LSN 101. Same value, idempotent, harmless. **Correct.**

**4. Crash during index update.** The index is pure in-memory derived state, rebuilt from the log at startup. A half-updated index dies with the process. Recovery rebuilds it from records 1..100 (or snapshot + tail). **Correct — and this is precisely why the index must never be the source of truth.**

**5. Crash after index update, before/after client ack.** The log has 100, the memory would have had 100. Recovery reaches 100 either way. If the ack was sent, the write is durable and honored. If it wasn't, the client retries and gets an idempotent no-op-ish result. **Correct.**

**6. Crash during snapshot.** Snapshots are written to `snap/<lsn>.tmp`, fsynced, then `os.Rename`d to `snap/<lsn>.snap`, then the directory is fsynced. Rename is atomic on POSIX. A crash mid-write leaves only a `.tmp`, which recovery ignores and deletes. **The WAL is never truncated before the rename has been fsynced.** So the worst case is a wasted snapshot and a longer replay. **Correct.**
*(Windows note: `os.Rename` over an existing file behaves differently and directory fsync isn't meaningful. See §18.4 — durability tests run in Linux containers.)*

**7. Crash during replication.** Sub-cases:
   - *Leader crashes after its own fsync, before sending.* Record 100 exists only on the leader, uncommitted, client not acked. On failover the follower is promoted at LSN 99 and starts a new epoch. When the old leader returns as a follower, its log has an extra record 100 that the new leader's log doesn't — **log divergence**. Resolution pre-Raft: the returning node compares `(epoch, lsn)` with the leader; if it holds records above the leader's `commit_lsn` from an older epoch, it **truncates them** and catches up. Because those records were never acked, truncating them violates nothing.
   - *Follower crashes after fsync, before ack.* Leader times out, may retry the append; follower sees `lsn == applied_lsn` (already have it) and re-acks. **Idempotent by LSN.**
   - *Leader crashes after follower fsync but before acking the client.* Follower has 100 and is promoted with it. The client never got an ack but the write survives. This is legal: G6 says the outcome of an un-acked write is undefined. It is *not* legal in the other direction (acked but lost), and sync replication is what prevents that.

**Weaknesses of §8:** no per-segment index, so seeking to an LSN means scanning a segment from its start (bounded by 64 MiB — acceptable, but a footer index is an easy later win). No snapshot checksums in v1. `SyncAlways` will dominate write latency and we must not hide that.

---

## 9. Storage engine

### 9.1 The choice

> **WAL + in-memory hash map + in-memory vector index + periodic full snapshot.** No LSM tree. No B-tree. No mmap'd data files.

**Justification against the alternatives:**
- An **LSM tree** exists to make writes sequential and to hold datasets larger than RAM. Our dataset fits in RAM by assumption (A3/§2 budget), and our WAL already makes writes sequential. An LSM would add compaction, level management, bloom filters, and read amplification — a large amount of machinery serving zero requirement here. **Rejected as premature.**
- A **B-tree** serves range scans and out-of-core point lookups. We have neither (A13). **Rejected.**
- **mmap'd vector files** would reduce GC pressure and allow >RAM datasets. Genuinely attractive at 1M×768. **Deferred to M13** as a measured optimization, not a starting assumption.

The revisit trigger is explicit: **if the working set exceeds RAM, this design must change.** Documented in §21.

### 9.2 Memory layout

```go
// Shard is the unit of ownership. All mutable state below is owned
// by this struct and guarded by its single RWMutex.
type Shard struct {
    id  core.ShardID
    dim int

    mu    sync.RWMutex
    data  map[string]*core.Vector  // id -> record (incl. tombstones)
    index index.Index              // brute force in v1, HNSW later

    // Log state, owned by the single writer goroutine (§7.5).
    wal        *wal.Log
    nextLSN    core.LSN
    commitLSN  core.LSN
    appliedLSN core.LSN
    epoch      core.Epoch
}
```

**One `Shard` = one lock domain = one WAL = one leader.** Nothing is shared between shards. Four shards on a node means four independent lock domains, so a slow search on shard 2 does not block a write on shard 0. That is the main reason logical shards buy you something even on a single node.

> **Go Note — `RWMutex` and the read path.** Search holds `RLock` for the duration of the scan. A 1M-vector brute-force scan holding a read lock for 30 ms will starve writers if writes are frequent, because Go's `RWMutex` blocks new readers once a writer is waiting. Two mitigations, in order of preference: (1) copy-on-write the index snapshot so search takes no lock at all beyond grabbing a pointer; (2) chunk the scan and release/reacquire. We will start with the simple `RWMutex`, *measure the starvation*, and then fix it — because seeing the p99 spike in a benchmark is worth more than avoiding it by luck.

### 9.3 Persistence & recovery

```
STARTUP
  1. list snap/ → pick highest <lsn>.snap ; delete any *.tmp
  2. load snapshot → data map + rebuild index ; appliedLSN = snapshot_lsn
  3. list wal/ segments ; skip those entirely below snapshot_lsn
  4. replay records with lsn > snapshot_lsn, in order:
       - assert contiguity (ErrLSNGap otherwise)
       - apply PUT / DELETE to data map + index
       - appliedLSN = rec.lsn
  5. truncate the torn tail if any (§8.5)
  6. nextLSN = appliedLSN + 1 ; commitLSN = appliedLSN
  7. announce readiness to the controller with (shard, epoch, appliedLSN)
```

Recovery time ≈ (snapshot load) + (records since snapshot × apply cost). Snapshot frequency is therefore a direct RTO knob, and `recovery_duration_seconds` is a first-class metric (§15).

### 9.4 Snapshots, compaction, tombstones

**Snapshot** = a full serialized image of the shard's live data at a specific LSN. Trigger: `wal_bytes_since_snapshot > threshold` (default 256 MiB, arbitrary) **or** a manual/periodic trigger. Written by a background goroutine from a copy-on-write view so it doesn't block writes for the duration.

**"Compaction" here means log truncation, not LSM compaction.** After a snapshot at LSN *L* is durably renamed:
```
safe_truncate_lsn = min(L, min over ISR members of applied_lsn)
delete every WAL segment whose last LSN < safe_truncate_lsn
```
The `min over ISR` term is essential: truncating past a live follower's cursor forces a full snapshot transfer for what could have been a 10-record catch-up. Getting this wrong is a classic and expensive bug.

**Tombstones** are dropped at snapshot time, but only if `tombstone.version < safe_truncate_lsn`. Dropping a tombstone that a lagging follower hasn't seen would resurrect the key on that follower. (Cassandra's `gc_grace_seconds` exists for exactly this hazard; ours is LSN-based instead of time-based, which is strictly better because it doesn't depend on clocks.)

**Versioning:** single-version. `data[id]` holds only the latest record. No MVCC. This is why search is not snapshot-consistent (§3-G4) — there is no old version to read. Adding MVCC would be the correct path to consistent search and is explicitly out of scope.

**Weaknesses of §9:** full snapshots are O(dataset) in time and disk; at 1M×768 that's a 3 GB write. Incremental snapshots are the fix and are deferred. Snapshot-while-writing needs a COW mechanism that is itself a source of bugs (a plain `map` copy at 1M entries is not cheap — likely we snapshot per-chunk with the lock released between chunks, and record the LSN *before* starting).

---

## 10. Vector index

### 10.1 Metric

Supported: **cosine similarity** and **squared Euclidean (L2²)**.

> **Default: cosine.** Nearly all text/image embedding models (OpenAI, Cohere, sentence-transformers, CLIP) are trained with cosine or inner-product objectives; magnitude carries little semantic signal, direction carries it all. Cosine is also scale-invariant, so it's robust to whatever normalization the client did or didn't do.

We rank by **`dot(q,v) / ‖v‖`**, dropping the constant `‖q‖` — it's identical for every candidate and cannot change the ordering. `‖v‖` is the precomputed `Norm` field (§5.1). So the per-candidate cost is one dot product plus one divide.

We return **squared** L2 for the Euclidean metric — `sqrt` is monotone, so it cannot change the ranking, and skipping it saves a transcendental per candidate. The API documents this explicitly (returning something called "distance" that is actually the square is a classic footgun; naming it `l2_squared` avoids it).

### 10.2 Brute force (v1)

```
For one query over N vectors of dimension D:
  FLOPs      = N × D fused multiply-adds  (+ N divides)
  Memory read= N × D × 4 bytes            ← this is the real bound
  Time       = O(N·D)
  Space      = O(1) beyond the top-K heap
Top-K selection with a bounded min-heap:
  Time       = O(N log K)
  Space      = O(K)
```

**It is memory-bandwidth bound, not compute bound.** At N=1M, D=128 that's 512 MB streamed per query. At a realistic ~10 GB/s effective bandwidth, the floor is ~50 ms per query *per full scan* — before any Go overhead. Sharded 4 ways, each shard streams 128 MB ≈ 13 ms, and the shards run in parallel.

**Predicted (to be measured in M1, not claimed):** single-core scalar Go, D=128 — order 1–3 GB/s effective, i.e. **~200–500 ms at N=1M single-threaded**, dropping to tens of ms with per-shard parallelism and goroutine fan-out within a shard. Go does **not** auto-vectorize, so a naive loop leaves 4–8× on the table versus SIMD. That gap — measured — is the entire motivation for HNSW, and for keeping the kernel in one small file that could later be replaced with assembly.

> **Go Note — writing the kernel so the compiler helps you.** Bounds-check elimination matters here. Write `q = q[:len(v)]` before the loop so the compiler can prove both slices are the same length and drop the per-iteration bounds checks — often a 20–30% win. Verify with `go build -gcflags="-d=ssa/check_bce/debug=1"`. This is the kind of thing you should *measure*, not assume: it is one `go test -bench` away.

**Top-K with a min-heap:** keep a heap of size K ordered by *worst* score at the root. For each candidate, if `score > heap.min`, pop and push. `container/heap` is fine; a hand-rolled fixed array of size K is faster for small K (K≤100) because it avoids interface dispatch. Benchmark both in M1 — it's a good first lesson in "measure, don't assume".

### 10.3 HNSW (v2, M12 — after distribution works)

Design sketch only; full design happens at M12.

- **Structure:** a multi-layer proximity graph. Layer 0 contains every node; each higher layer is an exponentially-sampled subset (level ~ `floor(-ln(uniform()) · mL)`). Higher layers are sparse "highways" for coarse navigation; layer 0 is dense for fine search.
- **`M`** — max neighbours per node per layer (typ. 16). Memory ≈ `N × M × 2 × 4 B` for layer-0 links ≈ 128 MB at N=1M, M=16. Higher M = better recall, more memory, slower build.
- **`efConstruction`** — candidate list size during insert (typ. 200). Build-time quality knob.
- **`efSearch`** — candidate list size during query (typ. 50–200), `efSearch ≥ K`. **Runtime recall/latency dial.** Must be a per-request parameter.
- **Search:** greedy descent from the top layer's entry point to a local minimum, drop a layer, repeat; at layer 0 run a best-first beam search with `efSearch` candidates. Complexity ~ O(log N) hops × M distance computations.
- **Insert:** find `efConstruction` nearest at each layer from the top down, select M neighbours using the *heuristic* selector (which prefers diverse directions over pure nearest — this is what keeps the graph navigable and is the single subtlest part of HNSW), add bidirectional links, prune over-full neighbour lists.
- **Delete:** HNSW has no clean delete. Standard practice is a **soft delete**: mark the node, keep it in the graph as a routing waypoint, filter it from results, and rebuild when the tombstone ratio crosses a threshold (say 20%). Filtering at result time means `efSearch` must be inflated to compensate.
- **Persistence:** the graph is *derived* state, like the brute-force index. Two options: (a) rebuild from the data on startup — simple, but O(N·log N·M) and slow at 1M; (b) serialize the adjacency lists into the snapshot. **Recommendation: (b)**, with (a) as a fallback path, because recovery time is an SLO (N6).
- **Replication:** replicate **the log, not the graph.** Each replica builds its own graph from the same records. Because HNSW insertion is randomized (level assignment) and order-sensitive, replicas will have *structurally different graphs that are semantically equivalent*. This is fine and important to state: it means **replicas may return slightly different top-K for the same query**, which is a real, observable consequence of ANN + replication that most tutorials never mention. It also means our "replicas are identical" invariant weakens to "replicas contain identical data".

**Weaknesses of §10:** brute force at 1M×768 will be too slow to benchmark comfortably (§2). Cosine-only default may surprise users with L2-trained embeddings — hence both metrics from day one. HNSW + filtered search interact badly (filtering can disconnect the graph); out of scope.

---

## 11. Distributed search

### 11.1 Flow

```
CLIENT
  │ Search{q, k=10, request_id=r1, deadline=200ms}
  ▼
COORDINATOR  (any node; the one the client happened to hit)
  │ 1. read shard map (version 12) → 4 shards, pick a replica for each
  │ 2. ctx, cancel := context.WithTimeout(ctx, 200ms)
  │ 3. fan out 4 concurrent RPCs, each with the SAME ctx
  ├──────────┬──────────┬──────────┐
  ▼          ▼          ▼          ▼
SHARD 0    SHARD 1    SHARD 2    SHARD 3
 local      local      local      local
 top-10     top-10     top-10     top-10        ← O(N/S · D + (N/S)·log k)
  │          │          │          │
  └──────────┴────┬─────┴──────────┘
                  ▼
        GATHER: 40 candidates
                  ▼
        MERGE: k-way merge of 4 sorted lists → global top-10   O(S·k·log S)
                  ▼
        (optional) hydrate: fetch full vectors/metadata for the 10 winners
                  ▼
              CLIENT
```

### 11.2 Why local top-K is sufficient — the proof

**Claim.** Let `G` be the true global top-K over the union of all shards, under a total order on scores. Then `G ⊆ ⋃_s TopK(s)`.

**Proof.** Take any `x ∈ G`, residing on shard `s`. Since `x` is in the global top-K, at most `K−1` items in the *entire dataset* score strictly higher than `x`. Shard `s` is a subset of the dataset, so at most `K−1` items *on shard s* score higher than `x`. Therefore `x` ranks at position ≤ K within shard `s`, i.e. `x ∈ TopK(s)`. ∎

So merging `S·K` candidates is not an approximation — it is **exact**, and `K` per shard is the tight bound (K−1 would not be: construct a case where all K global winners live on one shard).

**Where the proof breaks — and you must know all four:**
1. **Approximate per-shard search (HNSW).** `TopK(s)` becomes "approximately the top K of s", so the union may miss a true global winner. Global recall then degrades roughly as per-shard recall. Mitigation: over-fetch `k' = k · overfetch` per shard (typ. 1.5–2×), and/or raise `efSearch`.
2. **Post-filtering.** If you take top-K then apply a metadata filter, you can end up with fewer than K, or zero. Filtering must happen *inside* the shard, before top-K selection.
3. **Ties.** If scores tie at the boundary, different shards may break ties differently. Fix: make the order total by using `(score, id)` as the sort key everywhere. Cheap, and it makes results deterministic and therefore testable.
4. **Partial results.** If a shard is missing, the answer is top-K over the shards that responded — correct for that subset, not for the dataset. Which is why it must be *flagged*.

### 11.3 Failure handling in the fan-out

| Situation | Behavior |
|---|---|
| **Timeout on one shard** | Per-shard deadline derived from the request deadline. On expiry the RPC is cancelled via `ctx`. If `allow_partial=false` → whole request fails `DEADLINE_EXCEEDED`. If `true` → proceed, set `partial=true`, list the shard in `failed_shards`. |
| **Failed replica** | Coordinator retries the *same shard* on another replica **once**, with the remaining budget. Search is a pure read, so retrying is always safe. |
| **Failed shard (all replicas down)** | Same as timeout: fail, or flag partial. |
| **Duplicate responses** | Can't happen per shard (one in-flight RPC per shard), but the merge dedupes by `id` anyway as defense in depth — an ID lives on exactly one shard, so a duplicate ID across shards signals a partitioner or placement bug, and we log it at ERROR. |
| **Slow shard (tail latency)** | The per-shard deadline bounds it. Optional at M13: **hedged requests** — if a shard hasn't answered by p95, fire a second request to another replica and take the first response. Classic Dean & Barroso "tail at scale" technique; a great thing to implement *after* you can measure the tail. |
| **Stale shard map** | A shard replies `NotLeader`/`NotServing` → coordinator refreshes the map and retries once. |

### 11.4 Partial results policy

> **`allow_partial` defaults to `false`.** A search that silently omits a quarter of the corpus and looks like a normal answer is worse than an error, because the caller cannot tell. When `true` is set explicitly, the response *must* carry `partial=true` and `failed_shards`, and the coordinator must log it. Metric: `search_partial_total`.

### 11.5 Cost

| | |
|---|---|
| Network per search | S request messages (~D×4 B each = 3 KB at D=768) + S responses (~K×(16+4+4) B ≈ 240 B at K=10). **Tiny.** |
| Coordinator CPU | O(S·K·log S) merge — negligible. |
| Shard CPU | O(N/S · D) — **dominant**. |
| Latency | ≈ max over shards (parallel), so `RTT + (N/S·D)/bandwidth`, i.e. tail-latency dominated. |

The interesting consequence: **more shards = lower search latency** (each scans less, in parallel) but **more tail-latency exposure** (you wait for the slowest of S). That trade-off is why S=12 will behave measurably differently from S=4, and it's worth benchmarking both at M10.

> **Go Note — the fan-out pattern.** Use `golang.org/x/sync/errgroup` with `errgroup.WithContext`. It gives you: bounded concurrency (`g.SetLimit`), automatic context cancellation when the first error occurs, and a single `g.Wait()`. Do **not** hand-roll `sync.WaitGroup` + an error channel — you will get the "first error wins vs. all errors" semantics subtly wrong, and you'll leak goroutines when the caller cancels. Every fan-out RPC takes the *derived* context, so one `cancel()` tears down all four in-flight calls.

---

## 12. Failure detection and handling

### 12.1 Components

- **Heartbeats:** every node → controller, every `T_hb` (default **500 ms**, arbitrary), carrying `{node_id, per-shard (epoch, applied_lsn, role), map_version, load}`. Piggybacking replica progress on the heartbeat means the controller always knows who is caught up, which is exactly what failover needs.
- **Failure detector:** node is `suspect` after `T_suspect` (3 missed = 1.5 s), `dead` after `T_fail` (default **5 s**, arbitrary).
- **Health endpoint:** `/healthz` (process alive) and `/readyz` (recovered, shards loaded, caught up).

**Fixed timeout vs. phi-accrual.** Phi-accrual (Cassandra, Akka) adapts to observed inter-arrival variance and outputs a *suspicion level* rather than a boolean, which is genuinely better under variable latency. **Recommendation: fixed timeouts at M8.** In a Docker Compose cluster on one host, network variance is tiny and phi-accrual's adaptivity buys nothing while adding a statistical model to debug. Note it as a M13 upgrade with a benchmark that injects latency variance so the improvement is *demonstrable*.

**The unavoidable truth:** with an asynchronous network (A8), no failure detector can distinguish a crashed node from a slow one. Every timeout is a *guess*. All the epoch/fencing machinery exists because that guess is sometimes wrong.

### 12.2 The failover sequence, step by step

```
t=0.0  node-2 (leader of shard 1, epoch 4) is SIGKILLed
t=0.0  in-flight writes to shard 1 fail with Unavailable; clients retry
t=0.5  controller misses heartbeat 1
t=1.5  controller misses heartbeat 3 → node-2 = SUSPECT
       └─ routing stops sending new EVENTUAL reads to node-2
t=5.0  T_fail elapses → node-2 = DEAD
       └─ DECISION for shard 1:
            candidates = replicas of shard 1 that are (a) alive
                         and (b) were ReplicaInSync at suspicion time
            = { node-3 : applied_lsn = 8842 }
            if candidates empty → shard 1 goes READ_ONLY + alert. STOP.
       └─ STATE TRANSITION:
            shard 1.epoch  : 4 → 5          ← the fence
            shard 1.leader : node-2 → node-3
            map.version    : 12 → 13
t=5.0  controller pushes map v13 to all live nodes
t=5.0  node-3 receives (shard 1, epoch 5, role=leader):
            - verifies its applied_lsn
            - sets commit_lsn = applied_lsn   (everything it has is committed:
              it was in-sync, so the old leader had acked nothing above this)
            - nextLSN = applied_lsn + 1
            - opens its WAL for append at epoch 5
            - marks shard 1 as under-replicated (ISR = {node-3})
t=5.0  writes to shard 1 resume. Client-visible outage ≈ 5 s.
       └─ CLIENT-VISIBLE BEHAVIOR: writes to shard 1 failed with
          UNAVAILABLE for ~5 s and must be retried. Reads with
          consistency=EVENTUAL continued throughout (served by node-3).
          Search continued, with shard 1 either served by node-3 or,
          if allow_partial, flagged.
       └─ Shards 0,2,3 were never affected. Blast radius = 1/S.
LATER  controller schedules re-replication of shard 1 to node-1
       (snapshot transfer + WAL catch-up, §14) to restore RF=2.
LATER  node-2 restarts, recovers locally to LSN 8850 (it had 8 records
       beyond commit — never acked), reports (shard 1, epoch 4, lsn 8850).
       Controller replies: "you are a FOLLOWER at epoch 5."
       node-2 sees leader.commit_lsn = 8842 < its 8850 from an older epoch
       → truncates its log to 8842 → catches up. No data loss:
       those 8 records were never acknowledged to any client.
```

### 12.3 The failure→behavior table

| Failure | Detection | Decision | State transition | Recovery | Client sees |
|---|---|---|---|---|---|
| Leader crash | heartbeat timeout, 5 s | promote most-caught-up ISR member | epoch++, map version++ | old leader returns as follower, truncates uncommitted tail | writes UNAVAILABLE ~5 s, then resume; no acked write lost |
| Follower crash | heartbeat timeout | eject from ISR | ISR shrinks, epoch++, map version++ | rejoins, catches up via log or snapshot | writes get *faster* (one fewer ack); durability silently reduced — **must be alerted** |
| Slow follower | ack > `T_sync` (3 s) | eject from ISR | same as above | rejoins when caught up | brief write latency spike, then normal |
| Network partition (pre-Raft) | both sides time out | controller's side wins | epoch++ on controller side | minority-side leader keeps accepting writes until it learns otherwise ⚠️ | **split-brain window — see 12.4** |
| Node restart | node announces on startup | follower at current epoch | may truncate divergent tail | catch-up | none |
| Corrupt WAL tail | CRC mismatch at replay | truncate | applied_lsn lower than before | catch-up from leader | none |
| LSN gap (real corruption) | contiguity check | **refuse to start** | node stays down | manual: wipe data dir, full snapshot transfer | that node is out until fixed |
| Disk full | write error | fail the write | shard → READ_ONLY | manual | writes fail with RESOURCE_EXHAUSTED |

### 12.4 The split-brain window (do not gloss over this)

Pre-Raft, the controller is a single authority, which prevents *two controllers* disagreeing but not this:

> node-2 is partitioned from the controller but **still reachable by some clients**. The controller declares it dead and promotes node-3 at epoch 5. For a window, node-2 still believes it leads shard 1 at epoch 4 and accepts writes from those clients.

What the epoch fence *does* buy: node-2's replication RPCs to node-3 are rejected (`epoch 4 < 5`), so it can never corrupt the new leader's log, and it can never satisfy its own ISR requirement — so **under sync replication it cannot ack any write.** Its writes hang and eventually fail.

What it does **not** buy: node-2 will happily serve stale `STRONG` reads until it learns of the epoch change, violating G2 for that window.

**Mitigations, in ascending order of correctness:**
1. **Leader leases (M8).** A leader may serve `STRONG` reads only while holding an unexpired lease from the controller (`T_lease` < `T_fail`, so the old lease expires before the new leader is installed). Cheap, effective, and standard (this is essentially CockroachDB's range lease). **Recommended for M8.**
2. **Consensus (M11).** A leader is elected by a majority and therefore cannot coexist with another majority-elected leader. This is the actual fix.

Being explicit about this in the docs — "between M8 and M11 the system has a bounded stale-read window under partition" — is the difference between a learning project and a project that taught you a false belief.

**Weaknesses of §12:** the controller is a SPOF and its own failure is not handled until M11. All timeouts are arbitrary and untuned. Leases depend on bounded clock *drift rate* (not synchronized clocks) — a weaker but non-zero assumption that must be documented if we implement them.

---

## 13. Raft (M11 — only after the above is stable)

### 13.1 A correction you need to hear now

**RF=2 is incompatible with majority-quorum consensus.** A majority of 2 is 2. Losing either replica halts the group — strictly *worse* availability than the primary/backup scheme we'd be replacing. Raft requires **2f+1** replicas to tolerate *f* failures: **RF=3 tolerates 1, RF=5 tolerates 2.**

> **Therefore: the Raft milestone must also change RF from 2 to 3.** With N=3 nodes and RF=3, every node holds every shard — which is fine for a learning cluster and actually simplifies the transition, but it means "sharding" temporarily stops reducing per-node data. If you want sharding and Raft to both do real work you need N ≥ 4–5. I recommend N=3/RF=3 at M11 (keeps compose simple, proves the consensus logic) and optionally a 5-node compose profile for a proper multi-raft demo.

This is a genuine correction to the brief's parameters and I'd rather surface it in Phase 0 than at milestone 11.

### 13.2 Why a leader alone is insufficient

A leader gives you a *total order*. It does not give you **agreement on who the leader is**. Every mechanism in §12 — controller, epochs, leases — is an attempt to approximate that agreement with a single trusted process. It fails in exactly one way: when the deciding authority is itself partitioned or wrong. Consensus replaces "a trusted process decides" with "a majority decides", and because two majorities of the same set must intersect, two leaders in the same term are impossible.

**Split-brain, precisely:** two nodes simultaneously believe they lead the same shard and both accept writes, producing two divergent histories with no way to reconcile them without losing acknowledged data. Raft prevents it via three interlocking rules: (a) one vote per node per term, (b) a majority is required to win, (c) a candidate whose log is behind cannot win (the *log-up-to-dateness* check in `RequestVote`), which is what guarantees the **Leader Completeness** property — every committed entry is present in every future leader's log.

### 13.3 What M11 implements

Per-shard Raft groups (**multi-raft**), plus a meta group for the shard map.

- **Terms** — monotonic; replaces our `Epoch` (they're the same idea; Raft's is majority-issued rather than controller-issued).
- **Roles** — Follower / Candidate / Leader, with randomized election timeouts (150–300 ms) to avoid split votes.
- **`RequestVote`** — with the log up-to-dateness check (`lastLogTerm`, then `lastLogIndex`).
- **`AppendEntries`** — log replication *and* heartbeat, with the `prevLogIndex/prevLogTerm` consistency check that detects and repairs divergence by backing up `nextIndex`.
- **`commitIndex`** — advanced when an entry from the **current term** is on a majority. (The subtlety Figure 8 of the Raft paper exists for: never commit a prior-term entry by counting replicas alone.)
- **Log divergence & catch-up** — a follower with conflicting entries truncates from the first mismatch and refills from the leader.
- **Snapshots + `InstallSnapshot`** — our §9 snapshots, reused.

**What we reuse:** the WAL becomes the Raft log (same file format, one extra `term` field). The state machine is the shard's data map + index — literally the existing `apply()`. This is the payoff for keeping apply as a pure function of the log.

**Implementation rule (from your brief):** write it, don't import it. `etcd/raft` is excellent and we will *read* it, but the deliverable is a minimal Raft you can explain line by line, tested against a deterministic simulated network (drop, delay, reorder, partition) — the only sane way to test consensus.

**Weaknesses of §13:** a from-scratch Raft has many subtle bugs; expect this milestone to take as long as M1–M7 combined. Multi-raft with 4–12 groups means 4–12× the heartbeat traffic — real systems batch heartbeats across groups (CockroachDB's "coalesced heartbeats"), which we'll note but probably not implement.

---

## 14. Rebalancing (M10)

**Trigger:** node added, node permanently removed, or shard-count-per-node imbalance beyond a threshold. Decision made by the controller (M10) / meta-Raft group (post-M11), aiming for even *leader* count and even *replica* count per node, while never placing two replicas of one shard on one node.

**Moving shard 3's replica from node-1 to node-4:**

```
PHASE 1  ADD LEARNER
  map: shard 3 gains node-4 as a LEARNER (receives the log, is NOT in the ISR,
       does NOT count for write acks, does NOT serve reads)
  ⇒ writes keep flowing at full speed; the move cannot stall the shard
PHASE 2  SNAPSHOT TRANSFER
  leader ships snapshot @ snapshot_lsn (streamed, chunked, checksummed)
  node-4 installs it, applied_lsn = snapshot_lsn
PHASE 3  WAL CATCH-UP
  leader streams records snapshot_lsn+1 .. current
  meanwhile new writes continue; the gap shrinks because catch-up is
  faster than the write rate (if it is NOT, throttle writes or abort —
  an unbounded catch-up is a real failure mode)
PHASE 4  CUTOVER   ← the only sensitive moment
  when lag < threshold (e.g. 100 records):
    - leader briefly stops advancing commit_lsn (pause, single-digit ms)
    - node-4 finishes the tail, acks at exactly commit_lsn
    - map: node-4 LEARNER → ISR member ; node-1 → REMOVING
    - epoch++, map version++, pushed to all nodes
    - leader resumes
PHASE 5  DRAIN & CLEANUP
  node-1 finishes any in-flight reads it was serving,
  then deletes shard 3's data dir, WAL, and snapshots.
  Deletion happens only after the new map version is ACKed by a
  quorum of nodes — otherwise a node with a stale map could route a
  read to storage that has just been deleted.
```

**Consistency during movement:** untouched. The learner never serves reads and never counts for acks, so until cutover it is invisible to clients. At cutover the ISR changes atomically with a map version bump.

**Failure during movement:**
- *Learner dies mid-transfer* → abort, remove learner from the map, retry later. No client impact.
- *Leader dies mid-transfer* → the transfer dies with it; the new leader restarts it from scratch. Wasteful, correct.
- *Crash between cutover and cleanup* → node-1 still holds a full, correct copy of shard 3 but is no longer in the map. It's dead weight, not a correctness problem. A janitor task reclaims orphaned shard directories at startup by comparing local dirs against the map.
- *Network too slow to ever catch up* → detect via a lag-not-shrinking check, abort, alert. Never let a rebalance run forever.

**Weaknesses of §14:** the cutover pause is a real (if brief) write stall. A full snapshot transfer of a large shard competes for disk and network with live traffic — real systems rate-limit it, we should at least expose a knob and measure the impact.

---

## 15. Observability

### 15.1 Metrics (Prometheus, `/metrics` on every node)

**Traffic & errors** — `vecdb_requests_total{op,status,shard}`, `vecdb_request_duration_seconds{op,shard}` (histogram → p50/p95/p99), `vecdb_search_partial_total`, `vecdb_not_leader_total{shard}`.

**Write path / durability** — `vecdb_wal_bytes_total{shard}`, `vecdb_wal_fsync_duration_seconds{shard}` (histogram — *the* number to watch), `vecdb_wal_segments{shard}`, `vecdb_group_commit_batch_size`, `vecdb_snapshot_duration_seconds{shard}`, `vecdb_snapshot_lsn{shard}`.

**Replication** — `vecdb_replication_lag_lsn{shard,replica}` (gauge), `vecdb_replication_lag_seconds{shard,replica}`, `vecdb_isr_size{shard}`, `vecdb_replication_errors_total{shard,replica,kind}`.

**Cluster** — `vecdb_nodes{state="alive|suspect|dead"}`, `vecdb_shard_map_version`, `vecdb_shard_epoch{shard}`, `vecdb_shard_role{shard,role}`, `vecdb_failovers_total{shard}`, `vecdb_leader_lease_remaining_seconds{shard}`.

**Data** — `vecdb_vectors{shard,state="live|tombstone"}`, `vecdb_bytes_resident{shard}`, `vecdb_index_build_duration_seconds{shard}`, `vecdb_recovery_duration_seconds{shard}`.

The four that tell you almost everything when something is wrong: **`wal_fsync_duration_seconds` p99**, **`replication_lag_lsn`**, **`isr_size`**, **`shard_epoch`** (a rising epoch means flapping failover).

### 15.2 Structured logging

`log/slog` (Go 1.21+ stdlib — no third-party logger needed). JSON in containers, text locally.

**Every log line in a request path carries `request_id`.** It's generated at the coordinator (or accepted from the client), put into the `context`, propagated over gRPC metadata to every shard, and attached to a `*slog.Logger` stored in that context. One `request_id` grep then shows the coordinator's decision, all four fan-out RPCs, and every shard's local work.

Standard fields: `request_id`, `node_id`, `shard`, `epoch`, `lsn`, `op`, `duration_ms`.

> **Go Note — context-scoped loggers.** The idiom: `ctx = logging.With(ctx, "request_id", rid, "shard", s)` returns a context holding a derived `*slog.Logger`; every function does `log := logging.From(ctx)`. This avoids threading a logger through every signature *and* avoids a global logger. It is one of the few genuinely good uses of context values (context values are for request-scoped data that crosses API boundaries — a request-scoped logger is exactly that).

### 15.3 Tracing

OpenTelemetry spans at coordinator + per shard would make the scatter-gather tail visible immediately. **Deferred to M13** — nice-to-have, and `request_id` correlation covers 80% of it.

---

## 16. Performance & benchmarking

### 16.1 Rules

1. **No performance claim without a committed benchmark that produced it.** Every number in the final README cites a benchmark name and the machine it ran on.
2. Every benchmark records: dataset size, dimension, metric, K, shard count, RF, sync policy, concurrency, and Go version.
3. Report **p50 / p95 / p99 / max**, never just the mean. A mean latency in a distributed system is close to meaningless.
4. Measure **before** optimizing. Every optimization must show its before/after in the same table.

### 16.2 What we measure

**Micro (`go test -bench`, M1–M3):** distance kernel (ns/op, and derived GB/s) across D ∈ {128, 384, 768}; top-K heap vs. fixed array across K ∈ {1,10,100}; WAL append+fsync (ops/s and B/s) across sync policies; protobuf encode/decode of a `VectorRecord`; full-shard brute-force scan at N ∈ {10K, 100K, 1M}.

**Macro (load harness, M6+):** insert QPS at concurrency ∈ {1,8,64}; search QPS × latency percentiles; the ladder 10K/100K/1M at D=128 and 10K/100K at D=768 (per the §2 memory budget); replication lag under sustained write load; **recovery time** vs. WAL size; **failover time** (write-unavailability window) — measured by a client that writes continuously while we kill the leader.

**Comparisons that must appear in the final doc:**
- Brute force vs. HNSW: QPS, p99, **recall@10** (vs. brute-force ground truth), memory, build time. *An ANN benchmark without a recall number is not a benchmark.*
- `SyncAlways` vs. `SyncInterval` vs. group commit: write QPS and the durability window each implies.
- Sync vs. async replication: write p99 and measured data loss on kill-the-leader.
- S=4 vs. S=12: search p50 and p99 (§11.5's prediction — does more parallelism actually beat more tail exposure?).

### 16.3 Method

`testing.B` for micro. A dedicated `cmd/vecbench` for macro, using **open-loop** load generation (fixed arrival rate, not "as fast as the client can go") — closed-loop generators hide queueing delay and systematically understate tail latency (this is the "coordinated omission" problem). Datasets: random-normal vectors for scale tests, plus one real embedding set (SIFT-1M or a sentence-transformer dump) for recall, because random vectors in high dimensions are pathologically uniform and make ANN look better than it is.

---

## 17. Testing strategy

**Unit** (`go test -race ./...` on every commit): distance kernels vs. a naive reference implementation, on hand-checked vectors and edge cases (zero vector, unit vectors, orthogonal, D=1); top-K correctness against a full sort, property-tested with random inputs; partitioner determinism and distribution (χ² over 1M synthetic IDs); WAL round-trip, and **table-driven torn/corrupt-file cases** (truncate at every byte offset of a valid record and assert clean recovery — this single test catches most WAL bugs); serialization round-trip; storage recovery equivalence (`state_after_replay == state_before_crash`); replication apply idempotence (apply the same LSN twice).

**Integration** (`docker compose`-driven, 3 nodes / 4 shards / RF=2): insert 100K, search, verify against a locally computed brute-force ground truth; kill a node, verify search still works; restart it, verify it catches up to the same `applied_lsn`; verify shard map propagation.

**Fault injection** — this is where the design is actually validated. A `chaos` package with an interceptor-based fault layer, plus container-level `docker kill`/`docker pause`/`tc netem`:

| Injection | Assertion |
|---|---|
| Kill leader mid-write | every acked write is present after failover; no acked write lost |
| Kill replica | writes continue; ISR shrinks; metric fires |
| Restart node | catches up to leader's `applied_lsn` |
| Delay network (`tc netem 200ms`) | search deadline respected; partial flag correct |
| Drop N% of RPCs | retries succeed; no duplicate application (LSN idempotence) |
| Corrupt WAL (flip a byte / truncate at random offset) | node recovers to last good LSN, or refuses to start on a genuine gap |
| Stale replica (pause a container 60 s) | ejected from ISR; on resume, snapshot-transfer path is taken |
| Partition (`docker network disconnect`) | documented behavior; post-M11: minority cannot commit |
| Fill disk | write fails cleanly; shard goes read-only; no corruption |

**Deterministic simulation (M11).** For Raft, a virtual-clock, virtual-network test harness where the whole cluster runs in one process with a seeded RNG driving message delay/drop/reorder. Every failing run reproduces exactly from its seed. This is how FoundationDB and TigerBeetle test consensus, and it is the *only* practical way to find Raft bugs. Building this is part of M11, not an afterthought.

**Coverage target:** ≥80% on `core`, `wal`, `store`, `index` — the packages where a bug is silent data loss. Lower is acceptable on transport/glue.

---

## 18. Technology & repository layout

### 18.1 Stack

Go 1.22+ · gRPC + protobuf · `log/slog` · `prometheus/client_golang` · `cespare/xxhash/v2` · `golang.org/x/sync/errgroup` · stdlib `testing` + `testify/require` for assertion ergonomics. Docker + Compose. `golangci-lint`. **No FAISS, no hnswlib, no external vector library** — the index is ours.

### 18.2 Layout

```
vecdb/
  cmd/
    vecdb/            node binary (serve)
    vecctl/           CLI: insert, search, status, shardmap
    vecbench/         macro benchmark harness
  internal/
    core/             Vector, ShardID, LSN, Epoch, errors  — no deps
    index/            Index interface; bruteforce/; hnsw/ (M12)
    wal/              record codec, segment writer/reader, replay
    store/            Shard: map + index + wal + snapshot + recovery
    partition/        hash → ShardID
    cluster/          shard map, membership, heartbeats, failure detector
    replication/      leader/follower, ISR, catch-up
    coordinator/      scatter-gather, merge, hedging
    transport/        gRPC server + client, interceptors (request_id, metrics, chaos)
    raft/             (M11)
    obs/              logging, metrics, tracing
  api/v1/             *.proto and generated code
  deploy/             Dockerfile, docker-compose.yml, prometheus.yml
  test/               integration + chaos suites
  docs/               00-system-design.md (this), ADRs, benchmark results
```

Rule: `core` imports nothing internal. Dependencies point inward. `internal/` prevents accidental external coupling.

> **Go Note — where interfaces go.** Define `Index` in `store` (the *consumer*), not in `index` (the producer). Go interfaces are satisfied implicitly, so the consumer declares the minimal method set it needs and any implementation satisfies it without importing anything. This is the opposite of the Java habit and it's the single most important Go design idiom. Resist creating a `Storage` interface with one implementation — that's a premature abstraction and your brief bans it. Add the interface when the second implementation exists (which for `Index` is genuinely coming at M12).

### 18.3 Module path

`module github.com/<your-github-user>/vecdb` — tell me the handle you want and I'll bake it in at M0.

### 18.4 Windows development — a real, concrete caveat

You're on Windows and the durability design depends on POSIX semantics: `fsync` on a **directory** (meaningless on Windows), atomic `rename` over an existing file (different semantics on Windows), and `os.Remove` of an open file (fails on Windows).

> **Recommendation:** develop in the editor on Windows, but run **all** builds, tests, and especially durability/crash tests inside Linux — WSL2 or the Docker containers. Put `make test` behind a container so there is exactly one environment where results count. If you'd rather run tests natively on Windows too, we add a small `syncDir` abstraction that no-ops on Windows and a build-tagged rename helper — say the word and I'll design it into M0.

---

## 19. Implementation roadmap

Each milestone: design → interfaces → implementation → tests → failure tests → benchmark → review → stop.

| M | Name | Delivers | Acceptance criteria |
|---|---|---|---|
| **M0** | Skeleton | repo layout, go.mod, Makefile, `golangci-lint`, `slog` setup, config loading, CI | `make build test lint` green on an empty-but-real tree |
| **M1** | Vector engine | `core` types, cosine + L2² kernels, top-K heap, in-memory `Shard` (no persistence) | insert/get/delete/search correct vs. reference; `-race` clean; benchmarks recorded for D∈{128,384,768}, N∈{10K,100K,1M} |
| **M2** | WAL | record codec, segment writer/reader, sync policies, truncating replay | round-trip tests; **truncate-at-every-offset** test passes; fsync latency benchmarked |
| **M3** | Persistent engine | WAL↔store integration, snapshots, recovery, tombstones, log truncation | `kill -9` during load → restart → **zero acked writes lost**, state identical; recovery time measured vs. WAL size |
| **M4** | Single-node service | `.proto` API, gRPC server, `/metrics`, `/healthz`, Dockerfile, `vecctl` | `docker run` → insert 10K → search → restart → data intact |
| **M5** | Sharding (in-process) | partitioner, static shard map, S shards in one process, coordinator scatter-gather + merge | distribution χ² sane; **global top-K identical to single-shard brute force** over the same data — the key correctness test |
| **M6** | 3-node cluster | remote fan-out, deadlines, request IDs, retries, compose file | `docker compose up` → 3 nodes, 4 shards, correct distributed search; per-shard timeout honored |
| **M7** | Replication | leader/follower, sync replication, ISR, log shipping, snapshot transfer, replica reads | kill leader after ack → data on survivor; replication lag metric live; catch-up works from both cold and warm |
| **M8** | Failure handling | heartbeats, failure detector, controller, epoch fencing, leader leases, automatic failover | scripted leader kill → automatic failover; **failover window measured**; no acked write lost; chaos suite green |
| **M9** | Recovery hardening | re-replication to restore RF, divergent-tail truncation, corrupt-WAL handling | stale replica rejoins and converges; corrupted WAL recovers or refuses cleanly; RF restored automatically |
| **M10** | Rebalancing | learner role, snapshot+catch-up move, cutover, cleanup; **switch to S=12** | add a 4th node → shards move → no failed requests during the move → even placement; S=4 vs S=12 latency compared |
| **M11** | Raft | multi-raft per shard, meta group for the map, **RF 2→3**, deterministic simulation harness | seeded simulation: no split-brain, no lost committed entry, across thousands of randomized partition/delay/drop schedules |
| **M12** | HNSW | graph build, search, soft delete, persistence in snapshot, `efSearch` per request | **recall@10 ≥ 0.95 vs. brute-force ground truth** at documented ef; ≥10× search QPS at 1M; build time and memory recorded |
| **M13** | Optimization & docs | group commit, arena-allocated vectors, hedged requests, async durability option, phi-accrual, tracing; final architecture doc | every optimization has a before/after table; final doc covers architecture, consistency, failure model, trade-offs, benchmarks, limitations |

**Sequencing rationale:** persistence before distribution (a distributed system that loses data is not interesting); brute force before HNSW (so the distributed layer is debugged against an *exact*, verifiable index — debugging distributed search and approximate search simultaneously is a trap); ad-hoc failover (M8) before Raft (M11) so that when you write Raft you already know, from your own broken system, exactly which problem it solves.

---

## 20. Decisions I need from you before Phase 1

I've made a recommendation for each. Say "all defaults" and I'll proceed with the bold option.

1. **Shard count.** → **S=4 for M1–M9, S=12 from M10.** (§6.4)
2. **RF at the Raft milestone.** RF=2 cannot work with majority quorum. → **RF 2→3 at M11, keeping N=3; optional 5-node compose profile.** (§13.1)
3. **Default metric.** → **cosine**, with L2² also implemented from M1.
4. **Default sync policy.** → **`SyncAlways`.** Honest, and makes the fsync cost visible in every benchmark.
5. **Partial search results.** → **`allow_partial=false` by default**, opt-in per request, always flagged in the response.
6. **Benchmark ladder.** → **10K/100K/1M at D=128; 10K/100K at D=768** (RAM budget, §2).
7. **Windows testing.** → **All tests run in Linux containers / WSL2.** Alternative: I design a `syncDir` + rename abstraction into M0 so native Windows `go test` also works. (§18.4)
8. **Module path.** → need your GitHub handle for `github.com/<you>/vecdb`.
9. **Pace.** → **M1 next**, complete with tests and benchmarks, then stop for your review. Alternative: M0 first if you'd rather have the scaffolding, CI, and lint config landed before any real code.

---

## 21. Known limitations (to appear verbatim in the final README)

1. **Dataset must fit in RAM.** No out-of-core storage, no memory-mapped vectors.
2. **`num_shards` is fixed at cluster creation.** Changing it requires dump and reload.
3. **Search is not consistent across shards.** No global snapshot; a search may observe different shards at different points in time.
4. **No multi-key or cross-shard atomicity.** Every operation is single-key.
5. **Pre-M11, a stale-read window exists under network partition.** Leases bound it; only consensus eliminates it.
6. **Pre-M11, the controller is a single point of failure.**
7. **RF=2 tolerates exactly one replica failure per shard, and only if that replica was in-sync.** If no in-sync replica survives, the shard becomes read-only rather than risk silent data loss.
8. **No authentication, authorization, or encryption.** Trusted network assumed.
9. **No metadata-filtered search** in the initial scope.
10. **No quantization or compression.** Vectors stored as raw float32.
11. **HNSW deletes are soft**, requiring periodic rebuild; replicas may return slightly different top-K for the same query because their graphs differ structurally.
12. **Snapshots are full, not incremental**, and are not checksummed in v1.
13. **No backup, restore, or point-in-time recovery.**
14. **Single collection.** No namespaces, no multi-tenancy.

---

## Appendix A — Go idioms this project depends on

**Context first.** `func (s *Shard) Search(ctx context.Context, q []float32, k int) ([]Result, error)` — `ctx` is always the first parameter, never stored in a struct. Every RPC and every long loop checks `ctx.Err()`. A deadline set at the coordinator propagates automatically through gRPC to every shard; you get distributed cancellation for free, but only if you thread `ctx` everywhere.

**Errors wrap, and are inspected with `errors.Is`/`As`.** Sentinels in `core`: `ErrNotFound`, `ErrNotLeader`, `ErrDimMismatch`, `ErrShardReadOnly`. Wrap with context: `fmt.Errorf("shard %d: replay lsn %d: %w", id, lsn, err)`. Never `errors.New(err.Error())` — that destroys the chain.

**Goroutine lifecycle.** Every goroutine is started by a component that owns it, takes a `ctx`, and is joined via `sync.WaitGroup` in `Close()`. The shape:
```go
func (c *Component) Start(ctx context.Context) {
    c.wg.Add(1)
    go func() { defer c.wg.Done(); c.loop(ctx) }()
}
func (c *Component) Close() error { c.cancel(); c.wg.Wait(); return nil }
```
No naked `go func()` anywhere in the codebase. Your brief demands this and it is the difference between a system you can shut down cleanly and one that hangs in tests.

**One owner per piece of mutable state.** Either a mutex whose scope is a single struct, or a single goroutine that owns the state and receives requests on a channel. Never both, never ambiguous. The WAL writer uses the goroutine-owner pattern (§7.5); the shard's data map uses the mutex pattern (§9.2).

**Interfaces at the consumer, small.** `type Index interface { Add(*Vector); Remove(string); Search([]float32, int) []Result }` — three methods, defined where it's used. No `IStorage`, no factories, no DI container.

**Table-driven tests + `-race` always.** `for _, tc := range []struct{name string; ...}{...} { t.Run(tc.name, func(t *testing.T){ ... }) }`. CI runs `go test -race -count=1 ./...`; `-count=1` defeats the test cache so flakes surface instead of hiding.

**Benchmarks are code, not scripts.** `func BenchmarkCosine768(b *testing.B)` with `b.ReportMetric(gbPerSec, "GB/s")` and `b.SetBytes(n)`. Results are committed to `docs/benchmarks/` with the machine spec, so "before and after" is a `git diff`.

---

*End of Phase 0. Awaiting your decisions on §20 before Phase 1.*
