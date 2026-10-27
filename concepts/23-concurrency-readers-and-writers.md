# 23. Concurrency: readers and writers

> A search holds its data for milliseconds and a write holds it for microseconds, so whichever mechanism arbitrates between them decides which one starves — and the usual answer, copy-on-write, has a precondition that a vector index routinely violates.

## The problem

One goroutine is scanning the index. Another is inserting into it. They touch the same bytes.

Doing nothing is not an option, and not for a subtle reason: in most languages a concurrent read and write of shared memory is undefined behaviour, and in Go it is detected and fatal. A slice header is three words — pointer, length, capacity — written non-atomically, so a reader can observe a new pointer with an old length and index past the end of an allocation. A concurrent map read and write aborts the process by design.

So there are exactly two families of answer:

1. **Arbitrate the sharing.** A lock. Somebody waits.
2. **Remove the sharing.** Make what readers touch immutable. Nobody waits.

The interesting part is that for a vector index the obvious choice in each family is wrong for a reason that is specific to this workload, and both are measurable in an afternoon.

## How it works

### The asymmetry that decides everything

Write down how long each side holds the data:

| Operation | Critical section | Duration (N=100k, D=64) |
|---|---|---|
| Search | the entire scan | ~2,400 µs |
| Write | fsync, map insert, index append | ~130 µs |

The read holds it roughly **20× longer**. That single ratio predicts everything downstream, and it inverts the analysis people usually bring to reader-writer locks, which assumes short reads and is worried about writers being starved.

### Why a reader-writer lock produces a convoy

Go's `sync.RWMutex` — like most implementations — stops admitting **new readers** once a writer is waiting. That rule exists to prevent writer starvation, and with a long read it produces something worse than starvation on either side: strict alternation.

```
  read (2.4 ms) ── writer queues ── new readers block ──┐
                                                        ▼
                                            write (130 µs)
                                                        │
                        ◄───────────────────────────────┘
                            next read, and repeat
```

Measured directly, with four writer goroutines running flat out against a stream of searches:

| | writes completed | search p99 |
|---|---:|---:|
| baseline, no writers | — | 3,571 µs |
| 4 writers, fsync | 53 | 3,769 µs |
| 4 writers, **fsync removed entirely** | **53** | 3,252 µs |

Writes track the number of searches almost exactly — one write per search, across runs and across `N`. That is the signature of a convoy.

The load-bearing row is the third. **Removing the fsync changes write throughput not at all.** By this point the write path's own cost has become irrelevant; every bit of it is spent waiting for a scan. Any optimisation of the write path — group commit, a faster index insert, a cheaper encoder — would buy exactly nothing until the lock is addressed.

### Copy-on-write, and its precondition

The alternative is to publish immutable versions. Readers take a pointer and are done:

```go
snap := s.view.Load()   // atomic.Pointer — one acquire load, then nothing held
search(snap)            // snap can never change

s.wmu.Lock()            // writers serialise against EACH OTHER only
next := build(s.view.Load())
s.view.Store(next)      // one release store: the whole publication
s.wmu.Unlock()
```

Three details carry the correctness. Publication must be **one pointer store**, because a pointer is written atomically and a struct or slice header is not. It must be a **release store paired with an acquire load**, or a reader can see the pointer before the writes that built the object — real on weak memory models, and silent. And something must decide when the old version is reclaimable: C implementations need hazard pointers or RCU grace periods; a tracing garbage collector does it for free, which is a genuine simplification rather than a detail.

What COW does **not** do is serialise writers. Two writers each doing copy-modify-publish will lose one update. The writer lock stays.

**And it has a precondition, stated bluntly in the place most engineers first meet it.** Java's `CopyOnWriteArrayList` documentation says:

> "This is ordinarily too costly, but may be *more* efficient than alternatives when traversal operations vastly outnumber mutations, and is useful when you cannot or don't want to synchronize traversals" [1]

*Vastly outnumber.* COW is a read-mostly technique. Applying it to a write-heavy workload is not a tuning mistake; it is using the tool against its stated contract.

### Granularity is the whole design

Naive COW copies everything per write — `O(N)`, unusable at scale. Three ways out:

| Approach | Cost per write | Used by |
|---|---|---|
| Batch: accumulate, publish periodically | `O(N)` amortised over the batch | Elasticsearch's `refresh_interval` |
| Structural sharing: copy the path to the changed node | `O(log N)` | persistent data structures, HAMTs |
| Immutable base + small mutable head | `O(head)` | Lucene, LSM engines |

