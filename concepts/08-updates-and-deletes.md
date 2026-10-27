# 08. Updates and deletes in a vector index

> Every ANN index in production handles deletion the same way — mark the vector dead, keep it in the structure, filter it out at query time, and reclaim the space later during a bulk rebuild — because removing a node from a proximity graph destroys the routing paths that ran through it, and nobody has shipped a repair for that.

## The problem

An inverted index deletes cleanly. Drop the document ID from the posting lists it appears in and the index is exactly as correct as before. A proximity graph does not have this property, and the reason is structural rather than incidental.

The graph's correctness is a *global* property assembled from local edges. Greedy search works because there exists a path of monotonically-improving hops from the entry point to the query's neighbourhood ([Graph indexes and HNSW](07-graph-indexes-hnsw.md)). Any given node is not just a candidate answer — it is a waypoint for every query whose path happens to route through it. Remove it and every such path breaks. If the node happened to sit on a bridge between two regions, its removal can disconnect one of them from the entry point entirely, and the failure is silent: queries still return `K` results, they are just worse ones.

HNSW's own paper claims **O(1) element removal**, independent of `N`, because only the locally affected neighbour lists need updating [1]. That claim is true and irrelevant. It measures the mechanical cost of splicing out one node's adjacency, not the quality consequence for the rest of the graph. Nothing in the O(1) figure restores the edges that node was providing to its neighbours, and nothing detects that a region has become unreachable. This distinction — mechanical cost versus connectivity guarantee — is the whole topic.

Updates are the same problem wearing a hat. Changing a vector's coordinates changes which nodes should be its neighbours *and* which nodes should have it as theirs, so an in-place mutation invalidates edges in both directions. Nobody does it.

## How it works

### Soft delete

The universal answer is a tombstone: record the ID as deleted in a side structure, leave the graph untouched, and filter the deleted IDs out of the result set at query time.

```
delete(id):
    tombstones.set(id)                 # O(1), no graph mutation
    live_count -= 1

search(q, K):
    cands = graph_beam_search(q, ef)   # traverses dead nodes too
    return [c for c in cands if not tombstones.get(c)][:K]
```

That is the entire mechanism. It buys three things: the delete call is fast and cannot corrupt the graph; deleted vectors remain valid waypoints, so routing quality is preserved exactly; and the reclamation decision is decoupled from the write path.

It costs one thing, and the cost compounds.

### Why `efSearch` must be inflated

The beam is a fixed budget. `efSearch` bounds how many candidates the layer-0 search will hold, and dead nodes consume that budget on equal terms with live ones — they are traversed, distance-computed, and admitted to the candidate list, then discarded at the end. With a fraction `t` of the index tombstoned, an `efSearch` of `e` yields roughly `e·(1 − t)` live candidates. pgvector's version of this arithmetic is concrete: with 20% of index entries deleted, dead nodes consume roughly 20% of the traversal budget, cutting effective exploration from 40 candidates down to about 32 live ones per query [8].

The compensation is to scale the beam: use `efSearch / (1 − t)` instead of `efSearch`. This restores the live candidate count and pays for it in latency, linearly. What it does *not* restore is recall lost to graph damage, because with pure tombstoning there is none — the graph is intact. That is exactly why tombstoning wins over surgery.

The failure this creates is a **silent recall regression**. Nothing crashes. Latency creeps. Recall drifts down as the tombstone ratio climbs, and unless you are measuring recall against ground truth continuously, the first symptom is a user complaint. pgvector's documented practice is to force a rebuild at roughly **10% cumulative deletions** [8].

### Graph degradation under churn

Pure deletion is the benign case. Delete-and-reinsert churn is worse, for a reason specific to how these graphs are built.

