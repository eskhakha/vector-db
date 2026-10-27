# vecdb — Final Design and Plan

**Status:** FINAL. This document is the authority.
**Supersedes:** §19 (roadmap), §20 (open decisions) and §18.2 (layout) of `00-system-design.md`; all of `02-roadmap-revision.md`.
**Still in force:** §1–§17 and §21 of `00-system-design.md`, as amended by §3 below.
**Written to be executed, not admired.** If a section here conflicts with an earlier document, this one wins.

---

## 0. The need, stated plainly

You told me four things, and together they change the project:

1. **Understanding is the product.** Not a running service, not a resume line. You want to understand the vector-database stack — which is mostly a distributed-systems stack — well enough to reason about it as a staff engineer.
2. **You want the trade-offs of real systems**, not just the trade-offs of your own. Your existing conceptual README was, in your words, "purely conceptual."
3. **Four to eight weeks, full-time.** Call it 300 focused hours.
4. **Durability, replication and failover** is the one thing to do to a high standard. "Done" means every milestone checked off *and* it survives an expert's code review.

The rest of this document follows from those four facts, and from one conflict between them.

---

## 1. The conflict, and the only honest resolution

Points 3 and 4 are incompatible as the roadmap currently stands.

`00-system-design.md` §19 lists M0–M13. `02-roadmap-revision.md` adds I1–I6. That is twenty milestones. M11 alone — a from-scratch Raft with a deterministic simulation harness — is 150–300 hours for someone who has not written one before. I4 (HNSW with the diverse-neighbour heuristic, soft delete and persistence, at recall@10 ≥ 0.95) is another 80–150. The whole plan is 1,500–2,500 hours. At 300 hours you would finish, generously, through M7.

There are three ways out. Two of them are wrong.

**Wrong answer 1: extend the timeline.** You said 4–8 weeks. A plan that quietly assumes eighteen months is the plan you already have, and it is why you are asking me to finalize it.

**Wrong answer 2: cut milestones.** Delete the ANN track and Raft, ship M0–M9, call it done. This fits the budget but fails the need: you would finish without ever having confronted consensus or approximate indexing, which are two of the three things that make this domain interesting.

**Right answer: change what "done" means, per milestone.**

Your goal is understanding, not shipped code. Code is an *instrument* for understanding, and it is an expensive one — roughly 10× the cost of a rigorous written teardown, for maybe 3× the understanding on most topics. But that ratio is not uniform. For some mechanisms, building is the *only* way to understand them, because the bugs are the lesson. For others, reading three production implementations and writing down why they differ teaches more per hour than writing your own.

So every milestone gets one of three tiers:

| Tier | What it means | Cost |
|---|---|---|
| **BUILD** | Implemented, unit + property + fault-injection tested, benchmarked, and written to a standard that survives review. This is real code you would show someone. | 20–50 h |
| **SPIKE** | A minimal working implementation that proves the mechanism and demonstrates its failure mode. Correct enough to run, not integrated, not hardened. Lives in `spike/`, is allowed to be ugly, is allowed to be deleted. | 5–15 h |
| **STUDY** | A written teardown: how two or three production systems implement it, where they differ, what each trade-off costs, what breaks. Ends with a design *you could implement*, and the ability to whiteboard it under questioning. | 3–8 h |

"Every milestone checked off" is now achievable in 300 hours, because a checked-off milestone is no longer always a built one — and the tier is chosen deliberately, by argument, not by whatever you ran out of time for.

The allocation is in §5. The short version: **durability, replication, failover and rejoin are BUILD. Raft's control plane is BUILD. Everything in the ANN index track above brute force is STUDY. Rebalancing and distributed-search tail behaviour are SPIKE.**

---

## 2. The axis your design document was missing: who owns durability?

`00-system-design.md` assumes, without ever arguing for it, that the database owns a local write-ahead log. That is one of three architectures in production today, and the fact that the document never names the other two is its most significant gap — because the choice determines almost everything downstream.

### 2.1 Local WAL — the database owns durability

Each shard replica writes its own append-only log to local disk, fsyncs, then applies. Recovery replays it. Replication ships it.

