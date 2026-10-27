# vecdb

A distributed vector database built from scratch in Go, for **learning**. It is deliberately
minimal: enough machinery to make each concept real and testable, and nothing added for
production polish that does not teach something.

The reading material lives in [`concepts/`](../concepts/) — 22 files covering the search side
(similarity, IVF, quantization, HNSW, filtering), the durability side (storage engines, WAL,
recovery), and the distributed side (sharding, replication, consistency models, distributed search,
failure detection, consensus, rebalancing, observability, benchmarking). This README covers the code.

```
go test -race ./...        # everything, including the crash-recovery suite
go run ./cmd/vecdemo           # the whole system as a scripted story
go run ./cmd/vecdemo -crash    # hard-exit mid-load, then prove nothing was lost
go run ./cmd/vecdemo -replicate   # ISR, partition, catch-up, failover, rejoin
go run ./cmd/vecdemo -rebalance   # move a shard under load, safely and unsafely
go run ./cmd/vecdemo -splitbrain  # a partitioned controller, with and without consensus
```

Three documents sit alongside the code: [BENCHMARKS.md](BENCHMARKS.md) for measured numbers and
their caveats, [LESSONS.md](LESSONS.md) for every bug and every measurement that contradicted the
design, and [`concepts/`](../concepts/) for the reference material.

## What it does today

| Component | Status | Where |
|---|---|---|
| Exact top-K search, cosine and squared L2 | done | `internal/index` |
| Write-ahead log: CRC32C, segments, group commit, truncating replay | done | `internal/wal` |
| Shard: data map + index + WAL + snapshots + recovery | done | `internal/store` |
| Hash partitioning into fixed logical shards | done | `internal/cluster` |
| Scatter-gather distributed search with exact global top-K | done | `internal/coordinator` |
| Epoch fencing (leadership term) | done | `internal/store`, `internal/replication` |
| Synchronous leader/follower replication with an in-sync set | done | `internal/replication` |
| Manual promotion with an epoch bump | done | `internal/replication` |
| Catch-up: log shipping and snapshot transfer | done | `internal/replication` |
| Divergent-tail truncation on a returning leader | done | `internal/store`, `internal/wal` |
| Fault-injecting transport (drop, delay, partition) | done | `internal/replication` |
| Heartbeats and a timeout failure detector | done | `internal/cluster` |
| Controller: versioned shard map, automatic failover | done | `internal/controller` |
| Leader leases bounding the stale-read window | done | `internal/cluster`, `internal/replication` |
| Raft: pre-vote, CheckQuorum, log repair, Figure-8 commit rule | done | `internal/raft` |
| Deterministic simulation with seeded crash/partition/drop/reorder | done | `internal/raft` |
| Raft-replicated shard map (control plane only) | done | `internal/raft` |
| Controller backed by the Raft log; decisions must commit | done | `internal/controller` |
| Rebalancing: snapshot, catch-up, pause, cutover, cleanup | done (spike) | `internal/rebalance` |

Approximate indexing (IVF, PQ, HNSW) is **out of scope by decision, not by deferral**. The
distributed layer is debugged against an exact index because the exactness proof for
scatter-gather (below) depends on it. See `concepts/09-choosing-an-index.md`.

## Design in one page

### The write path

```
Put(v)
  │
  ├─ 1. validate (dimension, id length)
  ├─ 2. lsn = nextLSN                 ← the total order is established here
  ├─ 3. wal.Append + fsync            ← DURABLE. only now may we acknowledge
  ├─ 4. apply to map + index
  ├─ 5. commitLSN = appliedLSN        ← what readers are allowed to see
  └─ 6. return version
```

Step 3 before step 4 is the entire meaning of "write-ahead". Apply first and crash before the
fsync, and memory held a write the log did not — recovery loses it silently, but only
*sometimes*, depending on whether a read observed it first.

Step 5 exists so a failover cannot make a previously-read value disappear. Reads filter on
`version <= commitLSN`.

### Recovery

State is a pure function of what is on disk:

```
1. load the newest complete snapshot        → appliedLSN = snapshotLSN
2. replay WAL records above it              → appliedLSN advances
3. truncate any torn tail                   → the log is a contiguous prefix
```

A **torn tail** — a half-written record from a crash — is truncated, not skipped. Skipping
could leave an LSN hole, and the gapless-LSN invariant is what replication depends on. The
cost of truncating is losing the tail, which is exactly the set of writes that were never
acknowledged, because acknowledgement requires a completed fsync.

