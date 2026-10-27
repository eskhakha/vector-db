package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
	"github.com/eskhakha/vecdb/internal/wal"
)

const dim = 24

func shardCfg(dir string, id core.ShardID, m core.Metric) store.Config {
	return store.Config{ID: id, Dir: dir, Dim: dim, Metric: m, Sync: wal.SyncNever, SegmentSize: 1 << 20}
}

// TestGlobalTopKMatchesSingleShard is the key correctness test for the whole
// distributed layer: scattering across S shards and merging must produce
// byte-identical results to running the same query against one shard holding
// all the data. If this fails, nothing above it can be trusted.
func TestGlobalTopKMatchesSingleShard(t *testing.T) {
	ctx := context.Background()
	rng := rand.New(rand.NewSource(42))

	for _, metric := range []core.Metric{core.Cosine, core.L2Squared} {
		for _, S := range []int{1, 2, 4, 8} {
			t.Run(fmt.Sprintf("%s/S=%d", metric, S), func(t *testing.T) {
				part, err := cluster.NewPartitioner(S)
				if err != nil {
					t.Fatal(err)
				}
				// The sharded cluster.
				shards := map[core.ShardID]Searcher{}
				open := map[core.ShardID]*store.Shard{}
				for i := 0; i < S; i++ {
					sh, err := store.Open(shardCfg(t.TempDir(), core.ShardID(i), metric))
					if err != nil {
						t.Fatal(err)
					}
					defer sh.Close()
					open[core.ShardID(i)] = sh
					shards[core.ShardID(i)] = sh
				}
				// The single-shard oracle.
				oracle, err := store.Open(shardCfg(t.TempDir(), 0, metric))
				if err != nil {
					t.Fatal(err)
				}
				defer oracle.Close()

				const n = 500
				for i := 0; i < n; i++ {
					id := fmt.Sprintf("id-%d", i)
					e := make([]float32, dim)
					for j := range e {
						e[j] = rng.Float32()*2 - 1
					}
					v := &core.Vector{ID: id, Embedding: e}
					if _, err := open[part.Shard(id)].Put(ctx, v); err != nil {
						t.Fatal(err)
					}
					if _, err := oracle.Put(ctx, v); err != nil {
						t.Fatal(err)
					}
				}

				c := New(part, shards, metric)
				q := make([]float32, dim)
				for j := range q {
					q[j] = rng.Float32()*2 - 1
				}
				for _, k := range []int{1, 5, 10, 50} {
					resp, err := c.Search(ctx, SearchRequest{Query: q, K: k})
					if err != nil {
						t.Fatal(err)
					}
					want, err := oracle.Search(ctx, q, k)
					if err != nil {
						t.Fatal(err)
					}
					if len(resp.Results) != len(want) {
						t.Fatalf("k=%d: got %d results, want %d", k, len(resp.Results), len(want))
					}
					for i := range want {
						if resp.Results[i].ID != want[i].ID || resp.Results[i].Score != want[i].Score {
							t.Fatalf("k=%d rank %d: got %s/%.6f, want %s/%.6f",
								k, i, resp.Results[i].ID, resp.Results[i].Score,
								want[i].ID, want[i].Score)
						}
					}
					if resp.Partial {
						t.Fatalf("k=%d: unexpectedly partial", k)
					}
				}
			})
		}
	}
}

// failingShard stands in for a dead replica.
type failingShard struct{}

func (failingShard) Search(context.Context, []float32, int) ([]core.Result, error) {
	return nil, errors.New("shard unavailable")
}

func TestPartialResultsAreRefusedByDefault(t *testing.T) {
	ctx := context.Background()
	part, _ := cluster.NewPartitioner(2)
	sh, err := store.Open(shardCfg(t.TempDir(), 0, core.Cosine))
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	if _, err := sh.Put(ctx, &core.Vector{ID: "a", Embedding: make([]float32, dim)}); err != nil {
		t.Fatal(err)
	}
	c := New(part, map[core.ShardID]Searcher{0: sh, 1: failingShard{}}, core.Cosine)

	if _, err := c.Search(ctx, SearchRequest{Query: make([]float32, dim), K: 5}); err == nil {
		t.Fatal("a failed shard must fail the request when AllowPartial is false")
	}

	resp, err := c.Search(ctx, SearchRequest{Query: make([]float32, dim), K: 5, AllowPartial: true})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Partial {
		t.Fatal("partial result was not flagged")
	}
	if len(resp.FailedShards) != 1 || resp.FailedShards[0] != 1 {
		t.Fatalf("FailedShards = %v, want [1]", resp.FailedShards)
	}
}

func TestPartitionerIsStableAndBalanced(t *testing.T) {
	const S = 8
	p, err := cluster.NewPartitioner(S)
	if err != nil {
		t.Fatal(err)
	}
	// Stability: the same ID must always map to the same shard. If this ever
	// depended on process state, keys would land on different shards on
	// different nodes.
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("id-%d", i)
		if p.Shard(id) != p.Shard(id) {
			t.Fatalf("partitioner is not deterministic for %s", id)
		}
	}
	// Distribution: a chi-square style sanity check, not a proof.
	counts := make([]int, S)
	const n = 100000
	for i := 0; i < n; i++ {
		counts[p.Shard(fmt.Sprintf("vec-%d", i))]++
	}
	expected := float64(n) / S
	var chi2 float64
	for _, c := range counts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	// 7 degrees of freedom; 24.3 is roughly the 0.999 critical value.
	if chi2 > 24.3 {
		t.Fatalf("shard distribution is badly skewed: chi2=%.2f counts=%v", chi2, counts)
	}
}
