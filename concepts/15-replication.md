# 15. Replication

> Keeping more than one copy of a shard, so that losing a machine does not lose data — and the exact conditions under which that promise silently fails.

## The problem

A shard on one machine dies with that machine. Replication puts `RF` copies on distinct nodes so the data survives, and lets reads spread across them.

The hard part is not making copies. It is deciding **when a write is allowed to be called successful**, because that single decision determines whether "acknowledged" means anything at all. Get it wrong and you have a system that reports success and loses data — which is strictly worse than one that reports failure, because nobody investigates a success.

Jepsen's analysis of Elasticsearch 1.5.0 measured exactly this failure: up to 25% of acknowledged writes lost under a network partition, and roughly 10% lost on an ordinary process crash with no partition at all, because acknowledgements were being issued before the translog fsync [1]. The data was never on disk when the client was told it was.

## How it works

### Roles

- **Leader** — the single node accepting writes for a shard in a given epoch. It assigns sequence numbers and owns the log.
- **Follower** — applies the leader's log. May serve stale reads. Never accepts client writes.
- **The replication log is the write-ahead log.** There is no second log. A follower is fed records from `follower.applied + 1` onward, which means one durability mechanism serves both crash recovery and replication. See [Write-ahead logging](12-write-ahead-logging.md).
- **`commit_lsn`** — the highest sequence number the leader considers durable per the replication policy. Reads only see records at or below it.
- **`applied_lsn`** (per replica) — the highest sequence number that replica has fsynced *and* applied.
- **Replication lag** — `leader.commit_lsn − follower.applied_lsn`.

### Synchronous versus asynchronous

```
SYNCHRONOUS                          ASYNCHRONOUS
client → leader                      client → leader
         WAL append + fsync                   WAL append + fsync
         │                                    │
         ├─→ follower                         ├─→ ack client  ◄── returns here
         │   append + fsync                   │
         │   ack                              └─→ follower (background)
         │
         └─→ ack client  ◄── returns here
```

| | Synchronous | Asynchronous |
|---|---|---|
| Acked write survives leader loss | yes | **no** — the loss window is the replication lag |
| Write latency | leader fsync + RTT + follower fsync | leader fsync only |
| A dead follower | blocks writes unless you eject it | no effect |

There is a middle position. MySQL's semisynchronous replication waits for at least one replica to acknowledge receipt before the client returns, without waiting for all of them [2].

**Choose synchronous.** Not because it is faster — it is not — but because asynchronous replication silently contradicts any claim of strong consistency, and a design that quietly breaks its own guarantee is worse than one that is honestly weak. It also sets up consensus naturally: synchronous-to-all-in-sync-replicas is one step from synchronous-to-a-majority, whereas going asynchronous first means throwing work away.

Offer asynchronous later as an explicit per-request durability level, and benchmark the two side by side. That comparison is worth having; making it the default is not.

### The in-sync replica set — where the honesty lives

If a follower dies, do you block writes forever, or eject it and keep going? Blocking means one dead node takes down half the cluster's shards. Ejecting means the shard now runs with one durable copy, and if the leader then dies, promoting the ejected follower loses writes.

The answer is Kafka's: maintain an **in-sync replica set (ISR)**. A follower that fails to acknowledge within a timeout is ejected; writes require acknowledgement from all *current* ISR members. Ejection bumps the shard epoch and is published in the shard map, so running under-replicated is cluster-visible state rather than a leader's private opinion [3].

**The hard rule: a replica that was not in the ISR at the moment of the leader's failure is never promoted.** If no in-sync replica survives, the shard goes read-only and alerts. Unavailability is chosen over silent data loss.

That rule is not optional, and Kafka is the proof. With `acks=all`, a write is acknowledged when all ISR members have it — but if the ISR has shrunk to the leader alone, "all ISR members" is trivially satisfied by one node. Jack Vanlightly's fault-injection run measured **194,524 of 500,000 messages lost with `acks=all` set** for exactly this reason [4]. The fix is `min.insync.replicas >= 2`, which refuses the write rather than accepting it into a one-member ISR — availability traded for durability, deliberately [3][4].

### Quorums, and the thing everyone gets wrong

An alternative to a leader is quorum replication: write to `W` replicas, read from `R`, and if `R + W > N` the read and write sets must intersect, so a reader is guaranteed to see at least one replica that has the latest write.

> **`R + W > N` guarantees quorum *overlap*, not linearizability.**

Overlap means the reader *contacts* a replica holding the newest value. It does not mean the reader *returns* it, and without further machinery reads can go backwards in time: a read that observes a new value can be followed by a read that observes an older one, because nothing wrote the new value back. Making quorum reads linearizable requires an ABD-style write-back phase, where the reader propagates the newest value it saw to a quorum before returning it [5]. This is the single most commonly mis-taught claim in the area.

