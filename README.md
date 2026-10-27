# vector-db

A distributed vector database built from scratch in Go, for learning: WAL and crash recovery,
hash sharding, synchronous replication with epoch fencing, scatter-gather top-K search, failure
detection, shard rebalancing and a Raft-backed control plane, each with tests that try to break it.

| Path | What it is |
|---|---|
| [`vecdb/`](vecdb/) | The Go module: code, tests, demos. Start with [`vecdb/README.md`](vecdb/README.md). |
| [`vecdb/BENCHMARKS.md`](vecdb/BENCHMARKS.md) | Measured numbers and their caveats. |
| [`vecdb/LESSONS.md`](vecdb/LESSONS.md) | Every bug, and every measurement that contradicted the design. |
| [`concepts/`](concepts/) | 23 reference chapters on the search, durability and distributed sides. |
| [`docs/`](docs/) | Design history: system design, master prompt, final plan, current status. |
| [`skills/`](skills/) | The build-to-understand skill used to write all of the above. |

```
cd vecdb
go test -race ./...            # everything, including the crash-recovery suite
go run ./cmd/vecdemo           # the whole system as a scripted story
make crash | replicate | rebalance | splitbrain
```

Requires Go 1.24+.
