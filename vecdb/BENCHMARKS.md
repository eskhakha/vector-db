# Measured numbers

Every number here came from a run on the machine described below. Nothing is
estimated, extrapolated, or copied from a paper. Where a measurement contradicts
the intuition the code was written with, the contradiction is recorded rather
than smoothed over — that is the most useful thing in this file.

## Machine and method

```
CPU     Intel Xeon @ 2.80GHz, 2 vCPU
RAM     7 GiB
Kernel  Linux 6.18 x86_64
Go      go1.24.7
Disk    container overlay filesystem (NOT a real disk — see the caveat below)
Method  go test -bench, -benchtime=200x, ONE run per configuration
```

Reproduce with `make bench` (excludes the contention suite) and `make contention`.

**Two caveats that bound every number below.**

*The storage is not a disk.* The fsync figures come from a container filesystem,
not a physical device with a write cache. Treat the **ratio** between fsync
settings as meaningful and the absolute microseconds as fiction. On real NVMe
expect fsync in the tens of microseconds; on a spinning disk, milliseconds. The
shape of the trade-off does not change; the constant does.

*One run is not a sample.* Go's own tooling says a benchmark should be run "at
least 10 times to gather a statistically significant sample of results" [1].
These are single runs. §5 shows what that costs, using this file's own history
as the evidence.

---

## 1. Durability: what fsync costs

| Benchmark | ns/op | ops/sec |
|---|---:|---:|
| `AppendSyncAlways` (fsync per record) | 215,018 | ~4,650 |
| `AppendGroupCommit64` (one fsync per 64) | 10,376 | ~96,400 |

**20.7× throughput for one fsync amortised across 64 records.**

Group commit does not weaken the guarantee for a *batch* — every record in it is
durable before any of them is acknowledged. What it costs is latency for the
first writer, who now waits for the batch to fill or the timer to fire. A system
that acknowledges before the fsync is a different, weaker system; that is a
decision to make openly, not a performance tweak.

**On the magnitude, read §5 before quoting it.** This ratio has measured 25×,
10× and 20.7× across three runs of unchanged code.

## 2. Replication: what a second copy costs

| Followers | ns/op | vs. standalone |
|---:|---:|---:|
| 0 | 5,169 | 1× |
| 1 | 107,264 | 20.8× |
| 2 | 127,954 | 24.8× |

The step from 0 to 1 follower is the expensive one: the write now has to leave
the process and come back before it can be acknowledged. The step from 1 to 2 is
much cheaper (+19%), because the leader fans out concurrently and waits for the
slower of the two, not for their sum.

That asymmetry is the argument for synchronous replication to a small in-sync
set rather than to everybody: cost is set by the **slowest replica you wait
for**, so widening the set costs tail latency, not linear throughput. It is also
why an in-sync set that silently shrinks to one is so dangerous — latency
*improves*, which is exactly what makes the data loss invisible.

The +19% figure is unstable across runs (+7%, +49%, +19%). The *shape* — second
copy much cheaper than the first — has held every time. Quote the shape.

## 3. Search: brute force is memory-bandwidth-bound

| N | ns/op | throughput |
|---:|---:|---:|
| 1,000 | 23,103 | 2,770 MB/s |
| 10,000 | 190,011 | 3,368 MB/s |
| 100,000 | 1,897,582 | 3,373 MB/s |

From 10K to 100K, latency scales exactly linearly — 10× the vectors, 9.99× the
time — and **throughput is flat at ~3,370 MB/s.** That flatness is the finding:
the scan is limited by how fast memory can be streamed, not by arithmetic. The
kernel is already saturating the memory system, so micro-optimising the distance
computation would buy nothing.

This is the honest, measured case for an approximate index. Brute force is not
slow because the code is bad; it is slow because it must touch every byte. The
only way past it is to touch fewer bytes — which is what IVF (skip most lists),
quantization (smaller vectors) and HNSW (visit hundreds of candidates instead of
`N`) each do differently.

