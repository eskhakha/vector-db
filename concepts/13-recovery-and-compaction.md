# 13. Recovery, snapshots and compaction

> Recovery is a pure function of what is on disk; snapshots exist to bound how long that function takes to run; compaction exists because deletes never actually delete anything until segments merge.

## The problem

[Write-ahead logging](12-write-ahead-logging.md) got the bytes onto the disk. Three problems remain, and each one has bitten a shipped vector database.

**The log is not the database.** After a crash you have a file of records and an empty process. Turning one into the other is replay, and replay has to be correct in the presence of a log whose last record was being written when the power went out.

**The log grows forever.** Recovery time is proportional to log length. At 6 KB per 1536-`D` insert, a million inserts is roughly 6 GB of log to replay every time the process starts. Without checkpoints, a database that has been running for a year cannot restart within any availability budget you would accept.

**Deleted vectors do not go away.** A delete against an [HNSW graph](07-graph-indexes-hnsw.md) cannot be executed in place without damaging graph connectivity, so every system writes a tombstone and defers. The bytes, the memory and the traversal cost all persist until a merge. This is not an implementation shortcut; it is the design, everywhere.

## How it works

### Replay

```
1. find the newest complete snapshot; note its LSN (call it L)
2. load it into memory
3. open WAL segments; skip every record with LSN <= L
4. apply records in LSN order until the log ends
5. if the last record is torn: truncate the file at its start offset
6. resume accepting writes at the next LSN
```

Two properties make this correct, and both are consequences of the log-before-data rule.

**Replay must be idempotent.** Applying a record twice must produce the same state as applying it once. In practice that means an insert is an upsert, a delete of an absent ID is a no-op, and the version attached to each record lets you skip anything already reflected in the loaded state. PostgreSQL's redo does exactly this — reapply only what is missing, driven purely by WAL content [1]. Idempotence is what makes the process crash-safe *during recovery itself*: a crash halfway through replay is just another crash, and the next attempt starts from the same snapshot and reaches the same place.

**Recovery is a pure function of what is on disk.** Given the same snapshot and the same WAL prefix, two independent recovery runs must produce byte-identical state. This falls straight out of "never trust data-file state beyond what the WAL corroborates," and it is what makes the truncate-at-every-offset test below a valid strategy rather than a curiosity. If recovery depends on anything else — wall-clock time, a random seed, iteration order over a map — it is not a function, and you cannot test it as one.

### The torn tail: truncate, do not skip

A crash during an append leaves a partial record at the end of the log. The reader detects it in one of three ways: the length prefix claims more bytes than remain in the file; the magic does not match; or the CRC over the payload fails. Any of the three means the record was never durably complete.

**Truncate the file at the start offset of that record. Do not skip it and keep reading.**

The reason is not tidiness. Skipping forward past a bad record and resuming at the next plausible boundary produces a **hole in the LSN sequence**, and gapless LSNs are the invariant that everything downstream depends on:

- A replica asks "send me everything after 4471." If your log contains 4470, 4472, 4473, either you send a sequence the replica cannot verify, or you lie about what you have.
- Truncation at the tail is safe precisely because the torn record was never acknowledged to a client — it was mid-fsync when the process died. A skipped record in the *middle* has no such guarantee. Anything after it may have been acknowledged, and now you would be replaying acknowledged writes on top of a state that is missing an earlier one.
- Replication catch-up, WAL-delta shard transfer (Qdrant ships exactly this: transferring only the WAL entries a target replica is missing, identified by clock tags [2]) and checkpoint arithmetic all assume contiguity. A hole breaks all three at once, silently.

So: **a torn tail is expected and recoverable; an LSN gap inside a valid, checksum-passing record is genuine corruption.** The second case means a record that was complete and verified has gone missing or been reordered — a filesystem bug, a bad disk, or a bug of your own. The correct response is to refuse to start and report the gap loudly. Never repair it silently, never "fill in" the missing LSN, never continue with a warning. A database that starts after detecting that its log has a hole is a database that will hand you wrong answers with total confidence.

```
valid, valid, valid, torn      -> truncate at the torn record. Normal. Log it, continue.
valid, valid, GAP, valid       -> refuse to start. Corruption. Escalate to a human.
valid, bad-CRC, valid          -> refuse to start. A complete-length record failed
                                  its checksum with valid data after it: not a tail tear.
```

### Snapshots and checkpoints