HNSW's connectivity quality is order-of-insertion dependent: long-range edges are disproportionately created early, when the graph is sparse, and are disproportionately held by nodes that drew high layers [1]. That structure is history, not geometry. When you delete an early-inserted node and insert a replacement, the replacement enters a dense mature graph and gets ordinary local edges — the specific long-range structure is not reconstructed. Meanwhile `Mmax`/`Mmax0` caps mean most existing nodes' edge lists are already full, so a new insertion mostly displaces edges rather than adding them. Over many churn cycles the graph drifts toward locally-good, globally-poorly-connected — and every hop wasted visiting a tombstoned node compounds it. VectorChord's critique frames the operational version: HNSW updates and deletes "require cascading modifications throughout the graph," producing "substantial write amplification and computational overhead" [10]. That source is a vendor arguing against a competing design; take it as an argued position, not a settled measurement.

### Updates as delete-plus-reinsert

Because in-place edge repair is unsolved, updates become two operations: tombstone the old ID, insert the new vector under a new internal ID. This is not a shortcut, it is the only correct option available, and it means an update costs a full graph insertion — greedy search at `efConstruction`, neighbour heuristic evaluation, and potential neighbour-list shrinking cascades at every affected layer — plus a deferred cleanup obligation. Under a re-embedding workload, where the whole corpus is updated when the embedding model changes, this is a full rebuild wearing a disguise, and it is usually cheaper to admit that and do a real rebuild.

### Reclamation: rebuild, compaction, or merge

Space and traversal budget come back only through bulk work, triggered by a **tombstone ratio threshold**:

```
if tombstones / total > threshold:
      rebuild the affected unit
```

The "affected unit" is where architectures diverge, and it maps exactly onto the three-way persistence split from [Graph indexes and HNSW](07-graph-indexes-hnsw.md):

- **Per-segment immutable** → reclamation is *segment merge*. Deleted docs are dropped as segments combine. No separate delete machinery is needed; the general compaction pipeline does it.
- **Build-on-seal** → reclamation is *segment compaction*, an explicit operation that merges sealed segments and drops tombstoned vectors.
- **Mutable in place** → reclamation is a *background cleanup pass* that rebuilds affected parts of the live graph.

## Parameters and knobs

| Parameter | Controls | Typical value | If you get it wrong |
|---|---|---|---|
| Tombstone ratio threshold | When reclamation triggers | Qdrant `deleted_threshold` default **20%** [2]; pgvector practice ~**10%** [8] | Too low: constant background rebuild contending with queries. Too high: recall decays and memory is wasted |
| `efSearch` inflation | Compensates for dead nodes in the beam | `efSearch / (1 − tombstone_ratio)` | Not inflating gives silent recall decay; over-inflating pays latency for nothing |
| Compaction concurrency | Background rebuild parallelism | 1–2 workers | Unbounded workers turn reclamation into a latency incident |
| Segment/flush size | Reclamation granularity | Milvus seals at ~75% capacity or explicit flush [15] | Large units: cheap steady-state search, long rebuild pauses. Small units: fast reclamation, per-query fan-out grows |

## How real systems do it

The striking thing is the unanimity. Four systems with completely different storage architectures converged on the same answer, and none of them attempts incremental graph repair.

**Qdrant** soft-deletes: the point is marked and kept on disk so the delete call stays fast, and a background **vacuum optimizer** physically removes tombstoned points once their proportion crosses a configurable `deleted_threshold`, default **20%** [2]. The interesting part is Qdrant's own measurement of the trade-off. Deferring vacuuming to a 50% threshold gave **p95 latency of 4.3 ms**, against **22.7 ms** when vacuum ran at the 20% default — because vacuuming causes background write contention that competes with live queries [2]. Read that twice: the tuning that costs you memory and beam budget *improved* tail latency five-fold, because reclamation is itself the expensive event. Qdrant's segment model supports this directly, distinguishing appendable segments (insert/delete/query freely) from non-appendable ones (read and delete only), with the optimizer converting the former into the latter [13].