At 100K × 64 dims a query is 1.9 ms. At 1M it would be ~19 ms per shard, which is
where a 4-shard fan-out starts paying: four shards scan 250K in parallel, so the
query is bounded by the slowest shard, not the total.

*Faster than the previous run (2.33 ms → 1.90 ms at N=100K), part of which is
the lock-free read path in §7 removing per-search lock traffic and part of which
is a faster CPU. The two are not separated here, so do not attribute it.*

## 4. Recovery: when a snapshot actually helps

This benchmark is the reason this file exists, and it is the one result that has
**reproduced across runs and across machines**.

| Workload | Recovery | ns/op | verdict |
|---|---|---:|---|
| churn=1 (insert-only) | replay log | 57,371,767 | — |
| churn=1 (insert-only) | snapshot + replay | 64,945,711 | **13.2% SLOWER** |
| churn=10 (each key rewritten 10×) | replay log | 42,896,838 | — |
| churn=10 (each key rewritten 10×) | snapshot + replay | 8,984,914 | **4.8× faster** |

The previous run on a different CPU gave 13% slower and 4.8× faster. Identical
conclusions from independent runs — which is what distinguishes this from §5.

The first attempt at this benchmark was insert-only, and snapshot recovery came
out slower. The instinct was to call it noise. It is not. With one write per key
the snapshot contains **exactly as many records as the log**: it saves no work
and costs an extra file read. A snapshot's benefit is proportional to the log's
write amplification — records held per *live* key — and insert-only has an
amplification of 1.

So the accurate claim is not "snapshots make recovery faster". It is:

> A snapshot compacts away superseded versions. It speeds recovery in proportion
> to how many superseded versions exist, and it is a net loss on a workload that
> has none.

Two operational consequences follow, neither obvious from the first claim:
snapshotting an append-only shard on a timer is wasted I/O, and a snapshot only
shortens recovery if the WAL can then be **truncated** — which needs segments
small enough that whole ones can be dropped. One giant segment means replay
covers everything regardless, and you pay for the snapshot without collecting.

## 5. What single runs cost: this file's own history

Same code, three runs, two CPUs:

| Measurement | run 1 | run 2 | run 3 |
|---|---:|---:|---:|
| fsync vs. group commit | 25× | 10× | **20.7×** |
| second follower, cost over the first | +7% | +49% | **+19%** |
| snapshot on insert-only | — | 13% slower | **13.2% slower** |
| snapshot at churn=10 | — | 4.8× faster | **4.8× faster** |

The top two rows move by more than most optimisations would. The bottom two
barely move at all. That difference is the whole lesson: a result is worth
quoting when it **reproduces**, and reproduction is the only way to tell a
finding from noise.

Two environmental effects drive the instability, neither visible from inside the
benchmark:

- **CPU steal.** On a shared virtualised host, steal is "the percentage of time a
  virtual CPU waits for a real CPU while the hypervisor is servicing another
  virtual processor", and sustained values above roughly 10% materially degrade
  performance [2]. It varies with what neighbouring tenants do.
- **The host is not even constant.** Run 3 landed on a 2.80 GHz CPU; earlier runs
  on a 2.10 GHz one. Nothing in the benchmark reports that, and nothing stopped
  the earlier numbers being written into a document as if it were.

## 6. Consensus: what a majority round trip costs

| Benchmark | ns/op |
|---|---:|
| `ConsensusRoundTrip` (propose → committed, 3 nodes) | 16,247 |

Simulated and in-process, so it measures the protocol's own bookkeeping, not real
latency. It is still the right order of magnitude for the decision this project
made: a control-plane command costs ~16 µs of protocol work, while a replicated
data write costs ~107 µs of real waiting. Routing every vector write through
consensus would add a majority round trip to a path that a synchronous in-sync
set and an epoch fence already make safe.

That is why Raft here governs the shard map and nothing else — the same split
Qdrant and Weaviate arrived at independently.

## 7. Concurrency: the lock, and what replacing it did

`BenchmarkContention`, N=100,000, four writer goroutines against a stream of
searches, 50 iterations (`make contention`):

