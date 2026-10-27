# 16. Consistency models

> The precise statement of what a client is promised when it reads — and the reason "strongly consistent" on a marketing page tells you nothing.

## The problem

The moment there is more than one copy of the data, "what does a read return?" stops having an obvious answer. A follower may be behind. A deposed leader may still be answering from memory. Two clients may read at the same instant and disagree.

A consistency model is the contract that closes this question. Without one, every application-level bug becomes unfalsifiable: you cannot say a database is wrong if you never said what right meant.

The failure this prevents is specific and common. A user uploads a document, the write is acknowledged, the UI immediately re-queries, the read is routed to a follower that has not received the record, and the document appears not to exist. Nothing crashed. No error was returned. The system did exactly what it promised — the promise was just never written down.

For a vector database this matters more than the phrase "eventually consistent, it's just search" suggests. A search is a read over an index derived from replicated state. If the index on one replica lags, two identical queries return different neighbours, and recall becomes a function of which replica answered.

## How it works

The models form a lattice, from strongest to weakest. Each strengthening costs availability, latency, or both.

### Linearizability

Herlihy and Wing's definition, as restated precisely by Jepsen, requires three things: a **single total order** over all operations; that this order is **consistent with real (wall-clock) time**, so if A completes before B begins then A precedes B; and that each operation's return value obeys the object's sequential semantics in that order [1]. Every operation appears to take effect atomically at some instant between its invocation and its response [1].

Two consequences people routinely miss:

- Linearizability is a **per-object** guarantee. Some systems provide it per key, some per table, rarely across databases. The multi-object strengthening is **strict serializability**, not linearizability [1].
- It is **unavailable under partition** in general: "this model cannot be totally or sticky available; in the event of a network partition, some or all nodes will be unable to make progress" [1]. That is the CP side of CAP, made precise.

### Sequential consistency

Lamport's formulation, as restated by Jepsen: the result of any execution is the same as if all operations were executed in some sequential order, with each process's operations appearing in that sequence in program order [2]. It keeps the single total order and per-process order, and **drops the real-time constraint** [2].

The practical consequence is sharp: processes can observe arbitrarily stale data as long as everyone agrees on *some* consistent order [2]. That makes it unsuitable when a process learns about an event through a side channel — a human saying "I just wrote X" — and expects that observation respected. Only linearizability ties order to wall-clock time [2].

### Causal consistency

Decomposed by Jepsen, citing Viotti and Vukolić, into three conditions: **CausalVisibility** (happens-before is respected by what each process can observe), **CausalArbitration** (happens-before is respected by conflict resolution), and **RVal** (returned values respect single-object sequential semantics) [3]. Causally *unrelated* operations may be seen in different orders by different processes — that relaxation is the entire point [3].

Causal consistency is strictly stronger than read-your-writes: it "includes read-your-writes semantics" [3]. It is not free. If you need total availability, you must give up causal consistency and read-your-writes, though you can still keep writes-follow-reads, monotonic reads, and monotonic writes [3].

### Eventual consistency and the session guarantees

Vogels' canonical and weakest useful definition: "if no new updates are made to the object, eventually all accesses will return the last updated value" [4]. Read that carefully. It makes **no claim about how long** "eventually" is, and **no claim at all** about what any individual read returns before convergence. A system that returns garbage for an hour and then converges satisfies it.

The useful strengthenings are client-centric, and are what applications actually need [4]:

| Guarantee | Statement |
|---|---|
| Read-your-writes | after a process updates an item, it always sees the updated value and never an older one |
| Monotonic reads | if a process has seen a value, subsequent accesses never return previous values |
| Monotonic writes | the system serializes writes by the same process in issue order |
| Session consistency | the above, scoped to a session, with **no guarantee across session boundaries** |

These are four of the guarantees defined by Terry et al. in the Bayou work — read-your-writes, monotonic reads, monotonic writes, and **writes-follow-reads** [5]. Vogels restates three verbatim and folds writes-follow-reads under causal consistency [4].

Session consistency is usually the right target for a learning system. It is implementable with a per-client version token — the client sends the highest LSN it has observed, and any replica below that refuses to serve the read or waits — and it eliminates the "my own write vanished" class of bug, which is the one users actually report.

### CAP, and why it explains less than it seems to

Brewer conjectured in 2000 that a distributed service cannot simultaneously provide consistency, availability, and partition tolerance [6]. Gilbert and Lynch proved it for a specific case — an atomic (linearizable) read/write register — defining consistency as returning a response correct per the service specification, availability as every request eventually receiving a response, and partition tolerance as servers being split into groups that cannot communicate [7]. The theorem: in a network subject to communication failures, no service can implement atomic read/write shared memory that guarantees a response to every request [7]. The proof constructs a partition where a minority-side server cannot distinguish "no write happened" from "a write happened on the other side," so it must answer possibly-wrongly or not answer [7].

Kleppmann's criticism is the part worth internalising, because it is why CAP is misapplied constantly [8]:

