package wal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eskhakha/vecdb/internal/core"
)

func rec(lsn core.LSN, id string, dim int) *Record {
	emb := make([]float32, dim)
	for i := range emb {
		emb[i] = float32(i) * 0.5
	}
	return &Record{
		LSN: lsn, Epoch: 7, Op: OpPut,
		Vec: core.Vector{
			ID: id, Embedding: emb, Norm: 1.5, Version: lsn,
			Metadata:  map[string]string{"src": "test", "n": fmt.Sprint(lsn)},
			CreatedAt: 100, UpdatedAt: 200,
		},
	}
}

func openTmp(t *testing.T, dir string) *Log {
	t.Helper()
	l, err := Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	l := openTmp(t, dir)
	const n = 50
	for i := 1; i <= n; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 8)); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}

	l2 := openTmp(t, dir)
	defer l2.Close()
	if got := l2.LastLSN(); got != n {
		t.Fatalf("LastLSN = %d, want %d", got, n)
	}
	var got []core.LSN
	err := l2.Replay(0, func(r *Record) error {
		got = append(got, r.LSN)
		if r.Vec.ID != fmt.Sprintf("id-%d", r.LSN) {
			t.Errorf("lsn %d: id = %q", r.LSN, r.Vec.ID)
		}
		if len(r.Vec.Embedding) != 8 || r.Vec.Embedding[3] != 1.5 {
			t.Errorf("lsn %d: embedding round-trip failed: %v", r.LSN, r.Vec.Embedding)
		}
		if r.Vec.Metadata["src"] != "test" {
			t.Errorf("lsn %d: metadata round-trip failed", r.LSN)
		}
		if r.Epoch != 7 {
			t.Errorf("lsn %d: epoch = %d", r.LSN, r.Epoch)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != n {
		t.Fatalf("replayed %d records, want %d", len(got), n)
	}
}

func TestReplayFromCursor(t *testing.T) {
	dir := t.TempDir()
	l := openTmp(t, dir)
	for i := 1; i <= 20; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 4)); err != nil {
			t.Fatal(err)
		}
	}
	defer l.Close()
	var first core.LSN
	count := 0
	if err := l.Replay(12, func(r *Record) error {
		if first == 0 {
			first = r.LSN
		}
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if first != 13 || count != 8 {
		t.Fatalf("from=12 gave first=%d count=%d, want 13 and 8", first, count)
	}
}

// TestTruncateAtEveryOffset is the single highest-value test in this package.
//
// It takes a valid log, truncates it at EVERY byte offset, and asserts that
// recovery is clean at every one: the log opens, replays a contiguous prefix,
// and is immediately appendable again. A torn tail is the normal consequence of
// a crash mid-write, and it must never require human intervention.
func TestTruncateAtEveryOffset(t *testing.T) {
	// Build one golden log in memory first.
	golden := t.TempDir()
	l := openTmp(t, golden)
	const n = 12
	for i := 1; i <= n; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 6)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	segPath := filepath.Join(golden, segmentName(1))
	full, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) < 100 {
		t.Fatalf("golden log suspiciously small: %d bytes", len(full))
	}

	for cut := 0; cut <= len(full); cut++ {
		cut := cut
		t.Run(fmt.Sprintf("cut=%d", cut), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, segmentName(1)), full[:cut], 0o644); err != nil {
				t.Fatal(err)
			}
			lg, err := Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: 1 << 20})
			if err != nil {
				t.Fatalf("Open after truncation at %d: %v", cut, err)
			}
			defer lg.Close()

			// Replay must yield a contiguous prefix 1..k with no gaps.
			var seen []core.LSN
			if err := lg.Replay(0, func(r *Record) error {
				seen = append(seen, r.LSN)
				return nil
			}); err != nil {
				t.Fatalf("Replay after truncation at %d: %v", cut, err)
			}
			for i, got := range seen {
				if got != core.LSN(i+1) {
					t.Fatalf("cut=%d: non-contiguous replay at %d: got lsn %d", cut, i, got)
				}
			}
			if len(seen) > n {
				t.Fatalf("cut=%d: replayed %d records, more than the %d written", cut, len(seen), n)
			}
			// LastLSN must agree with what replay produced.
			want := core.LSN(len(seen))
			if got := lg.LastLSN(); got != want {
				t.Fatalf("cut=%d: LastLSN = %d, replay saw %d", cut, got, want)
			}
			// And the log must be immediately usable again.
			next := want + 1
			if err := lg.Append(rec(next, "after-recovery", 6)); err != nil {
				t.Fatalf("cut=%d: append after recovery: %v", cut, err)
			}
			if got := lg.LastLSN(); got != next {
				t.Fatalf("cut=%d: after append LastLSN = %d, want %d", cut, got, next)
			}
		})
	}
}