A **valid, checksummed record carrying the wrong LSN** is something else entirely: real
corruption, not a torn write. The shard refuses to start. It never repairs silently.

### Why merging per-shard top-K is exact

If `x` is in the global top-K, then at most `K-1` items in the entire dataset beat `x`. A shard
is a subset of the dataset, so at most `K-1` items *on x's own shard* beat `x`. Therefore `x`
is in that shard's local top-K. Merging `S·K` candidates is exact, and `K` per shard is the
tight bound.

`TestGlobalTopKMatchesSingleShard` asserts this directly: results across `S ∈ {1,2,4,8}` are
identical to a single shard holding all the data, for both metrics and `K ∈ {1,5,10,50}`.

### Replication

A write is acknowledged only once it is durable on the leader **and on every
in-sync replica**. That single rule is what makes "kill the leader after an ack, assert nothing is
lost" a deterministic test rather than a flaky one.

```
write ──► leader: lsn, WAL append + fsync, apply     ← durable here, NOT committed
            │
            ├──► follower: epoch check, append + fsync, apply, ack
            │
            └──► all in-sync replicas acked → commit_lsn = lsn
                                             └─► NOW readable, NOW acknowledged
```

Three rules the tests pin:

- **Refuse rather than silently degrade.** With the in-sync set down to the leader alone, writes are
  refused. This is the Kafka failure: `acks=all` becomes `acks=1` when the ISR shrinks to one,
  because "all in-sync replicas" is trivially satisfied — a measured fault-injection run lost
  194,524 of 500,000 acknowledged messages that way. `MinISR` is the fix and it costs availability
  on purpose.
- **Never promote a replica that was not in-sync.** Everything an in-sync replica holds is
  committed, because the leader could not have acknowledged anything above it without this replica
  acknowledging first.
- **The epoch is the fence.** Promotion bumps it; a deposed leader's messages then carry a lower
  epoch and are refused, so it cannot corrupt the new leader's log even while it still believes it
  leads.

The transport is an interface with an in-process implementation, so dropping, delaying and
partitioning are function calls against a seeded RNG. Every failure reproduces exactly.

### Failure detection, and the split-brain window

Heartbeats do not *fix* split brain — they **cause** it. A system with no failure detector never
promotes anyone, so it never has two leaders. Automatic promotion means accepting that a wrong guess
produces two nodes that both believe they lead, and Fischer/Lynch/Paterson proved the guess is
unavoidable. Four layers, each containing what the one below cannot:

```
heartbeats + timeout      CREATES the window (the guess is sometimes wrong)
      ▼
epoch fencing             BOUNDS the damage — a deposed leader cannot corrupt the new log
      ▼
leader leases             BOUNDS the stale-read window in time
      ▼
consensus (majority)      ELIMINATES it — two majorities must intersect
```

Fencing stops a deposed leader corrupting the new leader's log, because that takes a message and the
message is refused. It does **not** stop that node answering a read from its own memory, because
reading requires talking to nobody. A lease closes that with no message at all: the controller stops
renewing a leader it cannot reach, the lease lapses on its own, and reads are refused *before* the
replacement is installed. That ordering is why `LeaseDuration < DeadAfter` is validated at
construction rather than documented.

Two tests carry this. `TestSplitBrainWindowWithoutLeases` **deliberately asserts the broken
behaviour** — the deposed leader still serves a stale plain read — so the hole is visible rather than
assumed away. `TestLeaseClosesTheStaleReadWindow` runs the same scenario through `StrongRead` and
asserts it is refused. The diff between those two tests is the entire value of leases.

### Consensus, and what it is actually for

Raft governs the **control plane only** — who leads which shard. Point writes never touch it. That
split is not a shortcut: Qdrant runs Raft for cluster metadata while point writes bypass it entirely,
and Weaviate moved schema to Raft in v1.25 while keeping leaderless data replication. Two systems,
two languages, same conclusion.

What consensus buys is the single thing the M8 controller cannot do. **A controller cannot detect its
own isolation**, so a partitioned one promotes a replacement for a leader that is perfectly healthy
on the other side — and nothing stops it, because it is the only authority. A Raft-replicated map
cannot do that, because a minority cannot commit.
`TestPartitionedControllerCannotPromote` asserts exactly this: the isolated controller proposes a
promotion twenty times, never commits, and converges on the majority's decision after healing.

The implementation has **no goroutines and no locks**. A `Node` is a pure state machine — feed it a
tick or a message, get back the messages to send. That is what makes an entire cluster runnable
inside one deterministic test, and the simulation is the only practical way to find consensus bugs.
Antithesis found real safety and liveness defects in every major Raft library they pointed this
technique at.

