package store

import (
	"encoding/binary"
	"fmt"

	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/wal"
)

// This file is the shard's replication-facing surface: what a leader needs to
// ship its log, and what a follower needs to ingest one.
//
// The important property is that none of it is a second write path. Ingest
// reuses the same wal.Append and the same apply() that the leader and recovery
// use, in the same order. There is no code path that could apply a record
// differently on a follower than on a leader, which is the single biggest
// source of divergence bugs in a hand-rolled replication layer.

// Commit advances the commit point. Only the replication layer calls this, and
// only once every in-sync replica holds the record.
//
// It never moves backwards and never runs ahead of what has actually been
// applied: a commit point above applied state would let a reader observe a
// record this node does not have.
func (s *Shard) Commit(lsn core.LSN) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if lsn > s.appliedLSN {
		lsn = s.appliedLSN
	}
	if lsn > s.commitLSN {
		s.commitLSN = lsn
		s.publishLocked()
	}
}

// IngestCode tells the leader what happened to an append.
type IngestCode int

const (
	// IngestOK: the records were made durable and applied.
	IngestOK IngestCode = iota
	// IngestStaleEpoch: the sender is a deposed leader. Its records are
	// refused so it cannot corrupt this node's log.
	IngestStaleEpoch
	// IngestNeedCatchup: there is a gap between this node's applied position
	// and the first record offered. The leader must ship the missing range, or
	// a snapshot if it has already truncated past it.
	IngestNeedCatchup
	// IngestDivergent: this node holds records ABOVE the leader's commit point
	// that came from an older epoch. They were never acknowledged to any
	// client, so they are truncated.
	IngestDivergent
)

func (c IngestCode) String() string {
	switch c {
	case IngestOK:
		return "ok"
	case IngestStaleEpoch:
		return "stale_epoch"
	case IngestNeedCatchup:
		return "need_catchup"
	case IngestDivergent:
		return "divergent"
	default:
		return fmt.Sprintf("code(%d)", int(c))
	}
}

// Ingest applies a contiguous run of records from the leader.
//
// It is idempotent by sequence number: records at or below appliedLSN are
// already durable here and are skipped, so a retried or duplicated replication
// message cannot double-apply. That property is what makes the leader free to
// retry without coordination.
func (s *Shard) Ingest(epoch core.Epoch, recs []*wal.Record) (IngestCode, core.LSN) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return IngestStaleEpoch, s.appliedLSN
	}
	// The epoch fence. A leader that was declared dead because it was merely
	// slow can wake up and try to replicate. Refusing a lower epoch is what
	// stops it corrupting the current leader's log -- it cannot prevent that
	// node from briefly believing it still leads, only from being believed.
	if epoch < s.epoch {
		return IngestStaleEpoch, s.appliedLSN
	}
	s.epoch = epoch
	if len(recs) == 0 {
		return IngestOK, s.appliedLSN
	}
	// Skip anything already durable here.
	i := 0
	for i < len(recs) && recs[i].LSN <= s.appliedLSN {
		i++
	}
	if i == len(recs) {
		return IngestOK, s.appliedLSN
	}
	if recs[i].LSN != s.appliedLSN+1 {
		// A hole. The leader must fill it before this node can proceed;
		// applying anyway would break the gapless-LSN invariant that commit
		// tracking and catch-up both depend on.
		return IngestNeedCatchup, s.appliedLSN
	}
	for ; i < len(recs); i++ {
		r := recs[i]
		if r.LSN != s.appliedLSN+1 {
			return IngestNeedCatchup, s.appliedLSN
		}
		if err := s.log.Append(r); err != nil {
			// A failed append means this node is not durable at this position.
			// Report what it actually has and let the leader decide.
			return IngestNeedCatchup, s.appliedLSN
		}
		s.apply(r)
	}
	s.nextLSN = s.appliedLSN + 1
	s.publishLocked()
	return IngestOK, s.appliedLSN
}

