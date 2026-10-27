# 19. Consensus and Raft

> The only mechanism that eliminates split brain rather than merely bounding it — and the reason most vector databases deliberately keep it off the data path.

## The problem

Epochs and leases bound split brain. They do not remove it. See [Failure detection](18-failure-detection.md) for the ladder: heartbeats *create* split brain by promoting on suspicion, epochs *bound* it by making a stale leader's writes get refused, leases *bound it in time* by making a leader stop serving before its replacement starts.

There is one hole none of them close.

A single designated controller — one node that watches heartbeats and decides who leads each shard — **cannot detect its own isolation**. From inside a partition, "I cannot reach nodes 2 and 3" and "nodes 2 and 3 are dead" are the same observation. So a partitioned controller will promote a replacement for a leader that is perfectly healthy on the other side, and nothing stops it, because it *is* the authority. Epoch checking does not help: the controller issues the epochs.

Consensus closes exactly this hole, and it closes it with one property: **a minority cannot commit**. A partitioned controller's decision never takes effect anywhere, including on itself.

That is the whole justification. Everything below is machinery in service of it.

## How it works

Raft is a replicated log. Every node applies the same entries in the same order, so every node's derived state is identical without any coordination beyond the log.

### Terms

Terms act as a logical clock, and they let servers detect obsolete information such as a stale leader [1]. Term numbers increase monotonically and are exchanged on every RPC [1]. A node that sees a higher term steps down immediately.

Three states: **leader** (handles all client requests), **follower** (passive), **candidate** (running an election) [1].

### Election

Election timeouts are chosen randomly from a fixed interval — Raft's recommended range is **150–300 ms** [1]. Randomisation is not decoration: it makes split votes rare, because usually only one server times out first, and it bounds worst-case election time [1].

`RequestVote` carries `term, candidateId, lastLogIndex, lastLogTerm` and returns `term, voteGranted` [1].

### Replication

`AppendEntries` carries `term, leaderId, prevLogIndex, prevLogTerm, entries[], leaderCommit` and returns `term, success` [1]. With an empty `entries[]` it is also the heartbeat [1].

The **Log Matching Property** is what makes this cheap: "If two entries in different logs have the same index and term, then logs are identical in all preceding entries" [1]. That is why `AppendEntries` can carry a two-field consistency check (`prevLogIndex`, `prevLogTerm`) instead of exchanging whole logs.

### The two safety rules that are easy to get wrong

**Election restriction.** A vote is granted only if the candidate's log is at least as up-to-date as the voter's, compared by last term then last index. Raft "uses the voting process to prevent a candidate from winning an election unless its log contains all committed entries" [2][1]. This is the mechanism enforcing the **Leader Completeness Property**: "If a log entry is committed in a given term, then that entry will be present in the logs of leaders for all higher-numbered terms" [1].

**Only commit entries from the current term.** "Raft never commits log entries from previous terms by counting replicas" [1]. Entries from prior terms become committed only *indirectly*, once an entry from the leader's own current term has been committed by majority replication — which then commits everything before it, by Log Matching [1].

This is the Figure-8 scenario, and it is the single subtlest rule in the protocol. A follower can hold an entry replicated to only a minority under an old term. If a different server later becomes leader in a newer term, it could legally overwrite that entry with different data. So a leader must never declare an old-term entry committed purely because a majority currently has a copy [1]. Implementations that skip this rule pass ordinary tests and lose committed data under a specific, reachable interleaving.

**Append-only leader rule:** "A leader never overwrites or deletes entries in its log; it only appends new entries" [1].

### Log repair

The leader keeps a `nextIndex` per follower — "the index of the next log entry the leader will send to that follower" [1]. On a consistency-check failure "the leader decrements nextIndex and retries" until the logs match, then overwrites the follower's conflicting suffix [1]. Linear backtracking is `O(log length)` round trips in the worst case; real implementations return a conflict term/index hint so the leader can skip many decrements at once.

### Snapshotting

"The entire current system state is written to a snapshot on stable storage, then the entire log up to that point is discarded" [1]. Each server snapshots independently. The **InstallSnapshot** RPC exists for the case a follower is so far behind that the entries it needs have been discarded [1].