1. CAP's "Consistency" means *only* linearizability, not the colloquial or ACID sense. Its "Availability" requires *every non-failing node* to answer — far stronger than "highly available".
2. CAP models one object and one failure mode. It says nothing about multi-object transactions, and nothing about crashes, disk failures, misconfiguration, or bugs — which dominate real outages — nor about latency, which users care about more than availability.
3. A single-leader system refuses writes during a partition (failing CAP-Availability) *and* permits stale follower reads (failing linearizability). It is neither CP nor AP. "A huge amount of subtlety is lost by putting a system in one of two buckets" [8].

The recommendation is to stop labelling systems CP or AP and instead state the model guaranteed **per operation** [8].

### PACELC

Abadi's reformulation asks two questions: if there is a partition (P), how does the system trade availability and consistency; else (E), in normal operation, how does it trade latency and consistency [9]. The point is that CAP is silent about the common case, where every replicated system still faces a latency/consistency choice — wait for more replicas for stronger consistency, or return early for lower latency [9]. Dynamo is PA/EL; a strict single-leader-quorum system tends toward PC/EC.

This is the framework that makes synchronous versus asynchronous replication a *named* decision rather than an implementation detail. See [Replication](15-replication.md).

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Read routing (leader-only vs. any replica) | staleness of reads | leader for strong, any for search | follower reads silently violate read-your-writes |
| Client version token | session consistency | highest observed LSN | absent: "my write vanished" bugs |
| Write quorum `W` / read quorum `R` | overlap guarantee | `R + W > RF` | non-overlapping quorums read stale data with no error |
| Lease duration | stale-read window on a deposed leader | seconds | too long: a partitioned leader serves stale reads for the whole lease |
| Wait-for-commit before read | whether reads see uncommitted state | on | off: a failover makes a previously-read value disappear |

## How real systems do it

**Elasticsearch** ties consistency to sequence numbers and primary terms, and Jepsen's 2015 analysis of version 1.5.0 found up to 25% of acknowledged writes lost under partition and roughly 10% lost on an ordinary process crash with no partition at all, because acknowledgements were issued before the translog fsync [10]. The lesson generalises past Elasticsearch: a consistency model is worthless if the durability layer underneath it acknowledges early.

**MongoDB** is the clearest case of the model being weaker than the label. Jepsen's 2020 analysis of MongoDB 4.2.6 examined the interaction of read and write concerns and found that the defaults did not provide the guarantees users assumed [11]. The instructive part is not the specific findings but that the guarantee depended on a *combination* of per-operation settings, which is exactly Kleppmann's argument against whole-system labelling [8].

**Qdrant** exposes consistency as explicit per-request knobs — a `write_consistency_factor` and a read `consistency` parameter — and defaults the write factor to 1, meaning a write can be acknowledged after reaching a single replica [12]. Qdrant's position is that the client is told the write is `Acknowledged` rather than `Completed`, and should resubmit [12]. Whether an API-level word is adequate protection against a durability expectation is a genuine disagreement, not a settled question.

**Weaviate** is leaderless in the data path, Dynamo-style, with tunable quorums and read repair, and documents consistency per operation rather than per system [13].

**Vespa** takes the weakest documented position of the group: wall-clock last-writer-wins at document granularity [14]. By the standards of this chapter that is a defect, and it is a widely deployed production system — which is the honest evidence that consistency strength is a product decision, not a correctness ladder.

The convergent design worth noting: all four expose consistency **per operation**, none claim a single system-wide label. That convergence is a stronger argument than any of them makes individually.

## Complexity and cost

Consistency costs round trips, and the cost is paid in the common case, not only under failure:

| Model | Write cost | Read cost |
|---|---|---|
| Linearizable | majority or full in-sync set round trip | leader, lease-gated, or quorum read |
| Sequential | total order, no real-time gate | any replica in the agreed order |
| Causal | dependency tracking metadata per write | any replica that has the dependencies |
| Session | version token, no extra round trip | replica must be at or above the client's LSN |
| Eventual | local write, background propagation | any replica, any time |

Causal consistency's cost is metadata, not latency: dependency vectors grow with the number of writers. That is why almost no production system offers it as the default.

## Failure modes

- **Read-your-writes violated by follower routing.** The single most common real bug. A load balancer sends the read to a different replica than the write.
- **Reads going backwards.** A client reads value v2, then reads v1, because the second read hit a lagging replica. Monotonic reads exist to forbid exactly this.
- **A deposed leader serving stale reads.** Epoch fencing stops it corrupting the new log, because that requires sending a message that gets refused. It does **not** stop it answering a read from its own memory, because reading requires talking to nobody. Only a lease bounds that window. See [Failure detection](18-failure-detection.md).
- **Uncommitted reads surviving into a failover.** If reads can observe records above the commit point, a failover to a replica that never received them makes a previously-read value disappear.
- **Quorums that do not overlap.** `R + W ≤ RF` reads stale data with no error and no warning.
- **Session guarantees that end at the session boundary.** Vogels is explicit that there is no guarantee across sessions [4]. A client that reconnects starts over.

## How to test it

