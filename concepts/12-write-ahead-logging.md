# 12. Write-ahead logging and durability

> Write the intent to a sequential log and fsync it before touching anything else; the client's acknowledgement is a promise made at the moment of that fsync, and every data-loss bug in this domain is a system that made the promise earlier.

## The problem

A client sends an insert. You put it in a map. The map is in memory. The power goes out. The insert is gone, and you already told the client it succeeded. You lied — not maliciously, but the client's downstream state now disagrees with yours permanently, and no amount of retry logic on their side fixes it because their retry loop exited on your `200 OK`.

The naive fix is to write the data structure to disk on every write. That works and nobody does it, because rewriting an index — or an HNSW graph — per insert costs orders of magnitude more I/O than the insert is worth. The write-ahead log is the standard escape: instead of persisting the *structure*, persist the *intent*, sequentially, in a form small enough to be cheap and complete enough to reconstruct the structure from.

PostgreSQL states the rule in one sentence: "changes to data files (where tables and indexes reside) must be written only after those changes have been logged, that is, after WAL records describing the changes have been flushed to permanent storage" [1]. This is the write-ahead rule that ARIES formalised, and everything else in this file is a consequence of it.

The consequence that matters most: **a write is durable at fsync-of-log time, not at data-write time.** Data pages can lag arbitrarily far behind, because "any changes that have not been applied to the data pages can be redone from the WAL records" [1]. The log is the truth; everything else is a materialised view of the log that happens to be faster to query.

There is also a pure performance argument, independent of correctness. "The WAL file is written sequentially, and so the cost of syncing the WAL is much less than the cost of flushing the data pages," and with many concurrent small transactions "one fsync of the WAL file may suffice to commit many transactions" [1]. Sequential beats random, and one fsync can serve many clients.

## How it works

### Record format

A WAL is a file of self-delimiting, self-verifying records. The minimum viable layout:

```
┌────────┬────────┬────────┬────────┬──────┬──────────────────────┐
│ magic  │ length │ crc32c │  lsn   │ op   │       payload        │
│  4 B   │  4 B   │  4 B   │  8 B   │ 1 B  │      length bytes    │
└────────┴────────┴────────┴────────┴──────┴──────────────────────┘
```

Every field earns its place.

**Length prefix.** Without it a reader cannot find the next record boundary, and cannot distinguish "the file ends here" from "the file is 40 bytes into a record that was never finished." Length first means the reader can bounds-check before allocating: if `length` exceeds the bytes remaining in the file, the record is torn — stop.

**Checksum.** A length prefix alone cannot detect a record whose bytes are present but wrong — a half-written sector, a bit flip, a length field that itself got corrupted into something plausible. Checksum the LSN, op and payload, and verify before applying. **CRC32-Castagnoli (CRC32C) rather than CRC32-IEEE**, for one reason: x86-64 since SSE4.2 and ARM64 both have a hardware instruction for the Castagnoli polynomial, and a table-driven software CRC is roughly an order of magnitude slower. On a per-write hot path with 6 KB vector payloads, that is not a micro-optimisation, it is the difference between the checksum being free and the checksum being the second-largest cost in the write path. The two polynomials have comparable error-detection properties; there is no reason to choose IEEE.

**LSN.** A monotonically increasing sequence number, assigned by the single writer, gapless. It is what makes replay idempotent (skip anything already applied), what replication uses to say "send me everything after 4471," and what recovery uses to detect a real hole. Qdrant does exactly this: writes get "sequential number[s]" in the WAL before anything touches a segment [2].

**Operation type.** `INSERT`, `DELETE`, `UPDATE`, and — worth adding early — a `CHECKPOINT` marker recording that a snapshot up to some LSN is durable.

**Magic.** A constant at the record head. Cheap, and it catches the class of bug where you resume reading at a wrong offset: without it, garbage can parse as a plausible length.

### Segments and rotation

One infinite file is wrong for three reasons: you cannot delete a prefix of a file, recovery must scan everything ever written, and a filesystem-level corruption takes out the whole history. Roll the log at a fixed size — 64 MB or 128 MB — into numbered files (`000001.wal`, `000002.wal`). After a snapshot at LSN `L`, every segment whose highest LSN is below `L` can be deleted. Elasticsearch bounds the same thing with `index.translog.flush_threshold_size`, defaulting to **10 GB**, which is the point at which it forces a Lucene commit specifically to bound translog size and therefore recovery replay time [3].

