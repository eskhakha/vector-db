package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/index"
	"github.com/eskhakha/vecdb/internal/wal"
)

const dim = 16

func cfg(dir string) Config {
	tick := int64(0)
	return Config{
		ID: 0, Dir: dir, Dim: dim, Metric: core.Cosine,
		Sync: wal.SyncNever, SegmentSize: 1 << 20,
		Now: func() int64 { tick++; return tick },
	}
}

func randVec(rng *rand.Rand, id string) *core.Vector {
	e := make([]float32, dim)
	for i := range e {
		e[i] = rng.Float32()*2 - 1
	}
	return &core.Vector{ID: id, Embedding: e}
}

func mustOpen(t *testing.T, c Config) *Shard {
	t.Helper()
	s, err := Open(c)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestPutGetDelete(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	rng := rand.New(rand.NewSource(1))

	v := randVec(rng, "a")
	ver, err := s.Put(ctx, v)
	if err != nil {
		t.Fatal(err)
	}
	if ver != 1 {
		t.Fatalf("first version = %d, want 1", ver)
	}
	got, err := s.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.Embedding) != dim {
		t.Fatalf("bad record: %+v", got)
	}
	if got.Norm == 0 {
		t.Fatal("norm was not precomputed")
	}

	// Mutating the returned copy must not affect stored state.
	got.Embedding[0] = 999
	again, _ := s.Get("a")
	if again.Embedding[0] == 999 {
		t.Fatal("Get returned a live pointer into shard state")
	}

	if _, err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("a"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if s.Len() != 0 {
		t.Fatalf("index still holds %d vectors after delete", s.Len())
	}
}

func TestDimMismatchRejected(t *testing.T) {
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	_, err := s.Put(context.Background(), &core.Vector{ID: "x", Embedding: make([]float32, dim+1)})
	if !errors.Is(err, core.ErrDimMismatch) {
		t.Fatalf("err = %v, want ErrDimMismatch", err)
	}
}

// TestRecoveryEquivalence is the acceptance test for durability: state after
// recovery must equal state before the crash, for every acknowledged write.
func TestRecoveryEquivalence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(7))

	s := mustOpen(t, cfg(dir))
	const n = 200
	want := map[string]core.LSN{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%d", i)
		ver, err := s.Put(ctx, randVec(rng, id))
		if err != nil {
			t.Fatal(err)
		}
		want[id] = ver
	}
	// Delete a slice of them.
	for i := 0; i < n; i += 10 {
		id := fmt.Sprintf("id-%d", i)
		if _, err := s.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		delete(want, id)
	}
	appliedBefore := s.AppliedLSN()
	lenBefore := s.Len()
	// Close without a snapshot: recovery must come entirely from the log.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpen(t, cfg(dir))
	defer s2.Close()
	if got := s2.AppliedLSN(); got != appliedBefore {
		t.Fatalf("appliedLSN after recovery = %d, want %d", got, appliedBefore)
	}
	if got := s2.Len(); got != lenBefore {
		t.Fatalf("live count after recovery = %d, want %d", got, lenBefore)
	}
	for id, ver := range want {
		got, err := s2.Get(id)
		if err != nil {
			t.Fatalf("%s missing after recovery: %v", id, err)
		}
		if got.Version != ver {
			t.Fatalf("%s version = %d, want %d", id, got.Version, ver)
		}
	}
	for i := 0; i < n; i += 10 {
		if _, err := s2.Get(fmt.Sprintf("id-%d", i)); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("id-%d resurrected after recovery", i)
		}
	}
}