### Membership change: joint consensus

A two-phase reconfiguration. The cluster first switches to a transitional **joint consensus** configuration combining old (C_old) and new (C_new) member sets; during that phase "agreement requires separate majorities from both the old and new configurations"; once the joint entry commits, the leader moves to C_new alone [1].

The reason for two phases is a genuine bug in the naive one-step approach: switching directly to C_new creates a window where a C_old majority and a C_new majority could each elect a different leader — split brain during reconfiguration. This hazard is what motivated joint consensus, and it is corroborated by an etcd pull request titled "raft: internally support joint consensus" and a raft-dev thread titled "bug in single-server membership changes" [3].

### Commit is asynchronous, and the ordering is load-bearing

A detail that is easy to get wrong when consensus is wired into an existing controller rather than designed in from the start.

Proposing a decision is not making it. `Propose` returning successfully means "accepted for replication", not "in effect" — the decision takes effect when the entry **commits**, which happens later and may never happen at all.

So the side effect that acts on the outside world — installing a new leadership epoch on a replica, redirecting a router, deleting a lease — must fire when the entry is **applied**, not when the decision is made. Installing it at proposal time fences the old leader on the strength of a decision that may never commit, which is precisely the failure consensus was introduced to prevent, reappearing one layer up.

The shape that works is a cursor: on each tick, apply everything newly committed and diff it against what you have already acted on, then act on the difference. That also makes the component naturally idempotent under restart, because the cursor is derived from the log rather than remembered.

There is a subtlety on the failure path. If installing the epoch fails, the shard must be marked unavailable as a **committed fact**, not as one controller's private opinion — otherwise another controller reading the same log still believes the shard has a healthy leader. That means proposing a second command, which will also commit asynchronously. Naively re-applying immediately after proposing it produces an infinite loop: the state has not moved, so the same failed install is retried and proposed again, forever. Propose once per epoch and let the next tick observe the result.

### Multi-Raft

One Raft group per shard, not one per node — TiKV "divides the Raft group in terms of partitions, namely, Region" [4]. A single group per node would make partitioning pointless, since each node hosts many shards that must be independently replicated [4].

The cost is heartbeat storms: hundreds of groups each ticking independently swamps CPU and network. TiKV's three mitigations are the standard answer [4]:

- **Batched ticking** — one event loop "polls all the Raft groups to drive the Raft state machine every 1000ms".
- **Batched persistence** — traverse ready groups and "use a RocksDB's WriteBatch to handle all appending data and persist the corresponding result at the same time", so many groups' log writes coalesce into one fsync.
- **Connection multiplexing** — "TiKV reuses the connection between two nodes for multiple Raft groups".

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Election timeout | how fast a dead leader is replaced | randomised in 150–300 ms [1] | too low: spurious elections under load; too high: long unavailability |
| Heartbeat interval | how often a leader asserts itself | roughly 1/10 of the election timeout | too close to the timeout: constant re-elections |
| Randomisation range | split-vote probability | the full timeout interval | fixed timeouts: repeated split votes, elections that never resolve |
| PreVote | whether a disconnected node can disrupt | on | off: a rejoining node deposes a healthy leader |
| CheckQuorum | whether an isolated leader steps down | on | off: an isolated leader keeps serving |
| Snapshot threshold | log length before compaction | tens of thousands of entries | too high: slow restarts and huge InstallSnapshot transfers |
| Group count (multi-Raft) | shards per node | tens to hundreds with batching | unbatched: heartbeat storm |

## How real systems do it

**Raft does not guarantee liveness under all network faults.** Howard and Abraham showed that in the original protocol a partially-connected topology can prevent a stable leader from ever emerging, even when a majority remain mutually connected: "Raft, as described in the original paper, does not guarantee liveness in all such cases" [5].

The concrete five-server scenario: servers 1–3 mutually connected, server 4 connects only to server 2, server 5 only to server 3. Server 3 is leader. Server 4 times out, sends `RequestVote` to server 2, server 2 bumps its term, and that forces the healthy leader to step down. A new leader is elected, 4 or 5 is still disconnected from it, and the cycle repeats — "This system will not be able to establish a stable leader" [5].

