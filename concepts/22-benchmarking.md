# 22. Benchmarking

> Producing numbers that mean something — and the documented methodology bugs that made a widely used ANN benchmark under-report throughput by 20×.

## The problem

Every claim about a vector database is a performance claim. "HNSW is faster than brute force", "quantization costs recall", "this index scales to a billion vectors" — none of them are statements you can evaluate without a measurement, and all of them are routinely made without one.

Benchmarking exists to convert those into facts. It fails in three distinct ways, and confusing them is why the field is full of unreproducible numbers:

1. **Measuring the wrong thing.** Reporting single-query latency and calling it throughput.
2. **Measuring the right thing badly.** No warm-up, timing that includes process startup, thread counts nobody controlled.
3. **Measuring honestly but incomparably.** Different hardware, different recall targets, different datasets — a perfectly executed benchmark that answers no question anyone asked.

There is also a fourth problem that is not technical: some vendors contractually forbid you from publishing what you measured.

## How it works

### The governing trade-off: recall versus throughput

An approximate index has no single performance number. Every algorithm has knobs that trade recall for speed, so "QPS" without "at what recall" is meaningless.

**ANN-Benchmarks** established the correct framing: measure the **recall-versus-QPS trade-off**, sweeping each algorithm's tunable parameters and retaining only the points on the resulting Pareto frontier [1]. A single point is not a result; the frontier is the result.

It ships 14 pre-generated HDF5 datasets — SIFT-1M (128-d), GIST, GloVe variants, a DEEP1B subset (9.99M, 96-d), Fashion-MNIST, MNIST — with Euclidean, angular and Jaccard distances and **100 precomputed ground-truth nearest neighbours per query** [1].

### Ground truth

Recall requires knowing the true answer. For these datasets, ground truth means the *exact*, brute-force-computed k-nearest-neighbour set for each query under the chosen metric, computed once offline and shipped alongside the vectors so any algorithm's output can be scored against a known-correct answer [1].

This is why you build the exact index first. It is your oracle, and without it recall is unmeasurable.

SIFT1M (1M × 128-d SIFT descriptors) and GIST1M (1M × 960-d) are the classic INRIA/IRISA TEXMEX sets, distributed as **fvecs** (float), **ivecs** (integer, used for ground-truth ID lists) and **bvecs** (uint8, used for billion-scale SIFT1B). The exact byte layout of those formats could not be verified from a primary source in the research behind this file — the canonical corpus page returned a redirect loop — so treat the format description as widely-reported rather than confirmed [2].

### Metrics

Standard reported metrics are recall@k, QPS, and latency **percentiles** p50/p95/p99/p99.9, with index build time and index size as the cost-side metrics [3].

**Mean latency is close to useless for a database workload.** Query latency distributions are heavy-tailed — cache misses, GC pauses, background compaction, and query-dependent graph-traversal length all produce a small fraction of slow queries that dominate user-perceived experience while barely moving the mean. This is the standard justification for reporting percentiles, and it connects directly to the aggregation arithmetic in [Observability](21-observability.md): a percentile averaged across hosts is not a percentile.

### What ANN-Benchmarks deliberately does not measure

The project states its own limitations plainly, and they matter more than the results [1]:

- "only one CPU is saturated during experimentation, i.e., no multi-threading" — single-threaded
- focused on "datasets that fit in RAM" — in-memory only
- static train/test splits — **no update or insert workload**
- "mainly support[s] CPU-based ANN algorithms"
- intended for the 100–1,000 dimensional range

It also has no filtered-search or metadata-predicate support. The README now flags the project as "no longer actively maintained", pointing to a successor benchmark [1].

Each of those exclusions is a production workload. An index that is excellent under a static, single-threaded, in-memory, unfiltered benchmark may be poor under concurrent inserts with filters, and the benchmark will not tell you.

### What the NeurIPS Big ANN tracks added, and why

The 2023 edition "departed from previous competitions" by adding four tracks addressing "the evolving complexity of real-world search workloads beyond classical nearest neighbor problems" [4]:

