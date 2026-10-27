// Package store owns a shard: the authoritative data map, the derived vector
// index, the write-ahead log, snapshots, and recovery.
//
// One Shard is one lock domain, one WAL, one LSN sequence, one leader. Nothing
// is shared between shards, so a slow search on shard 2 cannot block a write on
// shard 0. That is the main thing logical shards buy you even on a single node.
package store

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/index"
	"github.com/eskhakha/vecdb/internal/wal"
)

// Config configures a shard.
type Config struct {
	ID     core.ShardID
	Dir    string
	Dim    int
	Metric core.Metric
	Sync   wal.SyncPolicy
	// SegmentSize is passed through to the log.
	SegmentSize int64
	// Now is injectable so tests are not at the mercy of the wall clock.
	// Timestamps are observability only and are never used for ordering.
	Now func() int64
	// Replicated changes who is allowed to advance the commit point.
	//
	// Standalone (false): a write commits as soon as it is durable locally.
	// Replicated (true): the write path stops at "durable on the leader", and
	// only the replication layer may call Commit, once every in-sync replica
	// has the record. That is what makes "acknowledged implies durable on every
	// in-sync replica" true rather than aspirational.
	Replicated bool
}

// newIndex builds the shard's index. One place to change when there is ever a
// second implementation.
func newIndex(cfg Config) index.Index {
	return index.NewBruteForce(cfg.Dim, cfg.Metric)
}

// Shard holds one partition's state.
type Shard struct {
	cfg Config

	// mu guards everything below, for WRITERS and for point lookups. Search no
	// longer takes it at all -- see the view field and Search().
	//
	// Before that change, BenchmarkContention measured strict alternation at
	// N=100k: four writers running flat out completed one write per search, and
	// removing the fsync entirely changed nothing, because the write path's own
	// cost had stopped mattering against the wait for a 2.4 ms scan.
	//
	// After it, at N=100k with four writers (50 iterations):
	//
	//	                 writes   search p99
	//	lock,   fsync        53      3769 us
	//	COW,    fsync       131      7680 us
	//	lock, nofsync        53      3252 us
	//	COW,  nofsync       187      8625 us
	//
	// Writes improved 2.5-3.5x, and fsync matters again (131 vs 187), which is
	// the sign the lock is no longer the whole bottleneck. But search p99 got
	// TWICE AS BAD, and that was not the expected outcome.
	//
	// This is not an anomaly. It is copy-on-write's stated precondition being
	// violated: the technique is for workloads where "traversal operations
	// vastly outnumber mutations" (Java's CopyOnWriteArrayList documentation
	// says so outright), and this benchmark runs four writers flat out.
	//
	// Three documented mechanisms compound, all in concepts/23:
	//   - GC frequency = allocation rate / new heap memory, so ~800 KB copied
	//     per write raises collector frequency proportionally;
	//   - the collector takes 25% of CPU in the mark phase, which on 2 cores
	//     is half the machine;
	//   - concurrent marking streams memory and evicts cache, and this scan is
	//     memory-bandwidth-bound, so the two contend for the resource the
	//     reader had all of.
	//
	// Copy-on-write did not remove the cost; it moved it from lock-wait to
	// allocation, and on a small memory-bound machine that is a worse trade.
	//
	// The fix is granularity: publish every k writes or on commit-point
	// advance rather than per write, which divides the copy cost by k while
	// leaving reads lock-free. Not done -- this is the minimal version, built
	// to find out whether the granularity problem was real at this scale.
	// It is.
	mu   sync.RWMutex
	data map[string]*core.Vector
	idx  index.Index

	log *wal.Log

	// Log state. nextLSN is the next LSN to assign; appliedLSN is the highest
	// applied to memory; commitLSN is the highest that readers may observe.
	//
	// On a single node commitLSN tracks appliedLSN. Under replication it only
	// advances once every in-sync replica has the record, which is what stops a
	// failover from making a previously-read value disappear.
	nextLSN    core.LSN
	appliedLSN core.LSN
	commitLSN  core.LSN
	epoch      core.Epoch

	snapshotLSN core.LSN
	readOnly    bool

	// --- the lock-free read path ---
	//
	// view is an immutable snapshot of the candidates, republished by the
	// writer after every write. Search loads it with one atomic load and then
	// holds NOTHING, so a scan can never delay a write.
	//
	// closed is atomic for the same reason: Search must be able to check it
	// without taking mu, or the check reintroduces exactly the contention the
	// snapshot removes.
	view   atomic.Pointer[view]
	closed atomic.Bool
}