**Milvus** records deleted IDs in a **DEL file** represented as an in-memory bitmap — "the data structure of DEL is a bitmap in memory, where an active bit represents a deleted offset" — and the similarity search simply excludes those offsets, so "the vector corresponding to the active bit will no longer be included in the distance calculation" [3]. Each segment carries raw data, its index file, and its own DEL file. Deletion is deferred until the next flush, and space is reclaimed only via an explicit `compact()` that merges segments and drops tombstoned vectors — manually triggered at the time of writing, with automatic triggering listed as a planned enhancement [3].

**Weaviate** uses "a tombstoning approach — inspired by Cassandra" [4]. A deleted object is "immediately hidden on future query results" while the underlying HNSW node survives until an **asynchronous cleanup** pass "rebuilds the affected parts of the index" in bulk, which is "considerably faster than individual cleanups would be" [4]. Weaviate is also the clearest public statement of the update rule: an update "deletes the old doc id under the hood, assigns a new one, and reimports the object into the vector index" [4] — delete plus reinsert, explicitly, not mutation. Persistence follows the same deferred pattern: the HNSW index has its own append-only commit log separate from the LSM write-ahead log, and since v1.39 Weaviate loads the most recent snapshot at startup and replays only the commit-log entries written after it, with a background compactor pruning old logs [14].

**Lucene and Elasticsearch** inherit the pattern from twenty years of inverted-index practice: mark deleted in a per-segment bitset, reclaim only when segments merge. Vector search rides the same machinery — the ANN index is segment-scoped and rebuilt or merged as part of ordinary segment-merge compaction, with no independent vector-index lifecycle [6]. What is new is that merging HNSW graphs is genuinely harder than merging posting lists. Lucene's optimized approach retains the largest graph and inserts vectors from smaller ones into it, computing a "join set" of roughly **1/5 of the smaller graph's vertices** via a greedy gain heuristic, inserting those normally, then reusing their established neighbour relationships to place the rest more cheaply than a from-scratch search per vector — reported at **1.28× indexing / 1.72× force-merge** with int8 quantization and **1.33× / 1.34×** with BBQ quantization [5].

**pgvector** is the cautionary case, because it has the tombstone half without the reclamation half. "HNSW indexes do not reclaim space when rows are deleted: tombstoned nodes stay in the graph until the index is rebuilt," and `REINDEX CONCURRENTLY` is "prohibitively expensive for large indexes" [7] — which for 1M rows of embeddings can mean an 8 GB index [16] and, at 10M rows of 1536-D vectors, hours of single-core build producing tens of gigabytes [7]. The measured consequence is the beam-budget arithmetic above [8]. There is a second-order effect too: because a 1536-D vector is roughly 6 KB before row overhead, the `UPDATE`-heavy "re-embed and replace" pattern generates large dead tuples faster than default autovacuum settings can reclaim them, bloating the table until the planner falls back to sequential scans [8].

**Vespa** is the one system that took a different route on the index side, supporting real-time mutable HNSW with CRUD "in true real time" [11] — no segment model for vectors, therefore no merge-driven reclamation. It also demonstrates a distinct *design-level* dodge worth naming: in one billion-scale hybrid architecture, the expensive centroid structure is placed in a schema that is never deleted from, while the mutable raw data lives in a separate, independently-updatable schema, so the costly index never has to absorb a delete at all [12]. Avoiding the problem is a legitimate answer to it.

**FreshDiskANN** is the only system in this set explicitly designed to break the rebuild dependency for graph indexes, claiming real-time absorption of inserts, deletes and searches at billion scale — "thousands of concurrent real-time inserts, deletes, and searches per second each" at >95% 5-recall@5, with a claimed 5–10× cost reduction versus periodic full rebuilds [9]. Its exact consolidation mechanics are not established here.

The takeaway: nobody does incremental graph-edge repair. HNSW's advertised O(1) removal is mechanical only.

## Complexity and cost