### The write path, in exact order

```
1. serialise the record, assign LSN                     (in memory)
2. write() to the current segment                       (page cache — NOT durable)
3. fsync() / fdatasync() the segment                    (durable — the promise point)
4. apply to the in-memory map / arena / index
5. return success to the client
```

Step 3 before step 5 is the whole discipline. Steps 4 and 5 may be reordered; steps 3 and 5 may not.

## The hard facts about fsync

This is where most from-scratch implementations are wrong, and where several shipped systems have been wrong.

**`write()` does not persist.** A successful `write()` return means the bytes are in the OS page cache. Nothing more. In a language with a buffered writer there are now *two* layers of lying: flushing the buffered writer moves bytes from your process into the kernel, which is not durability either. `Flush()` is not `Sync()`. Grep your own code for a buffered writer wrapping the WAL file and confirm the sync goes to the underlying descriptor.

**Creating a segment file is not durable until the parent directory is fsynced.** The file's *contents* are made durable by fsyncing the file; the *directory entry that says the file exists* is metadata in the parent directory, and it is durable only after fsyncing the directory itself. Skip this and a crash can leave you with a fully-fsynced segment that the filesystem does not list — data written, acknowledged, and unreachable. The same rule governs `rename()`, which is why the snapshot protocol in [Recovery](13-recovery-and-compaction.md) fsyncs the directory after the rename.

**A failed fsync is not retryable on Linux.** In 2018 Craig Ringer discovered that when `fsync()` fails with `EIO`, the kernel reports the error once and then clears the error flag on the file descriptor; a subsequent `fsync()` on the same fd reports success even though the write never landed. Mechanically the kernel marks the dirty page clean after the failed writeback attempt rather than keeping it dirty for retry, leaving it "neither written to disk successfully nor marked dirty" [4]. The consequence, as stated in the thread that uncovered it: "The write never made it to disk, but we completed the checkpoint, and merrily carried on our way. Whoops, data loss." [4]

This is "fsyncgate." It was reported on the PostgreSQL hackers list and covered by LWN [5]. It was worst on XFS; FreeBSD and Illumos reportedly keep pages dirty after an I/O error and are safer; the `errseq_t` work in Linux 4.13+ improved error *reporting* without changing the underlying page-marked-clean behaviour [4]. Rebello et al. took the general question to USENIX ATC 2020 — "Can Applications Recover from fsync Failures?" — and the broad answer is that most applications are simply not written to treat fsync failure as a first-class case [6].

The operational rule that falls out is blunt and it is what PostgreSQL adopted: **if fsync fails, do not retry it and do not continue. Crash the process.** On restart, recovery replays from the log, which is the only path whose correctness you can actually reason about. An error return you handle gracefully here is worse than a panic, because it hides unrecoverable state.

**Torn and partial writes are real.** A page is 8 KB; sector-level atomicity on the device is 512 B to 4 KB. A crash mid-write can therefore leave a page as a mixture of old and new sectors — a torn page [7]. This matters because WAL records are logical deltas ("delete tuple 7") that assume they are being replayed onto a *coherent* prior page image; replaying a delta onto a torn page compounds the corruption, since "the structures recovery needs to consult are a mixture of two different versions of the page" [7].

PostgreSQL's canonical mitigation is **full-page writes**: on the first modification of a page after each checkpoint, the entire page image goes into the WAL, so recovery can replace any torn page wholesale with a known-good image before replaying logical records on top [7]. The documented cost is a sawtooth in WAL volume — a burst of full-page images immediately after each checkpoint, decaying as the working set gets touched once — mitigated by lengthening checkpoint intervals or compressing WAL [7].

The design lesson generalises past Postgres: **append-only structures sidestep torn pages entirely.** If you never modify a byte that has already been written, there is no coherent-prior-image to tear. This is one more argument for append-only vector segments over in-place mutation, and it is why a WAL that only ever appends needs a per-record checksum rather than full-page writes.

**Drives lie about flush completion.** PostgreSQL's own reliability documentation is unusually blunt: "When the operating system sends a write request to the storage hardware, there is little it can do to make sure the data has arrived at a truly non-volatile storage area" [8]. Disk and controller write-back caches are volatile and "will lose [their] contents in a power failure," and "many [SSDs] do not honor cache flush commands by default" [8]. The documented mitigations are disabling volatile write-back caching, using battery-backed controller caches, and empirically testing the I/O stack [8].