Consistency bugs are ordering bugs, and ordering bugs do not reproduce under a fixed schedule. Three techniques carry almost all the weight:

- **History checking.** Record every operation's invocation and response with timestamps, then check the recorded history against the model. This is what Jepsen does, and it is the only method that can *falsify* linearizability rather than fail to find a counterexample. A linearizability checker is expensive (the problem is NP-hard in general) but tractable on short histories.
- **Fault injection during the read path**, not just the write path. Partition the cluster, promote a replica, then read from the old leader. Assert the model you claimed — if you claimed session consistency, assert the client's token is honoured; if you claimed nothing, say so in the README rather than testing nothing.
- **A test that asserts the violation you accept.** If a deposed leader may serve stale plain reads, write a test that demonstrates it and names it. An accepted weakness with a test is documentation; an accepted weakness without one is a bug nobody has found yet.

Property to assert on every run: for each client, the sequence of values it observes for a key is non-decreasing in version. That single check catches monotonic-read violations, follower-routing bugs, and uncommitted-read leakage.

## Minimal demo version

Implement **session consistency with a client version token**, and nothing else.

The write path returns the assigned LSN. The client stores the highest LSN it has seen and sends it with every read. A replica below that LSN refuses the read (or waits, briefly). That is perhaps thirty lines, and it makes the mechanism visible: you can watch a read get refused by a lagging replica, then succeed after catch-up.

Then add one deliberately broken mode behind a flag — route reads to any replica, ignoring the token — and a test that shows read-your-writes being violated. The pair is the lesson.

**What you can cut:** linearizable reads, quorum reads, causal metadata, read repair, and any form of multi-object guarantee. None of them change what the demo teaches.

**What you cannot cut without the demo becoming a lie:** the distinction between applied and committed. If reads can observe uncommitted state, every consistency claim above it is meaningless, and a failover demo will "work" for the wrong reason.

## Open questions and contested claims

**Is CAP still useful?** Kleppmann argues the CP/AP labelling should be retired entirely because the definitions are narrower than everyone assumes and the classification loses the subtlety that matters [8]. Gilbert and Lynch's theorem itself is not disputed — it is a proof [7]. The disagreement is entirely about whether the framework helps or misleads practitioners, and the weight of recent argument is on "misleads".

**Is a default of `write_consistency_factor = 1` defensible?** Qdrant says yes, on the grounds that the API distinguishes `Acknowledged` from `Completed` and the client should resubmit [12]. The counter-argument is that defaults are what get deployed, and a default that acknowledges after one replica is asynchronous replication regardless of what the response field is called. Both positions are held by people who have shipped systems.

**How weak is too weak?** Vespa's wall-clock last-writer-wins [14] would be judged a defect by this chapter's standards and runs in production at scale. The honest reading is that the rung you pick is a product decision — but that whichever rung you pick must be stated precisely and tested, which is the part systems most often skip.

## Sources

[1] Kingsbury — "Linearizability", Jepsen Consistency Models — https://jepsen.io/consistency/models/linearizable
[2] Kingsbury — "Sequential Consistency", Jepsen Consistency Models — https://jepsen.io/consistency/models/sequential
[3] Kingsbury — "Causal Consistency", Jepsen Consistency Models — https://jepsen.io/consistency/models/causal
[4] Vogels — "Eventually Consistent" — ACM Queue 6(6), 2008 — https://www.cs.princeton.edu/courses/archive/fall15/cos518/papers/ec-vogels.pdf
[5] Terry, Demers, Petersen, Spreitzer, Theimer, Welch — "Session Guarantees for Weakly Consistent Replicated Data" — PDIS 1994
[6] Brewer — "Towards Robust Distributed Systems" — PODC 2000 keynote — https://people.eecs.berkeley.edu/~brewer/cs262b-2004/PODC-keynote.pdf
[7] Gilbert, Lynch — "Brewer's Conjecture and the Feasibility of Consistent, Available, Partition-Tolerant Web Services" — ACM SIGACT News 33(2), 2002 — https://groups.csail.mit.edu/tds/papers/Gilbert/Brewer2.pdf
[8] Kleppmann — "Please stop calling databases CP or AP" — 2015 — https://martin.kleppmann.com/2015/05/11/please-stop-calling-databases-cp-or-ap.html
[9] Abadi — "Consistency Tradeoffs in Modern Distributed Database System Design: CAP is Only Part of the Story" — IEEE Computer 45(2), 2012
[10] Kingsbury — "Jepsen: Elasticsearch 1.5.0" — 2015 — https://aphyr.com/posts/323-jepsen-elasticsearch-1-5-0
[11] Kingsbury — "Jepsen: MongoDB 4.2.6" — 2020 — https://jepsen.io/analyses/mongodb-4.2.6
[12] Qdrant — "Consistency Guarantees" — https://qdrant.tech/documentation/scaling/consistency-guarantees/
[13] Weaviate — "Consistency", Replication Architecture — https://docs.weaviate.io/weaviate/concepts/replication-architecture/consistency
[14] Vespa.ai — "Vespa Consistency Model" — https://docs.vespa.ai/en/content/consistency.html
