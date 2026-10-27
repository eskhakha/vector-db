# Lessons

Every bug this project produced, every assumption a measurement contradicted, and
the rule each one generalises to.

This file exists because the bugs are the most transferable output of the whole
exercise. The code shows what the system does; this shows what was wrong first,
what caught it, and why. A learning project that ships only its final state has
thrown away most of what it produced.

Three things are recorded per entry: **what was wrong**, **what caught it**, and
**the rule**. The middle column is the one worth reading twice — it is the
argument for which testing techniques earn their cost.

---

## Part 1 — Measurements that contradicted the design

### 1.1 Snapshots made recovery slower

**The assumption.** Snapshots exist to bound recovery time. This is stated in
every storage-engine text and it was built into this system on that basis.

**The measurement.** `BenchmarkRecovery`, insert-only, 50,000 writes:

| Workload | Replay only | Snapshot + replay | |
|---|---:|---:|---|
| One write per key | 48.4 ms | 54.6 ms | **13% slower** |
| Each key rewritten 10× | 36.1 ms | 7.5 ms | **4.8× faster** |

**Why.** A snapshot compacts away *superseded* versions. With one write per key
there are none, so the snapshot contains exactly as many records as the log,
saves no replay work, and costs one extra file read.

**The first instinct was to call it noise.** It is not noise; it is the
mechanism working exactly as specified, on a workload where the mechanism has
nothing to do.

**The rule.** A snapshot's benefit is proportional to the log's **write
amplification** — records per *live* key — not to dataset size. Two operational
consequences follow that the naive claim hides: snapshotting an append-only
collection on a timer is wasted I/O, and a snapshot only shortens recovery if
the WAL can then be **truncated**, which requires segments small enough that
whole ones can be dropped. One giant segment means replay covers everything
regardless, and you have paid for the snapshot without collecting.

Written up in `concepts/13-recovery-and-compaction.md` § "When a snapshot is a
net loss" and `BENCHMARKS.md` §4.

### 1.2 Numbers that moved between runs — NOT a finding, a methodology failure

**What happened.** The second follower measured **+7%** in one run and **+49%**
in another. The fsync ratio measured **25×** once and **10×** later. A write
burst measured **1,287** once and **52** on repeat.

**I originally recorded this as a contradiction. It is not.** It is noise, and
the cause is that each configuration was run **once**.

Go's own tooling is blunt about it: benchstat's documentation says "Each
benchmark should be run at least 10 times to gather a statistically significant
sample of results", and the p-value it computes exists precisely to measure "how
likely it is that any differences were due to random chance (i.e., noise)" [B1].

Two environmental amplifiers, neither visible from inside the benchmark:

- **CPU steal.** On a shared virtualised host, steal is "the percentage of time a
  virtual CPU waits for a real CPU while the hypervisor is servicing another
  virtual processor", and sustained values above ~10% materially degrade
  performance [B2]. It varies with what neighbouring tenants are doing. On
  2 vCPUs the proportional effect is larger, because displaced work has nowhere
  to go.
- **Container storage.** fsync here goes through an overlay filesystem onto
  virtual block storage, not a device with a power-loss-protected cache. The
  latency is a property of that stack, not of the durability mechanism.

**What survives.** The *shape* of the replication claim still holds — cost is set
by the slowest replica you wait for, not the sum, because fan-out is concurrent.
The magnitude does not, and quoting it was the error.

**The rule.** A benchmark without its machine, its run count and its spread is
not a measurement. This entry is an instance of rule 14 with me on the wrong
side of it — which is the most useful thing in the file, because it is the
failure mode that survives knowing better.

### 1.3 Search throughput was flat, and that was the finding

**What was expected.** Latency scales linearly with `N`. It does — 100× the
vectors, 99× the time.

**What was informative.** Throughput stayed at 2.7–3.5 GB/s across a 100× change
in dataset size. Flat throughput is the signature of a **memory-bandwidth-bound**
loop.

**The rule.** That single fact kills a whole category of work: micro-optimising
the distance kernel would buy nothing, because the kernel is already saturating
the memory system. The only way past brute force is to touch fewer bytes — which
is what quantization, IVF and HNSW each do differently. A benchmark that
redirects effort is worth more than one that confirms a design.

### 1.4 Copy-on-write improved writes and made reads twice as slow