### Group commit

One fsync per client write caps your write throughput at `1 / fsync_latency`, regardless of how many cores you have. Group commit breaks that ceiling: let commit records from concurrent writers accumulate in the WAL buffer and issue a single fsync covering all of them. PostgreSQL's wiki states the mechanism directly — "a group commit feature enables PostgreSQL to commit a group of transactions in batch, amortizing the cost of flushing WAL" [9] — and the theoretical basis is in the WAL documentation itself: "one fsync of the WAL file may suffice to commit many transactions" [1].

The cost is latency for the first writer in a batch, who waits for the batch to fill or a timer to expire. The gain is that fsync stops being a per-request tax and becomes a per-batch one. This is the single highest-leverage optimisation in a WAL, and it is about twenty lines: a mutex, a condition variable, and a "did someone else already fsync past my LSN?" check.

### The numbers that make this concrete

Measured with `pg_test_fsync` on real hardware [10]:

| Device | fdatasync, 8 kB | fsync, 8 kB |
|---|---|---|
| Consumer NVMe (Samsung 990 Pro, no power-loss protection) | **1643 µs (~1.6 ms)** | 5636 µs |
| Enterprise NVMe (Micron 7400, power-loss protection) | **24 µs** | 25 µs |

The source's own observation on the consumer drive: "even the single 8kB physical write done by fdatasync still takes 1.6 milliseconds," attributed to the drive's lack of a power-loss-protected write cache, which forces an actual flush to media rather than a cache acknowledgement [10].

**Read that gap correctly: it is roughly 65–85×, and it is about power-loss protection, not about NVMe.** Both drives are NVMe. The enterprise drive has a capacitor-backed cache, so it can acknowledge a flush the instant data reaches its own volatile cache, because that cache will survive a power cut. The consumer drive has to push to flash. "We're on NVMe so fsync is fast" is a false inference; the question is always whether the cache is power-loss protected. Cloud block storage adds a network round trip on top of whatever the media does, which is why cloud databases lean even harder on group commit.

At 1.6 ms per fsync, a naive one-fsync-per-write design tops out near 600 writes/sec no matter what else you do. Group commit is how you get past that.

## Parameters and knobs

| Parameter | What it controls | Typical value | What happens if you get it wrong |
|---|---|---|---|
| Sync policy | fsync per write vs batched vs interval | per-request for safety | Interval-based silently converts crashes into data loss (see Jepsen, below) |
| Group-commit window | Max wait before flushing a partial batch | 0.5–5 ms | Too long: latency floor rises for everyone. Too short: no batching benefit |
| Segment size | WAL file rotation point | 16–128 MB | Too small: file churn and directory fsyncs. Too large: slow recovery, coarse truncation |
| Flush/checkpoint threshold | When to snapshot and truncate the log | ES: 10 GB translog [3] | Too high: recovery replays for a long time. Too low: constant snapshot I/O |
| Checksum algorithm | Corruption detection on replay | CRC32C | IEEE polynomial: same detection, no hardware instruction, ~10× slower |
| `index.translog.durability` (ES) | fsync per request vs every `sync_interval` | `request` (default); `async` syncs every 5 s [3] | `async`: up to ~5 s of acknowledged writes lost on crash [3] |

## How real systems do it

**Elasticsearch** exposes the durability decision as one knob, and it is the clearest teaching example in production. The translog is its WAL: "All index and delete operations are written to the translog after being processed by the internal Lucene index but before they are acknowledged" [3]. `index.translog.durability` has exactly two settings [3]:

- **`request`** (the default): fsync and commit after every request — "In the event of hardware failure, all acknowledged writes will already have been committed to disk."
- **`async`**: fsync every `index.translog.sync_interval`, **default 5 seconds**, in the background. Up to five seconds of acknowledged writes can vanish on a crash.

Elasticsearch also draws a distinction that a from-scratch implementation should copy before it copies anything else: **refresh is not flush** [3]. *Refresh* opens a new searchable Lucene segment from the in-memory buffer, making recent documents visible to search; it runs on a timer (commonly ~1 s) and implies nothing about durability. *Flush* is a Lucene commit — fsync of segment files plus starting a new translog generation — and it is what bounds durability and recovery time. Visibility and durability are two independent state transitions with two independent guarantees, and conflating them is the most common Elasticsearch misconception [3]. Your own design has the same two transitions whether or not you name them.