The fix is two mechanisms together: **PreVote** (a trial election round that does not increment the real term unless the candidate could actually win) and **CheckQuorum** (a leader that stops hearing from a majority steps down proactively). "Raft with PreVote and CheckQuorum guarantee[s] liveness" [5]. Both are standard production hardening; the original paper does not include them.

**"Understandable" is not "easy to implement correctly."** Antithesis, using deterministic simulation with fault injection, found bugs in **every** Raft implementation they tested — HashiCorp Raft, Aeron Cluster, OpenRaft, and MicroRaft — without needing real crashes or disk corruption; randomised scheduling and network faults sufficed [6].

Three distinct bugs in HashiCorp Raft alone [6]:

1. **Broken Consensus due to Async Heartbeats** — violated 4 of the 5 core Raft safety properties and caused different nodes to apply different commands at the same log index.
2. **Deadlock after Leadership Transfer** — the cluster became permanently unable to accept client requests.
3. **Livelock in Snapshot Installation** — followers could not replicate further and could exhaust disk creating temporary snapshot files.

A "Chain of Blocks" workload on 3-node clusters was enough to find the divergence. No exotic workload needed [6].

The epistemic point is the important one: "formal verification is not enough to maintain consensus in real systems", because "formal specifications are rarely truly complete — in the case of Raft, the TLA spec covers the core protocol, but doesn't speak to details like `installSnapshot`" [6]. The TLA+ spec proves the core protocol safe; real implementations add snapshotting, leadership transfer, and heartbeat batching, and that is exactly where bugs concentrate.

**etcd** is the most widely deployed implementation, and Jepsen confirmed **strict-serializable** consistency for etcd 3.4.3's key-value reads, writes and transactions across fault injection [7]. The gaps were not in log replication but in features built *on top* — the distributed lock service lost roughly 18% of updates under contention, plus a lease-revalidation bug, and an undocumented watch-from-revision-0 behaviour [7]. "Raft is safe" and "the database built around Raft is safe" are different claims, and most real incidents live in the surrounding machinery.

**Comparison to Paxos and Viewstamped Replication.** Paxos famously punts on leader election — Lamport writes that the details of choosing a new president are not specified — leaving liveness as an engineering problem outside the safety protocol, which is a major reason real Paxos deployments diverge from each other [8]. Viewstamped Replication (Oki and Liskov, 1988) uses **view changes** rather than terms: on suspecting the primary failed, replicas install a new one, which broadcasts `StartView` with the operation history; backups will not accept a `Prepare` for op-number n until they have all earlier entries — a direct analogue of Log Matching. The new primary's log is chosen by "the largest view-number; if several messages have the same view-number it selects the one among them with the largest op-number", functionally identical to Raft's election restriction in different vocabulary [8].

Raft's distinguishing claim is not new safety properties beyond VR or Paxos. It is understandability and a fully specified, implementable protocol: a strong leader model where entries only flow leader-to-follower, explicit separation of election, replication, safety, membership change and snapshotting, and a worked-out reconfiguration protocol [1][8].

### The design decision worth copying

**Vector databases keep consensus off the data path.** Qdrant uses Raft for cluster metadata while point writes go through direct leader-to-replica replication [9]; Weaviate added Raft for schema and cluster metadata in v1.25 while data replication stays leaderless [10]. Two independent teams, same split.

The reasoning: routing every vector write through a majority round trip buys a guarantee that a synchronous in-sync replica set plus an epoch fence already provides, at the cost of a quorum on the hot path. What consensus uniquely provides — a decision authority that cannot act while isolated — is needed for the *shard map*, which changes rarely, not for point writes, which are the hot path.

That convergence is stronger evidence than either system's own argument.

## Complexity and cost

For a group of `n` nodes:

| | Cost |
|---|---|
| Commit latency | one round trip to a majority, `⌈(n+1)/2⌉` nodes |
| Messages per entry | `O(n)` — leader to each follower, plus acks |
| Fault tolerance | `⌊(n-1)/2⌋` failures; 3 nodes tolerate 1, 5 tolerate 2 |
| Log storage | grows without bound until snapshotted |
| Election time | one to a few election timeouts, randomised |
| Log repair | `O(divergence)` round trips naively; `O(1)` with conflict hints |