Three rules that are easy to omit and fatal to omit:

- **The Figure-8 commit rule.** A leader may only commit an entry from its *own* term by counting
  replicas. A previous-term entry sitting on a majority is not safe to commit that way — a later
  leader could still overwrite it, meaning an acknowledged entry disappeared.
- **Pre-vote.** Raft as published does not guarantee liveness under partial partitions: an isolated
  node times out, bumps its term, and forces healthy nodes to step down, repeatedly. Pre-vote asks
  "would you vote for me?" without adopting the term.
- **CheckQuorum.** Without it an isolated leader keeps believing it leads, indefinitely.

## Measured numbers

Full results, machine spec and caveats in **[BENCHMARKS.md](BENCHMARKS.md)**; `make bench`,
`make contention`. Latest run, 2-core Linux container, Xeon 2.80GHz, Go 1.24.7, one run per
configuration:

| Measurement | Result | What it means |
|---|---|---|
| fsync per write vs. group commit of 64 | 215,018 → 10,376 ns/op | **20.7×**, durability unchanged — the fsync is amortised, not skipped |
| Replicated write, 0 → 1 → 2 followers | 5,169 → 107,264 → 127,954 ns/op | first remote copy costs 21×; the second only +19%, because fan-out is parallel |
| Search, N = 10K → 100K (dim 64) | ~3,370 MB/s, flat | brute force is **memory-bandwidth-bound**; the only way faster is touching fewer bytes |
| Recovery, insert-only workload | snapshot **13.2% slower** | a snapshot with no superseded versions to compact away is pure overhead |
| Recovery, each key rewritten 10× | snapshot **4.8× faster** | benefit is proportional to write amplification, not data size |
| Search under 4 concurrent writers, RWMutex | 53 writes, and **removing fsync changes nothing** | a convoy: the write path's own cost is irrelevant next to waiting for a 2.4 ms scan |
| Same, copy-on-write | 131–187 writes, search p99 **2× worse** | COW moved the cost from lock-wait to allocation; the granularity is wrong |

Three things are worth saying about these numbers rather than just quoting them.

**Two of them contradicted the design.** Snapshots were built on the premise that they speed
recovery; on an insert-only workload they slow it down. Copy-on-write was expected to help readers;
it helped writers 2.5–3.5× and made reads twice as slow. Both are explained in
[LESSONS.md](LESSONS.md), and the second turned out to be copy-on-write's documented precondition
being violated rather than anything mysterious.

**One "contradiction" was noise.** The fsync ratio has measured 25×, 10× and 20.7× across three
runs of unchanged code; the second-follower cost +7%, +49% and +19%. The recovery result, by
contrast, reproduced to within 0.2% across runs *and* across two different CPUs. That difference is
the whole point: a result is worth quoting when it reproduces.

**These are single runs**, against a documented minimum of ten. Every table above inherits that.

**The failover window** is measured by `TestFailoverWindowIsMeasured`, not claimed: it equals the
detector's `DeadAfter` threshold, because no detector beats its own timeout.

## Tests worth knowing about

- **`TestTruncateAtEveryOffset`** (`internal/wal`) — takes a valid log, truncates it at *every
  byte offset*, and asserts clean recovery at each one: contiguous replay, correct `LastLSN`,
  and immediately appendable again. One table-driven test that catches most WAL bugs that
  exist.
- **`TestRecoveryEquivalence`** (`internal/store`) — every acknowledged write, and only those,
  survives a close and reopen with no snapshot.
- **`TestSearchMatchesReference`** (`internal/store`) — differential test against a naive
  score-everything-and-sort implementation. An exact index that disagrees with the obvious
  implementation is the bug that invalidates every downstream test.
- **`TestGlobalTopKMatchesSingleShard`** (`internal/coordinator`) — the exactness proof above,
  as an assertion.
- **`TestLSNGapIsRefused`** (`internal/wal`) — proves the reader distinguishes a torn tail
  (recoverable) from a real hole (refuse to start).
- **`TestAckedWriteSurvivesLeaderLoss`** (`internal/replication`) — the acceptance test for
  replication. 200 acknowledged writes, kill the leader, promote the survivor, assert every one is
  present at the right version. Deterministic under synchronous replication; it would be flaky
  under asynchronous, which is why asynchronous makes a poor default.
- **`TestChaosNoAckedWriteIsLost`** (`internal/replication`) — partition and heal replicas at random
  under 400 writes with a 5% message drop rate, then assert every acknowledged write is readable and
  every replica converged on identical state. It guards against passing vacuously: the run fails if
  fewer than 100 writes were acknowledged, or if no write was ever refused.