**Qdrant** runs a per-shard WAL that "orders all operations and assigns them a sequential number," with the durability boundary stated as: "once a change has been added to the WAL, it will not be lost even if a power loss occurs" [2]. Segment and index updates are decoupled and allowed to lag. This is the design a from-scratch build should copy: **WAL fsync gates the client ack; index update is asynchronous.**

**Weaviate** carries two logs for two stores: a WAL in front of its own LSM key-value store, where "if an unexpected crash happens and Weaviate encounters an 'incomplete' WAL, it will recover from it," and a separate append-only commit log for the HNSW index per shard [11]. Two structures with independent durability requirements, two logs — a reasonable answer, and one that creates a new problem, namely keeping the two consistent with each other.

**Milvus has no local WAL at all, by design.** Durability is externalised to a message queue (Pulsar, Kafka, or its own Woodpecker), which is the system of record; compute nodes are stateless subscribers [12]. The architecture is described as "log as data," structured "as a group of log publish/subscribe microservices" [13]. The instructive part is what this buys and costs: it removes local-disk durability entirely, and it separates *acknowledged* (in the log) from *flushed* (in object storage) into two distinct points in time [12] — a gap that has consequences examined in [Recovery](13-recovery-and-compaction.md).

### The canonical demonstration: Jepsen on Elasticsearch 1.5.0

Kyle Kingsbury's 2015 Jepsen analysis of Elasticsearch 1.5.0 is the single most useful artefact in this whole area, because it converts "ack before fsync is bad" from an opinion into measurements [14]:

- Intersecting network partitions, with one node visible to both halves: two live primaries for about 90 seconds, losing 20+ acknowledged documents per run.
- A clean network split: **~25% of acknowledged documents lost** during the ~90-second election window.
- Isolating just the primary: **~25% of acknowledged writes vanished**.
- Simulated GC pauses on the primary: **9.3% loss**, worse under repeated pauses.
- **A plain `kill -9` with no partition at all: ~10% of acknowledged writes lost** — because writes were acknowledged to the client before the 5-second-interval fsync had happened.

That last line is the one to internalise. **No exotic failure. No network partition. No split brain. Just a process crash — and one write in ten that a client was told had succeeded was gone**, purely because the acknowledgement preceded the fsync. GitHub issue #10933 independently corroborates the ~10% figure on the same version and explicitly contrasts Elasticsearch's behaviour with the Postgres/MySQL convention of fsync-before-ack [15].

Ack-before-fsync is not a performance tuning choice with a durability cost you can price. It is a data-loss bug that shows up as a percentage of your traffic under ordinary operational events. If you want the throughput, the answer is group commit — batch the fsyncs, ack after the batch — not moving the ack.

**One honest caveat: the public record disagrees about when the Elasticsearch default changed.** Issue #462 against the official Definitive Guide reports the reverse of the modern documentation — the guide said the translog was fsynced every 5 seconds while the code default was `request` — and it was closed unresolved when the guide repository was archived in 2021 [16]. Today's documentation is unambiguous that `request` is the default [3], and the Jepsen-era reports describe async-like behaviour [14][15], but the exact version in which the default changed was not something the public sources pin down. Treat "modern Elasticsearch defaults to fsync-per-request" as documented and current, and treat the historical timeline as genuinely uncertain.

## Complexity and cost

Per write: one serialisation, one CRC over the payload, one `write()`, and a share of one fsync. Disk cost is `sizeof(record)` per operation, dominated by the vector payload — `D × 4` bytes at `float32`, so 6 KB per 1536-`D` insert. A million inserts is roughly 6 GB of WAL before truncation, which is why segment rotation and snapshot-driven truncation are not optional at scale.

Latency is the fsync, and the fsync is the device: ~24 µs with power-loss protection, ~1.6 ms without [10]. Throughput without group commit is `1 / fsync_latency`; with group commit it is bounded by sequential write bandwidth instead. That is the whole cost model.

## Failure modes

