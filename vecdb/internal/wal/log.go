// Package wal implements a write-ahead log: append-only, checksummed,
// segmented, and able to recover cleanly from a torn tail.
//
// The contract it exists to uphold is one sentence: the log is never behind the
// state. A record is durable when Sync returns, and only then may the write be
// acknowledged to a client. Everything else here follows from that.
package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/eskhakha/vecdb/internal/core"
)

// castagnoli is CRC-32C, not the IEEE polynomial. Go's hash/crc32 uses SSE4.2
// or ARMv8 hardware instructions for Castagnoli and falls back to a table
// lookup for IEEE: same 4 bytes, roughly an order of magnitude faster.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// SyncPolicy controls when data reaches the disk.
type SyncPolicy int

const (
	// SyncAlways fsyncs every append. The honest default for a system claiming
	// durability, and it keeps the fsync cost visible in every benchmark.
	SyncAlways SyncPolicy = iota
	// SyncNever leaves data in the OS page cache. Benchmarks and tests only:
	// a crash loses everything not yet flushed.
	SyncNever
)

// Options configure a log.
type Options struct {
	Dir string
	// SegmentSize rolls to a new segment once a segment exceeds this size.
	// Segments make prefix truncation an os.Remove of whole files instead of a
	// rewrite, and bound the damage a single corrupt segment can do.
	SegmentSize int64
	Sync        SyncPolicy
}

func (o *Options) setDefaults() {
	if o.SegmentSize <= 0 {
		o.SegmentSize = 64 << 20
	}
}

const segmentExt = ".log"

func segmentName(first core.LSN) string {
	// Zero-padded so lexical order equals numeric order.
	return fmt.Sprintf("%020d%s", uint64(first), segmentExt)
}

func parseSegmentName(name string) (core.LSN, bool) {
	if !strings.HasSuffix(name, segmentExt) {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSuffix(name, segmentExt), 10, 64)
	if err != nil {
		return 0, false
	}
	return core.LSN(n), true
}

// Log is an append-only write-ahead log over a directory of segments.
// It is safe for concurrent use; appends are serialized.
type Log struct {
	opts Options

	mu      sync.Mutex
	f       *os.File
	size    int64
	first   core.LSN // first LSN in the open segment
	lastLSN core.LSN // highest LSN appended, 0 if empty
	closed  bool
	// syncErr latches a failed Sync. On Linux a failed fsync is NOT retryable:
	// the kernel may have already dropped the dirty pages, so a later fsync can
	// report success on data that never landed. This is the 2018 PostgreSQL
	// "fsyncgate" lesson. The only correct response is to stop accepting writes.
	syncErr error
}