A snapshot is the materialised state at a given LSN, written so that replay only has to cover records after it. For a vector store the snapshot has to cover three structures that can drift independently:

1. **Vector data** — the arena, or the sealed segments.
2. **The ID map** — external ID to internal slot or offset. Losing this loses your ability to answer any point lookup or apply any delete, even though every vector is intact.
3. **The index structure** — HNSW adjacency lists, or IVF centroids and posting lists.

They must be captured at one consistent point. The trick that makes this easy is the same one from [Storage engines](11-storage-engines.md): **if sealed segments are immutable, a snapshot is a list of segment names plus a WAL offset, not a copy of bytes.** Nothing is copied because nothing can change.

### When a snapshot is a net loss

Snapshots are normally justified with "they make recovery faster", and that claim is wrong often enough to be worth stating precisely.

A snapshot compacts away **superseded versions**. Its benefit is therefore proportional to the log's **write amplification** — records held per *live* key — and not to the size of the dataset.

Consider an insert-only workload, one write per key. The snapshot contains exactly as many records as the log does. It saves no replay work at all, and it costs one additional file read on every startup, so recovery gets *slower*. A measurement on a two-core Linux container, 50,000 writes, brute-force index, makes the size of the effect concrete:

| Workload | Replay only | Snapshot + replay | |
|---|---:|---:|---|
| One write per key (amplification 1) | 48.4 ms | 54.6 ms | **13% slower** |
| Each key rewritten 10× (amplification 10) | 36.1 ms | 7.5 ms | **4.8× faster** |

The correct claim, then, is not "snapshots make recovery faster" but:

> A snapshot speeds recovery in proportion to how many superseded versions it can discard, and it is a net loss on a workload that has none.

Two operational consequences follow, and neither is visible from the naive version of the claim:

- **Snapshotting an append-only collection on a timer is wasted I/O.** Trigger on estimated amplification — records written since the last snapshot divided by live keys — rather than on elapsed time or raw log size.
- **A snapshot only shortens recovery if the WAL can then actually be truncated.** Truncation reclaims whole *segments*, so a single large segment means replay covers everything regardless and you have paid for the snapshot without collecting. If your segment size exceeds your dataset, snapshots are pure cost.

The second point is the one that bites in practice, because it is a configuration mistake rather than a design mistake, and it produces a system that looks correct and is simply slower than the version without the feature.

### The atomic snapshot protocol

Writing a snapshot in place is a way to lose both the old snapshot and the new one at the same time. The protocol:

```
1. write everything to snapshot.tmp
2. fsync(snapshot.tmp)                  <- contents durable
3. rename(snapshot.tmp, snapshot.0042)  <- atomic within the directory
4. fsync(the directory)                 <- the NAME is now durable
5. only now: truncate WAL segments whose highest LSN < 42
6. fsync(the directory) again after unlinking
```

Step 4 is the one everybody forgets. `rename()` is atomic with respect to concurrent readers, but the directory entry it creates is metadata, and metadata is not durable until the *directory* is fsynced. Crash between 3 and 4 and you can come back to a fully-written snapshot file that the filesystem does not list.

**Step 5 must come after step 4 is durable, without exception.** Truncating the WAL before the snapshot's name is on disk is the classic way to build a system that loses everything on one unlucky crash: the snapshot vanishes, the log that could have rebuilt it is gone, and the collection is empty. This is not hypothetical — Weaviate has open, unresolved issues reporting exactly this outcome on ordinary Kubernetes pod restarts, with logs showing "empty write-ahead-log found" across property indexes and no data returning [3], and a separate thread where a 1.26.1→1.26.4 upgrade left the schema intact and every object gone, despite logs reporting successful snapshot restoration [4]. Both were open at the time of writing, in a mature product, on routine restarts rather than exotic failures.

Pillai et al. is the reason to be paranoid about the whole protocol: crash-consistent application code routinely encodes implicit assumptions about write ordering, append atomicity and `rename()` semantics that hold on some filesystem and mount-option combinations and not on others, so identical code can be crash-safe on ext4 and silently corrupting elsewhere [5].

### Snapshotting while writes continue

Blocking writes for the duration of a snapshot is acceptable for a demo and unacceptable in production. The standard approach for a vector store:

1. Take a lock, record the current LSN as the snapshot point, seal the current mutable segment, release the lock. Milliseconds.
2. Write the sealed (now immutable) state to `.tmp` in the background. New writes go to a fresh mutable segment and a growing WAL tail, none of which the snapshot touches.
3. Rename, fsync the directory, truncate WAL below the recorded LSN.

