# 20. Rebalancing

> Moving a shard between nodes while it is still serving writes — and the ordering that makes the difference between a brief pause and silent data loss.

## The problem

Capacity changes. A node is added, a node is retired, a disk fills, or round-robin placement leaves one node leading three of four shards while another leads none. Without a way to move a shard, there are two options: stop the world and copy, or never change the cluster. The first is unacceptable for a system claiming availability; the second is unacceptable for one claiming to scale.

The difficulty is not copying data. It is that **the source keeps accepting writes behind the copy cursor**, so a naive copy is wrong the instant it begins. By the time the last byte lands on the destination, the source has moved on.

For a vector database this is worse than for a key-value store, for a reason specific to the domain — see "the vector-specific cost" below.

## How it works

The safe ordering is five phases, and every one of them is load-bearing:

```
   SNAPSHOT        CATCH-UP        PAUSE       CUTOVER      CLEANUP
   ────────        ────────        ─────       ───────      ───────
  copy state      ship the       refuse      flip authority  drop the
  up to LSN S     tail S+1..     writes,     at a NEW EPOCH  source copy
                                 close the
  writes flow     writes flow    last gap    the only        only now is
  normally        normally       to zero     irreversible    the fallback
                                             step            expendable
   minutes         seconds       milliseconds
```

### Phase 1 — snapshot

Fix an LSN `S` **when the phase starts**, not when the transfer finishes, and copy everything up to it. The copy is of a consistent past state; everything written while it is in flight is deliberately left for catch-up to carry.

This is the long phase — minutes to hours for a large shard — and it is deliberately done with **no pause at all**. That is the entire reason the protocol has five phases instead of one.

Raft's `InstallSnapshot` RPC exists for exactly this purpose: bringing a far-behind follower up to date by transferring the whole snapshot rather than replaying a discarded log [1].

### Phase 2 — catch-up

Ship records `S+1` onward as they arrive. The gap shrinks only if the destination applies faster than the source accepts. If it does not, **the move must never cut over**, and it must eventually give up.

The right abandonment criterion measures **progress, not elapsed time**. A deadline is wrong because a large shard legitimately takes a long time. Track the smallest gap seen; if it stops shrinking for some budget of ticks, abandon the move. Holding resources for a transfer that will never finish is worse than not starting it: the destination accumulates a copy it will never use, and the source cannot truncate its log past the destination's cursor.

### Phase 3 — the write pause

When the gap is small enough that closing it takes milliseconds, stop accepting writes and let the destination reach the source exactly.

This is the only unavailability in the whole procedure, and it is short *because* catch-up did the work. Writes are refused, not queued and silently dropped — an honest refusal is always better than an acknowledgement the system cannot keep.

Bound the pause. Exceeding the budget aborts back to the source rather than extending it, because an unbounded pause is an outage with extra steps.

### Phase 4 — cutover

Flip authority at a **new epoch**. The epoch bump is the fence: from that moment the source's writes are stamped below the destination's epoch and are refused by the receiver, even if the source never learns it lost the shard. Safety cannot depend on the old owner finding out — it may be in a long GC pause, partitioned, or a stopped VM.

This is the one irreversible step.

### Phase 5 — cleanup

Only now may the source drop its copy. Dropping it earlier converts every recoverable failure into data loss, because the source copy is the only fallback for every failure before the cutover.

### Why the ordering is the whole lesson

Two orderings look reasonable and are wrong:

**Cutting over before the gap is zero loses acknowledged writes.** Everything above the destination's cursor is simply gone. It is worth building this version first and measuring the loss, because otherwise the pause phase looks like gratuitous downtime that someone will optimise away.

**Cleaning up before the cutover is durable** leaves nothing to fall back to.

The corollary is that **an abort is free at every phase before the cutover**. The source is untouched, so the destination can die at any point and writes continue with no interruption at all. That property is what makes rebalancing safe to attempt automatically — and it exists only because cleanup runs last.

### The learner pattern

etcd's `learner` is the same idea expressed in consensus terms: a new replica joins as a **non-voting** member that receives the full log and data stream but does not count toward quorum and cannot vote [2].

The rationale is precise. Adding a brand-new member as a full voter immediately increases quorum size while that member is still empty and needs a large amount of replication — risking unavailability if a majority-losing event happens mid-catch-up, and risking leader disruption from the extra load. Learners "don't change the size of quorum", which makes misconfiguration "always reversible without losing the quorum" [2].

The workflow is two-phase and explicitly operator-driven: `member add --learner` makes the node a standby receiving updates; once its "log has caught up to leader's", `member promote` converts it to a voter, and etcd validates that precondition before allowing the promotion [2].

### Cutover in a consensus group: joint consensus

Moving a replica from node A to node B naively — add B, then later remove A — can force an unsafe intermediate configuration. CockroachDB's example: temporarily having two replicas of a range in the same failure domain, where a single region failure loses quorum and breaks the deployment's stated resilience guarantee [3].