// Open opens or creates a log in opts.Dir and positions it for appending after
// the last valid record. A torn tail is truncated as a side effect.
func Open(opts Options) (*Log, error) {
	opts.setDefaults()
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, err
	}
	l := &Log{opts: opts}
	segs, err := l.segments()
	if err != nil {
		return nil, err
	}
	if len(segs) == 0 {
		if err := l.openSegment(1); err != nil {
			return nil, err
		}
		return l, nil
	}
	// Scan the final segment to find the end of valid data and the last LSN.
	last := segs[len(segs)-1]
	path := filepath.Join(opts.Dir, segmentName(last))
	end, lastLSN, err := scanSegment(path, last)
	if err != nil {
		return nil, err
	}
	if err := truncateAt(path, end); err != nil {
		return nil, err
	}
	// Recover the highest LSN across all segments, since the final segment may
	// be empty after truncation.
	if lastLSN == 0 {
		for i := len(segs) - 2; i >= 0 && lastLSN == 0; i-- {
			p := filepath.Join(opts.Dir, segmentName(segs[i]))
			if _, ll, err := scanSegment(p, segs[i]); err == nil {
				lastLSN = ll
			}
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	l.f, l.size, l.first, l.lastLSN = f, end, last, lastLSN
	return l, nil
}

func (l *Log) segments() ([]core.LSN, error) {
	ents, err := os.ReadDir(l.opts.Dir)
	if err != nil {
		return nil, err
	}
	var out []core.LSN
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if n, ok := parseSegmentName(e.Name()); ok {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// openSegment creates a new segment beginning at first.
func (l *Log) openSegment(first core.LSN) error {
	path := filepath.Join(l.opts.Dir, segmentName(first))
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	// A newly created file is not durable until its PARENT DIRECTORY is
	// fsynced. Skip this and a crash can leave a segment with data but no
	// directory entry. Almost every from-scratch WAL gets this wrong.
	if err := syncDir(l.opts.Dir); err != nil {
		f.Close()
		return err
	}
	l.f, l.size, l.first = f, 0, first
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// Directory fsync is meaningless on some platforms; a failure there is not
	// a durability violation worth failing the write over.
	if err := d.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return err
	}
	return nil
}

// LastLSN returns the highest LSN in the log, or 0 if the log is empty.
func (l *Log) LastLSN() core.LSN {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastLSN
}

// Append writes a record and, under SyncAlways, makes it durable before
// returning. LSNs must be contiguous: a gap means a caller bug, not a
// recoverable condition.
func (l *Log) Append(r *Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(r)
}

// AppendBatch writes several records and fsyncs ONCE for the whole batch.
//
// This is group commit, and it is the highest-leverage write optimization
// available: it converts fsync cost from per-record to per-batch. It falls out
// almost free from having a single serialized writer.
func (l *Log) AppendBatch(rs []*Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range rs {
		if err := l.writeLocked(r); err != nil {
			return err
		}
	}
	return l.syncLocked()
}

func (l *Log) appendLocked(r *Record) error {
	if err := l.writeLocked(r); err != nil {
		return err
	}
	return l.syncLocked()
}

func (l *Log) writeLocked(r *Record) error {
	if l.closed {
		return core.ErrClosed
	}
	if l.syncErr != nil {
		return l.syncErr
	}
	if l.lastLSN != 0 && r.LSN != l.lastLSN+1 {
		return fmt.Errorf("%w: appending %d after %d", core.ErrLSNGap, r.LSN, l.lastLSN)
	}
	payload := encodePayload(r)
	if len(payload) > maxRecordSize {
		return fmt.Errorf("wal: record %d is %d bytes, exceeds %d", r.LSN, len(payload), maxRecordSize)
	}
	// Roll to a new segment on size, but never mid-record.
	if l.size >= l.opts.SegmentSize && l.size > 0 {
		if err := l.f.Sync(); err != nil {
			l.syncErr = fmt.Errorf("wal: fsync failed, log is poisoned: %w", err)
			return l.syncErr
		}
		if err := l.f.Close(); err != nil {
			return err
		}
		if err := l.openSegment(r.LSN); err != nil {
			return err
		}
	}
	buf := make([]byte, headerSize+len(payload))
	binary.LittleEndian.PutUint32(buf[0:], magic)
	binary.LittleEndian.PutUint32(buf[4:], uint32(len(payload)))
	binary.LittleEndian.PutUint64(buf[12:], uint64(r.LSN))
	copy(buf[headerSize:], payload)
	// CRC covers LSN || PAYLOAD.
	binary.LittleEndian.PutUint32(buf[8:], crc32.Checksum(buf[12:], castagnoli))

	n, err := l.f.Write(buf)
	l.size += int64(n)
	if err != nil {
		return err
	}
	l.lastLSN = r.LSN
	return nil
}

func (l *Log) syncLocked() error {
	if l.opts.Sync == SyncNever {
		return nil
	}
	if err := l.f.Sync(); err != nil {
		l.syncErr = fmt.Errorf("wal: fsync failed, log is poisoned: %w", err)
		return l.syncErr
	}
	return nil
}

// Close flushes and closes the log.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	return err
}

// Replay calls fn for every record with LSN > from, in order.
//
// A torn tail is truncated and replay stops there. A valid, checksummed record
// carrying the WRONG LSN is a different thing entirely -- real corruption, not
// a torn write -- and returns core.ErrLSNGap so the caller can refuse to start
// rather than silently repairing a hole.
func (l *Log) Replay(from core.LSN, fn func(*Record) error) error {
	segs, err := l.segments()
	if err != nil {
		return err
	}
	expected := core.LSN(0)
	for i, s := range segs {
		// Skip segments entirely below the cursor.
		if i+1 < len(segs) && segs[i+1] <= from+1 {
			continue
		}
		path := filepath.Join(l.opts.Dir, segmentName(s))
		stop, err := replaySegment(path, s, from, &expected, fn)
		if err != nil {
			return err
		}
		if stop {
			// A torn tail ends the log. Anything in later segments would create
			// a hole, so replay stops here.
			break
		}
	}
	return nil
}

// replaySegment returns stop=true if it hit a torn tail.
func replaySegment(path string, first, from core.LSN, expected *core.LSN, fn func(*Record) error) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	off := 0
	for {
		rec, next, ok := decodeAt(data, off)
		if !ok {
			// Torn or absent: everything from here on is untrustworthy.
			return off < len(data), nil
		}
		if *expected != 0 && rec.LSN != *expected {
			return false, fmt.Errorf("%w: found %d, expected %d in %s", core.ErrLSNGap, rec.LSN, *expected, filepath.Base(path))
		}
		*expected = rec.LSN + 1
		if rec.LSN > from {
			if err := fn(rec); err != nil {
				return false, err
			}
		}
		off = next
	}
}