Odd group sizes only: 4 nodes tolerate the same single failure as 3 while needing a larger majority, so the fourth node costs latency and buys nothing.

## Failure modes

- **Liveness loss under partial connectivity**, as described above — no leader ever stabilises, and the cluster is unavailable while every node looks healthy [5].
- **Figure-8 data loss.** Committing a previous-term entry by counting replicas loses committed data under a specific interleaving [1]. It will not show up in ordinary testing.
- **Split brain during reconfiguration** if membership changes in one step rather than through joint consensus [1][3].
- **Divergence at the same log index** — different nodes applying different commands at the same position. This is the catastrophic case, and it was found in a production Raft library [6].
- **Snapshot livelock**, where followers cannot make progress and accumulate temporary snapshot files until the disk fills [6].
- **Heartbeat storms** in multi-Raft when group count grows without batched ticking [4].
- **Bugs in the machinery around Raft** rather than in Raft — locks, watches, leadership transfer [7].
- **Acting on a decision before it commits.** The side effect fires, the entry never commits, and the cluster is left fenced by a decision that does not exist anywhere.
- **Re-applying immediately after proposing a follow-up command**, which under asynchronous commit re-observes unchanged state and proposes again on every pass — an infinite loop that presents as a hang, with no error.

## How to test it

Consensus cannot be tested by running it and seeing if it works. The bugs live in interleavings you will not produce by accident.

**Deterministic simulation is the technique.** Make each node a pure state machine — `Tick()` and `Step(msg)` return outgoing messages, with no threads, no locks and no wall clock. Drive everything from a seeded RNG and a virtual clock. Inject drop, delay, reorder and partition in the harness. This is how Antithesis found bugs in every implementation they tested, using nothing more exotic than randomised scheduling and network faults [6].

**Assert safety after every step, not at the end.** The five properties from the paper — election safety, leader append-only, log matching, leader completeness, state machine safety [1] — are checkable in a few lines each against the whole simulated cluster. A run that fails at step 412 of seed 137 is a bug report; a run that fails at the end is a mystery.

Specific scenarios worth writing:

- **The partitioned-controller test.** Isolate the leader into a minority, have it propose, and assert the proposal never commits anywhere including on itself. Then heal, and assert its uncommitted entry is discarded and it converges on the majority's decision. This is the test that justifies the whole component.
- **Keep the single-authority version and assert its failure.** A test that shows the lone decider promoting a replacement for a healthy leader is the motivation, and if it ever stops failing that way the contrast has silently disappeared and the consensus layer is no longer buying anything demonstrable.
- **Assert the side effect ran after the commit, not before.** Record the committed state at the instant the side effect fires and assert it already reflects the decision. Without this the ordering can be wrong for a long time and every functional test still passes.
- **The Figure-8 interleaving**, constructed by hand. Ordinary randomised runs may not produce it.
- **Partial connectivity**, not just clean partitions — the Howard–Abraham topology where 1–3 are connected, 4 reaches only 2, and 5 reaches only 3 [5]. Assert a leader stabilises, which fails without PreVote.
- **Partitioned senders, not just receivers.** A transport that only blocks inbound messages cannot model an isolated node at all.
- **Leader churn under load** — hundreds of elections with writes in flight, asserting no committed entry is ever lost.

Guard against vacuous runs. A chaos test that passes having committed six entries out of four hundred proposals is a green light on a broken system. Every fault-injection test needs liveness floors: a minimum number of successful commits, and an assertion that the interesting path actually fired.

## Minimal demo version

Implement Raft. Do not import it. Raft *is* the lesson, and a project that imports a Raft library has learned how to read a README.

The smallest version that genuinely teaches it: a pure state machine with terms, `RequestVote`, `AppendEntries`, the election restriction, the current-term commit rule, and `nextIndex` backtracking — driven by a seeded simulation with drop, delay, reorder and partition, asserting the safety properties after every step.