Immutability is what makes this work: the snapshot writer is reading structures nobody can mutate. Weaviate's HNSW path now works this way — as of v1.39, snapshots are automatic, and "when Weaviate starts, it loads the most recent snapshot and replays only the commit log entries written after it," with a background compactor merging and pruning old logs after each snapshot [6]. Vespa's Proton takes the same shape from the other end: mutations are "written and synced to the transaction log via sequential I/O, then applied to the in-memory index, later flushed to disk indexes," and by default it flushes attributes and the memory index at shutdown specifically so a scheduled restart recovers quickly — which means an unclean crash, with no flush, recovers more slowly through fuller transaction-log replay [7].

## Persisting the ANN index versus rebuilding it

This is the decision with a real number attached, and the number settles it.

**Rebuilding** means the snapshot stores only vectors and the ID map; on load you reconstruct the graph. It eliminates an entire consistency surface — there is no way for a persisted index to disagree with the vectors it indexes, because it does not exist between runs. Recovery is a pure function of a smaller amount of state. The cost is startup time.

**Persisting** means serialising the graph and validating it against the vector data on load. Fast restart, one more structure to make crash-safe, one more version-consistency check to get right.

Here is the number. pgvector's own engineering benchmarks, on 1 million vectors at 1536-`D` with `m=16` and `ef_construction=200` [8]:

| Build configuration | Wall clock |
|---|---|
| Serial (pgvector 0.5.1) | **1 h 27 min** |
| Parallel, 15 workers (pgvector 0.6.0) | **9.5 min** |

And build time scales with `ef_construction`, roughly linearly [8]:

| `ef_construction` | Serial | Parallel |
|---|---|---|
| 64 | 38 m 08 s | 1 m 38 s |
| 100 | 1 h 06 m 59 s | 2 m 10 s |
| 200 | 1 h 27 m 45 s | 3 m 37 s |

Parallelism changed wall-clock time and nothing else: recall (0.963) and QPS (~950) were unchanged [8]. Crunchy Data's independent figure for the same class of workload — "For moderate datasets of > 1M rows, it can take 6 minutes to build some of the simplest of indexes," with an index for 1M rows of embeddings running "8GB or larger" [9] — is the low end of the same range.

**An index that takes an hour to build cannot be rebuilt at startup.** That is the whole argument. Above roughly a hundred thousand vectors, rebuild-on-load stops being a simplification and becomes an availability outage measured in the same units as your incident response time. Below that, rebuilding is genuinely the better engineering choice, because it deletes a class of bug rather than managing it.

Milvus persists, and structures the whole system around doing it once: a dedicated index node loads a sealed segment's binlog from object storage, builds the index, serialises it, and writes it back to object storage; query nodes then load the pre-built index file rather than building anything [10]. The build happens once per segment lifecycle, asynchronously, at seal time — not per query-node load and not per restart. Growing segments, which are still mutating and cannot be indexed, are searched with a lightweight brute-force-oriented path instead [11], so a single query may combine an approximate search over sealed segments with an exact scan over the small growing one.

**The middle path, and it is a good one:** persist the index, but treat it as a cache. Store the LSN it was built at inside the serialised file. On load, verify that LSN against the snapshot; if it does not match, or the file fails its checksum, discard it and rebuild. You get fast restarts in the normal case and correctness by construction in every abnormal one, and the persisted index never becomes something you have to trust.

## Compaction and space reclamation

Lucene's position is the clearest statement of the mechanism, and it applies unchanged to vector segments. Deleting or updating a document "simply marks a bit in a per-segment bitset"; searches "skip any deleted documents"; and "It is not until segments are merged that the bytes consumed by deleted documents are reclaimed" [12].

Until that merge, a deleted document still costs you [12]:

- **Disk**, because the bytes are still there.
- **RAM**, for any in-memory per-document structures.
- **CPU**, because every query checks the deletion bitset for every candidate.
- **Correctness of statistics**, because term and vector statistics used for ranking are skewed by entries that no longer logically exist.

Elastic observed deleted-document percentages oscillating between 35% and 60% between merges — a sawtooth, not a steady state [12].