Dynamo relaxes quorums further with **sloppy quorums and hinted handoff**: when the intended replica is unreachable, the write goes to a substitute that holds it as a hint and forwards it later. This buys availability and gives up the intersection property that made the quorum meaningful [6].

### The write path, precisely ordered

```
CLIENT           LEADER (shard 1, epoch 7)              FOLLOWER
  │
  ├─ Insert ────►│ 1. am I leader for this shard at the
  │              │    current map version? else redirect
  │              │ 2. lsn = ++nextLSN     ← total order fixed here
  │              │ 3. wal.Append + fsync  ← DURABLE ON LEADER
  │              │ 4. apply to map + index
  │              │
  │              ├─ Replicate{epoch:7, from:lsn} ──►│ a. epoch >= mine? else REJECT
  │              │                                   │ b. lsn == applied+1? else CATCHUP
  │              │                                   │ c. append + fsync ← DURABLE
  │              │                                   │ d. apply
  │              │◄────────── Ack{applied_lsn} ──────│
  │              │ 5. all ISR acked → commit_lsn = lsn
  │◄─ OK{lsn} ───│ 6. ack the client
```

Three steps are load-bearing:

- **fsync before apply (3 before 4).** Apply first and crash before the fsync, and memory held a write the log did not. Recovery loses it — but only sometimes, depending on whether a read observed it first.
- **Epoch check on the follower (a).** A leader that was declared dead because it was merely slow can wake up and try to replicate. Rejecting a stale epoch means it can never corrupt the new leader's log. See [Failure detection](18-failure-detection.md).
- **`commit_lsn` advances only after ISR acknowledgement (5).** If reads observed uncommitted records, a failover could make a previously-read value vanish.

### Catch-up

A follower behind by `gap = leader.commit_lsn − follower.applied_lsn`:

- `gap == 0` — in sync; stream new records.
- `gap` within the leader's retained log — **log shipping** from `applied_lsn + 1`. Cheap.
- `gap` beyond it (the leader already truncated) — **snapshot transfer**, then log-ship the tail.

This is why WAL truncation must never pass the slowest ISR member's `applied_lsn`. Truncating past a live follower's cursor turns a ten-record catch-up into a full snapshot transfer, which is a classic and expensive bug. See [Recovery and compaction](13-recovery-and-compaction.md).

## Parameters and knobs

| Parameter | Controls | Typical | If you get it wrong |
|---|---|---|---|
| `RF` | copies per shard | 2 with a leader; 3 with consensus | RF=2 tolerates one loss and leaves zero margin. RF=2 with *majority quorum* is strictly worse than primary/backup: a majority of 2 is 2, so either loss halts the group |
| Sync mode | when the client is told "yes" | synchronous | Async silently voids any durability claim |
| ISR timeout | how long a slow follower blocks writes | 3–10 s | Too short: healthy followers flap out under load spikes. Too long: writes stall behind a dead node |
| Minimum ISR size | refuse writes below this | 2 | At 1 you get the Kafka failure above: acked writes lost with `acks=all` |
| `W`, `R` (quorum systems) | intersection | `R+W>N` | Gives overlap, not linearizability, without write-back |

## How real systems do it

**Elasticsearch** has the most complete and reusable pattern, built after Jepsen [1] showed it losing acknowledged writes. Three mechanisms interlock:

- **Primary terms** — a monotonic number bumped on every primary change, used to reject operations from a stale primary [7]. This is epoch fencing.
- **Sequence numbers** — a per-shard increasing number giving a total order and a replication cursor [7].
- **In-sync allocation IDs** — the explicit set of copies eligible for promotion [8].

If you build one replication design, build this one. Naming your fields after theirs makes the correspondence to a production system checkable rather than something you reconstruct later.

**Qdrant** deliberately does not route data through consensus. Raft governs only cluster metadata — shard placement, replica state transitions — while point writes fan out directly from the coordinating node to all replicas, gated by `write_consistency_factor` (default **1**, range 1 to `replication_factor`). If the number of active replicas drops below that factor the write is rejected outright [9][10]. Partial writes are left partial by design: the API returns `Acknowledged` rather than `Completed` and tells the client to resubmit — availability over atomicity, stated as policy [9].

**Weaviate** independently reached the same split: Raft for schema since v1.25, because the pre-Raft design could not handle concurrent schema changes [11], with data replication remaining leaderless and Dynamo-style with tunable ONE / QUORUM / ALL and read repair [12].

Two systems, two languages, no shared code, same conclusion: **consensus for the control plane, something cheaper for the data plane.**