| Track | What it tests | Why it was needed |
|---|---|---|
| Filtered Search | ANN jointly with metadata constraints | the 2021 format was filter-free |
| Out-of-Distribution | queries from a different distribution than the index | real queries are not sampled from the corpus |
| Sparse Vectors | ANN over learned-sparse representations | the format assumed dense embeddings |
| Streaming ANNS | incremental insert and update | the format was static build-then-query |

That list is a direct enumeration of what the earlier benchmark format under-tested. It is the most useful single artefact for deciding what your own benchmark is missing.

### End-to-end database benchmarking

**VectorDBBench** targets the whole database rather than the index in isolation [3]. It measures QPS via concurrent top-100 query workloads, recall against precomputed ground truth, `serial_latency_p99`, `max_load_count` (continuous insertion until failure, to find capacity limits), and `load_duration` (time to insert plus build the index) [3].

The distinction matters: insertion cost and index-build cost are first-class, and filtered search is a scenario, neither of which pure-index tools cover [3].

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Warm-up duration | whether you measure steady state | until latency stabilises | measuring cold-cache and JIT-warmup time |
| Concurrency level | throughput realism | swept, not fixed at 1 | single-threaded timing reported as throughput |
| Query count | measurement stability | enough to run for tens of seconds | ~5,000 queries finishes in ~0.2 s, dominated by startup [5] |
| Recall target | comparability | fixed across systems | comparing QPS at different recalls is meaningless |
| Dataset | what you are actually measuring | matched to your real dimensionality | random synthetic vectors are pathologically uniform in high dimensions and give recall numbers that do not transfer |
| Repetitions | confidence | **at least 10** [10] | one run, reported to four significant figures |
| Host | whether the number is reproducible at all | dedicated, or steal-time monitored [11] | shared VM: results move between runs with no code change |
| Timing boundaries | what is inside the measurement | the query only | including connection setup and process spawn [5] |

## How real systems do it

### The documented methodology bugs

This is the most valuable material in the topic, because these are verified defects in published results rather than hypothetical concerns.

Evgenii Ivanov's analysis of ANN-Benchmarks-derived DBMS results itemises five concrete bugs [5]:

1. **QPS mis-defined.** The tool computes QPS as `1.0 / best_search_time` — an inverted *single-query latency*, which "ignores concurrency" and is not throughput under parallel load at all.
2. **Timing pollution.** Many forks measure wall-clock spans that include "time spent on creating new processes, connecting to the database, and opening sessions".
3. **Too little test data.** Datasets with only ~5,000 test queries complete in ~0.2 seconds — too short to amortise startup, so the measurement is dominated by initialisation rather than steady state.
4. **Thundering herd from unstaggered concurrent launches.** The corrected methodology showed **QPS 3× lower** on short, naively parallelised runs, "despite a warm cache".
5. **GIL-bound threading instead of multiprocessing** in Python harnesses, artificially capping parallel scalability.

After correcting these — **without changing the database under test** — Ivanov measured **~20× higher QPS on pgvector** than the original methodology reported [5].

Read that number carefully. The methodology was wrong by more than an order of magnitude, in published results, for a widely used system. Any performance comparison you have read that derives from this tooling may be off by a similar factor in either direction.

### Vendor self-interest, contractually enforced

benchANT's analysis found that **4 of 13** vector databases surveyed impose **DeWitt clauses** — contract terms legally forbidding customers or third parties from publishing independent benchmark results [6].

The corroborating detail is the interesting one: **all four** of those vendors "only provide the vector search support in their DBaaS offer", meaning there is no self-hostable version anyone could benchmark independently. benchANT's conclusion is that this pattern is "an indication that their performance is not (yet) as good as their marketing claims" [6].

This is stronger than the usual "vendors tune benchmarks favourably" complaint. Some vendors have made independent verification a breach of contract.

The practical consequence: when comparing systems, check whether independent results exist at all. Their absence is information.

## Complexity and cost

