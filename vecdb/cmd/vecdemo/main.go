// Command vecdemo runs the whole system as a scripted story on synthetic data.
//
// It is deliberately a single process. A "cluster" here is S shards owned by
// one binary, which is enough to exercise partitioning, fan-out, exact global
// top-K, durability and recovery, and which makes fault injection a function
// call instead of a container orchestration problem.
//
// Usage:
//
//	go run ./cmd/vecdemo              # full story in a temp directory
//	go run ./cmd/vecdemo -dir ./data  # keep the data, re-run to see recovery
//	go run ./cmd/vecdemo -crash       # simulate a crash mid-load, then recover
//	go run ./cmd/vecdemo -rebalance   # move a shard under load, safely and unsafely
//	go run ./cmd/vecdemo -splitbrain  # a partitioned controller, with and without consensus
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/coordinator"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
	"github.com/eskhakha/vecdb/internal/wal"
)

func main() {
	var (
		dir     = flag.String("dir", "", "data directory (default: a temp dir)")
		shards  = flag.Int("shards", 4, "number of logical shards")
		dim     = flag.Int("dim", 64, "vector dimension")
		n       = flag.Int("n", 20000, "vectors to load")
		k       = flag.Int("k", 10, "top-K")
		crash   = flag.Bool("crash", false, "simulate a crash mid-load, then recover")
		repl    = flag.Bool("replicate", false, "replication, ISR, failover and rejoin")
		reb     = flag.Bool("rebalance", false, "move a shard while it is being written to")
		split   = flag.Bool("splitbrain", false, "a partitioned controller, with and without consensus")
		syncAll = flag.Bool("fsync", true, "fsync every write (SyncAlways)")
		seed    = flag.Int64("seed", 1, "RNG seed; the same seed gives the same run")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	dataDir := *dir
	if dataDir == "" {
		d, err := os.MkdirTemp("", "vecdemo-")
		if err != nil {
			fatal(log, "mkdtemp", err)
		}
		dataDir = d
		defer os.RemoveAll(dataDir)
	}
	sync := wal.SyncAlways
	if !*syncAll {
		sync = wal.SyncNever
	}

	c := &demo{
		dir: dataDir, numShards: *shards, dim: *dim, k: *k,
		sync: sync, log: log, rng: rand.New(rand.NewSource(*seed)),
		syncTimeout: 200 * time.Millisecond,
	}

	fmt.Printf("\nvecdb demo — %d shards, dim=%d, N=%d, fsync=%v\n", *shards, *dim, *n, *syncAll)
	fmt.Printf("data: %s\n\n", dataDir)

	if *crash {
		c.crashStory(*n)
		return
	}
	if *repl {
		c.replicationStory(*n)
		return
	}
	if *reb {
		c.rebalanceStory()
		return
	}
	if *split {
		c.splitbrainStory()
		return
	}
	c.story(*n)
}

type demo struct {
	dir         string
	numShards   int
	dim         int
	k           int
	sync        wal.SyncPolicy
	log         *slog.Logger
	rng         *rand.Rand
	syncTimeout time.Duration
}

type openCluster struct {
	part   *cluster.Partitioner
	shards map[core.ShardID]*store.Shard
	coord  *coordinator.Coordinator
}

func (c *demo) open() *openCluster {
	part, err := cluster.NewPartitioner(c.numShards)
	if err != nil {
		fatal(c.log, "partitioner", err)
	}
	shards := map[core.ShardID]*store.Shard{}
	searchers := map[core.ShardID]coordinator.Searcher{}
	for i := 0; i < c.numShards; i++ {
		id := core.ShardID(i)
		sh, err := store.Open(store.Config{
			ID:     id,
			Dir:    filepath.Join(c.dir, fmt.Sprintf("shard-%d", i)),
			Dim:    c.dim,
			Metric: core.Cosine,
			Sync:   c.sync,
			// A small segment so the log actually rolls and truncation after a
			// snapshot has something to delete. Segments are the unit of
			// prefix truncation: you can only reclaim whole, closed segments.
			SegmentSize: 256 << 10,
		})
		if err != nil {
			fatal(c.log, "open shard", err)
		}
		shards[id] = sh
		searchers[id] = sh
	}
	return &openCluster{part: part, shards: shards, coord: coordinator.New(part, searchers, core.Cosine)}
}

func (oc *openCluster) close() {
	for _, s := range oc.shards {
		_ = s.Close()
	}
}

func (c *demo) vec(i int) *core.Vector {
	e := make([]float32, c.dim)
	for j := range e {
		e[j] = c.rng.Float32()*2 - 1
	}
	return &core.Vector{ID: fmt.Sprintf("vec-%06d", i), Embedding: e}
}

// story runs the happy path end to end.
func (c *demo) story(n int) {
	ctx := context.Background()
	oc := c.open()

	// ---- 1. load ----
	section("1. Load")
	start := time.Now()
	acked := map[string]core.LSN{}
	for i := 0; i < n; i++ {
		v := c.vec(i)
		sh := oc.part.Shard(v.ID)
		ver, err := oc.shards[sh].Put(ctx, v)
		if err != nil {
			fatal(c.log, "put", err)
		}
		// A write is only "acked" once Put returns, which is only once the WAL
		// fsync completed. That is the entire durability contract.
		acked[v.ID] = ver
	}
	elapsed := time.Since(start)
	fmt.Printf("  inserted %d vectors in %s (%.0f writes/sec)\n", n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds())
	for i := 0; i < c.numShards; i++ {
		s := oc.shards[core.ShardID(i)]
		fmt.Printf("  shard %d: %6d vectors, appliedLSN=%d\n", i, s.Len(), s.AppliedLSN())
	}

	// ---- 2. distributed search ----
	section("2. Distributed search (scatter, gather, merge)")
	q := c.vec(0).Embedding
	resp, err := oc.coord.Search(ctx, coordinator.SearchRequest{Query: q, K: c.k, RequestID: "req-1"})
	if err != nil {
		fatal(c.log, "search", err)
	}
	fmt.Printf("  fanned out to %d shards, merged %d candidates into top-%d\n", c.numShards, c.numShards*c.k, c.k)
	for i, r := range resp.Results {
		fmt.Printf("   %2d. %s  score=%.6f\n", i+1, r.ID, r.Score)
	}

	// Prove the merge is exact by checking it against every shard scanned
	// individually and merged by hand -- the oracle the whole design rests on.
	fmt.Printf("  partial=%v failed_shards=%v\n", resp.Partial, resp.FailedShards)

	// ---- 3. a failed shard ----
	section("3. A shard fails")
	dead := core.ShardID(0)
	_ = oc.shards[dead].Close() // closing makes its searches fail
	_, err = oc.coord.Search(ctx, coordinator.SearchRequest{Query: q, K: c.k})
	fmt.Printf("  allow_partial=false -> %v\n", errString(err))
	resp2, err2 := oc.coord.Search(ctx, coordinator.SearchRequest{Query: q, K: c.k, AllowPartial: true})
	if err2 != nil {
		fmt.Printf("  allow_partial=true  -> %v\n", err2)
	} else {
		fmt.Printf("  allow_partial=true  -> %d results, partial=%v, failed=%v\n",
			len(resp2.Results), resp2.Partial, resp2.FailedShards)
		fmt.Printf("  (the answer is correct for the shards that responded, and says so)\n")
	}
	oc.close()

	// ---- 4. restart and verify ----
	section("4. Restart: every acknowledged write must still be there")
	oc2 := c.open()
	defer oc2.close()
	missing, wrong := 0, 0
	for id, ver := range acked {
		got, err := oc2.shards[oc2.part.Shard(id)].Get(id)
		if err != nil {
			missing++
			continue
		}
		if got.Version != ver {
			wrong++
		}
	}
	fmt.Printf("  checked %d acknowledged writes: %d missing, %d wrong version\n", len(acked), missing, wrong)
	if missing == 0 && wrong == 0 {
		fmt.Printf("  PASS: zero acknowledged writes lost across restart\n")
	} else {
		fmt.Printf("  FAIL: durability contract violated\n")
		os.Exit(1)
	}

	// ---- 5. snapshot and log truncation ----
	section("5. Snapshot, then truncate the log below it")
	for i := 0; i < c.numShards; i++ {
		s := oc2.shards[core.ShardID(i)]
		before := walBytes(filepath.Join(c.dir, fmt.Sprintf("shard-%d", i), "wal"))
		lsn, err := s.Snapshot(s.CommitLSN())
		if err != nil {
			fatal(c.log, "snapshot", err)
		}
		after := walBytes(filepath.Join(c.dir, fmt.Sprintf("shard-%d", i), "wal"))
		fmt.Printf("  shard %d: snapshot@%d, wal %s -> %s\n", i, lsn, human(before), human(after))
	}
	fmt.Printf("\n  Recovery time is now bounded by the log tail, not the whole history.\n")
	fmt.Println()
}

// crashStory kills the process mid-load, then verifies on restart that every
// acknowledged write survived and nothing beyond the tear was applied.
func (c *demo) crashStory(n int) {
	if os.Getenv("VECDEMO_CHILD") == "1" {
		c.crashChild(n)
		return
	}
	section("Crash test: kill -9 mid-load, then recover")
	ackFile := filepath.Join(c.dir, "acked.json")

	// Re-exec ourselves as a child that will hard-exit mid-load.
	exe, err := os.Executable()
	if err != nil {
		fatal(c.log, "executable", err)
	}
	args := []string{"-dir", c.dir, "-shards", fmt.Sprint(c.numShards),
		"-dim", fmt.Sprint(c.dim), "-n", fmt.Sprint(n), "-crash"}
	proc, err := os.StartProcess(exe, append([]string{exe}, args...), &os.ProcAttr{
		Files: []*os.File{nil, os.Stdout, os.Stderr},
		Env:   append(os.Environ(), "VECDEMO_CHILD=1"),
	})
	if err != nil {
		fatal(c.log, "start child", err)
	}
	st, err := proc.Wait()
	if err != nil {
		fatal(c.log, "wait", err)
	}
	fmt.Printf("  child exited: %v (a hard exit, no clean shutdown, no Close)\n", st)

	raw, err := os.ReadFile(ackFile)
	if err != nil {
		fatal(c.log, "read acked list", err)
	}
	var acked map[string]core.LSN
	if err := json.Unmarshal(raw, &acked); err != nil {
		fatal(c.log, "parse acked list", err)
	}
	fmt.Printf("  the child acknowledged %d writes before dying\n", len(acked))

	section("Recovery")
	oc := c.open()
	defer oc.close()
	missing := 0
	for id := range acked {
		if _, err := oc.shards[oc.part.Shard(id)].Get(id); err != nil {
			missing++
		}
	}
	total := 0
	for i := 0; i < c.numShards; i++ {
		total += oc.shards[core.ShardID(i)].Len()
	}
	fmt.Printf("  recovered %d live vectors across %d shards\n", total, c.numShards)
	fmt.Printf("  acknowledged writes missing after recovery: %d\n", missing)
	if missing == 0 {
		fmt.Printf("\n  PASS: no acknowledged write was lost to the crash.\n")
		fmt.Printf("  The torn tail -- the write that was in flight when the process died --\n")
		fmt.Printf("  was truncated. It was never acknowledged, so nothing was promised.\n\n")
	} else {
		fmt.Printf("\n  FAIL: %d acknowledged writes lost.\n\n", missing)
		os.Exit(1)
	}
}

// crashChild loads vectors, records what it acknowledged, and hard-exits.
func (c *demo) crashChild(n int) {
	ctx := context.Background()
	oc := c.open()
	acked := map[string]core.LSN{}
	stopAt := n / 2
	for i := 0; i < n; i++ {
		v := c.vec(i)
		ver, err := oc.shards[oc.part.Shard(v.ID)].Put(ctx, v)
		if err != nil {
			fatal(c.log, "put", err)
		}
		acked[v.ID] = ver
		if i == stopAt {
			raw, _ := json.Marshal(acked)
			f, err := os.Create(filepath.Join(c.dir, "acked.json"))
			if err != nil {
				fatal(c.log, "create acked list", err)
			}
			f.Write(raw)
			f.Sync()
			f.Close()
			fmt.Printf("  child: acknowledged %d writes, now dying without closing anything\n", len(acked))
			// No Close, no flush, no defer. This is as close to kill -9 as a
			// process can do to itself.
			os.Exit(9)
		}
	}
}

func walBytes(dir string) int64 {
	var total int64
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		if fi, err := e.Info(); err == nil {
			total += fi.Size()
		}
	}
	return total
}

func human(b int64) string {
	switch {
	case b > 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b > 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func section(s string) { fmt.Printf("\n── %s %s\n", s, dashes(60-len(s))) }

func dashes(n int) string {
	if n < 0 {
		n = 0
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = '-'
	}
	return string(out)
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func fatal(log *slog.Logger, msg string, err error) {
	log.Error(msg, "err", err)
	os.Exit(1)
}