- **Ack before fsync.** Measured at ~10% loss on a plain crash [14]. The default failure of this domain.
- **Buffered writer never synced.** Everything looks correct in tests, because tests close the file cleanly.
- **Missing directory fsync after creating a segment.** Loses a whole segment on crash, rarely, in a way that reproduces roughly never in development.
- **Retrying a failed fsync.** Returns success on data that never landed [4]. Crash instead.
- **Torn trailing record treated as corruption.** Refusing to start after every crash is the mirror-image bug of replaying garbage; see [Recovery](13-recovery-and-compaction.md).
- **Unbounded log.** No snapshotting means recovery time grows without limit; a restart eventually exceeds every availability budget you have.
- **LSN assigned outside the lock that orders appends.** Two writers get LSNs in one order and append in another; the log is now unordered and replication desynchronises silently.

## How to test it

- **Crash consistency, at every offset.** Write a valid WAL, truncate it at every byte offset from 0 to `len`, and assert recovery succeeds cleanly at each. This is the highest-value test in the entire domain and it gets its own treatment in [Recovery](13-recovery-and-compaction.md).
- **Bit-flip fuzzing.** Flip one bit at a random offset in a record; assert the checksum rejects it and recovery reports corruption rather than applying a mangled record.
- **Real kill.** `SIGKILL` under continuous load, restart, and assert every write that received a success response is present. Not `SIGTERM` — `SIGTERM` runs your shutdown path, which is exactly the code path a crash does not run.
- **Ack-ordering assertion.** Instrument the write path so that returning to the client without a completed sync is a test failure. This turns the Jepsen finding into a compile-time-ish guarantee.
- **LSN gaplessness invariant.** After any run, assert the LSNs read back from disk form a contiguous ascending sequence with no duplicates.
- **fsync counter.** Count syncs and writes under a concurrent load; if group commit is working, the ratio is well below 1. If it is exactly 1, your batching does nothing.

## Minimal demo version

**A real WAL is about 300 lines, and it is the highest-value thing to build in the whole project.** Not the index — the WAL. It is where the discipline lives, it is where the interesting failure modes live, and unlike an ANN index there is no library that hides the lesson from you.

The shape:

```go
type Record struct {
    LSN     uint64
    Op      uint8      // insert | delete | checkpoint
    Payload []byte     // id + vector + metadata
}

// Append: serialise, crc32c, write, fsync, return LSN.
// Reader: read header, validate length against bytes remaining,
//         read payload, verify crc, yield record.
//         On any failure at the tail: stop cleanly, report truncation offset.
```

**What can be cut without losing the lesson:** segment rotation (one file is fine for a demo dataset); group commit (build it second, and measure the before/after — that measurement *is* the lesson); WAL compression; full-page writes (append-only makes them unnecessary); multiple ops per record; a separate index log.

**What cannot be cut without the demo becoming a lie:**

1. **The checksum.** Without it you cannot distinguish a torn record from a valid one, and your recovery path is guessing. Every interesting failure in this domain is a corruption-detection failure.
2. **The length prefix.** Without it the reader cannot find record boundaries and cannot bounds-check before allocating; a corrupt length field becomes a multi-gigabyte allocation or a panic.
3. **The truncating reader.** Recovery must stop cleanly at the first invalid record and report where. A reader that panics on a torn tail means your database cannot start after any crash — which is a worse outcome than the data loss you were trying to prevent.
4. **The fsync before the ack.** Cut this and the demo demonstrates the opposite of the thing it claims to demonstrate.

**For one extra hour:** add group commit and publish two numbers — writes/sec with one fsync per write, and writes/sec with batching, on the same hardware. Then run `pg_test_fsync` on that machine and see which of the two drives in the table above yours behaves like. Three numbers, and you understand durability cost better than most people who use databases professionally.

## Open questions and contested claims