| | Cost |
|---|---|
| Ground-truth computation | `O(N · Q · D)` — a full brute-force scan for every query; this is why it is precomputed and shipped |
| Parameter sweep | multiplicative in the number of knobs; the Pareto frontier is the useful output, not every point |
| Index build | often the dominant cost in a database benchmark, and the one pure-index tools omit [3] |
| Statistical stability | tens of seconds of steady-state measurement minimum, after warm-up [5] |
| A recall measurement in production | as expensive as the exact search the index exists to avoid |

Ground-truth cost is the reason recall is measured offline on a sample rather than continuously in production. See [Observability](21-observability.md).

## Failure modes

- **Single-query latency reported as throughput** — the `1.0 / best_search_time` bug, off by the concurrency factor [5].
- **Timing includes setup** — process spawn, connection, session establishment inside the measured span [5].
- **Runs too short to leave warm-up** — 0.2 seconds of measurement, dominated by initialisation [5].
- **Unstaggered concurrent starts** — a thundering herd depressing measured QPS by 3× [5].
- **Language-runtime artefacts** — a GIL-bound harness capping the scalability of the system under test [5].
- **Comparing at different recalls.** The most common conceptual error, and it makes any comparison meaningless.
- **Synthetic random vectors.** Pathologically uniform in high dimensions, so recall measured on them does not transfer to real embeddings. Fine for testing an *exact* index, where recall is 1 by construction; not fine for tuning an approximate one.
- **One run, reported precisely.** Variance is not reported, so a 5% difference between systems is indistinguishable from noise — and on a shared host the run-to-run spread can exceed the effect being claimed [10][11].
- **An undeclared machine.** Container fsync, CPU steal, and core count each change results by more than most optimisations do.
- **Benchmarking the cache.** A working set that fits in L3 measures something real, and not what you meant.
- **Absent independent results**, because publishing them is contractually forbidden [6].

## How to test it

Benchmarks are code, and they need their own correctness checks — the failures above are all bugs in the measuring instrument, not in the system measured.

- **Verify the throughput definition.** Run a workload with a known artificial concurrency and assert the reported QPS matches concurrency ÷ latency. This catches the `1/latency` bug directly [5].
- **Assert the measurement excludes setup.** Time an empty workload; if it is not near zero, setup is inside the span.
- **Assert steady state.** Report the first and last decile of measured latencies separately. If they differ substantially, warm-up was insufficient.
- **Report variance, always.** Run each configuration several times and report the spread. A benchmark that reports a mean without a spread cannot support any comparison.
- **Record the machine and the run parameters alongside every number**, in the same file. A number without its machine is not a measurement.
- **Sanity-check against a known ratio.** Some results have expected shapes — group commit should beat per-write fsync by roughly an order of magnitude; an approximate index should beat brute force by much more than 2× at scale. A result outside the expected shape is a bug in the harness until proven otherwise.

The deeper practice: **let the benchmark contradict you**. A measurement that only ever confirms the design is not being read carefully enough. A benchmark that produces a result you did not expect — a snapshot that makes recovery slower, a second replica that costs almost nothing — has done its job, and the correct response is to explain the mechanism, not to re-run until it agrees.

## Minimal demo version

Use the language's own benchmark harness rather than a script. It handles iteration counts, warm-up and reporting, and it makes the benchmark a committed, re-runnable artefact rather than a number in a chat log.

Benchmark the **trade-offs the system claims**, not everything:

- fsync per write versus group commit — the durability knob
- replicated write with 0, 1 and 2 followers — the cost of a second copy, and whether fan-out is parallel
- search across a 100× range of `N` — reporting bytes-per-second, not just latency, so a bandwidth-bound loop is visible as flat throughput
- recovery time with and without a snapshot, **at more than one churn level**

Then write the results into a file with the machine spec, the exact command, and one paragraph per result explaining the *mechanism* behind the number.

**What you can cut:** SIFT and GIST datasets, recall sweeps (if the index is exact, recall is 1 by construction), concurrency sweeps, comparisons against other databases, and p99 reporting — the standard harness reports means, and claiming a tail from a mean is exactly the error this file is about.

**What you cannot cut without the demo becoming a lie:**