Then apply it to the **control plane only**: a replicated log of shard-map commands (`promote`, `set_isr`, `mark_read_only`), with a state machine that is a pure function of the committed log.

Build the **single-controller version first** and demonstrate the failure it has. Watching a partitioned controller promote a zombie leader, and then watching the same partition produce nothing at all under Raft, is the entire argument in two runs.

**What you can cut:** joint consensus (do single-node changes and say why they are unsafe), snapshotting and `InstallSnapshot`, leadership transfer, multi-Raft, batched persistence, and any real network.

**What you cannot cut without the demo becoming a lie:**

- **PreVote and CheckQuorum.** Without them the cluster deadlocks under partial connectivity and you will spend an evening finding out why [5]. Also: clear the cached leader when campaigning, or the CheckQuorum guard makes every node refuse every other node and nothing is ever elected — a failure with no error message.
- **The current-term commit rule.** Skipping it produces a system that passes tests and loses committed data.
- **The election restriction.** Without it a node missing committed entries can win, and Leader Completeness fails.
- **Asserting invariants every step.** End-of-run assertions turn reproducible bugs back into flakes.

## Open questions and contested claims

**Is Raft actually more understandable than Paxos?** That is the paper's central claim and it was supported by a user study [1]. Antithesis's finding of bugs in every implementation tested [6] does not refute understandability, but it does undercut the practical inference people draw from it — that an understandable protocol yields correct implementations. Those are different claims, and the evidence separates them.

**Should consensus govern the data path?** Qdrant and Weaviate both say no [9][10]. Systems like TiKV and CockroachDB say yes, running one Raft group per range [4]. The disagreement is not really about consensus; it is about what the workload needs. A transactional database needs a total order over key ranges; a vector search index does not, and an in-sync replica set with epoch fencing gives it the durability it needs without a quorum on every write. Neither side is wrong for its workload, and any system that adopts one position without stating the workload assumption has skipped the argument.

**How much does formal verification buy?** The Raft TLA+ specification proves the core protocol safe, and implementations still had safety bugs, because the spec does not cover `installSnapshot`, leadership transfer, or heartbeat batching [6]. The disagreement is over whether the answer is "verify more" or "verification cannot cover the operational surface, so invest in simulation testing instead". Antithesis argues the latter [6].

## Sources

[1] Ongaro, Ousterhout — "In Search of an Understandable Consensus Algorithm (Extended Version)" — Stanford; USENIX ATC 2014 — https://raft.github.io/raft.pdf
[2] "On Consensus Algorithms: Paxos, Viewstamped Replication and Raft" — Medium explainer citing Lamport 1998, Oki & Liskov 1988, Ongaro & Ousterhout 2014 — https://medium.com/@li.ying.explore/on-consensus-algorithms-paxos-viewstamped-replication-and-raft-c2638af5a97e
[3] etcd-io — "raft: internally support joint consensus", PR #10889, and the raft-dev thread "bug in single-server membership changes" — https://github.com/etcd-io/etcd/pull/10889 , https://groups.google.com/g/raft-dev/c/t4xj6dJTP6E
[4] TiKV — "Multi-raft", TiKV deep-dive documentation — https://tikv.org/deep-dive/scalability/multi-raft/
[5] Howard, Abraham — "Raft does not Guarantee Liveness in the face of Network Faults" — Decentralized Thoughts, 2020 — https://decentralizedthoughts.github.io/2020-12-12-raft-liveness-full-omission/
[6] Lim, Padhye, Primi — "Finding bugs in Raft implementations" — Antithesis, 2026 — https://antithesis.com/blog/2026/finding-bugs-in-raft-implementations/
[7] Kingsbury — "Jepsen: etcd 3.4.3" — 2020 — https://jepsen.io/analyses/etcd-3.4.3
[8] "On Consensus Algorithms: Paxos, Viewstamped Replication and Raft" — as [2]
[9] Qdrant — "Consistency Guarantees" — https://qdrant.tech/documentation/scaling/consistency-guarantees/
[10] Weaviate — "Manage cluster metadata with RAFT for better reliability", issue #4280 — https://github.com/weaviate/weaviate/issues/4280
