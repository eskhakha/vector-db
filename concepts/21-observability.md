# 21. Observability

> Instrumenting a distributed database so that a failure can be diagnosed after it happens — including the arithmetic mistake that makes most latency dashboards understate the tail by half.

## The problem

A distributed failure is not reproducible on demand. By the time you know something is wrong, the interleaving that caused it is gone. If the instrumentation was not already there, the incident is unexplainable.

That is the general case. There are three specific failures observability exists to prevent, and they are the ones that actually bite:

1. **You cannot tell which shard is slow.** A fan-out query is bounded by its slowest shard, so an aggregate latency number tells you the query was slow and nothing about why.
2. **You cannot follow one request across nodes.** Without a shared identifier, a request that touched a coordinator and four shards produces five unconnected log streams.
3. **Your dashboard lies about the tail.** Not metaphorically — arithmetically, by roughly a factor of two in the worked example below.

Observability cannot be retrofitted into a failure you are trying to reproduce. It has to be there before.

## How it works

Three frameworks cover the ground, and they are complementary rather than competing.

### RED — for services

Rate, Errors, Duration. Created by Tom Wilkie in 2015 as a deliberately microservices-oriented alternative to USE: "The USE Method doesn't really apply to services; it applies to hardware, network disks, things like this. We really wanted a microservices-oriented monitoring philosophy, so we came up with the RED Method" [1].

- **Rate** — "the number of requests per second"
- **Errors** — "the number of those requests that are failing"
- **Duration** — "the amount of time those requests take" [1]

The justification is that it is customer-facing: "The RED Method is a good proxy to how happy your customers will be. If you've got a high error rate, that's basically going through to your users and they're getting page load errors" [1].

### USE — for resources

Utilization, Saturation, Errors. Brendan Gregg's method, aimed at **resources** — CPU, memory, disk, network, and kernel-level software resources like mutexes and file descriptors — not services [2].

- **Utilization** — the percentage of time the resource is busy doing work
- **Saturation** — the degree to which work is queued beyond what the resource can currently service
- **Errors** — error and fault event counts on that resource [2]

"The USE Method provides a strategy for performing a complete check of system health, identifying common bottlenecks and errors" [2].

### The four golden signals — in between

Google's SRE book: Latency, Traffic, Errors, Saturation [3].

- **Latency** — "The time it takes to service a request. It's important to distinguish between the latency of successful requests and the latency of failed requests", with the warning that "a slow error is even worse than a fast error" [3]. That distinction matters more than it sounds: a flood of fast failures can *improve* your average latency while the service is completely broken.
- **Traffic** — "A measure of how much demand is being placed on your system, measured in a high-level system-specific metric" [3].
- **Errors** — "The rate of requests that fail, either explicitly (e.g., HTTP 500s), implicitly (for example, an HTTP 200 success response, but coupled with the wrong content), or by policy" [3]. The implicit case is the one vector search hits: a query that returns results from three of four shards is a 200 with wrong content.
- **Saturation** — "How 'full' your service is... emphasizing the resources that are most constrained", with the caveat that "Many systems degrade in performance before they achieve 100% utilization, so having a utilization target is essential", and that "latency increases are often a leading indicator of saturation" [3].

The relationship: USE diagnoses *why* a resource is a bottleneck; RED and the golden signals track *whether the service is healthy* from a request point of view. Golden signals sit between, adding USE-style saturation to a RED-style view. Use RED for the request path, USE for the resources underneath it, and reach for one when the other says something is wrong but not why.

No independently fetched primary critique of RED or USE was located — the literature is full of "how to choose between them" explainers and short on published limitations pieces.

### Percentiles: the arithmetic that breaks dashboards

This is the part most teams get wrong, and it is not a matter of taste.

Percentiles are **order statistics** — computing one requires the full sorted distribution — so a *finished* percentile from one host cannot be recombined with another host's finished percentile by averaging. "Percentiles are order statistics: computing one requires the full sorted distribution, and a finished percentile value cannot be recombined" [4].

The worked example [4]:

| | Requests | p99 |
|---|---|---|
| Host A | 1,000 @ 100 ms | 100 ms |
| Host B | 900 @ 100 ms, 100 @ 1,000 ms | 1,000 ms |
| Naive average of the two p99s | | **550 ms** |
| True fleet p99 over all 2,000 | | **1,000 ms** |

"Averaging per-host p99s reports 550 ms where the true fleet p99 is 1,000 ms" [4] — understating the real tail by roughly half. A dashboard doing this is not approximately right; it is confidently wrong in the direction that hides problems.

The fix is to **merge the underlying distributions, then compute the percentile once**. Two mergeable representations [4]:

- **Bucketed histograms** (Prometheus classic or native, OpenTelemetry) — bucket counts are additive across instances and time windows, so sum the buckets first and compute the quantile after. Accuracy is bounded by bucket width.
- **Quantile sketches** (t-digest, DDSketch, HDR Histogram) — adapt to the data and "guarantee a fixed relative error and full mergeability".

