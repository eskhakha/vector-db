# 18. Failure detection, fencing, and split brain

> Deciding that a node is dead when you fundamentally cannot know, and containing the damage when that decision is wrong.

## The problem

A leader stops answering. Is it crashed, garbage-collecting, swapping, or on the far side of a network partition? **You cannot tell.** Every distinguishing test you could run is itself a message that might be delayed or lost.

This is not an engineering gap. Fischer, Lynch and Paterson proved in 1985 that no deterministic algorithm solves consensus in an asynchronous system if even one process may crash [1]. Chandra and Toueg's response was to stop pretending: model the failure detector as an explicitly *unreliable* oracle, characterise it by two properties — **completeness** (every crashed process is eventually suspected) and **accuracy** (correct processes are not suspected forever) — and show that an eventually-weakly-accurate detector, `◇S`, is the weakest one sufficient for consensus [2][3].

So every practical failure detector is a timeout, and every timeout is a guess. The design question is not how to be right. It is **what happens when you are wrong.**

## How it works

### The four layers, and what each actually buys

This is where most explanations go wrong, by treating these as alternatives rather than a stack. They are a stack, and each layer contains the damage the one below it cannot prevent.

```
   heartbeats + timeout          CREATES the split-brain window
        │                        (it is a guess, and guesses are sometimes wrong)
        ▼
   epoch / term fencing          BOUNDS the damage to stale reads
        │                        (a deposed leader cannot corrupt the new log)
        ▼
   leader leases                 BOUNDS the stale-read window in time
        │                        (rests on bounded clock drift)
        ▼
   consensus (majority)          ELIMINATES it
                                 (two majorities must intersect)
```

**Heartbeats do not fix split brain. They cause it.** A system with no failure detector has no split brain, because nothing ever promotes anyone — it just stays down. The moment you add automatic promotion you have accepted that a wrong guess produces two nodes that both believe they lead.

### Heartbeats and the timeout trade

Every node sends a periodic heartbeat carrying its identity and, usefully, its per-shard progress. Piggybacking replication progress means the deciding authority always knows who is caught up, which is exactly what promotion needs.

Three timeouts, and the tension between them:

| | Too short | Too long |
|---|---|---|
| Heartbeat interval | wasted traffic | slow detection floor |
| Suspect threshold | healthy nodes flap under load spikes | slow detection |
| Failure threshold | premature promotion, split brain | long write unavailability |

The failure threshold is the honest one to argue about, because it sets your write-unavailability window directly. A five-second threshold means five seconds of failed writes on every leader crash, and a shorter one means more false positives — more promotions of nodes that were merely slow.

### Phi-accrual: suspicion as a number, not a boolean

A fixed timeout throws away information. Phi-accrual instead maintains a sliding window of inter-arrival times and outputs a continuous suspicion level `φ`, where `φ ≈ X` corresponds roughly to a `10^-X` probability that the node is actually alive [4][5]. Akka and Cassandra default to a threshold of 8, meaning roughly one false suspicion in `10^8` [5].

The value is adaptivity: a network whose latency doubles overnight raises the effective timeout automatically instead of flapping every node. The cost is a statistical model to debug, and on a low-variance network it buys nothing at all.

### SWIM: separating detection from dissemination

Naive all-to-all heartbeating is `O(n²)` messages. SWIM's contribution is to split two jobs that are usually conflated [6]:

- **Detection** — each node periodically probes one random peer. If the direct probe fails, it asks `k` other nodes to probe indirectly, which distinguishes "this peer is dead" from "my link to this peer is bad".
- **Dissemination** — membership changes propagate by gossip, piggybacked on the probe traffic.

Plus a **suspected** state between alive and dead, so a slow node gets a chance to refute before it is declared gone. Message load per node is constant regardless of cluster size [6].

### Split brain, precisely

> Two nodes simultaneously believe they lead the same shard, and both accept writes, producing two divergent histories with no way to reconcile them without discarding acknowledged data.

That last clause is what makes it serious. Divergence you can reconcile is a merge problem. Divergence where both sides acknowledged writes to clients is data loss no matter which side you keep.

### Fencing tokens: the actual fix for the damage

Kleppmann's example is the canonical one [7]. A client holds a lock, pauses for a long garbage collection, its lease expires, another client acquires the lock — and then the first client wakes up and writes, still believing it holds the lock. It has no way to know time passed.

A timeout cannot solve this, because the paused process cannot observe its own pause. **The fix is a monotonically increasing token issued with the lock and checked by the storage layer**: once the storage has seen token 34, it rejects a write carrying token 33 [7]. The paused writer is refused not because anyone detected its pause, but because the world moved on and its token is stale.

