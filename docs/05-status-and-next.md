# Status and next steps

**Supersedes the milestone table in `04-final-design-and-plan.md` §5.1.** Everything else in that
document — the design, the corrections in §3, the review checklist in §6 — stands.

---

## What changed

You said: learning and demo, not production; minimal code; no real data. Three things follow.

**The cluster runs in one process.** Not Docker, not gRPC, not real networking. `S` shards owned
by one binary. This removes most of the plumbing hours, needs no infrastructure, and is *better*
for the stated goal: fault injection becomes a function call, which is how FoundationDB and
TigerBeetle actually test consensus. The transport becomes an interface later, and a real gRPC
implementation is a swap, not a rewrite.

**Synthetic data only.** N = 10K–100K random vectors, dim 64. No SIFT-1M, no benchmark ladder.
Because the index is exact, synthetic data is sufficient for correctness. It would *not* be
sufficient for recall, which is one more reason the ANN track stays out.

**Benchmarks only where they make a trade-off visible.** Not "how fast is it" but "what does this
choice cost". Three of them exist already and all three earned their place.

Revised budget: roughly **120 hours**, not 300.

---

## Done

| | |
|---|---|
| M0 Skeleton | Go module, no third-party dependencies at all, Makefile, `go vet` clean |
| M1 Vector engine | core types, cosine + L2² kernels with bounds-check elimination, bounded top-K heap, exact brute-force index |
| M2 WAL | CRC32-Castagnoli, magic + length prefix, segments, group commit, truncating replay, poisoned-log-on-failed-fsync |
| M3 Persistent engine | shard = map + index + WAL + snapshot + recovery; tombstones; log truncation |
| M5 Sharding | stable FNV-1a partitioner, fixed logical shards |
| M5b Coordinator | scatter, gather, exact global top-K merge, partial-result policy |

`go test -race ./...` is green. 3,117 lines including tests. `go run ./cmd/vecdemo -crash`
acknowledges 10,001 writes, hard-exits, recovers, and loses none.

**Measured, on a 2-core container:**

- fsync per write: 167,650 ns/op. Group commit at batch 64: 6,682 ns/op. **25× for free**, with
  no change to the durability contract.
- Search throughput stays flat at 4–5 GB/s as N grows 100×, which is the signature of a
  memory-bandwidth-bound loop rather than a compute-bound one.

**Two real bugs, both worth the time they cost:**

1. Map iteration order made record encoding non-deterministic — two replicas would have written
   different bytes for the same write, making byte-level log comparison impossible. A test caught
   it.
2. A closed shard still served searches, so the coordinator's partial-result path never fired.
   The *demo* caught it, not the tests. Worth remembering: a scripted end-to-end story finds
   things unit tests do not.

---

## Remaining plan

Weeks are nominal at ~35 focused hours.

| # | Milestone | Tier | Est. | The thing to actually learn |
|---|---|---|---|---|
| **M7** | Replication: leader/follower, sync to in-sync set, log shipping, snapshot transfer | BUILD | 24 h | "Acknowledged implies durable on every in-sync replica" as a testable assertion. Kill the leader after an ack, assert nothing is lost. |
| **M8** | Failure detection, epoch fencing, failover | BUILD | 20 h | That every timeout is a guess, and that the epoch is what stops a wrongly-suspected leader corrupting the new one's log. |
| **M9** | Rejoin and reconciliation, chaos loop | BUILD | 24 h | **The highest-value week.** This is where Qdrant's and Weaviate's real production bugs live. Four cases: clean catch-up, snapshot required, divergent tail, genuine corruption. |
| **M11** | Raft for the control plane + deterministic simulation | BUILD | 32 h | Terms, elections, log matching, the current-term commit rule. One group governing the shard map — not multi-raft, which over three nodes is theatre. |
| **M10** | Rebalancing: learner, snapshot, catch-up, cutover | SPIKE | 8 h | The cutover moment. Skipping the learner role and just copying data teaches nothing. |
| **M12** | Trade-off benchmarks, final write-up | BUILD | 12 h | sync vs async replication; S=4 vs S=8; recovery time vs log size. |

**Next up: M7.** The interface is already in place — `Shard.apply` is shared by the leader path,
the follower path and recovery, which is exactly what makes replication a small addition rather
than a second write path.

---

## The concepts pack

13 of 22 files written (`01`–`13`): foundations, metrics, the search problem, brute force and
kernels, trees and LSH, IVF, quantization, HNSW and graph indexes, updates and deletes, index
selection, filtering and hybrid search, storage engines, write-ahead logging, recovery and
compaction.

Each carries a **"How real systems do it"** section — Milvus, Vespa, Elasticsearch, Qdrant,
Weaviate, pgvector, turbopuffer, LanceDB, and where each was publicly wrong — and a **"Minimal
demo version"** section saying what you can cut and what you cannot cut without the demo becoming
a lie.

Remaining: `14`–`22` (sharding, replication, consistency, distributed search, failure detection,
consensus and Raft, rebalancing, observability, benchmarking). Those nine map exactly onto M7–M12,
so the natural order is to write each one in the week you build the thing it describes. The
research corpus for all of them is already gathered.