- **`TestCatchUpBySnapshot`** (`internal/replication`) — forces the path where the leader has
  truncated past the follower's cursor, so only a full snapshot closes the gap. This is the path
  that rots undetected because nothing exercises it in normal operation.
- **`TestDivergentTailIsTruncated`** (`internal/replication`) — a returning leader holding records
  above the new leader's commit point, written under an older epoch. They were never acknowledged,
  so they must go; the committed prefix must survive.
- **`TestRandomizedSchedules`** (`internal/raft`) — 300 seeds × 400 steps of randomized crash,
  partition, restart, 10% drop and message reordering, asserting no split brain, log matching, and
  state-machine safety **after every single step**. Safety is checked continuously rather than at the
  end, because a violation that heals itself is still a violation — a client may have read the wrong
  answer in between. Every failure reproduces exactly from its seed.
- **`TestCommittedEntriesSurviveLeaderChanges`** (`internal/raft`) — 40 runs that kill whoever leads
  at random, tracking every entry any node ever reported committed and asserting none is ever lost or
  contradicted.
- **`TestFigure8CommitRule`** (`internal/raft`) — hand-builds the exact scenario and asserts the
  leader refuses to commit a previous-term entry on a majority.
- **`TestPartitionedControllerCannotPromote`** (`internal/raft`) — the reason consensus is here.
- **`TestSplitBrainWindowWithoutLeases`** / **`TestLeaseClosesTheStaleReadWindow`**
  (`internal/controller`) — the first asserts the system is still broken in a specific, named way;
  the second asserts leases fix exactly that. A test that documents a known hole is worth more than
  a comment claiming there isn't one.
- **`TestNoInSyncReplicaGoesReadOnly`** (`internal/controller`) — with no replica known to have been
  in sync, the shard must go read-only rather than promote. Unavailability over silent data loss.
- **`TestSearchIsDeterministicUnderTies`** (`internal/store`) — deletes and reinserts to
  scramble physical scan order, then asserts results are unchanged. Without the `(score, id)`
  total order, results would depend on write history and be untestable.

## Bugs the tests, the demo and the benchmarks found

Worth recording, because these are the kind that ship silently. The full account —
every bug with what caught it and the rule it generalises to, plus the three
places a measurement contradicted the design — is in **[LESSONS.md](LESSONS.md)**.

1. **Non-deterministic encoding.** Go randomizes map iteration order, so metadata was encoded
   in a different byte order every call. Two replicas applying the same write would produce
   different log bytes, making a byte-level log comparison between replicas impossible.
   `TestGroupCommitEquivalence` caught it. Keys are now sorted before encoding.
2. **A closed shard still served reads.** `Search` did not check `closed`, so an "unavailable"
   replica quietly returned stale results and the coordinator's partial-result path never
   fired. The demo caught it, not the tests.
3. **An ejected follower could never rejoin on its own.** Catch-up only contacted in-sync replicas,
   so ejection was permanent without an operator action — not how a real leader behaves. Found by
   strengthening the chaos test, which had been passing vacuously with 6 of 400 writes
   acknowledged.
4. **A partition only blocked inbound messages.** `Transport.Append` took only the recipient, so a
   partitioned *leader* could still replicate outward — meaning the transport could not model a
   partition at all, which is the single most important case the failure-handling design exists for.
   Found by a test asserting a partitioned leader cannot acknowledge writes; it acknowledged them.
   The signature is now `Append(ctx, from, to, req)`.
5. **The controller re-granted a lease at the instant of promotion.** `Tick` deleted the old lease
   and then unconditionally renewed leases at the end of the same call, handing the new leader
   authority inside the very window the mechanism protects. A leader now holds no lease until it has
   heartbeated *as leader at the new epoch*.
6. **A pre-vote deadlock that produced no leader and no error.** Campaigning resets the election
   timer, and the CheckQuorum pre-vote guard refuses a pre-vote from a node that still believes it
   has a live leader. So every node that timed out became a pre-candidate with a *fresh timer* and a
   *stale leader pointer*, and therefore refused every peer — all five campaigned, all five refused
   each other, and the cluster sat at pre-candidate forever. Deciding to campaign is deciding the
   leader is gone, so `campaignPre` now clears it. Found by instrumenting a stuck simulation rather
   than by reading the code.