This is exactly what an epoch (or Elasticsearch's *primary term*) is. Kleppmann's specific criticism of the Redlock algorithm is that it provides no such token [7].

**What fencing does and does not buy.** It guarantees a deposed leader cannot corrupt the new leader's log — its replication messages are refused. It does **not** stop that node from serving stale reads from its own memory until it learns it has been deposed, because reading requires talking to nobody.

### Leases: bounding the stale-read window

A leader may serve strong reads only while holding an unexpired lease. Set the lease duration shorter than the failure threshold, and the old leader's lease expires before the new leader is installed — so there is no instant at which two nodes both believe they may serve strong reads. This is essentially CockroachDB's range lease.

The assumption is bounded **clock drift rate**, not synchronised clocks. That is a much weaker requirement, but it is not nothing, and it should be stated rather than assumed.

## Parameters and knobs

| Parameter | Controls | Typical | If you get it wrong |
|---|---|---|---|
| Heartbeat interval | detection floor | 250 ms – 1 s | Too long: nothing detects quickly. Too short: traffic for nothing |
| Suspect threshold | when to stop routing reads | 3 missed beats | Too tight: flapping under load |
| Failure threshold | when to promote | 5–10 s | Too tight: promotions of merely-slow nodes, i.e. split brain. Too loose: long write outage |
| Lease duration | stale-read window | < failure threshold | If it exceeds the failure threshold, two leaders can both serve strong reads |
| `φ` threshold (phi-accrual) | suspicion sensitivity | 8 [5] | Lower: more false positives. Higher: slower detection |
| Indirect probe count `k` (SWIM) | distinguishing bad link from dead node | 3 [6] | Too low: your own bad link reads as a dead peer |

## How real systems do it

**Elasticsearch** issues a **primary term** — a monotonic number bumped on every primary change and used to reject operations from a stale primary [8]. Textbook fencing, and the most directly reusable pattern here.

**Qdrant** derives failure signals from the data path rather than a separate heartbeat, absorbing brief blips in a soft local registry before escalating to its Raft-managed cluster state. Failure detection is a consequence of replication traffic rather than a parallel subsystem.

**Milvus** fails coordinators over using an etcd lease with a 60-second default, and its own documentation states that the handover between active and standby is **not strongly consistent** [9]. A production system, documenting its own split-brain window rather than hiding it — which is the right instinct even though the window is large.

**Vespa** has an open, self-documented split-brain bug in its cluster controller: the leader writes cluster state to ZooKeeper **without a test-and-set**, so a deposed leader can race a newly elected one and clobber the state [10]. This is the best available evidence that split brain is not a theoretical concern — it is a live issue in a mature, widely deployed system.

**etcd** shows the subtler failure. Jepsen found etcd 3.4.3's core Raft key-value layer strict-serializable, and its *distributed lock* "fundamentally unsafe", losing roughly 18% of updates under contention with 2-second leases [11]. **Raft-safe does not make everything built on Raft safe.** A lock handed out by a consensus system is still just a lease, and a lease without a fencing token still has the GC-pause problem.

The pattern across all five: everyone has fencing at the log layer, and the residual exposure is always at the layer above — stale reads, cluster-state races, locks.

## Complexity and cost

| | Cost |
|---|---|
| All-to-all heartbeats | `O(n²)` messages per interval |
| SWIM | `O(1)` per node per interval, independent of `n` [6] |
| Detection latency | at least the failure threshold; no detector beats its own timeout |
| Write unavailability per failover | failure threshold + promotion time |
| Fencing check | one integer comparison per replication message |
| Lease check | one clock read per strong read |

Detection latency and false-positive rate trade against each other and cannot both be optimised. That is the FLP result showing up as an engineering constraint rather than a theorem.

## Failure modes

- **A garbage collection pause longer than the lease.** The paused leader cannot observe its own pause. Only a fencing token saves you [7].
- **Asymmetric partition.** A can reach B, B cannot reach A. Naive detectors declare each other dead; SWIM's indirect probes exist for exactly this [6].
- **Flapping.** A node oscillates between suspected and alive, and each transition bumps an epoch and moves leadership. A rising epoch counter is the metric that reveals this, and it is worth alerting on.
- **The deciding authority is itself partitioned.** A single controller has no way to know it is the isolated one. This is the failure mode that no amount of heartbeat tuning fixes and that consensus exists to solve.
- **Promoting a node that was not in sync.** Every write it missed is silently gone. See [Replication](15-replication.md).
- **Trusting a lock as if it were a lock.** It is a lease. Without a fencing token it is advisory [7][11].
- **Clock jumps.** An NTP step backwards can extend a lease past its intended expiry. Use a monotonic clock for lease arithmetic, never wall time.

## How to test it

The valuable thing here is that **you can test split brain directly**, which most people never do because it needs real network partitions. In a simulated cluster it is a function call.

- **Stale-epoch rejection.** Send a follower a message from a deposed leader and assert it is refused and nothing was applied.
- **The demonstration test.** Partition a leader, promote a survivor, then let the old leader serve a read *before* it learns it was deposed — and assert it returns stale data. Write the test to assert the broken behaviour, with a comment saying exactly which mechanism will fix it. A test that documents a known hole is worth more than a comment claiming there isn't one.
- **Then add leases and flip that test** to assert the read is now refused. The diff between those two runs is the entire value of leases, made concrete.
- **False positives under load.** Inject latency above the failure threshold on a *healthy* node and assert the system either tolerates it or fails safely. This is where fixed timeouts flap.
- **Asymmetric partition.** Break one direction only.
- **Pause, don't kill.** `SIGSTOP` (or a simulated equivalent) models a GC pause far better than a kill, because the process comes *back* believing nothing happened. This is the case fencing tokens exist for and the case a kill test never exercises.
- **Epoch monotonicity as an invariant.** Across a long chaos run, assert the epoch never decreases anywhere.

## Minimal demo version

Heartbeats plus a fixed timeout plus a monotonic epoch on every replication message is roughly 120 lines, and with an in-process transport you can inject partitions deterministically from a seed.

**What you must not cut:**

- **The epoch on every message, checked by the receiver.** Without it the demo cannot exhibit the failure the whole subsystem exists to prevent.
- **The rule that only an in-sync replica may be promoted.** Four lines, and it is the difference between a failover and a data-loss event.
- **A test that demonstrates the residual split-brain window.** Building failover and *not* showing where it is still broken teaches the wrong lesson — that heartbeats solved the problem. They did not; they created it and then bounded it.

**What to cut:** phi-accrual and SWIM. Phi-accrual adapts to latency variance, and an in-process simulated network has none, so it would be a statistical model with nothing to model. SWIM solves an `O(n²)` problem that does not exist at three nodes. Understand both; build neither.

**Sequencing.** Build the ad-hoc version — heartbeats, a single controller, epochs, leases — *before* consensus, deliberately. When you then write Raft you will already know from your own broken system exactly which problem it solves, rather than taking it on faith. That is worth more than arriving at the correct answer first.

## Open questions and contested claims

**Is a single controller acceptable before consensus?** It is a single point of failure and it cannot detect its own isolation. The argument for building it anyway is pedagogical, not architectural. Milvus ships something structurally similar and documents the weakness [9]; Vespa ships one with a known race [10]. Reasonable engineers disagree about whether shipping this shape at all is defensible, and both shipped it.

**Fixed timeouts or phi-accrual?** Phi-accrual is strictly better under variable latency and strictly more machinery under stable latency. No source found gives a variance threshold at which it starts paying for itself.

**How much does fencing actually buy without consensus?** More than usually credited. Under synchronous replication with a minimum in-sync set, a partitioned old leader cannot satisfy its own replication requirement, so it cannot acknowledge writes at all — its exposure collapses from "divergent histories" to "stale reads". Whether that residual is acceptable is a product decision, but it is a much narrower hole than the usual framing implies.

## Sources

[1] Fischer, Lynch, Paterson — "Impossibility of Distributed Consensus with One Faulty Process" — Journal of the ACM, 1985 — https://www.cs.rochester.edu/u/scott/courses/458/notes/FLP.pdf
[2] Chandra, Toueg — "Unreliable Failure Detectors for Reliable Distributed Systems" — Journal of the ACM 43(2), 1996 — https://www.cs.utexas.edu/~lorenzo/corsi/cs380d/papers/p225-chandra.pdf
[3] Aspnes — "Failure Detectors" — Yale University course notes — https://cs.yale.edu/homes/aspnes/pinewiki/FailureDetectors.html
[4] Hayashibara, Défago, Yared, Katayama — "The φ Accrual Failure Detector" — IEEE SRDS, 2004
[5] "Phi Accrual Failure Detector: Suspicion as a Continuous Score" — explainer referencing Hayashibara et al. 2004 and the Akka and Cassandra defaults — https://unseel.com/cs/phi-accrual-failure-detector
[6] Das, Gupta, Motivala — "SWIM: Scalable Weakly-consistent Infection-style Process Group Membership Protocol" — Cornell, DSN 2002 — https://www.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf
[7] Kleppmann — "How to do distributed locking" — Feb 2016 — https://martin.kleppmann.com/2016/02/08/how-to-do-distributed-locking.html
[8] Elastic — "Sequence IDs: Coming Soon to an Elasticsearch Cluster Near You" — https://www.elastic.co/blog/elasticsearch-sequence-ids-6-0
[9] Zilliz — "Coordinator HA" — Milvus documentation — https://milvus.io/docs/coordinator_ha.md
[10] Vespa — "Cluster controller ZooKeeper writes should be gated with a test-and-set", issue #4532 (open) — https://github.com/vespa-engine/vespa/issues/4532
[11] Kingsbury — "Jepsen: etcd 3.4.3" — Jan 2020 — https://jepsen.io/analyses/etcd-3.4.3