For a vector index the third is right, and structural sharing is a trap: the scan is memory-bandwidth-bound (see [Brute force and kernels](03-brute-force-and-kernels.md)), and a tree of nodes destroys the contiguity that makes streaming fast. You would fix contention and lose more to cache misses.

### Why naive COW made reads *worse* here

Implemented at the naive granularity — republish the whole candidate slice on every write — the result was:

| | writes | search p99 |
|---|---:|---:|
| RWMutex, fsync | 53 | 3,769 µs |
| **COW, fsync** | **131** | **7,680 µs** |
| RWMutex, no fsync | 53 | 3,252 µs |
| **COW, no fsync** | **187** | **8,625 µs** |

Writes improved 2.5–3.5×, and fsync started mattering again (131 vs 187), which is the signal that the lock is no longer the sole bottleneck. But **search p99 roughly doubled.**

Three mechanisms, each individually documented, compound:

**The allocation rate drives collector frequency.** Go's GC guide gives the relationship directly: `GC frequency = (Allocation rate) / (New heap memory)`, and "the allocation rate of a Go program is a major factor in GC frequency"; in steady state "doubling allocation rate doubles GC CPU cost" [2]. Publishing ~800 KB per write raises the allocation rate by orders of magnitude.

**The collector takes a fixed share of the machine.** "the GC takes 25% of CPU resources when in the mark phase" [2]. On a 2-core machine that is half a core gone. Goroutines that out-pace the collector are additionally conscripted into `runtime.gcAssistAlloc` to do collection work themselves [2].

**Marking competes for the resource the reader is already saturating.** Concurrent GC streams memory and evicts cache lines. Measured on an LLC-sensitive microbenchmark, concurrent marking caused "3 × increase in iteration duration" and "a 1700-fold increase in LLC misses"; on ordinary application workloads the same collector caused "at most 10 % slow-down", because typical workloads "lack sensitivity to the LLC size" [3].

A brute-force vector scan is the sensitive case, not the ordinary one. That is why this result is specific to vector search and would not reproduce on a key-value store: the reader and the collector are contending for memory bandwidth, and the reader had all of it before.

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Publish interval (writes per publish) | copy cost vs. read staleness | batch; not per write | per write: allocation rate tracks `N` and the collector eats the machine |
| Publish trigger | what readers can see | on commit-point advance | on apply: readers observe uncommitted state, and a failover makes a read disappear |
| Copy granularity | write amplification | immutable base + small head | whole-index copy: `O(N)` per write |
| Writer lock | writer/writer serialisation | required, always | omitted: lost updates, silently |
| Publication type | atomicity of the swap | `atomic.Pointer` | a plain field or a slice: torn publication |
| `GOGC` / memory limit | collector frequency | raise it if copy churn is unavoidable | default, with high churn: GC dominates |
| Reader lifetime | live heap | short scans | long scans pin old versions and multiply memory |

## How real systems do it

**Linux RCU** is the most aggressive point on the spectrum: in the classic configuration the read side executes no atomic operations at all — reads are genuinely free — and writers pay by waiting for a grace period before reclaiming. The cost is that it is famously easy to misuse (a missing `rcu_dereference` on a weak memory model, sleeping inside a read-side critical section), which is why the kernel ships static analysis specifically to catch it.

**Lucene and Elasticsearch** apply COW at *segment* granularity, and make the publication interval user-facing: `refresh_interval`, defaulting to one second. That is why Elasticsearch is documented as *near*-real-time rather than real-time. It is the clearest example anywhere of COW staleness being an explicit product decision rather than a hidden cost, and it is the batching row of the table above, shipped.

**LMDB** is a copy-on-write B+tree with a single writer: readers never block and never see a torn page, and versioning falls out of page immutability.

**ZFS and Btrfs** apply it to blocks — never overwrite in place, write elsewhere, swap the pointer — which is how they get atomic snapshots. It is also where the honest downside is documented: COW fragments random-write workloads, which is why Btrfs ships `nodatacow` and why it is routinely recommended for database files and VM images. The mechanism that buys atomic snapshots costs sequential layout.

**Java's `CopyOnWriteArrayList`** is the cautionary one. It copies the entire backing array on every mutation, it is correct, it is in the standard library, and the documentation warns you it is "ordinarily too costly" [1] — which does not stop it being a recurring performance trap, because the API does not enforce its own precondition.

The convergence is worth naming: a kernel, two filesystems, a search engine and a language runtime independently landed on "publish a new version by swapping one pointer". What they *disagree* about is granularity, and that disagreement is the entire design space.

## Complexity and cost