Joint consensus avoids this by requiring agreement from **both** the old and new configurations during the transition, so the intermediate state "survives a region outage just fine, since both C₁ and C₂ do so individually"; only after the joint step commits does the cluster cut over fully [3]. CockroachDB adopted this via upstream contributions to etcd/raft, landing in 19.2 [3].

Note what this is and is not: joint consensus makes the *configuration change* safe. It does not address the bulk data transfer, which is `InstallSnapshot` or an equivalent [1].

### The vector-specific cost

Two things make vector shards more expensive to move than equivalently sized key-value shards.

They are **large** per record — hundreds of thousands to millions of high-dimensional float vectors, plus an index structure over them.

And an HNSW graph generally **cannot be trivially copied**. Graph pointers, layer assignments and entry points are specific to the build history and machine-local memory layout, so depending on the implementation the destination may need to **rebuild the index from the transferred raw vectors** rather than copy a graph structure. That means the destination is not query-ready when the bytes arrive; it is query-ready when the index build finishes, which can be the dominant cost.

This paragraph is informed synthesis from HNSW's structure rather than a directly quoted claim from a vendor engineering source; no primary Milvus or Qdrant document on index-rebuild-during-rebalance cost was located. Treat it as a strong architectural expectation, verify against a specific implementation before relying on it.

The practical consequence is real regardless: a rebalance plan for a vector database must budget **transfer time plus index build time**, and the catch-up phase must survive the index build, because writes keep arriving throughout it.

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Lag threshold to enter the pause | how long the outage is | small enough to close in one round trip | too low: the pause never starts; too high: a long pause |
| Pause budget | maximum outage | hundreds of milliseconds | unbounded: the "pause" is an outage |
| Stall budget in catch-up | when to abandon | tens of ticks with no improvement | absent: a move that can never finish holds resources forever |
| Transfer rate limit | impact on foreground traffic | a fraction of link capacity | unlimited: the move starves live queries |
| Concurrent moves | total rebalance impact | one or two per node | too many: the cluster degrades while "healing" |
| Trigger threshold | when imbalance justifies a move | a meaningful skew, not any skew | too sensitive: constant churn |
| Cooldown after a move | oscillation damping | minutes | absent: rebalancing storms |

## How real systems do it

**etcd** exposes the two-step learner/promote flow as explicit **operator commands** rather than doing it automatically [2]. That is a design position, not an omission.

**CockroachDB** solved the configuration-change half with joint consensus and contributed it upstream to etcd/raft [3], which is why the mechanism is now available to anyone using that library.

**TiKV** runs one Raft group per region and moves regions between nodes as a routine operation, which is only tractable because of the batching mitigations described in [Consensus and Raft](19-consensus-and-raft.md) [4].

The instructive divergence is **automatic versus operator-triggered**. etcd and CockroachDB expose explicit commands; systems that rebalance automatically must additionally solve the false-positive problem, and that is harder than it looks.

### Rebalancing storms

A transient failure — a brief network hiccup, a slow GC pause, a rolling restart — can trip a failure detector into declaring a node dead, which kicks off a reassignment. If the "failed" node then returns, because it was never really dead, the result is **oscillation**: repeated back-and-forth reassignment consuming far more network, CPU, and (for a vector database) index-rebuild cost than the original blip would have, and potentially causing cascading timeouts elsewhere.

This pattern is well known from Kafka consumer-group and Connect rebalance storms. It is corroborated here at the level of the pattern's name and general shape rather than a quote-level primary source, and **no primary source on rebalancing storms in a shard-replica context was located** — which is itself worth knowing, because it means the failure mode is folklore-documented rather than well studied in this domain.

The mitigation implied by etcd's and CockroachDB's explicit operator commands is a **human in the loop** before committing to data movement triggered by failure detection [2]. This bounds the blast radius of a false-positive detection to an alert, rather than to gigabytes of transfer and an index rebuild. The term "operator confirms" is a description of the observed practice, not a named pattern found in a primary source.

The deeper point connects to [Failure detection](18-failure-detection.md): a failure detector is a timeout gamble, and rebalancing is the most expensive action you can take on the strength of a gamble. Failover is cheap and reversible; moving a shard is neither.

## Complexity and cost

For a shard of `N` vectors, `D` dimensions, with a write rate `w` and a destination apply rate `a`:

| | Cost |
|---|---|
| Bytes transferred | `O(N · D · 4)` for float32, plus metadata; the index may or may not transfer |
| Snapshot phase duration | bytes / effective transfer rate |
| Catch-up duration | converges only if `a > w`; duration is roughly `(accumulated gap) / (a − w)` |
| Catch-up divergence | if `a ≤ w`, the gap grows without bound and the move can never complete |
| Pause duration | (remaining gap) / `a`, which is why the lag threshold matters |
| Destination CPU | index build, potentially `O(N log N)` for a graph index, on top of the transfer |
| Availability impact | one bounded pause; zero if the move aborts |

The condition `a > w` is the whole feasibility question, and it is worth stating as an invariant rather than discovering it as a hang.

## Failure modes