**Milvus** does something different again: replicas are read-scaling caches, not durability mechanisms [13]. Durability belongs to the log service and object storage beneath them. If you think of a replica as "a spare copy that protects data", Milvus's replica is not one.

**Vespa** is the honest weak end of the spectrum: explicitly eventually consistent, resolving conflicts by wall-clock last-writer-wins at whole-document granularity [14], with no epoch or term concept. A serious production system chose weaker than the design above and shipped it.

**Where they were wrong, and where it still hurts.** The instructive finding is *where* the bugs actually are. Qdrant's real production consistency bugs were not in its WAL and not in its documented consistency trade-off — they were in the rejoin path: upserts partially committed during a node restart [15], a node restart during deletes leaving points with empty payloads [16], and data inconsistent after migration [17]. Weaviate has open issues reporting data loss on an *ordinary pod restart* [18]. Replication is comparatively well understood; **reconciliation after a replica comes back is where systems still break.**

## Complexity and cost

| | Cost |
|---|---|
| Write latency (sync, RF=2) | `leader_fsync + RTT + follower_fsync` |
| Write latency (async) | `leader_fsync` |
| Network per write | `RF − 1` copies of the record |
| Storage | `RF ×` the dataset |
| Catch-up (log shipping) | `O(gap)` records |
| Catch-up (snapshot) | `O(shard size)` bytes, competing with live traffic |
| Failure tolerance (leader/follower) | `RF − 1` losses, *only if survivors were in-sync* |
| Failure tolerance (majority quorum) | `⌊(RF−1)/2⌋` losses — RF=3 tolerates 1, RF=5 tolerates 2 |

Synchronous replication roughly doubles a write's sensitivity to fsync latency, since two fsyncs are now on the critical path. Group commit is the mitigation and it is not free. See [Write-ahead logging](12-write-ahead-logging.md).

## Failure modes

- **Acknowledging before fsync.** The measured cost is 10% of writes lost on an ordinary crash [1]. This is a data-loss bug, not a tuning choice.
- **An ISR that shrinks to one.** `acks=all` becomes `acks=1` without saying so. 194,524 of 500,000 messages lost in a measured run [4].
- **Promoting a stale replica.** Every write the ejected follower missed is gone, silently. The only defence is refusing to promote a non-ISR member and going read-only instead.
- **Truncating the log past a live follower's cursor.** Converts a cheap catch-up into a full snapshot transfer.
- **Believing `R + W > N` gives linearizability.** It gives overlap. Reads can still go backwards without a write-back phase [5].
- **Wall-clock last-writer-wins.** Clock skew silently discards the newer write. Order with sequence numbers, not timestamps.
- **Rejoin without reconciliation.** The most under-tested path in the field, and empirically where real systems break [15][16][17][18].
- **A dead follower making writes faster.** Ejection from the ISR removes an fsync from the critical path, so throughput *improves* while durability silently halves. This must be alerted on, because the metric moves the wrong way.

## How to test it

- **The acceptance test: kill the leader immediately after an acknowledgement, promote the survivor, and assert every acknowledged write is present.** Under synchronous replication this must pass deterministically. Under asynchronous it is expected to fail sometimes, which is why asynchronous makes a poor default — the test teaches nothing.
- **Idempotent apply.** Apply the same record twice and assert the state is unchanged. Retries and duplicated replication messages both depend on this.
- **Epoch rejection.** Send a follower a record stamped with a lower epoch and assert it is refused.
- **ISR ejection and rejoin.** Pause a follower past the timeout, assert it leaves the ISR and that the shard map reflects it; resume it and assert it catches up and rejoins.
- **Both catch-up paths.** Force log shipping with a small gap, then force snapshot transfer by truncating the leader's log past the follower's cursor. The second path is the one that rots undetected.
- **A chaos loop, which is the test that finds the real bugs.** Kill and restart replicas at random under sustained writes, with a verifier asserting continuously that every acknowledged write remains readable and that no two replicas diverge. This is the only test that would have caught the rejoin bugs above.
- **Divergent tail truncation.** Give a returning node records above the new leader's commit point from an older epoch, and assert it truncates them rather than keeping them.

## Minimal demo version

Synchronous replication to all ISR members over an **in-process transport** is roughly 150 lines and it is the right demo. A `Transport` interface with an in-memory implementation lets you inject drops, delays, reordering and partitions as function calls, with a seeded RNG so every failure reproduces exactly. Real gRPC is a swap later, not a rewrite — and it would teach you gRPC, not replication.

The single most valuable property: because the leader path, the follower path and recovery all call the *same* apply function in the same order, there is no second code path that could apply a record differently. That is what makes replication a small addition rather than a parallel implementation.