| | Reader-writer lock | Copy-on-write |
|---|---|---|
| Read | blocks behind a waiting writer | `O(1)` atomic load, never blocks |
| Write | blocks for the whole scan | `O(copy)` + `O(1)` publish; never waits on a reader |
| Writer/writer | serialised by the lock | serialised by a separate writer lock |
| Memory | one copy | ≥ 2 versions live; more if readers are slow |
| Deadlock | possible with lock ordering | impossible on the read side — nothing is acquired |
| Scaling with cores | contention grows | reads scale linearly |

## Failure modes

- **The convoy.** Long reads and any writes produce strict alternation. The tell is that optimising the write path changes nothing.
- **Allocation-driven collector pressure.** COW at the wrong granularity converts lock-wait into GC CPU, and on a small machine that is a worse trade.
- **Memory blow-up from a slow reader** pinning old versions. A *leaked* reader leaks an entire version. This is COW's characteristic outage.
- **Lost updates** if you assume COW serialises writers. It does not.
- **Torn publication** from publishing a struct or slice rather than a pointer.
- **Memory-ordering bugs** when publication is not a release store — invisible on x86, real on ARM.
- **Read-your-writes regression.** A client writes, gets an acknowledgement, reads, and does not see its own write because publication is lazy. See [Consistency models](16-consistency-models.md); the fix is a client version token, not a stronger lock.
- **Reads observing uncommitted state**, if you publish on apply rather than on commit. That breaks the invariant that makes failover safe.

## How to test it

- **The race detector, under concurrent load.** Every bug in this chapter is a data race, and this is the only tool that finds them reliably. A COW implementation that has not been run under it is unverified.
- **A contention benchmark, not a throughput benchmark.** Measure search p99 *and* completed writes together, with and without background writers. The diagnostic that identified the convoy was noticing that removing the fsync did not change the write count — a single-number throughput benchmark cannot express that.
- **Snapshot immutability.** Take a snapshot, perform many writes, then assert the snapshot's contents are exactly what they were. This catches in-place mutation of published data, which is the error that turns COW back into a race.
- **A reclamation test.** Assert old versions become collectable; a retained reference is a leak with no error.
- **Vary the core count.** A 25% collector share is invisible on 16 cores and dominant on 2. A result from one core count does not transfer.

## Minimal demo version

Build **both**, and measure them against each other. Neither number means anything alone.

The lock version is what you already have. The COW version is about thirty lines: an `atomic.Pointer` to an immutable view, a mutex that serialises writers only, publish on commit-point advance.

**What you can cut:** structural sharing, epoch-based reclamation (a GC covers it), multiple writers, lock-free writers, and any tuning of `GOGC`.

**What you cannot cut without the demo becoming a lie:**

- **The contention benchmark.** Without it you are choosing a concurrency strategy by reasoning, and the reasoning here points the wrong way — the standard intuition is that writers starve, and measurement shows readers starve writers.
- **The atomic pointer and the release/acquire pairing.** A plain field works on x86 and is broken on ARM. "It passed on my laptop" is not evidence.
- **The writer lock**, even though COW removed the reader/writer contention.
- **Publishing on commit rather than on apply**, or the consistency model quietly weakens while the benchmark looks better.

## Open questions and contested claims

**Is there a principled publish interval?** No source located gives a derivation. Elasticsearch's one second is convention. In practice the interval is chosen by feel and tuned after someone complains about staleness or about GC.

**Does COW help a vector index at all?** Our own measurement says the naive granularity does not: writes improved 2.5–3.5×, reads regressed about 2×, on a 2-core machine with a memory-bandwidth-bound scan. Whether the batched or segment-granularity version wins is untested here, and the honest position is that it is expected to and has not been demonstrated. Note that the measurement violates COW's own stated precondition [1] — a genuinely read-mostly workload would be a different experiment, and would likely give the textbook answer.

**How much does the runtime decide the outcome?** A C or Rust implementation pays for reclamation with hazard pointers or epochs but does not pay a tracing collector's CPU share [2]. It is plausible that the same design wins in one language and loses in another purely on that difference. No controlled cross-language comparison was located.

## Sources

[1] Oracle — `CopyOnWriteArrayList`, Java SE API documentation — https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/util/concurrent/CopyOnWriteArrayList.html
[2] The Go Authors — "A Guide to the Go Garbage Collector" — https://go.dev/doc/gc-guide
[3] Vavouliotis et al. — "Concurrent GCs and Modern Java Workloads: A Cache Perspective" — ISMM 2023 — https://gvavou5.github.io/Documents/Vavouliotis_ISMM23.pdf