- **Tombstone a delete:** O(1) time, one bit of memory per vector.
- **Query with tombstones:** unchanged asymptotically, but the constant scales as `1/(1 − t)` once you inflate `efSearch` to compensate.
- **Update:** one full insertion, O(`efConstruction` · `M` · log N) distance computations, plus a deferred tombstone.
- **Rebuild:** the full construction cost, O(N log N) with a large constant — 1 hour 27 minutes serial for 1M × 1536-D at `efConstruction=200`, 9.5 minutes parallel ([Choosing an index](09-choosing-an-index.md)).
- **Memory:** tombstoned vectors occupy their full `4·D` bytes plus edges until reclaimed. At a 20% threshold you are permanently carrying up to 20% dead weight, by design.

## Failure modes

- **Silent recall decay.** The headline failure. No error, no latency spike, just gradually worse answers as the tombstone ratio climbs [7][8].
- **Reclamation as a latency incident.** Vacuum, compaction and force-merge all contend with live queries; Qdrant's own numbers show a 5× p95 difference driven purely by when vacuuming runs [2].
- **Unbounded growth when reclamation is never triggered.** Manual-only compaction [3] and prohibitively expensive REINDEX [7] both produce indexes that grow forever in practice, because the operator never runs the expensive thing.
- **Delete lost across a restart.** If tombstones live only in memory and the delete was acknowledged before the bitmap was persisted, the vector comes back from the dead on recovery. Tombstones are log records, not annotations — see [Write-ahead logging](12-write-ahead-logging.md).
- **Re-embedding treated as an update workload.** Updating every vector via delete-plus-reinsert costs more than rebuilding and leaves a worse graph.
- **Tombstone ratio computed per index instead of per segment.** A global 5% can hide one segment at 60%, which is where all the damage is.

## How to test it

- **Recall-versus-churn curve.** Build, measure recall@10 against brute-force ground truth, then delete and reinsert in rounds — 5%, 10%, 20%, 40% of the corpus — remeasuring each time with `efSearch` held fixed, then again with `efSearch` inflated by `1/(1 − t)`. The gap between the two curves separates beam-budget loss from graph damage. This is the single test that makes the whole concept visible.
- **Delete-durability property test.** Delete, kill the process without a clean shutdown, recover, assert the deleted ID is still absent. Run it with the kill placed before and after the tombstone flush.
- **Reclamation invariant.** After compaction, assert: no tombstoned ID is reachable, live vector count matches expectation, and recall is at least what it was pre-deletion.
- **Connectivity after reclamation.** BFS from the entry point over layer 0 must reach every live node. A rebuild that drops nodes without repairing the edges pointing at them fails here immediately.
- **Latency during reclamation, not just after.** Measure p95 while compaction runs. The steady-state number is the number nobody's users experience.
- **Idempotence.** Deleting the same ID twice, and deleting a non-existent ID, must both be no-ops.

## Minimal demo version

Tombstones plus rebuild-at-threshold is about 50 lines and demonstrates the entire problem end to end:

```
tombstones: bitset over internal ids
delete(id):     tombstones.set(id); deleted += 1; maybe_rebuild()
update(id, v):  delete(id); insert(new_id, v)
search(q, K):   beam(q, ef / (1 - deleted/total)) filtered by tombstones, take K
maybe_rebuild(): if deleted/total > 0.2: reinsert all live vectors into a fresh graph
```

**Cut freely:** segment-scoped reclamation — rebuild the whole index, it is a demo; concurrent rebuild — pause writes, rebuild, resume; incremental or online migration; persistence of the tombstone bitset, provided you are honest that the durability question exists and is covered elsewhere.

**Cannot cut:** the instrumentation. A tombstone implementation with no recall measurement is indistinguishable from a correct one, which is precisely the lesson — this concept's entire content is that the failure is invisible. Log recall@10 against brute force and mean live-candidates-per-query at every churn round, and plot them. Also keep updates implemented as delete-plus-reinsert rather than in-place mutation, since implementing the shortcut teaches that in-place edge repair is easy, and it is the open problem of the field.

## Open questions and contested claims