The SRE book gives the same guidance from the other direction: prefer histograms over means, "Collect request counts bucketed by latencies... how many requests did I serve that took between 0 ms and 10 ms", with exponentially spaced bucket boundaries [3].

**Prometheus specifics.** The `Summary` type computes client-side quantiles that are accurate but **not aggregatable**: "aggregating the precomputed quantiles from a summary rarely makes sense... averaging the quantiles yields statistically nonsensical values", and "Other φ-quantiles and sliding windows cannot be calculated later" [5]. The `Histogram` type is aggregatable — sum per-bucket counters across instances first, then apply `histogram_quantile()`, which is why the idiomatic query is `histogram_quantile(0.95, sum(rate(..._bucket[5m])) by (le))` [5].

`histogram_quantile()` interpolates linearly **within** a bucket, which introduces real error: "given the bucket boundaries above, the true value could be anywhere between 210ms and 229ms, depending on the actual distribution of requests within the bucket" [5]. With coarse buckets a spike at 220 ms inside a 200–300 ms bucket can be estimated at ~295 ms — "potentially misleading regarding SLO compliance" if that spread crosses the SLO boundary [5]. Native histograms let you pick resolution at instrumentation time and query flexibly later, trading storage for much lower interpolation error [5].

### Request IDs and tracing

OpenTelemetry's context-propagation model attaches a `trace_id` shared across an entire distributed request plus per-hop `span_id`s, typically via a W3C Trace Context header or equivalent RPC metadata; when a coordinator fans out to shard-local searches, each shard call is a **child span** of the coordinator's span sharing the `trace_id`, so a scatter-gather top-K query can be reconstructed as one trace across `S` shard RPCs. This framing is corroborated at search-snippet level only; the OpenTelemetry context-propagation documentation was not fetched at quote level, so treat the exact header format as unverified here.

The low-tech version does most of the work: a single `request_id` generated at the entry point and threaded through every downstream call and every log line. It gives no span timing or hierarchy, but it lets an operator find every log line for one logical request across every node it touched. That is standard practice rather than a citable claim, and it is the single highest-value thing to add first.

## Parameters and knobs

| Parameter | Controls | Typical | If wrong |
|---|---|---|---|
| Histogram bucket boundaries | quantile accuracy | exponentially spaced, covering the real range | too coarse: interpolation error crossing SLO boundaries [5]; too fine: cardinality explosion |
| Metric type (histogram vs summary) | whether percentiles aggregate | histogram | summary: per-host percentiles that cannot be combined [5] |
| Label cardinality | storage and query cost | bounded sets only | a vector ID as a label kills the metrics backend |
| Trace sampling rate | trace volume vs. coverage | 1% baseline, 100% on error | uniform low sampling misses the rare slow request, which is the one you need |
| Scrape interval | resolution vs. load | 10–60 s | too coarse to see a 5-second failover window |
| Log level | signal vs. volume | info, with debug per request ID | logging every write at info makes the log unusable in the incident |

## How real systems do it

The three frameworks above are the industry consensus, and their authors are explicit about their scope [1][2][3]. The interesting divergence is not between them but between what a *general* service needs and what a **vector database** additionally needs.

**Recall cannot usually be measured online.** For an approximate index, recall — the fraction of true top-K neighbours actually returned — can only be computed against exact ground truth, which is precisely the expensive computation the index exists to avoid. In production this means recall is measured **offline and periodically** on a held-out sample of queries, running both the ANN index and a brute-force scan and comparing, rather than continuously on live traffic.

That is a genuine measurement gap with no analogue in RED or USE, both of which are computable online from request traffic alone. It has a real consequence: **the most important quality metric of a vector database is not on the dashboard**, and a recall regression from a bad index rebuild or a misconfigured `ef_search` will show up as user complaints, not as an alert. Systems that do not schedule periodic offline recall measurement have no way to detect it. This paragraph is synthesis from the structure of the problem; no vendor engineering source stating it explicitly was located.

The remaining vector-specific metrics are standard applications of the general frameworks, and it is worth being explicit about which is which rather than presenting them as a special list:

| Metric | Framework | Why |
|---|---|---|
| Replication lag | saturation-adjacent | how far behind a follower is, in log position or time |
| WAL fsync latency | duration on a resource | the disk, not the service |
| Index build time | USE-style capacity | dominates rebalance readiness |
| Segment count | USE-style capacity | compaction health |
| Queue depth | textbook USE saturation | write queue, index-build queue, replication-send queue |
| Shards responding per query | golden-signals *implicit* errors | a 200 with three of four shards is wrong content [3] |
| In-sync replica set size | leading indicator | a shrinking set is the precursor to refused writes or lost ones |

No dedicated vector-database source discussing these by name was fetched; this is a mapping onto the general frameworks, not a sourced claim.

## Complexity and cost