For vector indexes the CPU cost is worse than for text, because a dead node in an HNSW graph is not merely skipped in a filter — it consumes a slot in the search's beam. Measured on pgvector: with 20% of index entries deleted, dead nodes consume roughly 20% of the traversal budget, cutting effective exploration from about 40 live candidates to about 32 per query [13]. That is a **silent recall regression** — no error, no crash, no alert, just quietly worse answers. The threshold at which teams typically force a rebuild is around 10% cumulative deletions [13]. pgvector states the underlying constraint plainly: "HNSW indexes do not reclaim space when rows are deleted: tombstoned nodes stay in the graph until the index is rebuilt," and `REINDEX CONCURRENTLY` is "prohibitively expensive for large indexes" [14].

**Delete amplification** is the accounting: one logical delete costs a tombstone write now, continued storage and traversal cost until the next merge touches that segment, and the merge I/O of rewriting the segment's survivors — plus, for vectors, rebuilding the merged graph. Structurally the same shape as LSM write amplification (RocksDB documents leveled write amplification as "often larger than 10" [15]) but triggered by deletes.

Merging vector segments is also its own hard problem, distinct from merging text indexes. The naive approach — keep the largest graph and re-insert every vector from the smaller ones through the standard insertion path — discards the proximity information the smaller graph already encoded. Lucene's newer approach computes a "join set" of roughly 20% of the smaller graph's vectors, inserts those normally, then uses their established neighbour relationships to accelerate inserting the rest, reporting 1.28–1.33× faster indexing and 1.34–1.72× faster force-merges in Lucene 10.2 [16]. That this was still being solved recently, in the most mature segment-merge machinery in the industry, is a fair signal of how deep the problem is.

The compaction knob is a real trade-off, not a tuning detail. Qdrant's own tuning guidance names both ends: "fewer, larger segments give the fastest steady-state search but the longest recovery window; more, smaller segments clear the backlog quickly at the cost of slower queries once settled" [17].

## Versioning: sequence numbers, not wall clocks

Every record needs a version so replay can be idempotent and replicas can resolve disagreement. There are two candidates and one of them is a trap.

**Monotonic sequence numbers.** Assigned by a single writer per shard, gapless, ordered by construction. Qdrant assigns version numbers via the WAL, and segments track per-point versions against them [18]. Elasticsearch pairs a per-operation sequence number with a **primary term** — an epoch counter incremented on every primary promotion, where "new terms win over older terms" — giving a total order that survives failover, plus a global checkpoint (the sequence number guaranteed present on all active copies) that turns recovery into "replay operations after the global checkpoint" rather than "compare whole segment files" [19]. That `(primary term, sequence number)` tuple is the most directly reusable durability mechanism in this entire area; see [Replication](15-replication.md) and [Consensus and Raft](19-consensus-and-raft.md).

**Wall-clock timestamps.** Dangerous, and the danger is specific. Clocks on different machines skew, jump when NTP corrects them, and can move backwards. Under last-writer-wins keyed on wall clock, a write that causally happened *later* can carry an *earlier* timestamp than a concurrent write on a skewed node, and the correct, newer data is silently discarded. No error is raised — that is the entire problem. Vespa's content cluster does exactly this: "Each document write assigns a new wall-clock timestamp... the newest available version of a document will 'win'," at whole-document granularity [20], and its own documentation frames the model as eventual consistency, "a limited subset of AP" under CAP [20]. It is a real, shipping design; it is also meaningfully weaker than Elasticsearch's epoch-fenced counter, and Vespa's docs do not discuss clock-skew mitigation.

For a single-writer-per-shard log, this is not a hard choice: **use the LSN.** It is already there, it is already gapless, and it is already the thing replication asks for.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| Snapshot interval | How much WAL replay a restart must do | Trigger on write amplification, not elapsed time | Too rare: unbounded recovery time. Too frequent: snapshot I/O competes with queries. On an append-only collection, *any* interval is wasted I/O |
| Segment size vs dataset size | Whether truncation can free anything | Segments much smaller than the data | One giant segment: replay covers everything after a snapshot, so the snapshot is pure cost |
| WAL retention after snapshot | How far back you can replay | Keep segments above the snapshot LSN | Too aggressive: a bad snapshot leaves nothing to fall back to |
| Merge trigger (deleted fraction) | When to reclaim space | Rebuild around 10% deletions for HNSW [13] | Too high: silent recall decay [13]. Too low: constant merge I/O |
| Segment size target | Steady-state latency vs recovery window | Workload-dependent [17] | Both ends documented as real costs [17] |
| Index persist vs rebuild | Restart time vs consistency surface | Persist above ~10⁵ vectors [8] | Rebuild at 1M/1536-`D`: 1 h 27 min of unavailability, serial [8] |
| `ef_construction` at build | Recall vs build time | 64–200 [8] | 64→200 roughly doubles serial build time, 38 m → 88 m [8] |
| Flush/checkpoint threshold | Bound on translog size | ES: 10 GB [21] | Larger: longer replay on restart |

