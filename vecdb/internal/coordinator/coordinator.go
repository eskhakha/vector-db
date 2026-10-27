// Package coordinator fans a search out across shards and merges the results.
package coordinator

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/index"
)

// Searcher is the per-shard capability the coordinator needs. Declaring it here
// -- at the consumer -- rather than in the store keeps the store from having to
// know a coordinator exists.
type Searcher interface {
	Search(ctx context.Context, q []float32, k int) ([]core.Result, error)
}

// Coordinator scatters a query to shards and gathers a global top-K.
type Coordinator struct {
	part   *cluster.Partitioner
	shards map[core.ShardID]Searcher
	metric core.Metric
}

// New returns a coordinator over the given shards.
func New(p *cluster.Partitioner, shards map[core.ShardID]Searcher, m core.Metric) *Coordinator {
	return &Coordinator{part: p, shards: shards, metric: m}
}

// SearchRequest is one fan-out query.
type SearchRequest struct {
	Query []float32
	K     int
	// AllowPartial defaults to false, and that default is deliberate. A search
	// that silently omits a quarter of the corpus and looks like a normal answer
	// is worse than an error, because the caller cannot tell.
	AllowPartial bool
	// PerShardTimeout bounds each shard independently. Zero means only the
	// request context's deadline applies.
	PerShardTimeout time.Duration
	RequestID       string
}

// SearchResponse carries the merged result and the honesty flags.
type SearchResponse struct {
	Results      []core.Result
	Partial      bool
	FailedShards []core.ShardID
	RequestID    string
}

// Search fans out, gathers, and merges.
//
// Merging per-shard top-K into a global top-K is EXACT, not an approximation,
// as long as each shard's search is exact. The proof: if x is in the global
// top-K then at most K-1 items in the whole dataset beat x; a shard is a subset
// of the dataset, so at most K-1 items on x's own shard beat x; therefore x is
// in that shard's local top-K. K per shard is the tight bound.
//
// That proof is why this system keeps the index exact. An approximate per-shard
// index breaks it, and the usual patch -- over-fetching k' > k per shard --
// has no value that provably restores exactness.
func (c *Coordinator) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	if req.K <= 0 {
		return &SearchResponse{RequestID: req.RequestID}, nil
	}
	type shardResult struct {
		id  core.ShardID
		res []core.Result
		err error
	}

	// One derived context for the whole fan-out, so a single cancel tears down
	// every in-flight shard request rather than leaking goroutines.
	fanCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan shardResult, len(c.shards))
	var wg sync.WaitGroup
	for id, sh := range c.shards {
		wg.Add(1)
		go func(id core.ShardID, sh Searcher) {
			defer wg.Done()
			sctx := fanCtx
			if req.PerShardTimeout > 0 {
				var c2 context.CancelFunc
				sctx, c2 = context.WithTimeout(fanCtx, req.PerShardTimeout)
				defer c2()
			}
			r, err := sh.Search(sctx, req.Query, req.K)
			out <- shardResult{id: id, res: r, err: err}
		}(id, sh)
	}
	wg.Wait()
	close(out)

	lists := make([][]core.Result, 0, len(c.shards))
	var failed []core.ShardID
	var firstErr error
	for sr := range out {
		if sr.err != nil {
			failed = append(failed, sr.id)
			if firstErr == nil {
				firstErr = sr.err
			}
			continue
		}
		lists = append(lists, sr.res)
	}

	if len(failed) > 0 && !req.AllowPartial {
		return nil, fmt.Errorf("search: %d of %d shards failed: %w", len(failed), len(c.shards), firstErr)
	}
	// Sort the failed list so the response is deterministic.
	for i := 1; i < len(failed); i++ {
		for j := i; j > 0 && failed[j] < failed[j-1]; j-- {
			failed[j], failed[j-1] = failed[j-1], failed[j]
		}
	}
	return &SearchResponse{
		Results:      index.MergeTopK(lists, req.K, c.metric),
		Partial:      len(failed) > 0,
		FailedShards: failed,
		RequestID:    req.RequestID,
	}, nil
}