**The expectation.** Replace the shard's read lock with an immutable published
snapshot, and readers stop blocking. Standard technique, well understood.

**The measurement**, N=100k, four writers, 50 iterations:

| | writes | search p99 |
|---|---:|---:|
| RWMutex, fsync | 53 | 3,769 µs |
| COW, fsync | 131 | 7,680 µs |
| RWMutex, no fsync | 53 | 3,252 µs |
| COW, no fsync | 187 | 8,625 µs |

Writes improved 2.5–3.5× — and fsync started mattering again (131 vs 187),
which is the signal the lock is no longer the sole bottleneck. Search p99
roughly doubled.

**Why, and it is not mysterious.** Copy-on-write has a stated precondition. Java's
`CopyOnWriteArrayList` documentation puts it plainly: "This is ordinarily too
costly, but may be *more* efficient than alternatives when traversal operations
vastly outnumber mutations" [B3]. This benchmark runs four writers flat out. The
technique was applied against its own contract.

Three documented mechanisms then compound:

- `GC frequency = (Allocation rate) / (New heap memory)`, and "doubling
  allocation rate doubles GC CPU cost" [B4]. Copying ~800 KB per write raises
  the allocation rate by orders of magnitude.
- "the GC takes 25% of CPU resources when in the mark phase" [B4]. On two cores
  that is half the machine.
- Concurrent marking streams memory and evicts cache. On an LLC-sensitive
  microbenchmark this caused "3 × increase in iteration duration" and "a
  1700-fold increase in LLC misses", while ordinary workloads saw "at most 10 %
  slow-down" because they "lack sensitivity to the LLC size" [B5]. A brute-force
  vector scan is the sensitive case.

**The rule.** Copy-on-write does not remove a cost, it *moves* it — from
lock-wait to allocation. Whether that is a win depends on the read:write ratio,
the copy granularity, whether the reader is memory-bandwidth-bound, and how many
cores the collector's fixed share is taken from. This workload was on the wrong
side of five of six. Check a technique's stated precondition before concluding
its behaviour is anomalous.

Written up in `concepts/23-concurrency-readers-and-writers.md`.

**Sources for Part 1**

[B1] benchstat — https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
[B2] AppSignal — "CPU Steal Time: A Crucial Metric for Cloud Servers and VMs" — https://blog.appsignal.com/2021/09/15/cpu-steal-time-a-crucial-metric-for-cloud-servers-and-vms.html
[B3] Oracle — `CopyOnWriteArrayList` — https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/util/concurrent/CopyOnWriteArrayList.html
[B4] The Go Authors — "A Guide to the Go Garbage Collector" — https://go.dev/doc/gc-guide
[B5] Vavouliotis et al. — "Concurrent GCs and Modern Java Workloads: A Cache Perspective", ISMM 2023 — https://gvavou5.github.io/Documents/Vavouliotis_ISMM23.pdf

---

## Part 2 — Bugs, in the order they were found

### 2.1 Non-deterministic WAL encoding

**Wrong.** Metadata was encoded by iterating a Go map. Map iteration order is
randomised, so two replicas encoding the same logical record produced different
bytes.

**Caught by.** `TestGroupCommitEquivalence`, which compares the byte stream from
per-record appends against the batched path.

**Rule.** Any serialisation that feeds replication, checksums or a byte-identity
test must have a **total order over its fields**. Sort the keys. In a real
system this is a replication divergence bug, not a test failure.

### 2.2 A closed shard kept serving reads

**Wrong.** `Search` and `Get` had no `closed` check. An "unavailable" replica
returned stale results, so the coordinator's partial-result path never fired.

**Caught by.** The demo. Not the tests — the tests asserted what I had thought to
assert, and I had not thought of this.

**Rule.** An unavailable component must actually **fail**, loudly, at its
boundary. A component that degrades quietly makes every downstream failure-handling
path untestable, because the failure never arrives. And: run the system by hand.
Several of the sharpest bugs here surfaced in a demo against a fully green suite.

### 2.3 Truncation freed nothing

**Wrong.** The demo used the default 64 MiB segment size, so the whole log lived
in one open segment and truncation could not reclaim anything.

**Caught by.** Watching disk usage in the demo not move.