- **Cutover before the gap closes** — acknowledged writes above the destination's cursor are lost, silently.
- **Cleanup before cutover is durable** — a later failure has no fallback.
- **A destination that never converges** — the move holds resources indefinitely, and the source cannot truncate its log past the destination's cursor, so the log grows too.
- **Rebalancing storms** — oscillation driven by a flapping failure detector, costing far more than the original fault.
- **The zombie source.** The old owner wakes up believing it still owns the shard. Only the epoch fence saves you, and only because the *receiver* enforces it.
- **The move starves foreground traffic.** An unrated transfer competes with live queries; the cluster gets slower while it is "healing".
- **An unsafe intermediate configuration** — two replicas in one failure domain during a consensus-group move, defeating the resilience guarantee the deployment claims [3].
- **Index rebuild not budgeted.** Bytes arrive, the destination is promoted, and queries are slow or wrong until the build completes.

## How to test it

This is one of the best candidates in a distributed system for **pure deterministic simulation**. There is no need for disk or network: model the source and destination as logs, drive the phases with `Step()`, and inject faults at chosen ticks. A bug becomes "seed 7, tick 41".

Invariants to assert after **every** step:

1. Write authority is single-valued. Two writers means two divergent logs claiming the same positions, which no later merge can repair.
2. Before the cutover, the destination is never ahead of the source.
3. After the cutover, the source never holds write authority.
4. On completion, the destination holds **every acknowledged write**. This is the invariant the whole protocol exists to preserve.
5. On abort, the source holds every acknowledged write.
6. A cleaned-up replica is never the writer.

Scenarios:

- **Abort at every phase.** Kill the destination in planned, snapshot, catch-up and pause, and assert in each case that the move aborts, the source keeps authority and all data, and writes resume **immediately** — an abort is not an outage.
- **The slow destination.** Apply rate below the write rate. Assert the move never cuts over and eventually abandons rather than hanging.
- **The naive cutover.** Disable the pause and assert acknowledged writes **are** lost. Keeping this test is what stops the pause being removed as an optimisation later.
- **The zombie source.** After cutover, replay a write from the old owner at its remembered epoch and assert the receiver refuses it.
- **Randomised runs** — hundreds of seeds with the destination flapping and occasional source failures, asserting invariants every step.

Guard against vacuity. Require a floor on completed moves *and* a floor on aborted moves; a suite where every seed aborted at tick 1 passes every safety check and proves nothing.

## Minimal demo version

A pure state machine with the five phases, a `Write` that goes to whoever currently holds authority, a `Step` that advances one tick, and a `Check` that asserts the invariants. Perhaps 250 lines with no dependencies.

Run it twice in the demo: once with the naive cutover, once with the pause, on the same load. Print acknowledged and lost counts for each. Then kill the destination mid-catch-up and show writes continuing on the source with nothing lost. Three numbers, and the ordering argument is made.

**What you can cut:** real disk and network, joint consensus, rate limiting, the actual index transfer and rebuild, multi-shard planning, and any automatic trigger. Move one shard, on command.

**What you cannot cut without the demo becoming a lie:**

- **Writes must continue during the move.** A move with no concurrent writes is a file copy, and it teaches nothing, because the entire difficulty is the moving target.
- **The naive version.** Without it the pause looks like unnecessary downtime.
- **The epoch bump at cutover**, enforced by the receiver. A cutover with no fence is a cutover that works only when the old owner cooperates.
- **Cleanup last.** Reordering it makes every abort path a data-loss path, and the abort tests would then be asserting the wrong thing.

## Open questions and contested claims

**Automatic or operator-triggered?** etcd and CockroachDB both expose explicit commands [2][3]. Fully automatic rebalancing is the more attractive engineering goal and the more dangerous one, because it couples an expensive irreversible-ish action to a failure detector that is definitionally a guess. There is no settled answer; the observable evidence is that the systems with the strongest correctness reputations chose the manual path for membership changes.

**How much does the vector-specific rebuild cost matter?** This is genuinely under-documented. The architectural argument — that HNSW graph structure is build-history and layout dependent, so the destination may rebuild rather than copy — is strong, but no fetched primary source quantifies it, and implementations differ in whether they can serialise and transfer a graph directly. Anyone planning a rebalance for a graph-indexed system should measure it on their own implementation rather than assume either answer.

**Is there a principled trigger threshold?** No source located gives one. In practice thresholds are chosen by feel and then tuned downward after the first rebalancing storm. That is an honest description of the state of the art, not a gap in the search.

## Sources

[1] Ongaro, Ousterhout — "In Search of an Understandable Consensus Algorithm (Extended Version)", §7 — https://raft.github.io/raft.pdf
[2] etcd — "Learner" design document, v3.5 — https://etcd.io/docs/v3.5/learning/design-learner/
[3] Cockroach Labs — "Availability and region failure: Joint consensus in CockroachDB" — https://www.cockroachlabs.com/blog/joint-consensus-raft/
[4] TiKV — "Multi-raft", TiKV deep-dive documentation — https://tikv.org/deep-dive/scalability/multi-raft/