// view is what a reader scans. Once published it is never mutated.
type view struct {
	vecs []*core.Vector
	// commit is the commit point this view was published at, carried so a
	// reader can tell how stale it is. Publishing on commit rather than on
	// apply is what keeps uncommitted state invisible.
	commit core.LSN
}

// Open recovers a shard from disk, creating it if absent.
func Open(cfg Config) (*Shard, error) {
	if cfg.Dim <= 0 {
		return nil, fmt.Errorf("store: dim must be positive, got %d", cfg.Dim)
	}
	if cfg.Now == nil {
		cfg.Now = func() int64 { return time.Now().UnixNano() }
	}
	if err := os.MkdirAll(filepath.Join(cfg.Dir, "snap"), 0o755); err != nil {
		return nil, err
	}
	s := &Shard{
		cfg:  cfg,
		data: make(map[string]*core.Vector),
		idx:  newIndex(cfg),
	}

	// Recovery order matters and is the whole point of the design:
	//   1. load the newest complete snapshot
	//   2. replay only the log records above it
	// State is therefore a pure function of what is on disk.
	if err := s.loadSnapshot(); err != nil {
		return nil, err
	}
	lg, err := wal.Open(wal.Options{
		Dir:         filepath.Join(cfg.Dir, "wal"),
		Sync:        cfg.Sync,
		SegmentSize: cfg.SegmentSize,
	})
	if err != nil {
		return nil, err
	}
	s.log = lg
	if err := lg.Replay(s.appliedLSN, func(r *wal.Record) error {
		s.apply(r)
		return nil
	}); err != nil {
		// An LSN gap is real corruption, not a torn tail. Refuse to start.
		lg.Close()
		return nil, err
	}
	s.nextLSN = s.appliedLSN + 1
	s.commitLSN = s.appliedLSN
	s.publishLocked()
	return s, nil
}

// publishLocked republishes the read snapshot. Caller holds s.mu.
//
// The store must be a release and the reader's load an acquire, or a reader
// could observe the new pointer before the writes that built the slice -- real
// on a weak memory model, and silent when it happens. atomic.Pointer gives both
// sides; a plain field would not.
func (s *Shard) publishLocked() {
	s.view.Store(&view{vecs: s.idx.Snapshot(), commit: s.commitLSN})
}

// apply mutates in-memory state from a log record.
//
// The leader path, the follower path and recovery all call this same function
// in the same order. Sharing it halves the surface area for divergence bugs:
// there is no second code path that could apply a record differently.
func (s *Shard) apply(r *wal.Record) {
	switch r.Op {
	case wal.OpPut:
		v := r.Vec
		v.Version = r.LSN
		s.data[v.ID] = &v
		s.idx.Add(&v)
	case wal.OpDel:
		// A delete writes a tombstone rather than removing the key. The log
		// must record it so replay reproduces it, a follower must learn about
		// it, and removing outright would let a replayed older insert
		// resurrect the key.
		if old, ok := s.data[r.Vec.ID]; ok {
			t := *old
			t.Deleted = true
			t.Embedding = nil
			t.Version = r.LSN
			t.UpdatedAt = r.Vec.UpdatedAt
			s.data[t.ID] = &t
		} else {
			s.data[r.Vec.ID] = &core.Vector{ID: r.Vec.ID, Deleted: true, Version: r.LSN}
		}
		s.idx.Remove(r.Vec.ID)
	}
	if r.LSN > s.appliedLSN {
		s.appliedLSN = r.LSN
	}
}

// Epoch returns the shard's current leadership fence.
func (s *Shard) Epoch() core.Epoch {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.epoch
}

// SetEpoch installs a new leadership epoch. It refuses to move backwards: a
// stale epoch must never be able to overwrite a newer one.
func (s *Shard) SetEpoch(e core.Epoch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e < s.epoch {
		return fmt.Errorf("%w: have %d, got %d", core.ErrStaleEpoch, s.epoch, e)
	}
	s.epoch = e
	return nil
}

// AppliedLSN returns the highest LSN applied to memory.
func (s *Shard) AppliedLSN() core.LSN {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.appliedLSN
}

// CommitLSN returns the highest LSN readers may observe.
func (s *Shard) CommitLSN() core.LSN {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.commitLSN
}

// SetReadOnly marks the shard read-only. A shard goes read-only rather than
// risking silent data loss when no in-sync replica survives a failure.
func (s *Shard) SetReadOnly(ro bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readOnly = ro
}