## Complexity and cost

Replay is `O(records since snapshot)`, each record costing one index insert — so for an HNSW index it is `O(R · log N · efConstruction)`, not `O(R)`. Recovery time is therefore dominated by index maintenance, not by reading the log, and this surprises people who reason about it as "how fast can I read 6 GB off an NVMe drive."

Snapshot cost is `O(state size)` in bytes written; if sealed segments are immutable, most of that state is already on disk and the snapshot is a small manifest. Merge cost is `O(bytes in merged segments)` plus graph reconstruction over the survivors. Space: you need headroom for the merge output before the inputs are freed, so peak disk usage during compaction exceeds steady state — budget for it or your compaction fails exactly when the disk is fullest.

## Failure modes

- **WAL truncated before the snapshot rename is durable.** Total loss on one unlucky crash — the pattern reported against Weaviate on ordinary restarts [3][4].
- **Ack-in-log but not flushed-to-storage, lost on restart.** Milvus separates acknowledgement (durable in the log) from flush (durable in object storage) by design [11], and its issue tracker carries a recurring cluster of restart-related data-loss reports concentrated at that boundary — including #36648, "Catastrophic Data Loss (Again)," where all collections vanished after a plain machine restart, leaving an empty object-storage bucket, and which was closed without a maintainer root cause [22]. Several similar titles exist across versions [23]. **Any architecture with a gap between "acknowledged" and "materialised" must test that gap explicitly**, because it is the exact window in which crashes hurt.
- **Torn tail treated as fatal.** The database refuses to start after every crash. Worse in practice than the loss you were preventing.
- **A gap treated as a tear.** Silently truncating from an LSN hole to the end discards acknowledged writes and reports success.
- **Non-deterministic replay.** Recovery depends on map iteration order or a random seed, so the same log yields different graphs. Every downstream comparison becomes meaningless.
- **Snapshot and index out of sync.** A persisted index built at LSN 900 loaded next to a snapshot at LSN 1200. Queries return stale or absent results and nothing errors. The LSN stamp inside the index file exists to make this impossible.
- **Silent recall decay from unmerged deletions** [13]. No alarm fires. Only a recall measurement finds it, which means you must measure recall in production, not only at build time.
- **Merge starves under sustained ingest.** Deleted fraction climbs monotonically and the system degrades on a curve nobody is watching.

## How to test it

**The single highest-value test in this entire domain: take a valid WAL, truncate it at every byte offset, and assert clean recovery at each one.**

```
write a WAL containing K known records (K ≈ 50 is plenty)
for offset in 0 .. len(wal):
    copy the WAL, truncate the copy to `offset`
    recover from it
    assert: no panic, no hang, no unbounded allocation
    assert: the recovered set is exactly the records
            fully contained within `offset`
    assert: recovering the same truncated file twice
            yields identical state
```

It is about twenty lines. It catches, in one loop: reading a length prefix that runs past EOF, a header split across the truncation point, a payload cut mid-vector, an off-by-one in the boundary check, a CRC computed over the wrong byte range, a reader that panics instead of stopping, and a recovery path that is not deterministic. Those are most of the WAL bugs that exist. Run it in CI on every commit.

Then add:

- **Bit-flip fuzzing.** Flip one bit anywhere in a valid WAL; assert either clean detection at the tail, or a hard refusal to start for a mid-file corruption. Never silent acceptance.
- **Gap injection.** Hand-craft a log whose LSNs skip one. Assert the process refuses to start and names the missing LSN. If it starts, your gapless invariant is decorative.
- **Kill during snapshot.** `SIGKILL` at randomised points across the write/fsync/rename/dir-fsync sequence; after each, assert recovery yields either the old snapshot plus full WAL replay, or the new snapshot plus the shorter tail — and never anything else.
- **Recovery determinism.** Recover the same snapshot plus WAL twice in separate processes; assert the resulting state serialises identically. This is the property that makes everything else testable.
- **Delete-and-reclaim.** Insert `N`, delete 30%, assert disk usage and recall are both unchanged; force a merge; assert bytes drop and recall recovers. If recall does not measurably move, your delete path is not exercising the beam-budget effect [13] and your test is not testing what you think.
- **Restart-time regression test.** Assert recovery time stays under a fixed budget as `N` grows. This is what turns the persist-vs-rebuild decision from an opinion into a number on your own hardware.

