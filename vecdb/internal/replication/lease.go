package replication

import (
	"errors"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
)

// ErrNoLease is returned when a strong read is attempted without a valid lease.
//
// Refusing the read is the correct behaviour and it is not a bug being worked
// around: a leader that cannot confirm it is still the leader must not answer
// as though it were.
var ErrNoLease = errors.New("replication: no valid lease for strong reads")

// GrantLease installs a lease from the controller.
//
// A lease is a right to serve strong reads for a bounded time, not a right to
// lead. Leadership is established by the epoch; the lease only bounds how long
// this node may keep answering reads without hearing that it has been replaced.
func (l *Leader) GrantLease(lease cluster.Lease) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lease = lease
}

// HoldsLease reports whether this leader may currently serve strong reads.
func (l *Leader) HoldsLease() bool {
	l.mu.Lock()
	lease, epoch := l.lease, l.epoch
	l.mu.Unlock()
	return lease.Valid(l.clock.Now(), l.ID, epoch)
}

// StrongRead returns committed state, but only while holding a valid lease.
//
// This is the mechanism that closes the one hole epoch fencing cannot. Fencing
// stops a deposed leader corrupting the new leader's log, because that takes a
// message and the message is refused. It does not stop that node answering a
// read out of its own memory, because reading requires talking to nobody.
//
// The lease closes it without any message at all: the controller stops renewing
// a leader it cannot reach, the lease lapses on its own, and reads start being
// refused before the replacement is ever installed. That ordering is why
// LeaseDuration must be shorter than the failure threshold.
func (l *Leader) StrongRead(id string) (*core.Vector, error) {
	if !l.HoldsLease() {
		return nil, ErrNoLease
	}
	return l.shard.Get(id)
}

// Report summarises this leader's state for a heartbeat.
//
// The in-sync set is included deliberately. The controller needs to know who
// was eligible for promotion at the moment of failure, and by then the leader
// is gone -- so it has to have been told in advance.
func (l *Leader) Report() (epoch core.Epoch, applied core.LSN, isr []NodeID) {
	return l.Epoch(), l.shard.AppliedLSN(), l.ISR()
}