- **When Elasticsearch's translog default changed.** The current documentation says `request` [3]; the archived Definitive Guide issue says the guide claimed 5-second async while the code did `request` [16]; the Jepsen and issue-tracker reports from the 1.x era describe behaviour consistent with async [14][15]. The sources are mutually consistent with a change having occurred, but none of them names the version. Do not assert a specific version.
- **Whether a WAL is required for durability at all.** It is not: "A database does not need a write-ahead log (WAL) to achieve durability. A database can write its long-term data structure durably to disk before returning to a client" [17]. Nobody does, because "it's cheaper to write only these small messages representing the client request to disk" [17]. The vector-specific twist is that the ANN index is reconstructible from raw vectors, so a system can log only the vectors — trading recovery time for a smaller log. Whether that trade is sane depends on index build time, covered in [Recovery](13-recovery-and-compaction.md).
- **How much application code can safely assume about the filesystem.** Pillai et al. showed that crash-consistent application code routinely encodes implicit assumptions about write ordering, append atomicity and `rename()` semantics that hold on some filesystem/mount-option combinations and not others — so the same code can be crash-safe on one filesystem and silently corrupt data on another [18]. The safe posture is to assume nothing beyond "fsync of the file, then fsync of the parent directory," which is the protocol used in [Recovery](13-recovery-and-compaction.md).

## Sources

[1] PostgreSQL Global Development Group — "Write-Ahead Logging (WAL)" — https://www.postgresql.org/docs/current/wal-intro.html — accessed 2026
[2] Qdrant — "Storage" — https://qdrant.tech/documentation/concepts/storage/ — accessed 2026
[3] Elastic — "Translog" (Elasticsearch Guide 8.19) — https://www.elastic.co/guide/en/elasticsearch/reference/8.19/index-modules-translog.html — accessed 2026
[4] Luu, D. — "Fsyncgate: errors on fsync are unrecoverable" (writeup of the 2018 Craig Ringer / PostgreSQL discovery) — https://danluu.com/fsyncgate/ — 2018
[5] LWN.net, reporting on the PostgreSQL hackers thread — "PostgreSQL's handling of fsync() errors is unsafe and risks data loss at least on XFS" — https://lwn.net/Articles/752063/ — 2018
[6] Rebello, A., Patel, Y., Alagappan, R., Arpaci-Dusseau, A. C., & Arpaci-Dusseau, R. H. — "Can Applications Recover from fsync Failures?" — USENIX ATC 2020 — https://www.usenix.org/conference/atc20/presentation/rebello — 2020
[7] Pettus, C. — "All Your GUCs in a Row: full_page_writes" — https://thebuild.com/blog/all-your-gucs-in-a-row-full_page_writes/ — accessed 2026
[8] PostgreSQL Global Development Group — "Reliability" (disk write caches) — https://www.postgresql.org/docs/9.1/wal-reliability.html — accessed 2026
[9] PostgreSQL Wiki — "Group commit" — https://wiki.postgresql.org/wiki/Group_commit — accessed 2026
[10] Poder, T. — "Using Postgres pg_test_fsync tool for testing low latency writes" — https://tanelpoder.com/posts/using-pg-test-fsync-for-testing-low-latency-writes/ — accessed 2026
[11] Weaviate — "Storage" — https://docs.weaviate.io/weaviate/concepts/storage — accessed 2026
[12] Milvus/Zilliz — "Data Insertion and Data Persistence in a Vector Database" (Deep Dive 4) — https://milvus.io/blog/deep-dive-4-data-insertion-and-data-persistence.md — accessed 2026
[13] Guo, R. et al. (Zilliz) — "Manu: A Cloud Native Vector Database Management System" — VLDB 2022 — https://arxiv.org/pdf/2206.13843 — 2022
[14] Kingsbury, K. — "Jepsen: Elasticsearch 1.5.0" — https://aphyr.com/posts/323-jepsen-elasticsearch-1-5-0 — 2015
[15] elastic/elasticsearch issue #10933 — "Node crashes can cause data loss" — https://github.com/elastic/elasticsearch/issues/10933 — 2015
[16] elastic/elasticsearch-definitive-guide issue #462 — "Default translog durability is 'request', but page about translog means 'async'" — https://github.com/elastic/elasticsearch-definitive-guide/issues/462 — accessed 2026
[17] Eaton, P. — "A write-ahead log is not a universal part of durability" — https://notes.eatonphil.com/2024-07-01-a-write-ahead-log-is-not-a-universal-part-of-durability.html — 2024
[18] Pillai, T. S., Chidambaram, V., Alagappan, R., Al-Kiswany, S., Arpaci-Dusseau, A. C., & Arpaci-Dusseau, R. H. — "All File Systems Are Not Created Equal: On the Complexity of Crafting Crash-Consistent Applications" — OSDI 2014 — https://www.usenix.org/conference/osdi14/technical-sessions/presentation/pillai — 2014