**Who does this:** Qdrant (per-shard WAL with a configurable `wal_capacity_mb` [1]), Weaviate (per-shard commit logs plus its own LSM key-value store [12]), PostgreSQL/pgvector (inherits Postgres's WAL, MVCC and streaming replication wholesale [18]), Elasticsearch (the translog, with `index.translog.durability` selecting `request` — fsync per request — or `async` [19]).

**Cost:** you own every hard problem — torn writes, fsync semantics, replication, catch-up, failover, rejoin. **Benefit:** sub-millisecond writes, and no operational dependency.

### 2.2 Externalized log — a log service owns durability

The database does not write a durable log at all. It publishes to a distributed log, and every node is a stateless subscriber. Durability, ordering and replication are the log's problem.

**Who does this:** Milvus. Its architecture puts a message queue (originally Pulsar or Kafka) on the write path as the system of record; compute nodes subscribe, build in-memory segments, and periodically flush sealed segments to object storage [4][5]. Its "replicas" are read-scaling caches, not durability mechanisms [7].

**This is the most interesting decision in the space, and Milvus has since partly reversed it.** They replaced Kafka/Pulsar with a purpose-built log service called Woodpecker, explicitly because operating an entire additional distributed system as "just the WAL" was too heavy [9]. And the reversal has a price tag: Woodpecker's object-storage-backed mode measures **166 ms** write latency against **1.8 ms** on local disk [9]. That single pair of numbers is the cost of not owning your own log.

There is a second, subtler cost. When the log is the system of record but object storage holds the queryable state, a write can be acknowledged (it is in the log) yet absent after a restart (it was never flushed). Milvus's issue tracker carries a recurring "data lost after restart" pattern consistent with exactly that gap [6][11].

### 2.3 Object storage as the log — no local durability step at all

Writes go to object storage directly; the local disk is a cache for read latency and nothing else. Durability is S3's problem.

**Who does this:** turbopuffer and LanceDB [22][25]. turbopuffer publishes its own trade-off page, which is unusually honest: p50 latency of roughly 165–200 ms to durably commit a write, traded deliberately for throughput, cost and multi-tenant elasticity [23][24].

**What it buys:** storage-compute separation, near-zero recovery time, trivial elasticity, and an order of magnitude lower cost per stored vector. **What it costs:** two orders of magnitude of write latency, and a new class of correctness problem — one independent builder documented a "zombie WAL" metadata-merge race in exactly this design [26]. It is not a free lunch; it is a different lunch.

### 2.4 The decision

> **vecdb owns a local WAL.** Architecture 2.1.

Not because it is best — for a real product in 2026, 2.3 is probably the right answer and 2.1 is the most work for the least differentiation. But your goal is to understand the mechanism, and architectures 2.2 and 2.3 work by **outsourcing precisely the mechanism you want to understand**. Choosing them would leave you with a system that runs and an engineer who has not learned fsync.

The other two architectures do not disappear from the project. They become the first two STUDY milestones (§5), because "why did Milvus externalize its log, and why did it partly walk that back" is a better staff-interview answer than anything you could build in the same hours.

---

## 3. Corrections to the previous design

These are changes, not clarifications. Each has a reason and each is now locked.

### C1 — Raft governs the control plane only. It does not replicate data.

`00-system-design.md` §13.3 specifies multi-raft: one Raft group per shard, plus a meta group. That is what CockroachDB and TiKV do, and it is the textbook answer.

**It is not what the vector databases closest to your design do.**

Qdrant runs Raft for cluster metadata — shard placement, replica state transitions such as `SetShardReplicaState` and `TransferShard` — while point writes **bypass Raft entirely**, fanning out directly to replicas gated by a `write_consistency_factor` [2][6]. Weaviate moved its schema to Raft in v1.25 specifically because the pre-Raft design could not handle concurrent schema changes [14], while data replication remains leaderless Dynamo-style with tunable ONE/QUORUM/ALL [13].

Two independently built systems, in two different languages, converged on the same split: **consensus for the control plane, something cheaper for the data plane.** That is a strong signal, and it is a better design for your purposes on three counts:

- **Cost.** One Raft group instead of `S`. With S=4 that is a quarter of the heartbeat traffic, a quarter of the state machines, and — crucially — one place where a Raft bug can live instead of four. It turns a 150–300 hour milestone into a 40–60 hour one.
- **Pedagogy.** You still write Raft from scratch. You still confront terms, elections, the log-matching property, the Figure-8 commit rule and log divergence. You just apply it to the object that actually needs distributed agreement — *who leads which shard* — rather than to the data path, where a leader plus fencing plus an in-sync set already gives you what you need.
- **Honesty.** Multi-raft over four shards on three nodes is theatre. Every group would have the same three members and elect on the same failure. You would be paying full price for a demonstration that proves nothing the meta group does not already prove.

**Consequence:** the RF=2→3 constraint in §13.1 of the old document **disappears.** That constraint existed because a majority of 2 is 2, so a Raft data group at RF=2 would halt on any single failure. With Raft confined to the meta group (3 members, all nodes, tolerating one failure), the data plane keeps leader/follower replication and RF stays at 2. Sharding therefore keeps doing real work: with N=3, S=4, RF=2, each node holds roughly 2.67 shard-replicas, not all of them.

### C2 — Rejoin and reconciliation is promoted to a first-class milestone.

The old plan buries this in M9 as "recovery hardening." The evidence says it is where real systems actually break.

Qdrant's production data-consistency bugs were not in its WAL and not in its documented consistency trade-off. They were in the rejoin path: upserts partially committed during node restart [5], node restart during deletes leaving points with empty payloads [9], and data inconsistent after migration [8]. Weaviate has two open issues reporting **actual data loss on an ordinary pod restart** in the 1.26.x line — "empty WAL found," data gone, schema intact [15][16].

This is the single most valuable thing in the project, for three reasons. It is where the field's real bugs are. It is cheap to build — a few hundred lines on top of replication you already have. And it is brutal to test, which means building it teaches you fault injection properly.

**It gets its own week.**

### C3 — Adopt Elasticsearch's naming for the failover triad.

Your design already invented all three mechanisms. Elasticsearch shipped them, named them, and wrote them up. Rename yours to match, so the mapping to production is explicit rather than something you have to reconstruct in an interview.

| Your name (`00-system-design.md`) | Elasticsearch's name | What it does |
|---|---|---|
| `Epoch` | **primary term** [21] | Monotonic fence, bumped on every leadership change. A message stamped with a stale term is rejected. |
| `LSN` | **sequence number** [21] | Per-shard, strictly increasing, assigned by the leader. Gives a total order and a replication cursor. |
| ISR membership | **in-sync allocation IDs** [22] | The set of copies eligible for promotion. A copy not in the set is never promoted. |

Keep your identifiers in code (`Epoch`, `LSN`, `ISR` read better in Go), but document the correspondence in the README. The point is that you did not invent a toy — you re-derived the protocol a production search engine uses, and you can say so with a citation.

Worth knowing why Elasticsearch built them: Jepsen's analysis of Elasticsearch 1.5.0 found up to 25% of acknowledged writes lost under partition, and roughly 10% lost on a plain process crash because acknowledgements were issued before fsync [24]. The sequence-number and primary-term work was the answer. This is the best available worked example of *why* the design you are building looks the way it does, and it belongs in your final README.

### C4 — The ANN index track is dropped from this project, not deferred.

`02-roadmap-revision.md` proposes two tracks: an index library (I1–I6: brute force, KD-tree, IVF, PQ, HNSW, SIMD kernels) alongside the distributed system. That document argued the tracks do not block each other, which is true, and that exactly one should be active at a time, which is also true — and then it did not follow through on what that implies at a fixed budget.

At 300 hours, a second track does not run "alongside." It competes for the same hours, one to one. And the two tracks are different disciplines: the index track is performance engineering (SIMD, cache behaviour, recall curves, statistical benchmarking); the distributed track is correctness engineering (durability, ordering, agreement, failure). Both are worth knowing. Neither is learned by alternating.

> **vecdb ships brute-force search, permanently. `index.Index` stays an interface with one implementation.**

Not "HNSW later." Dropped. If it is deferred, it sits at the end of the plan generating guilt and quietly redefining "done" as unreachable — which is the exact failure mode this document exists to fix.

**Three things make this the right call and not a loss:**

1. Brute force is the *correct* index for the distributed layer. §11.2 of the old document proves that per-shard top-K merges to an exact global top-K only when per-shard search is exact. An approximate index breaks that proof and forces an over-fetch factor chosen on evidence you would not have. Debugging distributed search and approximate search simultaneously is a trap, and the old document says so itself.
2. The ANN material is genuinely better learned by reading than by building, at this budget. Two STUDY milestones (§5) cover IVF, PQ, HNSW, DiskANN and the segment-versus-mutable-graph question with real numbers.
3. If you still want to build HNSW afterwards, it is a clean standalone project with a frozen interface waiting for it. That is a feature of having finished vecdb, not a consolation prize.

### C5 — Cluster shape is locked: N=3, S=4, RF=2. No S=12, no 5-node profile.

§6.4 of the old document recommends switching to S=12 at the rebalancing milestone, and §13.1 floats a 5-node compose profile. Both were reasonable when rebalancing was a BUILD milestone. It is now a SPIKE (§5), so the extra shard count buys a benchmark comparison you will not have time to run properly.

`S` remains a config value and the tests remain parameterized over it — that was correct and costs nothing. You will simply never run it at 12 in this project.

### C6 — `SyncAlways` stays the default, and group commit moves into the WAL milestone.

Unchanged in substance, but the sequencing moves. `00-system-design.md` puts group commit at M13, at the very end. That is too late: group commit falls almost free out of the single-writer-goroutine design in §7.5, and having it in week 1 means every subsequent benchmark in the project measures a realistic write path instead of a pathological one. Build it with the WAL, behind a flag, and measure both.

---

## 4. The locked design

Everything in `00-system-design.md` §1–§17 stands, as amended by §3 above. This section states only what changed or what needed sharpening. Read it as a diff.

### 4.1 Scope

```
                            CLIENT
                              │  gRPC
                              ▼
                    ┌──────────────────┐
                    │   ANY NODE       │  ← coordinator role, stateless
                    │  reads shard map │
                    └────────┬─────────┘
                             │  scatter, deadline-bounded
        ┌──────────┬─────────┴──────┬──────────┐
        ▼          ▼                ▼          ▼
     SHARD 0    SHARD 1          SHARD 2    SHARD 3
        │          │                │          │
   leader+ISR  leader+ISR      leader+ISR  leader+ISR      RF=2
        │          │                │          │
       WAL        WAL              WAL        WAL          local, fsync
        │          │                │          │
    map+index  map+index        map+index  map+index       brute force
        └──────────┴────────┬───────┴──────────┘
                            ▼
                      GLOBAL TOP-K

              ┌───────────────────────────┐
              │  META RAFT GROUP (3/3)    │  ← the ONLY consensus
              │  owns: shard map,         │
              │  membership, leadership,  │
              │  ISR membership           │
              └───────────────────────────┘
```

**In scope, built:** insert/get/delete/search; local WAL with fsync and group commit; snapshots and crash recovery; hash sharding; leader/follower synchronous replication with an in-sync set; heartbeat failure detection; epoch-fenced automatic failover; replica rejoin and reconciliation; scatter-gather distributed search with deadlines and partial-result flagging; a Raft-replicated control plane; Prometheus metrics and structured logging; a chaos suite; benchmarks.

**Out of scope, permanently:** approximate indexing of any kind; metadata-filtered search; quantization; multi-tenancy; auth/TLS; multi-datacenter; backup/restore; out-of-core storage.

### 4.2 The write path — this is the crown jewel, so it is specified exactly

```
CLIENT              LEADER (shard 1, epoch 7)                FOLLOWER
  │
  │── Insert ──────►│ 1. validate: dim, id length, metadata size
  │                 │ 2. am I leader for shard(id) at map version V?
  │                 │       no → NotLeader{leader, epoch, version} ──► client retries
  │                 │ 3. enqueue on the single writer goroutine
  │                 │ 4. lsn = ++nextLSN          ← total order established here
  │                 │ 5. wal.Append(rec)          ← buffered
  │                 │ 6. wal.Sync()               ← DURABLE ON LEADER
  │                 │       (group commit: steps 3-6 batch across
  │                 │        concurrent writers, one fsync per batch)
  │                 │ 7. apply to map + index     ← leader can now serve STRONG reads
  │                 │
  │                 │── Replicate{epoch:7, from:lsn} ──►│ a. epoch >= mine? else REJECT
  │                 │                                    │ b. lsn == applied+1? else CATCHUP
  │                 │                                    │ c. wal.Append + wal.Sync ← DURABLE
  │                 │                                    │ d. apply
  │                 │◄──────── Ack{applied_lsn} ─────────│
  │                 │ 8. all ISR acked → commit_lsn = lsn
  │◄─ OK{version} ──│ 9. ack client
```

Three invariants, each of which gets a test:

- **The log is never behind the state.** fsync (6) precedes apply (7). Violating this loses acknowledged writes on crash, sometimes, depending on whether a read observed them first — the worst kind of bug.
- **Reads never observe uncommitted records.** Reads filter on `version <= commit_lsn`. Without this, a failover can make a previously-read value vanish.
- **Acknowledged implies durable on every ISR member.** This is what makes "kill the leader, assert nothing is lost" a meaningful test rather than a flaky one.

**The hard rule on promotion, restated because it is the whole design:** a replica that was not in the ISR at the moment of the leader's failure is **never** promoted. If no in-sync replica survives, the shard goes read-only and alerts. Unavailability is chosen over silent data loss. This is exactly what Elasticsearch's in-sync allocation IDs enforce [22], and it is the difference between a database and a cache.

### 4.3 Rejoin and reconciliation — specified, because §3-C2 promoted it

A node returns after a crash, a restart, or a partition. It holds local state at some `(shard, epoch_old, applied_lsn_old)`. Four cases, and each is a test:

| Case | Condition | Action |
|---|---|---|
| **Clean catch-up** | `epoch_old == current`, `applied_lsn_old <= leader.commit_lsn`, and the gap is within the leader's retained WAL | Log-ship from `applied_lsn_old + 1`. Rejoin ISR when caught up. |
| **Snapshot required** | Same, but the leader has already truncated past `applied_lsn_old` | Snapshot transfer at `snapshot_lsn`, then log-ship the tail. |
| **Divergent tail** | `epoch_old < current` and `applied_lsn_old > leader.commit_lsn` | The node holds records from a dead epoch that were never acknowledged. **Truncate to `leader.commit_lsn`, then catch up.** Nothing is lost, because nothing above `commit_lsn` was ever acked. |
| **Genuine corruption** | An LSN gap inside a valid checksummed record | **Refuse to start.** Log loudly. Recovery is manual: wipe the data directory, take a full snapshot transfer. Never repair silently. |

**Why this deserves its own week:** every one of Qdrant's real consistency bugs lived here [5][8][9], and Weaviate's open data-loss issues are ordinary-restart failures [15][16]. The mechanism is simple; getting it right under concurrent writes, repeated restarts and overlapping failures is not. The test that catches these is not a unit test — it is a chaos loop that kills and restarts nodes at random while a client writes continuously and a verifier asserts that every acknowledged write is still readable.

### 4.4 Consistency — unchanged, but with a comparison you now owe the reader

The guarantees stay exactly as in `00-system-design.md` §3.2: per-key linearizability for writes and STRONG reads, opt-in bounded-staleness reads, search eventually consistent and not atomic across shards, no cross-key atomicity, undefined outcome for un-acknowledged writes.

What is new is that you can now defend it comparatively, which is what a staff interview actually probes:

- **Milvus** exposes four consistency levels (Strong, Bounded Staleness, Session, Eventually), all implemented as different values of a single `GuaranteeTs` watermark [2] — one mechanism, four names.
- **Vespa** ships **eventually consistent** and resolves conflicts by **wall-clock last-writer-wins at whole-document granularity** [13], with no epoch or term concept at all. A serious production system chose weaker than you are choosing, and shipped it.
- **Qdrant** exposes `write_consistency_factor` and returns `Acknowledged` rather than `Completed` on a partial write, explicitly telling the client to resubmit [2][6] — availability over atomicity, stated as policy.
- **Elasticsearch** enforces promotion eligibility through in-sync allocation IDs [22] — the model you are building.

Four systems, four different answers, all defensible. Being able to say *why* each chose what it chose is the deliverable.

### 4.5 What the index is, and why the answer is boring

Brute force. `O(N·D)` per query, memory-bandwidth-bound, cosine default with L2² also implemented, top-K by bounded heap, exact.

One thing worth carrying from the research: **Lucene builds an HNSW graph per immutable segment, so a query must search every segment's graph and merge the results — query cost scales with segment count, and merging vector graphs is a distinct, still-actively-optimized problem** [26][27]. Vespa took the opposite route, choosing a real-time mutable HNSW specifically to avoid this [16]. Milvus builds once per sealed segment [4]. That three-way split — per-segment immutable, mutable in place, build-on-seal — is the real architectural question in ANN persistence, and it is worth more to you as a STUDY milestone than a hand-rolled HNSW would be.

---

## 5. The plan

Eight weeks. Roughly 40 hours a week: **32 hours building, 4 hours on the teardown track, 4 hours on review and writing.** The teardown track is not optional and not filler — it is where requirement 2 of §0 is satisfied.

### 5.1 Milestone tiers

| # | Milestone | Tier | Hours |
|---|---|---|---|
| M0 | Skeleton: layout, `go.mod`, Makefile, lint, CI, `slog`, config | BUILD | 8 |
| M1 | Core types, distance kernels, top-K, in-memory shard | BUILD | 16 |
| M2 | WAL: codec, segments, sync policies, group commit, truncating replay | BUILD | 32 |
| M3 | Persistent engine: snapshots, recovery, tombstones, log truncation | BUILD | 28 |
| M4 | gRPC service, `.proto`, metrics, health, Docker, `vecctl` | BUILD | 20 |
| M5 | Sharding in-process + coordinator scatter-gather + merge | BUILD | 16 |
| M6 | 3-node cluster: remote fan-out, deadlines, request IDs, retries | BUILD | 20 |
| M7 | Replication: leader/follower, sync, ISR, log shipping, snapshot transfer | BUILD | 36 |
| M8 | Failure detection, controller, epoch fencing, leases, failover | BUILD | 32 |
| **M9** | **Rejoin and reconciliation + chaos suite** | **BUILD** | **32** |
| M10 | Rebalancing: learner role, snapshot + catch-up, cutover | SPIKE | 12 |
| M11 | Raft for the control plane + deterministic simulation harness | BUILD | 48 |
| M12 | Benchmarks, final documentation, self-review | BUILD | 20 |
| S1 | Externalized-log and object-storage architectures | STUDY | 6 |
| S2 | IVF, PQ and the quantization ladder | STUDY | 6 |
| S3 | HNSW, DiskANN, and ANN persistence strategies | STUDY | 8 |
| S4 | Filtered and hybrid search | STUDY | 5 |
| S5 | Consistency models and the CAP/PACELC literature | STUDY | 5 |
| S6 | Benchmarking methodology and its pathologies | STUDY | 4 |

Total: ≈ 300 hours excluding the teardown track's own 32, which overlaps S1–S6.

### 5.2 Week by week

**Week 1 — foundations and the log.** M0, M1, M2.
The WAL is the highest-leverage code in the project and gets four full days. Record format with CRC32-Castagnoli, segments named by first LSN, `SyncAlways` default, group commit behind a flag from day one. The acceptance test that matters: **take a valid WAL, truncate it at every single byte offset, and assert clean recovery at every one.** That one table-driven test catches most WAL bugs that exist. Benchmark fsync latency with and without group commit, and commit both numbers with the machine spec.
*Teardown:* S1 — read Milvus's log-broker architecture and its Woodpecker reversal [4][9], and turbopuffer's trade-off page [23]. Write 800 words on who owns durability and what each choice costs. You now understand your own architecture by contrast.

**Week 2 — persistence and the service.** M3, M4.
Snapshot to `.tmp`, fsync, rename, fsync the directory, only then truncate the log. Recovery is snapshot plus tail replay, and it must be a pure function of what is on disk. The acceptance test: `kill -9` under sustained load, restart, assert **zero acknowledged writes lost** and byte-identical state. Then wrap it in gRPC and a container.
*Teardown:* S5 — Jepsen on Elasticsearch 1.5.0 [24], Elastic's sequence-numbers writeup [21], and the in-sync-copies writeup [22]. Write 800 words on what "acknowledged" means in four different systems.

**Week 3 — distribution.** M5, M6.
The partitioner, the static shard map, the coordinator. The correctness test that matters: **global top-K over S shards is byte-identical to single-shard brute force over the same data**, including tie-breaking by `(score, id)`. Then three nodes in compose, real fan-out, real deadlines, request IDs threaded through.
*Teardown:* S6 — benchmarking methodology. The measurement pathologies are real: published ANN throughput figures are often literally `1/latency` rather than concurrent throughput, and fixing warm-up, timing and threading has been shown to move measured QPS by 20× with no change to the system under test. Write down the rules you will hold your own benchmarks to.

**Week 4 — replication.** M7.
Leader/follower, synchronous to all ISR members, log shipping, snapshot transfer, the ISR ejection rule with `T_sync`. The test: kill the leader immediately after an acknowledgement and assert the data is on the survivor. Replication lag as a live metric.
*Teardown:* S2 — IVF and quantization. `nlist`/`nprobe`, the boundary effect that puts a hard ceiling on recall, product quantization's code-size arithmetic, and the ADC lookup table. 800 words.

**Week 5 — failure.** M8.
Heartbeats, the failure detector, the controller, epoch fencing, leader leases, automatic failover. Measure the failover window with a client that writes continuously while you kill the leader — that number goes in the README. Confront the split-brain window honestly: pre-Raft, a partitioned leader can serve stale STRONG reads until it learns otherwise, and leases bound but do not eliminate it.
*Teardown:* S3 — HNSW and DiskANN. The layer structure, `M`/`efConstruction`/`efSearch`, the diverse-neighbour heuristic, α-pruning, and the 2025 argument that the hierarchy adds little over a flat graph at high dimension. Plus the three ANN-persistence strategies from §4.5.

**Week 6 — rejoin.** M9. **The most valuable week in the plan.**
All four cases from §4.3, and a chaos suite that runs them concurrently: kill and restart nodes at random under sustained write load, with a verifier asserting every acknowledged write remains readable and no two replicas diverge. Corrupt WAL handling. Divergent-tail truncation. Re-replication to restore RF.
*Teardown:* S4 — filtered and hybrid search. Pre-, post- and in-filter strategies and the exact failure of each; the graph-disconnection problem; ACORN; RRF. 800 words.

**Week 7 — consensus.** M11, and M10 as a spike if time allows.
Raft for the meta group only: terms, randomized election timeouts, `RequestVote` with the log up-to-dateness check, `AppendEntries` with the `prevLogIndex`/`prevLogTerm` consistency check, `commitIndex` advanced only on current-term entries, log divergence repair by `nextIndex` backtracking. Then the deterministic simulation harness — one process, seeded RNG, virtual clock, injected drop/delay/reorder/partition — and thousands of randomized schedules asserting no split brain and no lost committed entry.
Two things to know going in. First, Raft as published **does not guarantee liveness under partial partitions**; PreVote and CheckQuorum are required in practice and are near-universal in production libraries. Implement them. Second, deterministic simulation of the kind FoundationDB pioneered has found real safety and liveness bugs in every major Raft library it has been pointed at — so expect your first implementation to be wrong, and build the harness that proves it.

**Week 8 — measure, document, review.** M12.
The benchmark ladder at D=128, N ∈ {10K, 100K, 1M} and D=768 up to 100K. Open-loop load generation, p50/p95/p99/max, never a bare mean. Every optimization gets a before/after table. Then the final document: architecture, consistency model, failure model, trade-offs, benchmarks, and the honest limitations list from §21 of the old document, verbatim.
Then the review pass in §6.

### 5.3 The teardown track — how to make it count

Four hours a week, one topic, one output. The output is **not notes**. It is a short document that answers, for that topic:

1. What is the mechanism, precisely enough that you could implement it?
2. How do two or three production systems implement it, and where do they differ?
3. What does each trade-off cost, in a number if a number exists?
4. Where has it been publicly wrong — a Jepsen report, an open data-loss issue, a documented reversal?
5. What would you choose, and under what conditions would you choose differently?

Question 4 is the one that separates a staff-level answer from a senior one. "Elasticsearch uses sequence numbers and primary terms" is a fact. "Elasticsearch built sequence numbers and primary terms because Jepsen showed 1.5.0 losing up to 25% of acknowledged writes under partition and ~10% on plain crash, since acks preceded fsync" [24] is understanding.

Six teardowns, plus your existing conceptual README, is the written half of the deliverable. It is the half that survives when the code goes stale.

---

## 6. What "passes an expert's code review" means

You named this as half of "done," so here is the checklist. Run it yourself in week 8 before you show anyone.

**Durability — the reviewer will attack this first.**
- [ ] `f.Write()` is never mistaken for durability, and `bufio.Flush()` is never mistaken for `Sync()`.
- [ ] A newly created WAL segment is followed by an fsync of the **parent directory**. Almost every from-scratch WAL gets this wrong.
- [ ] A failed `Sync()` is treated as non-retryable: the shard stops accepting writes. (The PostgreSQL fsyncgate lesson — on Linux, a failed fsync may have already dropped the dirty pages, and a retried fsync can report success on data that never landed.)
- [ ] Truncate-at-every-byte-offset recovery test exists and passes.
- [ ] A valid, checksummed record with a wrong LSN causes a refusal to start, never a silent repair.
- [ ] Snapshot is written to `.tmp`, fsynced, renamed, directory fsynced, and only then is the WAL truncated.
- [ ] WAL truncation never passes the slowest ISR member's `applied_lsn`.
- [ ] Tombstones are only dropped once every ISR member has seen them.

**Concurrency.**
- [ ] `go test -race -count=1 ./...` is green in CI, every commit.
- [ ] Every goroutine is owned by a component, takes a `ctx`, and is joined in `Close()`. No naked `go func()` anywhere.
- [ ] Every piece of mutable state has exactly one owner — a mutex scoped to one struct, or a single goroutine. Never both, never ambiguous.
- [ ] The read path's lock behaviour under a long scan has been **measured**, not assumed. A 1M-vector scan holding `RLock` will starve writers; if you have not seen that in a benchmark you do not know whether you have it.

**Distributed correctness.**
- [ ] Every RPC carries a deadline. Every fan-out derives its context so one cancel tears down all of it.
- [ ] Every replication RPC carries `(shard, epoch, lsn)` and a stale epoch is rejected.
- [ ] No non-ISR replica can be promoted, and there is a test that tries.
- [ ] Retries are idempotent by construction, and there is a test that applies the same LSN twice.
- [ ] Search results are deterministic: total order by `(score, id)`, verified across shard counts and thread counts.
- [ ] Partial results are impossible unless explicitly requested, and are always flagged when returned.

**Claims.**
- [ ] No performance number in the README lacks a committed benchmark and a machine spec.
- [ ] No latency is reported as a mean without percentiles beside it.
- [ ] The limitations section is present, honest, and specific.

That last group is the one people skip, and it is the one a staff-level reviewer notices first. A project that says "search p99 is 14 ms at N=100K, D=128, S=4, RF=2, `SyncAlways`, on a 2022 M1 Pro, benchmark `BenchmarkSearch100K`" is more credible than one claiming to be fast.

---

## 7. What this project will not teach you

Stating this is part of finalizing. Four to eight weeks buys depth in one direction, and you should know which directions you are not going.

| Not covered | Why | How to get it later |
|---|---|---|
| ANN performance engineering — SIMD, cache layout, recall curves | Dropped in §3-C4; it is a different discipline | A standalone index-library project against the frozen `index.Index` interface. Two to three weeks. |
| Storage–compute separation, object-storage-native design | Rejected in §2.4 for learning reasons | Covered as STUDY (S1). Build later if you want it. |
| Query planning, filtering, hybrid retrieval | Out of scope | Covered as STUDY (S4). |
| Multi-tenancy, auth, quotas, admission control | Out of scope, and mostly product work rather than systems work | On the job. |
| Operating a database at scale — capacity, upgrades, incidents | Requires production traffic, which a laptop cluster cannot fake | On the job, or by reading incident writeups. |
| Byzantine fault tolerance | Explicitly excluded in the failure model | Rarely relevant outside blockchains. |

If someone asks what your system does not do, the answer above is a better one than most people can give about systems they have shipped.

---

## 8. Decisions

There is one, and I have a recommendation.

**Your GitHub handle for the module path.** I will use `github.com/eskhakha/vecdb`, inferred from your existing C++ repository. Say otherwise if you want a different name.

Everything else in §3 is decided and locked. The old §20 list is closed:

1. Shard count → **S=4, fixed.** (§3-C5)
2. RF at the Raft milestone → **RF stays 2. The constraint that forced 3 no longer applies.** (§3-C1)
3. Default metric → **cosine**, with L2² implemented from M1.
4. Default sync policy → **`SyncAlways`**, with group commit measured alongside from week 1. (§3-C6)
5. Partial search results → **`allow_partial=false`**, opt-in per request, always flagged.
6. Benchmark ladder → **10K/100K/1M at D=128; 10K/100K at D=768.**
7. Windows testing → **all builds and tests run in Linux containers or WSL2.** One environment where results count.
8. Module path → **the one open question above.**
9. Pace → **M0 through M2 in week 1**, stopping for review at the end of each week rather than each milestone. Weekly is the right review cadence at this budget; per-milestone is too slow.

---

## Sources

[1] Storage — Qdrant documentation — https://qdrant.tech/documentation/concepts/storage/
[2] Consistency Guarantees — Qdrant documentation — https://qdrant.tech/documentation/scaling/consistency-guarantees/
[4] Data Insertion and Data Persistence in a Vector Database (Deep Dive 4) — Zilliz/Milvus engineering blog — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md
[5] Upserts partially committed during node restart — qdrant/qdrant issue #4626 — https://github.com/qdrant/qdrant/issues/4626
[6] Write Consistency and Replication (source-derived) — https://deepwiki.com/qdrant/qdrant/6.3-write-consistency-and-replication
[7] In-Memory Replica — Milvus documentation v2.5.x — https://milvus.io/docs/v2.5.x/replica.md
[8] Data inconsistent after migration — qdrant/qdrant issue #5101 — https://github.com/qdrant/qdrant/issues/5101
[9] We Replaced Kafka/Pulsar with a Woodpecker for Milvus — Zilliz/Milvus engineering blog — https://milvus.io/blog/we-replaced-kafka-pulsar-with-a-woodpecker-for-milvus.md
[11] Milvus data-loss-after-restart issue cluster (#38284, #40418, #41601, #25878, #30501, #30254, #21527) — https://github.com/milvus-io/milvus/issues
[12] Storage — Weaviate documentation — https://docs.weaviate.io/weaviate/concepts/storage
[13] Consistency — Weaviate replication architecture — https://docs.weaviate.io/weaviate/concepts/replication-architecture/consistency
[14] Manage cluster metadata with RAFT for better reliability — weaviate/weaviate issue #4280 — https://github.com/weaviate/weaviate/issues/4280
[15] Data loss in 1.26.4 whenever pod restarted — weaviate/weaviate issue #7162 (open) — https://github.com/weaviate/weaviate/issues/7162
[16] After upgrade 1.26.1 to 1.26.4, all data lost except schema — weaviate/weaviate issue #5869 (open) — https://github.com/weaviate/weaviate/issues/5869
[18] HNSW Indexes with Postgres and pgvector — Crunchy Data — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector
[19] Translog — Elasticsearch Guide 8.19 — https://www.elastic.co/guide/en/elasticsearch/reference/8.19/index-modules-translog.html
[21] Sequence IDs: Coming Soon to an Elasticsearch Cluster Near You — Elastic engineering blog — https://www.elastic.co/blog/elasticsearch-sequence-ids-6-0
[22] Elasticsearch Internals: Tracking in-sync shard copies — Elastic engineering blog — https://www.elastic.co/blog/tracking-in-sync-shard-copies
[23] Tradeoffs — turbopuffer documentation — https://turbopuffer.com/docs/tradeoffs
[24] Jepsen: Elasticsearch 1.5.0 — Kyle Kingsbury — https://aphyr.com/posts/323-jepsen-elasticsearch-1-5-0
[25] Storage Architecture in LanceDB — https://docs.lancedb.com/storage
[26] Lucene vector search: speeding up multi-graph vector search — Elasticsearch Labs — https://www.elastic.co/search-labs/blog/multi-graph-vector-search
[27] HNSW graphs: speeding up merging — Elasticsearch Labs — https://www.elastic.co/search-labs/blog/hnsw-graphs-speed-up-merging

Vespa's consistency model (eventually consistent, wall-clock last-writer-wins at document granularity) is documented at https://docs.vespa.ai/en/content/consistency.html; its real-time mutable HNSW at https://docs.vespa.ai/en/querying/approximate-nn-hnsw.html; and the open cluster-controller test-and-set gap at https://github.com/vespa-engine/vespa/issues/4532.

---

*End. §8 has one open question. Everything else is decided — start M0.*