// TestSnapshotThenRecover exercises the snapshot + log-tail recovery path,
// including that a tombstone survives a snapshot rather than resurrecting.
func TestSnapshotThenRecover(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	rng := rand.New(rand.NewSource(11))

	s := mustOpen(t, cfg(dir))
	for i := 0; i < 100; i++ {
		if _, err := s.Put(ctx, randVec(rng, fmt.Sprintf("id-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Delete(ctx, "id-5"); err != nil {
		t.Fatal(err)
	}
	snapLSN, err := s.Snapshot(s.CommitLSN())
	if err != nil {
		t.Fatal(err)
	}
	if snapLSN == 0 {
		t.Fatal("snapshot LSN is 0")
	}
	// More writes after the snapshot, so recovery must combine both paths.
	for i := 100; i < 130; i++ {
		if _, err := s.Put(ctx, randVec(rng, fmt.Sprintf("id-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	appliedBefore, lenBefore := s.AppliedLSN(), s.Len()
	s.Close()

	s2 := mustOpen(t, cfg(dir))
	defer s2.Close()
	if got := s2.AppliedLSN(); got != appliedBefore {
		t.Fatalf("appliedLSN = %d, want %d", got, appliedBefore)
	}
	if got := s2.Len(); got != lenBefore {
		t.Fatalf("live count = %d, want %d", got, lenBefore)
	}
	if _, err := s2.Get("id-5"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("tombstone did not survive the snapshot: id-5 resurrected")
	}
	if _, err := s2.Get("id-129"); err != nil {
		t.Fatalf("post-snapshot write lost: %v", err)
	}
}

// TestSearchMatchesReference is differential testing: the shard's search must
// agree exactly with a naive scan over the same data. An exact index that
// disagrees with the obvious implementation is the bug that invalidates every
// downstream test.
func TestSearchMatchesReference(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(3))
	for _, metric := range []core.Metric{core.Cosine, core.L2Squared} {
		t.Run(metric.String(), func(t *testing.T) {
			c := cfg(t.TempDir())
			c.Metric = metric
			s := mustOpen(t, c)
			defer s.Close()

			var all []*core.Vector
			for i := 0; i < 300; i++ {
				v := randVec(rng, fmt.Sprintf("id-%d", i))
				if _, err := s.Put(ctx, v); err != nil {
					t.Fatal(err)
				}
				v.ComputeNorm()
				all = append(all, v)
			}
			q := randVec(rng, "q").Embedding
			const k = 10

			got, err := s.Search(ctx, q, k)
			if err != nil {
				t.Fatal(err)
			}
			want := referenceTopK(q, all, k, metric)
			if len(got) != len(want) {
				t.Fatalf("got %d results, want %d", len(got), len(want))
			}
			for i := range want {
				if got[i].ID != want[i].ID {
					t.Fatalf("rank %d: got %s (%.6f), want %s (%.6f)",
						i, got[i].ID, got[i].Score, want[i].ID, want[i].Score)
				}
			}
		})
	}
}

// referenceTopK is the deliberately obvious implementation: score everything,
// sort, take k.
func referenceTopK(q []float32, all []*core.Vector, k int, m core.Metric) []core.Result {
	rs := make([]core.Result, 0, len(all))
	for _, v := range all {
		rs = append(rs, core.Result{ID: v.ID, Score: index.Score(q, v, m)})
	}
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && core.Less(rs[j], rs[j-1], m); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
	if len(rs) > k {
		rs = rs[:k]
	}
	return rs
}

// TestSearchIsDeterministicUnderTies pins the total order. Without breaking
// ties by ID, identical scores would order by insertion and deletion history,
// which makes distributed results non-reproducible and therefore untestable.
func TestSearchIsDeterministicUnderTies(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	e := make([]float32, dim)
	e[0] = 1
	for i := 0; i < 50; i++ {
		v := &core.Vector{ID: fmt.Sprintf("id-%02d", i), Embedding: append([]float32(nil), e...)}
		if _, err := s.Put(ctx, v); err != nil {
			t.Fatal(err)
		}
	}
	// Delete and reinsert to scramble the physical scan order.
	for i := 0; i < 50; i += 3 {
		id := fmt.Sprintf("id-%02d", i)
		if _, err := s.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(ctx, &core.Vector{ID: id, Embedding: append([]float32(nil), e...)}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.Search(ctx, e, 10)
	if err != nil {
		t.Fatal(err)
	}
	for trial := 0; trial < 5; trial++ {
		got, err := s.Search(ctx, e, 10)
		if err != nil {
			t.Fatal(err)
		}
		for i := range got {
			if got[i].ID != first[i].ID {
				t.Fatalf("trial %d rank %d: %s != %s", trial, i, got[i].ID, first[i].ID)
			}
		}
	}
	// All scores tie, so the result must be the lexicographically smallest IDs.
	for i := 0; i < 10; i++ {
		want := fmt.Sprintf("id-%02d", i)
		if first[i].ID != want {
			t.Fatalf("rank %d = %s, want %s (tie-break by ID)", i, first[i].ID, want)
		}
	}
}

func TestSearchIsCancellable(t *testing.T) {
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 20000; i++ {
		if _, err := s.Put(context.Background(), randVec(rng, fmt.Sprintf("id-%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Search(ctx, make([]float32, dim), 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestReadOnlyRefusesWrites(t *testing.T) {
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	s.SetReadOnly(true)
	_, err := s.Put(context.Background(), randVec(rand.New(rand.NewSource(1)), "a"))
	if !errors.Is(err, core.ErrShardReadOnly) {
		t.Fatalf("err = %v, want ErrShardReadOnly", err)
	}
}

func TestEpochNeverMovesBackwards(t *testing.T) {
	s := mustOpen(t, cfg(t.TempDir()))
	defer s.Close()
	if err := s.SetEpoch(5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEpoch(4); !errors.Is(err, core.ErrStaleEpoch) {
		t.Fatalf("err = %v, want ErrStaleEpoch", err)
	}
	if got := s.Epoch(); got != 5 {
		t.Fatalf("epoch = %d, want 5", got)
	}
}

func BenchmarkSearch(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprintf("N=%d", n), func(b *testing.B) {
			c := cfg(b.TempDir())
			s, err := Open(c)
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			rng := rand.New(rand.NewSource(1))
			for i := 0; i < n; i++ {
				if _, err := s.Put(context.Background(), randVec(rng, fmt.Sprintf("id-%d", i))); err != nil {
					b.Fatal(err)
				}
			}
			q := randVec(rng, "q").Embedding
			b.SetBytes(int64(n * dim * 4))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.Search(context.Background(), q, 10); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecovery measures the number nobody measures until an outage: how
// long a cold start takes, with and without a snapshot.
//
// The first attempt at this benchmark was insert-only, and snapshot recovery
// came out SLOWER than replaying the log. That is not noise and it is not a
// bug: with one write per key, the snapshot contains exactly as many records as
// the log, so it saves no work and costs one extra file read.
//
// A snapshot's benefit is proportional to the log's write amplification -- how
// many records the log holds per LIVE key. churn=1 is the worst case for
// snapshots; churn=10 is a normal update-heavy workload, and is where the
// mechanism earns its keep. Measuring only the flattering case would have
// produced a confident, wrong claim.
func BenchmarkRecovery(b *testing.B) {
	const writes = 50000

	build := func(b *testing.B, churn int, snapshot bool) string {
		dir := b.TempDir()
		c := cfg(dir)
		s, err := Open(c)
		if err != nil {
			b.Fatal(err)
		}
		rng := rand.New(rand.NewSource(1))
		keys := writes / churn
		for i := 0; i < writes; i++ {
			// Overwriting the same keys leaves the log long and the live set
			// small, which is what a snapshot compacts away.
			if _, err := s.Put(context.Background(), randVec(rng, fmt.Sprintf("id-%d", i%keys))); err != nil {
				b.Fatal(err)
			}
		}
		if snapshot {
			if _, err := s.Snapshot(s.CommitLSN()); err != nil {
				b.Fatal(err)
			}
		}
		if err := s.Close(); err != nil {
			b.Fatal(err)
		}
		return dir
	}

	for _, churn := range []int{1, 10} {
		for _, snap := range []bool{false, true} {
			name := fmt.Sprintf("churn=%d/replay-only", churn)
			if snap {
				name = fmt.Sprintf("churn=%d/snapshot", churn)
			}
			b.Run(name, func(b *testing.B) {
				dir := build(b, churn, snap)
				c := cfg(dir)
				want := writes / churn
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					s, err := Open(c)
					if err != nil {
						b.Fatal(err)
					}
					if s.Len() != want {
						b.Fatalf("recovered %d vectors, want %d", s.Len(), want)
					}
					s.Close()
				}
			})
		}
	}
}

// BenchmarkContention measures what the shard's single RWMutex actually costs
// when reads and writes run together. It exists to settle a design question
// before any code is written to answer it.
//
// The hypothesis in the README was that a long read starves writers. The
// numbers are the only way to know which direction the starvation runs, and
// the two critical sections are wildly asymmetric:
//
//	read  = the whole scan            ~2.3 ms at N=100k
//	write = fsync + map + index       ~130 us with SyncAlways
//
// So the read holds the lock roughly 20x longer than the write, which points
// the opposite way from the usual "writers block readers" analysis.
//
// Reported: search latency under concurrent writes, and insert throughput
// under concurrent searches, each against a no-contention baseline.
func BenchmarkContention(b *testing.B) {
	run := func(b *testing.B, n int, writers int, syncPolicy wal.SyncPolicy) {
		c := cfg(b.TempDir())
		c.Sync = syncPolicy
		s, err := Open(c)
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
		rng := rand.New(rand.NewSource(1))
		for i := 0; i < n; i++ {
			if _, err := s.Put(context.Background(), randVec(rng, fmt.Sprintf("id-%d", i))); err != nil {
				b.Fatal(err)
			}
		}
		q := randVec(rng, "q").Embedding

		// Background writers, started only after the load phase so the
		// measured window contains steady-state contention and nothing else.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var writes int64
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				r := rand.New(rand.NewSource(int64(100 + w)))
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					// Overwrite existing keys: the point is lock contention,
					// not growing the index under the benchmark.
					id := fmt.Sprintf("id-%d", r.Intn(n))
					if _, err := s.Put(context.Background(), randVec(r, id)); err == nil {
						atomic.AddInt64(&writes, 1)
					}
				}
			}(w)
		}

		b.ReportAllocs()
		b.ResetTimer()
		lat := make([]time.Duration, 0, b.N)
		for i := 0; i < b.N; i++ {
			t0 := time.Now()
			if _, err := s.Search(context.Background(), q, 10); err != nil {
				b.Fatal(err)
			}
			lat = append(lat, time.Since(t0))
		}
		b.StopTimer()
		close(stop)
		wg.Wait()

		// p99 is the number that matters here; a mean would hide exactly the
		// convoy this benchmark exists to detect.
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		p := func(q float64) time.Duration {
			if len(lat) == 0 {
				return 0
			}
			i := int(float64(len(lat)-1) * q)
			return lat[i]
		}
		b.ReportMetric(float64(p(0.50).Microseconds()), "p50-us")
		b.ReportMetric(float64(p(0.99).Microseconds()), "p99-us")
		b.ReportMetric(float64(atomic.LoadInt64(&writes)), "writes")
	}

	// Four configurations, not a sweep. These are the ones that carry the
	// finding, and the full grid took over two minutes for no extra signal.
	//
	// The load-bearing comparison is the last two: if removing the fsync does
	// not raise write throughput, the write path's own cost is irrelevant and
	// the lock is the whole story.
	for _, n := range []int{10000, 100000} {
		b.Run(fmt.Sprintf("N=%d/writers=0/fsync", n), func(b *testing.B) {
			run(b, n, 0, wal.SyncAlways) // baseline: no contention
		})
		b.Run(fmt.Sprintf("N=%d/writers=4/fsync", n), func(b *testing.B) {
			run(b, n, 4, wal.SyncAlways)
		})
		b.Run(fmt.Sprintf("N=%d/writers=4/nofsync", n), func(b *testing.B) {
			run(b, n, 4, wal.SyncNever)
		})
	}
}