// decodeAt parses one record at off. ok=false means "no valid record here",
// which is the normal end-of-log condition as well as the torn-tail condition;
// the two are indistinguishable by design, and both are handled the same way.
func decodeAt(data []byte, off int) (rec *Record, next int, ok bool) {
	if off+headerSize > len(data) {
		return nil, off, false
	}
	h := data[off:]
	if binary.LittleEndian.Uint32(h[0:]) != magic {
		return nil, off, false
	}
	n := int(binary.LittleEndian.Uint32(h[4:]))
	if n <= 0 || n > maxRecordSize || off+headerSize+n > len(data) {
		return nil, off, false
	}
	body := data[off+12 : off+headerSize+n] // LSN || PAYLOAD
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(h[8:]) {
		return nil, off, false
	}
	lsn := core.LSN(binary.LittleEndian.Uint64(h[12:]))
	r, err := decodePayload(lsn, data[off+headerSize:off+headerSize+n])
	if err != nil {
		// A record that checksums correctly but does not parse is a codec bug
		// or a deliberate corruption, not a torn write. Treat as end-of-log so
		// recovery is still possible, but never apply it.
		return nil, off, false
	}
	return r, off + headerSize + n, true
}

// scanSegment returns the offset just past the last valid record and the last
// LSN it saw.
func scanSegment(path string, first core.LSN) (end int64, lastLSN core.LSN, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	off := 0
	for {
		rec, next, ok := decodeAt(data, off)
		if !ok {
			return int64(off), lastLSN, nil
		}
		lastLSN = rec.LSN
		off = next
	}
}

// truncateAt physically truncates the file and fsyncs it, so subsequent
// appends cannot land after garbage.
//
// Truncating rather than skipping is the important decision. If a record fails
// validation we cannot know whether the following bytes are a real record or
// the middle of a half-written one, so skipping could produce an LSN hole --
// and the gapless-LSN invariant is what replication and commit tracking depend
// on. The cost of truncating is losing the tail, which is exactly the set of
// writes that were never acknowledged, because acknowledgement requires a
// completed fsync.
func truncateAt(path string, end int64) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Size() == end {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(end); err != nil {
		return err
	}
	return f.Sync()
}

// Truncate removes whole segments whose records all fall at or below upto.
//
// Callers must pass min(snapshotLSN, slowest in-sync replica's applied LSN).
// Truncating past a live follower's cursor forces a full snapshot transfer for
// what could have been a ten-record catch-up: a classic and expensive bug.
func (l *Log) Truncate(upto core.LSN) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	segs, err := l.segments()
	if err != nil {
		return err
	}
	for i := 0; i+1 < len(segs); i++ {
		// Safe only if the NEXT segment starts at or below the cutoff, which
		// proves this segment holds nothing above it.
		if segs[i+1] > upto+1 {
			break
		}
		if segs[i] == l.first {
			break // never delete the segment currently open for append
		}
		if err := os.Remove(filepath.Join(l.opts.Dir, segmentName(segs[i]))); err != nil {
			return err
		}
	}
	return syncDir(l.opts.Dir)
}

// TruncateAbove discards every record with LSN > keep.
//
// This exists for one situation: a returning leader whose log holds records
// above the new leader's commit point, written under an older epoch. Those
// records were never acknowledged to any client -- acknowledgement requires a
// completed fsync AND in-sync replication -- so discarding them violates
// nothing, and keeping them would leave two permanently divergent histories.
func (l *Log) TruncateAbove(keep core.LSN) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return core.ErrClosed
	}
	if l.lastLSN <= keep {
		return nil
	}
	segs, err := l.segments()
	if err != nil {
		return err
	}
	if l.f != nil {
		if err := l.f.Close(); err != nil {
			return err
		}
		l.f = nil
	}
	// Delete whole segments that start above the cut, then trim the one that
	// straddles it.
	var straddle core.LSN
	for _, s := range segs {
		if s > keep {
			if err := os.Remove(filepath.Join(l.opts.Dir, segmentName(s))); err != nil {
				return err
			}
			continue
		}
		straddle = s
	}
	if straddle == 0 {
		// Everything was above the cut. Start fresh at keep+1.
		if err := syncDir(l.opts.Dir); err != nil {
			return err
		}
		l.lastLSN = keep
		return l.openSegment(keep + 1)
	}
	path := filepath.Join(l.opts.Dir, segmentName(straddle))
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	end, last := 0, core.LSN(0)
	off := 0
	for {
		r, next, ok := decodeAt(data, off)
		if !ok || r.LSN > keep {
			break
		}
		end, last = next, r.LSN
		off = next
	}
	if err := truncateAt(path, int64(end)); err != nil {
		return err
	}
	if err := syncDir(l.opts.Dir); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	l.f, l.size, l.first, l.lastLSN = f, int64(end), straddle, last
	return nil
}

// Reset discards the entire log and restarts it at firstLSN.
//
// Used only when installing a snapshot: the follower's local history is one
// the leader can no longer reconstruct, so none of it may be kept.
func (l *Log) Reset(atLSN core.LSN) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	segs, err := l.segments()
	if err != nil {
		return err
	}
	for _, s := range segs {
		if err := os.Remove(filepath.Join(l.opts.Dir, segmentName(s))); err != nil {
			return err
		}
	}
	l.lastLSN = atLSN
	l.syncErr = nil
	l.closed = false
	return l.openSegment(atLSN + 1)
}

var _ = io.EOF