7. **A caught-up follower could be stale on the commit point.** When a replica was already current
   on data, the leader returned early and never sent the commit position, so the follower held
   committed records and silently omitted them from reads. Raft carries the commit index on its
   heartbeats for exactly this reason. Found by the fixed chaos test on its first real run.
8. **The rebalance snapshot was taken at the wrong moment.** The move fixed its snapshot LSN when
   the transfer *finished*, so the destination arrived perfectly caught up, the gap was zero and the
   catch-up phase never ran — which made the slow-destination test pass while proving nothing. In a
   simulation the model is part of the system under test, and a simplification that removes the
   difficulty produces green, worthless tests. The LSN is now fixed when the phase *starts*.
9. **An abort was classified as a completed cutover.** `PhaseAborted` is the largest constant in the
   `Phase` enum, so `phase >= PhaseCutover` was true for an aborted move and every ordering check
   treated an abort as a success. Four unrelated tests failed at once, which is what pointed at one
   shared cause. Ordering no longer carries semantics: `AuthorityMoved()` and `Live()` are explicit.

Measurements contradicted the design three times — most sharply, snapshots making recovery
**slower** on an insert-only workload, and copy-on-write doubling search p99 while improving write
throughput 2.5-3.5x. A fourth "contradiction" turned out to be single-run noise on a shared VM and
is kept in the file as the more useful lesson. See [LESSONS.md](LESSONS.md) and
[BENCHMARKS.md](BENCHMARKS.md) §4.

## Layout

```
cmd/vecdemo/          the scripted story: load, search, fail a shard, crash, recover
internal/
  core/               Vector, ShardID, LSN, Epoch, Metric, sentinel errors — imports nothing
  index/              distance kernels, bounded top-K heap, exact brute-force index
  wal/                record codec, segment writer/reader, truncating replay
  store/              Shard: map + index + WAL + snapshot + recovery
  cluster/            hash → ShardID
  coordinator/        scatter, gather, merge
  replication/        transport with fault injection, leader + ISR, follower ingest, leases
  cluster/            clock, failure detector, versioned shard map, lease type
  controller/         heartbeats in, failover out
  raft/               log, pure-state-machine node, deterministic simulation, control-plane commands
```

Dependencies point inward. `core` imports nothing from this module. There are **no third-party
dependencies at all** — `go.mod` has no `require` block.

## Known limitations

Stated plainly, because a limitations section that is vague is worse than none.

1. Dataset must fit in RAM. No out-of-core storage, no memory-mapped vectors.
2. `numShards` is fixed at creation. Changing it requires a dump and reload.
3. Search is not consistent across shards. There is no global snapshot, so a search may observe
   different shards at different points in time.
4. No cross-key atomicity. Every operation is single-key.
5. Snapshots are full, not incremental, and the copy happens under a read lock.
6. **Search takes no lock**, reading an immutable snapshot republished on every write. Measured:
   writes improved 2.5-3.5x, search p99 roughly doubled, because copying the candidate slice per
   write drives allocation and the scan is memory-bandwidth-bound. The granularity is wrong and
   known to be — batching the publish is the fix and is not done. `concepts/23` has the full
   analysis; `BenchmarkContention` has the numbers.
7. **Node liveness is not replicated, deliberately.** Two controllers can disagree about whether a
   node is reachable, and they should — forcing agreement through the log would turn every network
   blip into a cluster-wide decision. Only *placement* is consensus state.
8. Leases rest on a bounded clock **drift rate**. That is much weaker than synchronised clocks, but
   it is not nothing.
9. A deposed leader still serves stale *plain* reads until it learns it was replaced. Only
   `StrongRead` is lease-gated. `TestSplitBrainWindowWithoutLeases` asserts this rather than hiding
   it.
10. The controller ticks only when called. There is no background loop, so in the demo and tests
    failure detection advances when the harness advances it — which is what makes it deterministic.
11. Brute-force search only. No approximate index, by decision.
12. No authentication, authorization, or encryption.
13. **Rebalancing is a spike, not a component.** `internal/rebalance` is a pure state-machine
    simulation of a shard move: no disk, no network, no wiring into the real shard or the shard
    map. It proves the ordering (snapshot → catch-up → pause → cutover → cleanup) and the
    invariants, and it demonstrates the data loss a naive cutover causes. It does not move a
    real shard. That was a deliberate tier choice — the ordering is the lesson, the plumbing is
    not.
14. Benchmarks use synthetic random vectors. Random vectors in high dimensions are
    pathologically uniform, so no recall number from them would transfer to real embeddings.
    That is fine here because the index is exact, but it would not be fine for an ANN index.