**Rule.** Reclamation granularity is the **segment**, not the record. A segment
size larger than the dataset silently disables the entire mechanism. This is the
same root cause as lesson 1.1's second consequence — the two are one bug wearing
different clothes, and I met it twice before naming it.

### 2.4 An ejected follower could never rejoin

**Wrong.** The catch-up path only contacted members of the in-sync set. A
follower ejected for falling behind was therefore never contacted again, so it
could never catch up, so it stayed ejected. Permanently.

**Caught by.** A chaos test, after it was strengthened (see 2.9).

**Rule.** Every state a node can enter needs a stated path **out**. Write the
state machine down and check each state has an exit; "ejected" is not a terminal
state unless you meant it to be.

### 2.5 A caught-up follower stayed stale on the commit point

**Wrong.** When a follower had all records, the leader returned early — and so
never sent the commit position. The follower held committed records it believed
uncommitted, and omitted them from reads.

**Caught by.** The chaos test, after 2.4 was fixed and it could get this far.

**Rule.** The **fast path must carry the same metadata as the slow path**. Fixed
by sending an empty `AppendRequest` carrying `CommitLSN` — the heartbeat is not
an optimisation, it is how commit position propagates.

### 2.6 The transport could not model a partitioned sender

**Wrong.** `Append(ctx, to, req)` only knew the destination, so the fault
injector could block inbound messages and nothing else. An isolated *sender* —
the single most important partition scenario, since it is what a deposed leader
looks like — was unrepresentable.

**Caught by.** Trying to write the test and finding the API could not express it.

**Rule.** When a fault you need cannot be injected, that is a **defect in the
test harness**, not a reason to skip the test. Signature changed to
`Append(ctx, from, to, req)` with `route(from, to)` checking both directions.

### 2.7 The controller re-granted a lease at promotion

**Wrong.** `Tick` deleted expired leases and then unconditionally renewed. A node
being promoted got a lease for an epoch it did not hold.

**Caught by.** A controller test asserting lease-holder identity.

**Rule.** Renewal is not the same operation as **grant**. `renewLeasesLocked` now
requires `r.IsLeader && r.Epoch == p.Epoch` — a lease renews only for the node
that already legitimately holds it at the current epoch.

### 2.8 Pre-vote deadlock: no leader, no error

**Wrong.** All nodes stuck as pre-candidates at term 1. No leader was ever
elected and nothing failed — the cluster simply did nothing.

**Why.** Campaigning reset the election timer but left `n.leader` pointing at the
stale leader. The CheckQuorum pre-vote guard refuses a vote when the voter still
believes it has a live leader, so every node refused every other node, forever.

**Caught by.** Instrumenting a simulation that was hanging, and reading the state
of all five nodes at the same tick.