- **Is incremental connectivity-preserving graph repair possible at all?** No widely-deployed system does it. FreshDiskANN claims to have solved the streaming case [9], but its consolidation mechanics are not established here, and no source describes edge-by-edge repair on delete. Treat this as genuinely open.
- **Is HNSW's update cost a disqualifying weakness or a manageable one?** VectorChord argues the cascading-modification cost is a "significant disadvantage versus simpler indexing approaches" for write-heavy workloads [10]; Vespa's shipping of a real-time mutable HNSW [11] is the strongest counter-evidence that it is tractable, and neither side has published a controlled comparison.
- **Where the tombstone threshold should sit.** Qdrant defaults to 20% [2], pgvector practice suggests 10% [8], and Qdrant's own benchmark says 50% is better *for tail latency* [2]. These disagree by 5× and are all defensible, because they optimize different things — memory, recall, and p95 respectively. There is no universal number, only a stated objective.
- **Whether merge-based reclamation is strictly better than in-place cleanup.** Lucene's segment model gets reclamation free from machinery it already had, and pays for it with per-query graph fan-out [6]; Vespa's mutable graph pays concurrency-control costs its documentation does not quantify [11]. The comparison has not been run publicly.

## Sources

[1] Malkov, Y.A., & Yashunin, D.A. — "Efficient and Robust Approximate Nearest Neighbor Search Using Hierarchical Navigable Small World Graphs" — IEEE TPAMI — https://arxiv.org/abs/1603.09320 — 2016/2018
[2] Qdrant — "Configure Qdrant's Optimizer for Predictable Search Latency" — https://qdrant.tech/articles/tuning-qdrant-optimizer/ — accessed 2026
[3] Milvus — "How Does the Milvus Vector Database Realize the Delete Function" — https://medium.com/vector-database/how-milvus-realizes-the-delete-function-727406c27cff — accessed 2026
[4] Weaviate — "Weaviate, a vector database with ANN Index and CRUD support" — https://weaviate.io/blog/crud-support-in-weaviate — accessed 2026
[5] Elasticsearch Labs — "HNSW graphs: Speeding up merging" — https://www.elastic.co/search-labs/blog/hnsw-graphs-speed-up-merging — accessed 2026
[6] Elasticsearch Labs — "Lucene vector search: Speeding up multi-graph vector search" — https://www.elastic.co/search-labs/blog/multi-graph-vector-search — accessed 2026
[7] ParadeDB — "pgvector Limitations" — https://www.paradedb.com/learn/postgresql/pgvector-limitations — accessed 2026
[8] Perun Engineering — "pgvector in production: five failure patterns" — https://perun.au/insights/pgvector-production/ — accessed 2026
[9] Singh, A., Subramanya, S.J., Krishnaswamy, R., & Simhadri, H.V. — "FreshDiskANN: A Fast and Accurate Graph-Based ANN Index for Streaming Similarity Search" — https://arxiv.org/abs/2105.09613 — 2021
[10] Zhou, J. (VectorChord) — "Why HNSW is Not the Answer" — https://blog.vectorchord.ai/why-hnsw-is-not-the-answer — 2024/2025
[11] Vespa — "Approximate nearest neighbor search using HNSW index" — https://docs.vespa.ai/en/querying/approximate-nn-hnsw.html — accessed 2026
[12] Vespa — "Building Billion-Scale Vector Search — part two" — https://blog.vespa.ai/building-billion-scale-vector-search-part-two/ — accessed 2026
[13] Qdrant — "Storage" — https://qdrant.tech/documentation/concepts/storage/ — accessed 2026
[14] Weaviate — "Storage" — https://docs.weaviate.io/weaviate/concepts/storage — accessed 2026
[15] Milvus/Zilliz — "Data Insertion and Data Persistence in a Vector Database (Deep Dive 4)" — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md — accessed 2026
[16] Crunchy Data — "HNSW Indexes with Postgres and pgvector" — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector — accessed 2026