| | writes completed | search p99 |
|---|---:|---:|
| baseline, no writers | — | 3,571 µs |
| RWMutex, 4 writers, fsync | 53 | 3,769 µs |
| RWMutex, 4 writers, **no fsync** | **53** | 3,252 µs |
| copy-on-write, 4 writers, fsync | 131 | 7,680 µs |
| copy-on-write, 4 writers, no fsync | 187 | 8,625 µs |

**Under the lock, writes track searches one-for-one** — the signature of a
convoy. The diagnostic row is the third: removing the fsync entirely changes
write throughput by **zero**, which means the write path's own cost had become
irrelevant and every bit of it was spent waiting for a 2.4 ms scan. No
optimisation of the write path could have bought anything.

Replacing the read lock with an immutable published snapshot improved writes
**2.5–3.5×**, and made fsync matter again (131 vs 187) — the sign the lock is no
longer the sole bottleneck. But **search p99 roughly doubled**.

That is not an anomaly. Copy-on-write is for workloads where "traversal
operations vastly outnumber mutations" [3], and this benchmark runs four writers
flat out. Three documented mechanisms then compound: `GC frequency = allocation
rate / new heap memory` and "doubling allocation rate doubles GC CPU cost" [4],
so copying ~800 KB per write raises collector frequency proportionally; "the GC
takes 25% of CPU resources when in the mark phase" [4], which on two cores is
half the machine; and concurrent marking streams memory and evicts cache, which
on an LLC-sensitive workload cost "3 × increase in iteration duration" and "a
1700-fold increase in LLC misses" against "at most 10 %" on ordinary workloads
[5] — and a brute-force scan is the sensitive case, per §3.

Copy-on-write did not remove the cost; it **moved** it, from lock-wait to
allocation. The fix is granularity — publish per batch or per commit rather than
per write — and it is not done. `concepts/23-concurrency-readers-and-writers.md`
has the analysis.

## 8. Failover and rebalancing

Simulated on a virtual clock, so reported as *behaviour*, not timings:

- **Failover window** equals the failure-detector threshold, because that is what
  it is: detection is a timeout gamble and the window is the gamble's size.
  Shortening it trades availability for false promotions.
- **Rebalancing**, over 300 seeded runs with injected destination failures: 247
  moves completed, 53 aborted, 2,040 writes acknowledged, **zero acknowledged
  writes lost** in either outcome.
- The same simulation with the pause phase disabled loses acknowledged writes on
  every run. That test exists to stop the pause being optimised away.

## What is not measured

Stated plainly, because an unmeasured claim is worse than an absent one:

- **Nothing here is a statistically valid sample.** One run per configuration,
  against a documented minimum of ten [1]. §5 is the evidence for what that
  costs, and it applies to every table above.
- **No 1M-vector run.** The 100K figure is real; 1M is extrapolated from linear
  scaling and labelled as such.
- **No HNSW comparison.** Not implemented, so there is no brute-force vs. ANN
  number here.
- **No real network.** Everything is in-process.
- **No multi-machine run** — no NUMA effects, no real disk contention, no
  cross-AZ latency.
- **p50/p95/p99 only where explicitly reported.** `go test -bench` gives means;
  §7 computes percentiles itself. Reporting a mean while implying a tail is
  exactly the error this file exists to avoid.

## Sources

[1] benchstat — https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
[2] AppSignal — "CPU Steal Time: A Crucial Metric for Cloud Servers and VMs" — https://blog.appsignal.com/2021/09/15/cpu-steal-time-a-crucial-metric-for-cloud-servers-and-vms.html
[3] Oracle — `CopyOnWriteArrayList` — https://docs.oracle.com/en/java/javase/21/docs/api/java.base/java/util/concurrent/CopyOnWriteArrayList.html
[4] The Go Authors — "A Guide to the Go Garbage Collector" — https://go.dev/doc/gc-guide
[5] Vavouliotis et al. — "Concurrent GCs and Modern Java Workloads: A Cache Perspective", ISMM 2023 — https://gvavou5.github.io/Documents/Vavouliotis_ISMM23.pdf