- **The machine spec and the caveats.** An fsync number from a container filesystem is not an fsync number from a disk. Report the *ratio* as meaningful and the absolute microseconds as fiction, and say so.
- **At least one benchmark with more than one parameter value.** A single-point measurement cannot show a trade-off, and a trade-off is the only thing worth benchmarking.
- **The results that contradicted the design.** They are the most valuable output. A benchmarks file where every number confirms the architecture is a benchmarks file nobody read carefully.

## Open questions and contested claims

**Are published ANN benchmark comparisons trustworthy?** Ivanov's answer is essentially no, with itemised, verified defects and a 20× correction on a real system [5]. The counter-position is that the *relative* ordering of algorithms survives methodology bugs even when absolute numbers do not. That defence is plausible for the recall-QPS frontier, where all algorithms suffer the same harness overhead, and much weaker for cross-*database* comparisons, where one system's client library may be GIL-bound and another's not. No source settles it.

**Is there a numeric crossover between index families?** Practitioners want a rule like "below N million vectors use HNSW, above use DiskANN". No such controlled crossover benchmark was located. DiskANN's own paper gives absolute numbers for DiskANN — SIFT1B, 64 GB RAM, over 5,000 QPS, under 3 ms, 95%+ recall [7] — but no head-to-head threshold against HNSW or IVFPQ at smaller scales. The honest statement is that no controlled crossover benchmark was found, not an invented number.

**Does recall predict application quality?** Recall measures agreement with exact nearest neighbours. Whether that predicts downstream answer quality in a retrieval-augmented system is an open question with active disagreement in the literature [8][9] — a system can have lower recall and produce better answers if the neighbours it does return are more diverse. Optimising recall is the default, and it is a proxy, not the objective.

## Sources

[1] Aumüller, Bernhardsson, Faithfull — "ANN-Benchmarks: A Benchmarking Tool for Approximate Nearest Neighbor Algorithms" — Information Systems Vol. 87, 2020; project README — https://github.com/erikbern/ann-benchmarks/blob/main/README.md
[2] TEXMEX corpus (SIFT1M, GIST1M, fvecs/ivecs/bvecs formats), INRIA/IRISA — http://corpus-texmex.irisa.fr/ — format description unverified; the page could not be fetched
[3] Zilliz — "Performance Benchmarking with VectorDBBench" — https://docs.zilliz.com/docs/byoc/perf-benchmark-vectordb
[4] Simhadri, Aumüller, Douze, et al. — "Results of the Big ANN: NeurIPS'23 competition" — arXiv:2409.17424, 2024 — https://arxiv.org/abs/2409.17424
[5] Ivanov — "Are published ANN-Benchmarks DBMS results trustworthy?" — YDB.tech, 2026 — https://blog.ydb.tech/are-published-ann-benchmarks-dbms-results-trustworthy-f2573eca4e07
[6] benchANT — "To Benchmark Vector Databases or to Get Sued for Breaching a DeWitt Clause?" — 2024 — https://benchant.com/blog/vectordb-de-witt
[7] Subramanya, Devvrit, Kadekodi, Krishnaswamy, Simhadri — "DiskANN: Fast Accurate Billion-point Nearest Neighbor Search on a Single Node" — NeurIPS 2019 — https://proceedings.neurips.cc/paper_files/paper/2019/file/09853c7fb1d3f8ee67a61b6bf4a7f8e6-Paper.pdf
[8] Schwartz, Vasilyev, Sawaya — "How Important is Recall for Measuring Retrieval Quality?" — arXiv:2512.20854
[9] Deepchecks — "Retrieval Quality VS. Answer Quality: Why RAG Evaluation Fails" — https://deepchecks.com/retrieval-vs-answer-quality-rag-evaluation/
[10] The Go Authors — `benchstat`, golang.org/x/perf — https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
[11] AppSignal — "CPU Steal Time: A Crucial Metric for Cloud Servers and VMs" — 2021 — https://blog.appsignal.com/2021/09/15/cpu-steal-time-a-crucial-metric-for-cloud-servers-and-vms.html