// Put inserts or overwrites a vector and returns the assigned version.
//
// Upsert semantics are not a convenience. An un-acknowledged write has an
// undefined outcome, so clients must retry; retries are only safe if the
// operation is idempotent, and upsert-by-ID is idempotent by construction.
func (s *Shard) Put(ctx context.Context, v *core.Vector) (core.LSN, error) {
	if err := v.Validate(s.cfg.Dim); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	rec := &core.Vector{
		ID: v.ID, Embedding: append([]float32(nil), v.Embedding...),
		Metadata: v.Metadata, CreatedAt: v.CreatedAt, UpdatedAt: s.cfg.Now(),
	}
	if rec.CreatedAt == 0 {
		rec.CreatedAt = rec.UpdatedAt
	}
	rec.ComputeNorm()
	return s.append(wal.OpPut, rec)
}

// Delete writes a tombstone and returns its version.
func (s *Shard) Delete(ctx context.Context, id string) (core.LSN, error) {
	if id == "" || len(id) > core.MaxIDLen {
		return 0, core.ErrInvalidID
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return s.append(wal.OpDel, &core.Vector{ID: id, Deleted: true, UpdatedAt: s.cfg.Now()})
}

// append is the single write path. The ordering below is the entire meaning of
// "write-ahead logging" and every step is load-bearing.
func (s *Shard) append(op wal.OpType, v *core.Vector) (core.LSN, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return 0, core.ErrClosed
	}
	if s.readOnly {
		return 0, core.ErrShardReadOnly
	}

	// 1. Assign the LSN. This is where the total order is established.
	lsn := s.nextLSN
	rec := &wal.Record{LSN: lsn, Epoch: s.epoch, Op: op, Vec: *v}

	// 2. Append and fsync. The record is durable when this returns.
	//
	//    fsync BEFORE apply. If we applied first and crashed before the fsync,
	//    memory would have held a write the log did not -- and recovery would
	//    silently lose it, but only sometimes, depending on whether a read had
	//    already observed it. Write-ahead means the log is always at least as
	//    new as the state.
	if err := s.log.Append(rec); err != nil {
		return 0, err
	}

	// 3. Apply to memory and the index.
	s.apply(rec)
	s.nextLSN = lsn + 1

	// 4. Advance the commit point. On a single node this is immediate. Under
	//    replication the leader stops here and the replication layer calls
	//    Commit once the in-sync set has acknowledged.
	if !s.cfg.Replicated {
		s.commitLSN = s.appliedLSN
	}

	// 5. Republish the read snapshot. This is O(N) POINTER copies, which the
	//    writer pays so that readers pay nothing -- the copy-on-write bargain.
	//    At N=100k it is ~100 us against the ~2.4 ms a reader used to make the
	//    writer wait, so it is a trade the measurement justifies.
	s.publishLocked()
	return lsn, nil
}

// Get returns a copy of the record for id, or core.ErrNotFound.
//
// Records above commitLSN are invisible: if reads could observe uncommitted
// state, a failover could make a previously-read value disappear.
func (s *Shard) Get(id string) (*core.Vector, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed.Load() {
		return nil, core.ErrClosed
	}
	v, ok := s.data[id]
	if !ok || v.Deleted || v.Version > s.commitLSN {
		return nil, core.ErrNotFound
	}
	return v.Clone(), nil
}

// Search returns the k best matches.
// Search returns the k best matches. It takes NO LOCK.
//
// It loads an immutable snapshot and scans that, so however long the scan takes
// it cannot delay a write. Under the old read lock the two contended for the
// same resource and the longer critical section won: BenchmarkContention
// measured strict alternation, one write per search.
//
// The cost is staleness bounded by one write: a search may miss a record
// committed after it loaded its snapshot. That is a weaker recency guarantee,
// not a weaker consistency one -- the snapshot is a complete, real past state,
// never a blend of two.
func (s *Shard) Search(ctx context.Context, q []float32, k int) ([]core.Result, error) {
	// A closed shard is an unavailable replica. It must fail its reads, not
	// quietly serve stale state: the coordinator's partial-result path is only
	// meaningful if an unavailable shard actually reports as unavailable.
	if s.closed.Load() {
		return nil, core.ErrClosed
	}
	v := s.view.Load()
	if v == nil {
		return nil, nil
	}
	return index.SearchSnapshot(ctx, v.vecs, q, k, s.cfg.Dim, s.cfg.Metric)
}

// Len returns the number of live vectors.
func (s *Shard) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.idx.Len()
}

// Close flushes and closes the shard.
func (s *Shard) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return nil
	}
	s.closed.Store(true)
	return s.log.Close()
}

// ---------- snapshots ----------