| | Cost |
|---|---|
| Counter | one atomic add per event; negligible |
| Histogram | one bucket increment per observation, plus `buckets × labels` series in storage |
| Cardinality | multiplicative across labels — this is the cost that kills backends |
| Trace span | allocation plus serialisation per hop; sampling exists for this reason |
| Structured log line | dominated by I/O and serialisation; at high write rates this can exceed the work being logged |
| Percentile query | `O(buckets)` at query time on summed counters, not `O(observations)` |

The rule that matters: **cardinality is multiplicative**. Ten shards × five operations × three status values is 150 series and fine. Add a per-vector or per-user label and it is unbounded and fatal.

## Failure modes

- **Averaged percentiles.** Understating the true fleet tail by roughly half in the worked example [4]. Silent, and it makes the system look healthier than it is.
- **Interpolation error crossing an SLO boundary.** A quantile estimated at 295 ms when the true value is 220 ms, because the bucket is 200–300 ms [5].
- **Fast errors flattering the average.** A flood of quick failures improves mean latency. This is why the SRE book insists on separating successful and failed request latency [3].
- **Cardinality explosion.** One unbounded label takes down the metrics backend, usually during the incident you added it to debug.
- **No request ID.** Five log streams and no way to join them.
- **Sampling that misses the rare slow request** — uniform 1% sampling has a 1% chance of capturing the trace you need.
- **Metrics that only exist in the happy path.** Refusals, aborts, and partial results are the events worth counting, and they are the ones people forget to instrument.
- **Recall regressions with no signal**, as above.

## How to test it

Observability is code, and untested instrumentation is worse than none because it is trusted.

- **Assert the metric changes.** After a fault-injection test that refuses writes, assert the refusal counter incremented. A test that injects a fault and does not check the metric leaves you unable to distinguish "did not happen" from "not instrumented".
- **Check the percentile arithmetic with the worked example.** Feed the two-host distribution from [4] through your aggregation and assert it reports 1,000 ms, not 550 ms. This is a five-line unit test that catches a class of dashboard bug permanently.
- **Bound cardinality in a test.** Enumerate the label sets your code can emit and assert the count is below a ceiling. This is the only defence that works before production.
- **Assert the request ID propagates.** Issue one fan-out query and assert every shard-local log line carries the same ID. Trivially cheap, and it fails immediately whenever a new code path forgets to pass the context.
- **Schedule an offline recall measurement** and treat a drop as a test failure, not a dashboard curiosity.

## Minimal demo version

Two things, and only two.

**A request ID threaded through every hop**, present in every log line, with structured logging. This is what turns a fan-out failure from unexplainable into readable, and it costs one context field.

**Counters and one latency histogram** for the operations you will later make claims about: writes acknowledged, writes refused, searches, shards responding per query, replication lag, and the in-sync set size. Counters are enough for most of these; the histogram matters only where you intend to quote a percentile.

Then make the instrumentation earn its place: run the failure demo and read the *metrics* rather than the code to explain what happened. If you cannot, the instrumentation is wrong, and that is a finding.

**What you can cut:** Prometheus itself, an exporter, distributed tracing, sampling, dashboards, alerting rules, and every metric on the happy path that you do not intend to make a claim about.

**What you cannot cut without the demo becoming a lie:**

- **Counting refusals and partial results.** These are the events the whole design exists to produce. Instrumenting only successes means the demo cannot show the thing it is demonstrating.
- **The request ID.** Without it, the multi-shard story is untellable, which is the entire reason a distributed system needs observability rather than a debugger.
- **Merging distributions rather than averaging percentiles**, if you report a percentile at all. Reporting an averaged p99 in a document that also teaches percentiles is worse than reporting nothing.

## Open questions and contested claims

**RED versus USE versus golden signals** is presented in the literature as complementary, and their authors are explicit about scope [1][2][3]. Notably, no independently fetched primary source *criticising* either framework was located — which means the "they complement each other" consensus is well documented and the limitations are not. The obvious critiques (USE misses request-shape-dependent bottlenecks; RED misses resource contention entirely) are stated here as engineering reasoning, not as sourced claims.

**How to measure recall in production** has no settled answer. Offline sampling against brute-force ground truth is the only correct method, and it is expensive, so it tends to be done rarely or not at all. Whether periodic sampling is sufficient, and at what frequency, is not addressed by any source located here — which is a real gap in the literature rather than a search shortfall, and worth knowing before assuming the answer exists.

## Sources

[1] Grafana Labs — "The RED Method: How to Instrument Your Services" (Tom Wilkie's method, created 2015) — 2018 — https://grafana.com/blog/the-red-method-how-to-instrument-your-services/
[2] Gregg — "USE Method: Linux Performance Checklist" — 2013 — https://www.brendangregg.com/USEmethod/use-linux.html
[3] Google SRE — "Monitoring Distributed Systems", Site Reliability Engineering, Ch. 6 — https://sre.google/sre-book/monitoring-distributed-systems/
[4] ClickHouse — "Percentiles vs averages: why your latency dashboard lies" — https://clickhouse.com/resources/engineering/percentiles-vs-averages
[5] Prometheus — "Histograms and summaries" — https://prometheus.io/docs/practices/histograms/
