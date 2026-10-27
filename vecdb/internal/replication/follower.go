package replication

import (
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/store"
)

// Follower is the receiving side of replication. It owns a shard replica and
// never accepts client writes.
type Follower struct {
	ID    NodeID
	shard *store.Shard
}

// NewFollower wraps a shard as a replica.
func NewFollower(id NodeID, sh *store.Shard) *Follower {
	return &Follower{ID: id, shard: sh}
}

// Shard exposes the underlying replica, for reads and for assertions in tests.
func (f *Follower) Shard() *store.Shard { return f.shard }

// HandleAppend ingests records from the leader.
//
// Ordering matters here: the records are made durable and applied BEFORE the
// commit point moves. A follower that advanced its commit point first would be
// telling readers about state it might not survive a crash with.
func (f *Follower) HandleAppend(req *AppendRequest) *AppendResponse {
	code, applied := f.shard.Ingest(req.Epoch, req.Records)
	if code == store.IngestOK {
		// The leader's commit point is a ceiling, not an instruction: this
		// replica may only reveal what it has actually applied.
		f.shard.Commit(min(req.CommitLSN, applied))
	}
	return &AppendResponse{Code: code, Applied: applied, Epoch: f.shard.Epoch()}
}

// HandleSnapshot installs a full state image, discarding local history.
func (f *Follower) HandleSnapshot(req *SnapshotRequest) *AppendResponse {
	if req.Epoch < f.shard.Epoch() {
		return &AppendResponse{Code: store.IngestStaleEpoch, Applied: f.shard.AppliedLSN(), Epoch: f.shard.Epoch()}
	}
	if err := f.shard.SetEpoch(req.Epoch); err != nil {
		return &AppendResponse{Code: store.IngestStaleEpoch, Applied: f.shard.AppliedLSN(), Epoch: f.shard.Epoch()}
	}
	if err := f.shard.InstallSnapshot(req.Data); err != nil {
		return &AppendResponse{Code: store.IngestNeedCatchup, Applied: f.shard.AppliedLSN(), Epoch: f.shard.Epoch()}
	}
	return &AppendResponse{Code: store.IngestOK, Applied: f.shard.AppliedLSN(), Epoch: f.shard.Epoch()}
}

func min(a, b core.LSN) core.LSN {
	if a < b {
		return a
	}
	return b
}