**Rule.** Two rules, and the second is the more important. First: when entering a
new state, **clear the state the old one implied** — a stale cached value is a
bug with no error message. Second: a liveness failure produces *no signal at
all*, so simulations need a **progress assertion** ("a leader exists within N
ticks"), not only safety assertions. Safety checks pass beautifully on a system
that has stopped.

### 2.9 The chaos test passed vacuously

**Wrong.** The replication chaos test passed with **6 of 400 writes
acknowledged**. Every safety property held, because almost nothing happened.

**Caught by.** Reading the test's own output instead of its exit status.

**Fixed by.** Adding **liveness floors** — heal the fault unconditionally rather
than only on the success path, require ≥100 acknowledged writes, and require ≥1
refusal so the refusal path is known to have fired.

Strengthening it immediately exposed bugs 2.4 and 2.5.

**Rule.** This is the highest-yield debugging move in the whole project. Every
fault-injection test needs floors alongside its safety assertions: a minimum
number of successful operations, and proof that the interesting path actually
ran. A green chaos test with no floors is an unread test.

### 2.10 The rebalance snapshot was taken at the wrong moment

**Wrong.** The move fixed its snapshot LSN when the transfer *finished*, using
the source's current position — so the destination arrived perfectly caught up,
the gap was zero, and the catch-up phase was skipped entirely. The
slow-destination test passed while proving nothing.

**Caught by.** That test passing for the wrong reason: it asserted the move must
*not* complete, and the move completed.

**Rule.** In a simulation, **the model is part of the system under test**. A
simplification that removes the difficulty produces tests that are green and
worthless. The snapshot LSN is now fixed when the phase *starts*, which is also
what real snapshot transfer does and why catch-up exists at all.

### 2.11 An abort was classified as a completed cutover

**Wrong.** `PhaseAborted` is numerically the largest constant in the `Phase`
enum, so `phase >= PhaseCutover` was true for an aborted move. Every ordering
check — "has authority moved", "may we clean up", "who is the owner" — silently
treated an abort as a success.

**Caught by.** The invariant checker firing in four unrelated tests at once,
which is what pointed at a shared cause rather than four separate bugs.

**Rule.** **Do not encode semantics in enum ordering** unless the order is the
semantics. Terminal and exceptional states do not belong on the same axis as
progress. Replaced with explicit predicates: `AuthorityMoved()` and `Live()`.

### 2.12 Re-applying after proposing a follow-up command looped forever

**Wrong.** When installing a promotion failed, the controller proposed a
read-only command and then immediately re-applied. Under asynchronous commit the
new command has not landed, so the re-apply saw the same un-applied epoch bump,
failed to install again, and proposed again — on every pass, forever.

**Caught by.** A test hanging. No error, no output, no crash.

**Rule.** Under asynchronous commit, **do not act on state you have just
proposed**. The decision lands on a later tick, and that is what asynchronous
means. Also: guard the proposal itself so one is emitted per epoch rather than
per tick, or a slow commit collects a proposal storm.

---

## Part 3 — What the tests taught, ranked by yield

Ordered by bugs found per hour spent, which is not the order these techniques
usually appear in a testing chapter.

| Technique | Found | Worth it? |
|---|---|---|
| **Strengthening a vacuous test** | 2.4, 2.5, and exposed 2.9 itself | Highest yield in the project, by a wide margin |
| **A test that simply hangs** | 2.12 | A liveness failure emits nothing — only a timeout finds it |
| **Running the demo by hand** | 2.2, 2.3 | Finds what tests do not, because tests assert what you thought of |
| **Deterministic simulation with per-step invariants** | 2.8, 2.10, 2.11 | Turns "flaky distributed bug" into "seed 137, step 412" |
| **Asserting the broken behaviour on purpose** | none directly | Prevents the *future* bug: without it, the pause phase gets optimised away |
| **Truncate-at-every-offset** | none this round | Cheap, exhaustive, and the reason the WAL never produced a recovery bug |
| **Benchmarks** | 1.1, 1.3, 1.4 | Contradicted the design three times — and produced one false finding (1.2) |

Two observations from that table.

**The techniques that found the most were the cheapest.** Strengthening an
existing test and running the binary cost minutes. Neither is a testing
*framework*; both are a habit of not trusting a green result.

**One entry is a false finding, and it stays in the file.** 1.2 was recorded as a
contradiction and turned out to be single-run noise on a shared host. Deleting it
would make the file look better and teach less: the failure mode it demonstrates
— believing a number because you wrote it down yourself — is the one that
survives knowing the rule.

**Truncate-at-every-offset found nothing, and that is its result.** It was
written first, it is exhaustive over the failure it covers, and the WAL then
produced no recovery bugs across the whole project while five other components
produced eleven between them. An exhaustive test on a narrow surface is how a
component stops appearing in this file.

---

## Part 4 — The rules, collected

1. Never build a mechanism before you can state the failure it prevents.
2. A fault-injection test without liveness floors is unread.
3. An unavailable component must fail loudly at its boundary, not degrade quietly.
4. Every state a node can enter needs a stated path out.
5. The fast path must carry the same metadata as the slow path.
6. When entering a new state, clear the state the old one implied.
7. Liveness failures emit no signal — assert progress explicitly.
8. Do not encode semantics in enum ordering.
9. Any serialisation feeding replication or byte-identity must have a total field order.
10. Reclamation granularity is the segment; a segment larger than the dataset disables the mechanism.
11. A fault you cannot inject is a harness defect, not a test to skip.
12. In a simulation, the model is part of the system under test.
13. Renewal is not grant.
14. A benchmark without its machine and its run is not a measurement.
15. When a measurement contradicts the design, the measurement is the finding —
    *after* you have checked it reproduces and that you have not violated the
    technique's stated precondition.
16. Under asynchronous commit, act on APPLY, never on decide, and never act on
    state you have just proposed.
17. A technique that "should" help and does not is usually being used outside
    its documented contract. Read the contract before calling the result an
    anomaly.
