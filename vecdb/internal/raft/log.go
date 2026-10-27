// Package raft implements the Raft consensus algorithm, scoped to the control
// plane: the shard map, membership, and who leads which shard.
//
// Consensus is NOT on the data path. Point writes go through leader/follower
// replication with an in-sync set, which is what Qdrant and Weaviate both
// independently converged on. Putting Raft under every write would cost a
// majority round trip per record for a guarantee the epoch fence plus a
// synchronous in-sync set already provides.
//
// What consensus buys is the one thing a controller cannot do: it cannot detect
// its own isolation. A partitioned controller will happily promote a
// replacement for a leader that is perfectly healthy on the other side. Only a
// majority can know it is the majority.
//
// This implementation has no goroutines and no locks. A Node is a pure state
// machine: feed it a tick or a message, get back the messages to send. That is
// what makes the deterministic simulation in sim.go possible, and the
// simulation is the only practical way to find consensus bugs.
package raft

import "fmt"

// Term is Raft's logical clock. It increases monotonically and is the mechanism
// by which stale leaders are recognised: any message carrying a term below the
// receiver's is rejected, and any message above it forces the receiver to step
// down to follower.
type Term uint64

// Index is a position in the replicated log, 1-based.
type Index uint64

// Entry is one replicated command.
type Entry struct {
	Term  Term
	Index Index
	Data  []byte
}

func (e Entry) String() string { return fmt.Sprintf("(%d,%d)", e.Term, e.Index) }

// Log is the replicated log.
//
// entries[0] is a sentinel at (term 0, index 0) so that "the entry before the
// first real entry" is a real object rather than a special case. Every
// off-by-one bug in a hand-rolled Raft lives at that boundary.
type Log struct {
	entries []Entry
	commit  Index
}

// NewLog returns an empty log holding only the sentinel.
func NewLog() *Log {
	return &Log{entries: []Entry{{Term: 0, Index: 0}}}
}

// LastIndex returns the index of the final entry.
func (l *Log) LastIndex() Index { return l.entries[len(l.entries)-1].Index }

// LastTerm returns the term of the final entry.
func (l *Log) LastTerm() Term { return l.entries[len(l.entries)-1].Term }

// Commit returns the commit index.
func (l *Log) Commit() Index { return l.commit }

// TermAt returns the term of the entry at i, and whether the log holds it.
func (l *Log) TermAt(i Index) (Term, bool) {
	if i > l.LastIndex() {
		return 0, false
	}
	return l.entries[i].Term, true
}

// EntryAt returns the entry at i.
func (l *Log) EntryAt(i Index) (Entry, bool) {
	if i == 0 || i > l.LastIndex() {
		return Entry{}, false
	}
	return l.entries[i], true
}

// From returns entries with index >= i.
func (l *Log) From(i Index) []Entry {
	if i > l.LastIndex() {
		return nil
	}
	if i == 0 {
		i = 1
	}
	out := make([]Entry, l.LastIndex()-i+1)
	copy(out, l.entries[i:])
	return out
}

// Append adds entries at the end, assigning indices.
func (l *Log) Append(term Term, datas ...[]byte) Index {
	for _, d := range datas {
		l.entries = append(l.entries, Entry{
			Term: term, Index: l.LastIndex() + 1, Data: d,
		})
	}
	return l.LastIndex()
}

// MatchesAt reports whether the log holds an entry at index with the given
// term. This is the AppendEntries consistency check, and it is what gives Raft
// the Log Matching property: if two logs contain an entry with the same index
// and term, then the logs are identical in all preceding entries.
func (l *Log) MatchesAt(i Index, t Term) bool {
	got, ok := l.TermAt(i)
	return ok && got == t
}

// AppendFrom splices leader entries in after prevIndex, truncating any local
// entries that conflict.
//
// Truncation is safe because a conflicting entry was never committed: an entry
// is only committed once it is on a majority, and a majority that stored it
// would have prevented the current leader's election (its log would not have
// been up to date). Deleting it therefore cannot lose a committed entry.
//
// Existing entries that MATCH are left alone rather than re-appended, because
// truncating them would drop entries after them that a later, delayed message
// had already delivered correctly.
func (l *Log) AppendFrom(prev Index, ents []Entry) {
	for _, e := range ents {
		if existing, ok := l.TermAt(e.Index); ok {
			if existing == e.Term {
				continue // already have it, identical
			}
			// Conflict: this entry and everything after it must go.
			l.entries = l.entries[:e.Index]
		}
		l.entries = append(l.entries, e)
	}
	_ = prev
}

// SetCommit advances the commit index, never past the end of the log and never
// backwards.
func (l *Log) SetCommit(i Index) {
	if i > l.LastIndex() {
		i = l.LastIndex()
	}
	if i > l.commit {
		l.commit = i
	}
}

// UpToDate implements Raft's election restriction: a candidate's log is at
// least as up to date as this one if its last term is higher, or the terms are
// equal and its log is at least as long.
//
// This single check is what guarantees the Leader Completeness property. A
// candidate that is missing a committed entry cannot win, because any majority
// it needs votes from includes at least one node that holds that entry and will
// refuse. Without it, a leader could be elected that has never seen a committed
// write, and committed data would silently vanish.
func (l *Log) UpToDate(lastIdx Index, lastTerm Term) bool {
	if lastTerm != l.LastTerm() {
		return lastTerm > l.LastTerm()
	}
	return lastIdx >= l.LastIndex()
}