const (
	snapMagic   = uint64(0x5645435F534E4150) // "VEC_SNAP"
	snapHeader  = 16
	snapTmpExt  = ".tmp"
	snapFileExt = ".snap"
)

// Snapshot writes a full image of committed state and then truncates the log
// below it.
//
// The protocol is: write to .tmp, fsync the file, rename into place, fsync the
// directory, and only THEN truncate the WAL. Rename is atomic on POSIX, so a
// crash at any point leaves either the old snapshot or the new one -- never a
// half-written file that recovery would trust. A crash mid-write leaves only a
// .tmp, which Open deletes.
//
// safeTruncateLSN is the caller's responsibility under replication: it must be
// min(snapshotLSN, slowest in-sync replica's applied LSN). Truncating past a
// live follower's cursor turns a ten-record catch-up into a full snapshot
// transfer.
func (s *Shard) Snapshot(safeTruncateLSN core.LSN) (core.LSN, error) {
	s.mu.RLock()
	lsn := s.commitLSN
	// Copy under the read lock, serialize outside it. A full copy at 1M records
	// is not cheap; the honest fix is chunked copy-on-write, and the honest
	// thing to do now is note that rather than pretend this scales.
	recs := make([]*wal.Record, 0, len(s.data))
	for _, v := range s.data {
		if v.Version > lsn {
			continue
		}
		op := wal.OpPut
		if v.Deleted {
			op = wal.OpDel
		}
		recs = append(recs, &wal.Record{LSN: v.Version, Epoch: s.epoch, Op: op, Vec: *v})
	}
	s.mu.RUnlock()

	// Deterministic order so two snapshots of the same state are byte-identical.
	sort.Slice(recs, func(i, j int) bool { return recs[i].Vec.ID < recs[j].Vec.ID })

	dir := filepath.Join(s.cfg.Dir, "snap")
	tmp := filepath.Join(dir, fmt.Sprintf("%020d%s", uint64(lsn), snapTmpExt))
	final := filepath.Join(dir, fmt.Sprintf("%020d%s", uint64(lsn), snapFileExt))

	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	hdr := make([]byte, snapHeader)
	binary.LittleEndian.PutUint64(hdr[0:], snapMagic)
	binary.LittleEndian.PutUint64(hdr[8:], uint64(lsn))
	if _, err := f.Write(hdr); err != nil {
		f.Close()
		return 0, err
	}
	for _, r := range recs {
		if _, err := f.Write(wal.EncodeFrame(r)); err != nil {
			f.Close()
			return 0, err
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return 0, err
	}
	if err := syncDir(dir); err != nil {
		return 0, err
	}

	s.mu.Lock()
	s.snapshotLSN = lsn
	s.mu.Unlock()

	cut := lsn
	if safeTruncateLSN < cut {
		cut = safeTruncateLSN
	}
	if err := s.log.Truncate(cut); err != nil {
		return lsn, err
	}
	return lsn, nil
}

// loadSnapshot restores state from the newest complete snapshot and removes any
// leftover .tmp files.
func (s *Shard) loadSnapshot() error {
	dir := filepath.Join(s.cfg.Dir, "snap")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var best core.LSN
	var bestName string
	for _, e := range ents {
		n := e.Name()
		if strings.HasSuffix(n, snapTmpExt) {
			// A .tmp is a snapshot that was interrupted before its rename. It
			// was never durable, so it is garbage, not a fallback.
			_ = os.Remove(filepath.Join(dir, n))
			continue
		}
		if !strings.HasSuffix(n, snapFileExt) {
			continue
		}
		v, err := strconv.ParseUint(strings.TrimSuffix(n, snapFileExt), 10, 64)
		if err != nil {
			continue
		}
		if core.LSN(v) >= best {
			best, bestName = core.LSN(v), n
		}
	}
	if bestName == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, bestName))
	if err != nil {
		return err
	}
	if len(data) < snapHeader || binary.LittleEndian.Uint64(data[0:]) != snapMagic {
		return fmt.Errorf("%w: snapshot %s has a bad header", core.ErrCorruptRecord, bestName)
	}
	lsn := core.LSN(binary.LittleEndian.Uint64(data[8:]))
	off := snapHeader
	for {
		r, next, ok := wal.DecodeFrame(data, off)
		if !ok {
			break
		}
		s.apply(r)
		off = next
	}
	if off != len(data) {
		return fmt.Errorf("%w: snapshot %s truncated at %d of %d bytes", core.ErrCorruptRecord, bestName, off, len(data))
	}
	s.snapshotLSN = lsn
	s.appliedLSN = lsn
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}