FoundationDB's approach is the ceiling to aim at eventually: run the whole system inside a deterministic single-process simulation where "all sources of nondeterminism and communication are abstracted, including network, disk, time, and pseudo random number generator," with injected faults spanning "machine, rack, and data-center level fail-stop failures and reboots, a variety of network faults, partitions and latency problems, disk behavior (e.g. the corruption of unsynchronized writes when machines reboot)" [24]. A bug found there reproduces byte-for-byte. Their reported result: "CloudKit has deployed FDB for more than 0.5M disk years without a single data corruption event" [24]. Overkill for a demo; the right north star once failure injection becomes the interesting part of the project.

## Minimal demo version

**Snapshot plus replay is about 200 lines. The truncate-at-every-offset test is 20 lines and catches most of the WAL bugs that exist.** Build both.

```
Snapshot:  gob/protobuf-encode {vectors arena, id map, tombstones, lsn}
           -> snapshot.tmp -> fsync -> rename -> fsync(dir) -> truncate WAL

Recover:   load newest snapshot (fall back to the previous if it fails its checksum)
           replay WAL records with LSN > snapshot.lsn
           stop cleanly at the first invalid record; log the offset
           rebuild the ANN index from the loaded vectors
```

Rebuild the index on load in the demo. At demo scale it takes seconds, and it removes the whole index-persistence consistency surface. Then, deliberately, measure how long the rebuild takes at 10k, 100k and 1M vectors and extrapolate against the pgvector figures [8] — that measurement is how you *earn* the conclusion that production systems persist their indexes, rather than being told it.

**What can be cut without losing the lesson:** incremental snapshots, concurrent snapshotting (block writes for the duration; note in a comment why production cannot), multi-level compaction, index persistence, WAL segment retention policy beyond "delete below the snapshot LSN," parallel replay.

**What cannot be cut without the demo becoming a lie:**

1. **The truncating reader plus the every-offset test.** Without them you have not demonstrated crash recovery, you have demonstrated file loading.
2. **The `.tmp` → fsync → rename → fsync-directory protocol.** A snapshot written in place is a snapshot that eventually eats both copies, and the demo would be teaching the wrong protocol.
3. **Truncating the WAL only after the rename is durable.** Get this backwards and the demo contains the exact bug that produces total data loss in real products [3][4].
4. **Tombstones that survive recovery.** If a restart resurrects deleted vectors, the delete path is decorative.
5. **Refusing to start on a mid-file LSN gap.** A demo that recovers from every corrupt file is not demonstrating recovery, it is demonstrating optimism.

**For one extra hour:** write the chaos loop — insert continuously, `SIGKILL` at a random moment, restart, verify every acknowledged write survived and no deleted vector came back, repeat a thousand times. It is fifty lines of shell and it is the closest a small project gets to what [22] and [3] would have caught.

## Open questions and contested claims

- **Persist the index or rebuild it.** The three production answers genuinely differ. Milvus persists, building once per segment at seal time and having query nodes load the artefact [10]. Vespa's vectors ride the ordinary attribute flush and transaction-log replay path, and the public documentation does not make clear whether the HNSW graph structure itself is persisted directly or reconstructed — a real ambiguity, not a settled fact. pgvector rebuilds on `REINDEX` and is explicit that this is "prohibitively expensive for large indexes" [14]. The decision is driven entirely by build time [8][9], and build time is driven by `ef_construction` and parallelism, so the answer moves as hardware and build algorithms improve.
- **Whether ack-before-materialised is acceptable.** Milvus's architecture deliberately separates acknowledged-in-log from flushed-to-object-storage [11], which is defensible and is also where its recurring restart data-loss reports cluster [22][23]. The counter-position is that any window between those two points is a window a crash can land in, and it should be closed rather than documented.
- **Whether wall-clock last-writer-wins is ever the right call.** Vespa ships it [20]; Elasticsearch built epoch-fenced sequence numbers specifically because it was not sufficient for safe recovery after primary failure [19]. Vespa's answer is that convergence via document-timestamp comparison during bucket sync is good enough for its availability-first model [20]. Both systems are in production. The disagreement is real, and the evidence favours the fenced counter for anything where a lost write is worse than a slow one.