// RecordsFrom returns up to limit log records with LSN > from.
//
// ok=false means the leader has already truncated past `from` and cannot ship
// the gap from its log. The caller must fall back to a snapshot transfer. This
// is exactly why WAL truncation must never pass the slowest in-sync member's
// applied position: crossing it turns a ten-record catch-up into a full
// snapshot.
func (s *Shard) RecordsFrom(from core.LSN, limit int) (recs []*wal.Record, ok bool, err error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed.Load() {
		return nil, false, core.ErrClosed
	}
	var first core.LSN
	err = s.log.Replay(from, func(r *wal.Record) error {
		if first == 0 {
			first = r.LSN
		}
		if len(recs) < limit {
			recs = append(recs, r)
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	if len(recs) == 0 {
		// Nothing above `from`. Either the follower is caught up, or the
		// leader truncated past it. Distinguish by comparing to applied state.
		return nil, from >= s.appliedLSN, nil
	}
	if first != from+1 {
		return nil, false, nil // truncated past the follower's cursor
	}
	return recs, true, nil
}

// TruncateDivergentTail removes records above `keep` from an older epoch.
//
// This is the returning-leader case. A node that crashed after its own fsync
// but before its followers acknowledged holds records the new leader never
// committed. Those records were never acknowledged to any client, so
// discarding them violates nothing -- and keeping them would leave two
// permanently different histories.
func (s *Shard) TruncateDivergentTail(keep core.LSN) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appliedLSN <= keep {
		return nil
	}
	// Rebuilding from the retained log prefix is the honest way to do this: it
	// re-derives state through the same apply() as everything else rather than
	// trying to invert individual records, which is not generally possible
	// (there is no "un-tombstone").
	if err := s.log.TruncateAbove(keep); err != nil {
		return err
	}
	return s.rebuildLocked()
}

// rebuildLocked re-derives in-memory state from disk. Caller holds s.mu.
func (s *Shard) rebuildLocked() error {
	s.data = make(map[string]*core.Vector)
	s.idx = newIndex(s.cfg)
	s.appliedLSN = 0
	if err := s.loadSnapshot(); err != nil {
		return err
	}
	if err := s.log.Replay(s.appliedLSN, func(r *wal.Record) error {
		s.apply(r)
		return nil
	}); err != nil {
		return err
	}
	s.nextLSN = s.appliedLSN + 1
	if s.commitLSN > s.appliedLSN {
		s.commitLSN = s.appliedLSN
	}
	s.publishLocked()
	return nil
}

// ---------- snapshot transfer ----------

// SnapshotBytes serializes committed state for shipping to a follower whose
// gap is too large to fill from the log.
func (s *Shard) SnapshotBytes() ([]byte, core.LSN, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed.Load() {
		return nil, 0, core.ErrClosed
	}
	lsn := s.appliedLSN
	buf := make([]byte, snapHeader)
	binary.LittleEndian.PutUint64(buf[0:], snapMagic)
	binary.LittleEndian.PutUint64(buf[8:], uint64(lsn))
	ids := make([]string, 0, len(s.data))
	for id := range s.data {
		ids = append(ids, id)
	}
	sortStrings(ids)
	for _, id := range ids {
		v := s.data[id]
		if v.Version > lsn {
			continue
		}
		op := wal.OpPut
		if v.Deleted {
			op = wal.OpDel
		}
		buf = append(buf, wal.EncodeFrame(&wal.Record{
			LSN: v.Version, Epoch: s.epoch, Op: op, Vec: *v,
		})...)
	}
	return buf, lsn, nil
}

// InstallSnapshot replaces all local state with a leader's snapshot and resets
// the log to continue from the snapshot's position.
//
// This is destructive on purpose. A follower that needs a snapshot has, by
// definition, a local history the leader can no longer reconstruct, so keeping
// any of it would risk divergence.
func (s *Shard) InstallSnapshot(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) < snapHeader || binary.LittleEndian.Uint64(data[0:]) != snapMagic {
		return fmt.Errorf("%w: snapshot has a bad header", core.ErrCorruptRecord)
	}
	lsn := core.LSN(binary.LittleEndian.Uint64(data[8:]))

	s.data = make(map[string]*core.Vector)
	s.idx = newIndex(s.cfg)
	s.appliedLSN = 0
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
		return fmt.Errorf("%w: snapshot truncated at %d of %d bytes", core.ErrCorruptRecord, off, len(data))
	}
	if err := s.log.Reset(lsn); err != nil {
		return err
	}
	s.appliedLSN = lsn
	s.commitLSN = lsn
	s.nextLSN = lsn + 1
	s.snapshotLSN = lsn
	s.publishLocked()
	return nil
}

func sortStrings(ss []string) {
	for i := 1; i < len(ss); i++ {
		for j := i; j > 0 && ss[j] < ss[j-1]; j-- {
			ss[j], ss[j-1] = ss[j-1], ss[j]
		}
	}
}