**What you must not cut:**

- **The epoch on every replication message, and the follower's rejection of a stale one.** Without it the demo cannot exhibit the failure the whole mechanism exists to prevent.
- **The ISR, including ejection.** A demo with a fixed replica set never encounters the interesting question, which is what to do when a follower stops answering.
- **The refusal to promote a non-ISR replica.** This is the rule that separates a database from a cache, and it is four lines.
- **The kill-the-leader-after-ack test.** Everything else is scaffolding for this assertion.

**What you can cut:** read replicas and tunable read consistency, hinted handoff, sloppy quorums, and asynchronous mode. Add asynchronous mode only at the end, as a flag, so you can measure the data loss it causes rather than describe it.

## Open questions and contested claims

**Leader-based or leaderless?** Qdrant and Weaviate both keep consensus out of the data path but differ on what replaces it — Qdrant fans out from a coordinating node gated by a consistency factor [9][10], Weaviate is leaderless Dynamo-style with read repair [12]. Both ship. The evidence does not settle which is better; it settles only that neither chose to route every write through Raft.

**Is `write_consistency_factor` defaulting to 1 defensible?** It means a write can be acknowledged after reaching a single replica, which is asynchronous replication by another name. Qdrant's position is that the client is told the write is `Acknowledged` rather than `Completed` and should resubmit [9]. Whether an API distinction is sufficient protection against a durability expectation is a genuine disagreement.

**How weak is too weak?** Vespa's wall-clock last-writer-wins at document granularity [14] would be considered a defect by the standards of this chapter, and it is a widely deployed production system. The honest reading is that consistency strength is a product decision, not a correctness ladder — but that whichever rung you pick must be stated precisely and tested, which is the part systems most often skip.

## Sources

[1] Kingsbury — "Jepsen: Elasticsearch 1.5.0" — Jepsen, 2015 — https://aphyr.com/posts/323-jepsen-elasticsearch-1-5-0
[2] Oracle — "Semisynchronous Replication", MySQL Reference Manual §19.4.10 — https://dev.mysql.com/doc/refman/8.4/en/replication-semisync.html
[3] Confluent — "Kafka Replication" — Confluent documentation, accessed 2026 — https://docs.confluent.io/kafka/design/replication.html
[4] Vanlightly — "How to Lose Messages on a Kafka Cluster — Part 2" — 2018 — https://jack-vanlightly.com/blog/2018/9/18/how-to-lose-messages-on-a-kafka-cluster-part-2
[5] Ivanov — "Almost consensus: ABD and the edges of quorum replication" — The Consensus, Aug 2026 — https://theconsensus.dev/p/2026/08/02/almost-consensus.html
[6] DeCandia et al. — "Dynamo: Amazon's Highly Available Key-value Store" — SOSP 2007 — https://www.cs.cornell.edu/courses/cs5414/2017fa/papers/dynamo.pdf
[7] Elastic — "Sequence IDs: Coming Soon to an Elasticsearch Cluster Near You" — Elastic engineering blog — https://www.elastic.co/blog/elasticsearch-sequence-ids-6-0
[8] Elastic — "Elasticsearch Internals: Tracking in-sync shard copies" — Elastic engineering blog — https://www.elastic.co/blog/tracking-in-sync-shard-copies
[9] Qdrant — "Consistency Guarantees" — Qdrant documentation — https://qdrant.tech/documentation/scaling/consistency-guarantees/
[10] "Write Consistency and Replication" — source-derived reference for qdrant/qdrant — https://deepwiki.com/qdrant/qdrant/6.3-write-consistency-and-replication
[11] Weaviate — "Manage cluster metadata with RAFT for better reliability", issue #4280 — https://github.com/weaviate/weaviate/issues/4280
[12] Weaviate — "Consistency", Replication Architecture documentation — https://docs.weaviate.io/weaviate/concepts/replication-architecture/consistency
[13] Zilliz — "In-Memory Replica" — Milvus documentation v2.5.x — https://milvus.io/docs/v2.5.x/replica.md
[14] Vespa.ai — "Vespa Consistency Model" — Vespa documentation — https://docs.vespa.ai/en/content/consistency.html
[15] Qdrant — "Upserts partially committed during node restart", issue #4626 — https://github.com/qdrant/qdrant/issues/4626
[16] Qdrant — "Node restart during deletes causes inconsistency", issue #4627 — https://github.com/qdrant/qdrant/issues/4627
[17] Qdrant — "Data inconsistent after migration", issue #5101 — https://github.com/qdrant/qdrant/issues/5101
[18] Weaviate — "Data loss in 1.26.4 whenever pod restarted", issue #7162 — https://github.com/weaviate/weaviate/issues/7162