## Sources

[1] PostgreSQL Global Development Group — "Write-Ahead Logging (WAL)" — https://www.postgresql.org/docs/current/wal-intro.html — accessed 2026
[2] qdrant/qdrant issue #3477 — "Tracking issue: shard diff transfer - WAL delta" — https://github.com/qdrant/qdrant/issues/3477 — accessed 2026
[3] weaviate/weaviate issue #7162 — "Data loss in 1.26.4 version whenever pod restarted" — https://github.com/weaviate/weaviate/issues/7162 — 2025
[4] weaviate/weaviate issue #5869 — "after upgrade 1.26.1 to 1.26.4, all data lost except schema" — https://github.com/weaviate/weaviate/issues/5869 — accessed 2026
[5] Pillai, T. S., Chidambaram, V., Alagappan, R., Al-Kiswany, S., Arpaci-Dusseau, A. C., & Arpaci-Dusseau, R. H. — "All File Systems Are Not Created Equal: On the Complexity of Crafting Crash-Consistent Applications" — OSDI 2014 — https://www.usenix.org/conference/osdi14/technical-sessions/presentation/pillai — 2014
[6] Weaviate — "Storage" — https://docs.weaviate.io/weaviate/concepts/storage — accessed 2026
[7] Vespa.ai — "Proton" — https://docs.vespa.ai/en/proton.html — accessed 2026
[8] Supabase Engineering — "pgvector 0.6.0: 30x faster with parallel index builds" — https://supabase.com/blog/pgvector-fast-builds — accessed 2026
[9] Crunchy Data — "HNSW Indexes with Postgres and pgvector" — https://www.crunchydata.com/blog/hnsw-indexes-with-postgres-and-pgvector — accessed 2026
[10] Milvus — "Data Processing" — https://milvus.io/docs/data_processing.md — accessed 2026
[11] Milvus/Zilliz — "Data Insertion and Data Persistence in a Vector Database" (Deep Dive 4) — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md — accessed 2026
[12] McCandless, M. (Elastic) — "Lucene's Handling of Deleted Documents" — https://www.elastic.co/blog/lucenes-handling-of-deleted-documents — accessed 2026
[13] Perun Engineering — "pgvector in production: five failure patterns" — https://perun.au/insights/pgvector-production/ — accessed 2026
[14] ParadeDB — "pgvector Limitations" — https://www.paradedb.com/learn/postgresql/pgvector-limitations — accessed 2026
[15] RocksDB team (Meta) — "Leveled Compaction" (RocksDB wiki) — https://github.com/facebook/rocksdb/wiki/Leveled-Compaction — accessed 2026
[16] Elasticsearch Labs — "HNSW graphs: Speeding up merging" — https://www.elastic.co/search-labs/blog/hnsw-graphs-speed-up-merging — accessed 2026
[17] Qdrant — "Configure Qdrant's Optimizer for Predictable Search Latency" — https://qdrant.tech/articles/tuning-qdrant-optimizer/ — accessed 2026
[18] Qdrant — "Storage" — https://qdrant.tech/documentation/concepts/storage/ — accessed 2026
[19] Elastic — "Sequence IDs: Coming Soon to an Elasticsearch Cluster Near You" — https://www.elastic.co/blog/elasticsearch-sequence-ids-6-0 — accessed 2026
[20] Vespa.ai — "Vespa Consistency Model" — https://docs.vespa.ai/en/content/consistency.html — accessed 2026
[21] Elastic — "Translog" (Elasticsearch Guide 8.19) — https://www.elastic.co/guide/en/elasticsearch/reference/8.19/index-modules-translog.html — accessed 2026
[22] milvus-io/milvus issue #36648 — "Catastrophic Data Loss (Again)" — https://github.com/milvus-io/milvus/issues/36648 — accessed 2026
[23] milvus-io/milvus issues (restart/data-loss pattern: #38284, #40418, #25878, #30254, #21527) — https://github.com/milvus-io/milvus/issues — accessed 2026
[24] Zhou, J. et al. — "FoundationDB: A Distributed Unbundled Transactional Key-Value Store" — SIGMOD 2021 — https://www.foundationdb.org/files/fdb-paper.pdf — 2021