// TestCorruptByteIsDetected flips a byte inside a record body and asserts the
// checksum catches it, so the log recovers to the last good record rather than
// applying garbage.
func TestCorruptByteIsDetected(t *testing.T) {
	dir := t.TempDir()
	l := openTmp(t, dir)
	for i := 1; i <= 6; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 4)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	segPath := filepath.Join(dir, segmentName(1))
	data, err := os.ReadFile(segPath)
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt a byte inside the first record's payload.
	data[headerSize+4] ^= 0xFF
	if err := os.WriteFile(segPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	l2 := openTmp(t, dir)
	defer l2.Close()
	count := 0
	if err := l2.Replay(0, func(*Record) error { count++; return nil }); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if count != 0 {
		t.Fatalf("replayed %d records past a corrupted first record, want 0", count)
	}
}

// TestLSNGapIsRefused proves the reader distinguishes a torn tail (recoverable)
// from a real hole (not recoverable). A valid checksummed record with the wrong
// LSN must be refused, never silently repaired.
func TestLSNGapIsRefused(t *testing.T) {
	dir := t.TempDir()
	l := openTmp(t, dir)
	for i := 1; i <= 3; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 4)); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()

	// Hand-build a well-formed record with a deliberately skipped LSN and
	// append it, simulating a mis-assembled data directory.
	segPath := filepath.Join(dir, segmentName(1))
	f, err := os.OpenFile(segPath, os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	bogus := &Log{opts: Options{Dir: dir, Sync: SyncNever, SegmentSize: 1 << 30}, f: f, lastLSN: 0}
	if err := bogus.writeLocked(rec(99, "hole", 4)); err != nil {
		t.Fatal(err)
	}
	f.Close()

	l2 := openTmp(t, dir)
	defer l2.Close()
	err = l2.Replay(0, func(*Record) error { return nil })
	if !errors.Is(err, core.ErrLSNGap) {
		t.Fatalf("Replay error = %v, want core.ErrLSNGap", err)
	}
}

func TestAppendRejectsLSNGap(t *testing.T) {
	dir := t.TempDir()
	l := openTmp(t, dir)
	defer l.Close()
	if err := l.Append(rec(1, "a", 4)); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(rec(3, "c", 4)); !errors.Is(err, core.ErrLSNGap) {
		t.Fatalf("Append with a gap returned %v, want core.ErrLSNGap", err)
	}
}

func TestSegmentRollAndTruncate(t *testing.T) {
	dir := t.TempDir()
	// A tiny segment size forces frequent rolls.
	l, err := Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	const n = 60
	for i := 1; i <= n; i++ {
		if err := l.Append(rec(core.LSN(i), fmt.Sprintf("id-%d", i), 16)); err != nil {
			t.Fatal(err)
		}
	}
	segs, err := l.segments()
	if err != nil {
		t.Fatal(err)
	}
	if len(segs) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(segs))
	}
	// Truncating below the first segment boundary must not lose anything the
	// cutoff still covers.
	if err := l.Truncate(segs[1] - 1); err != nil {
		t.Fatal(err)
	}
	l.Close()

	l2, err := Open(Options{Dir: dir, Sync: SyncNever, SegmentSize: 512})
	if err != nil {
		t.Fatal(err)
	}
	defer l2.Close()
	if got := l2.LastLSN(); got != n {
		t.Fatalf("after truncation LastLSN = %d, want %d", got, n)
	}
	count := 0
	if err := l2.Replay(segs[1]-1, func(*Record) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("replay after truncation produced nothing")
	}
}

func TestGroupCommitEquivalence(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	a := openTmp(t, dirA)
	b := openTmp(t, dirB)
	var batch []*Record
	for i := 1; i <= 10; i++ {
		r := rec(core.LSN(i), fmt.Sprintf("id-%d", i), 4)
		if err := a.Append(r); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, rec(core.LSN(i), fmt.Sprintf("id-%d", i), 4))
	}
	if err := b.AppendBatch(batch); err != nil {
		t.Fatal(err)
	}
	a.Close()
	b.Close()

	fa, _ := os.ReadFile(filepath.Join(dirA, segmentName(1)))
	fb, _ := os.ReadFile(filepath.Join(dirB, segmentName(1)))
	if string(fa) != string(fb) {
		t.Fatal("group commit produced a different byte stream than per-record append")
	}
}

func BenchmarkAppendSyncAlways(b *testing.B) {
	dir := b.TempDir()
	l, err := Open(Options{Dir: dir, Sync: SyncAlways, SegmentSize: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 1; i <= b.N; i++ {
		if err := l.Append(rec(core.LSN(i), "bench", 128)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendGroupCommit64(b *testing.B) {
	dir := b.TempDir()
	l, err := Open(Options{Dir: dir, Sync: SyncAlways, SegmentSize: 1 << 30})
	if err != nil {
		b.Fatal(err)
	}
	defer l.Close()
	const batchSize = 64
	b.ReportAllocs()
	b.ResetTimer()
	lsn := core.LSN(0)
	for i := 0; i < b.N; i += batchSize {
		n := batchSize
		if i+n > b.N {
			n = b.N - i
		}
		batch := make([]*Record, n)
		for j := 0; j < n; j++ {
			lsn++
			batch[j] = rec(lsn, "bench", 128)
		}
		if err := l.AppendBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
}
