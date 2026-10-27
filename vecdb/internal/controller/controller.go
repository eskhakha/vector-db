// Package controller decides who leads each shard, and performs failover.
//
// The controller is a pure decision-maker: it watches heartbeats, works out
// what should change, and hands the changes to a Store. Where those decisions
// become cluster state is a separate question with two different answers, and
// separating them is what makes the difference visible.
//
//   - LocalStore keeps the map in this process and applies every decision
//     immediately. It NEVER refuses, because a single controller is the
//     authority and cannot detect its own isolation: from inside a partition,
//     "I cannot reach the other nodes" and "the other nodes are dead" are the
//     same observation. So a partitioned controller promotes a replacement for
//     a leader that is perfectly healthy on the other side, and nothing stops
//     it.
//
//   - RaftStore requires each decision to commit to a replicated log. A
//     minority cannot commit, so the same partitioned controller is refused --
//     including by itself.
//
// Both are kept, and the demo runs both, because the failure is the lesson. A
// consensus layer introduced before you have watched your own system fall into
// the hole it fills is memorised, not understood.
//
// COMMIT IS ASYNCHRONOUS, and the code is shaped around that rather than
// hiding it. Proposing a promotion does not promote anything; the epoch is
// installed on the replica when the decision is APPLIED. Installing it at
// proposal time would fence the old leader on the strength of a decision that
// may never commit -- which is the bug, wearing a different hat.
package controller

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/raft"
)

// ShardReport is one shard's state as reported by one node.
type ShardReport struct {
	Epoch    core.Epoch
	Applied  core.LSN
	IsLeader bool
	// ISR is meaningful only from the leader. Piggybacking it on the heartbeat
	// is what lets the controller know who is eligible for promotion WITHOUT
	// asking at failover time -- by then the leader is gone.
	ISR []cluster.NodeID
}

// Heartbeat is one node's periodic liveness and progress report.
type Heartbeat struct {
	Node   cluster.NodeID
	Shards map[core.ShardID]ShardReport
}

// Promoter installs a new leader for a shard at a new epoch. It is a callback
// so the controller stays a pure decision-maker and can be tested without a
// replication layer underneath it.
type Promoter func(shard core.ShardID, to cluster.NodeID, newEpoch core.Epoch) error

// Config configures the controller.
type Config struct {
	Detector cluster.DetectorConfig
	// LeaseDuration bounds how long a leader may serve strong reads without
	// hearing from the controller.
	//
	// It MUST be shorter than DeadAfter. Otherwise a deposed leader's lease can
	// still be valid at the instant its replacement is installed, and both
	// nodes believe they may serve strong reads -- which is the exact window
	// leases exist to close.
	LeaseDuration time.Duration
}

func (c *Config) validate() error {
	c.Detector.SuspectAfter = orDefault(c.Detector.SuspectAfter, 1500*time.Millisecond)
	c.Detector.DeadAfter = orDefault(c.Detector.DeadAfter, 5*time.Second)
	c.LeaseDuration = orDefault(c.LeaseDuration, 2*time.Second)
	if c.LeaseDuration >= c.Detector.DeadAfter {
		return fmt.Errorf("controller: LeaseDuration (%s) must be shorter than DeadAfter (%s), "+
			"or a deposed leader's lease can outlive its replacement's installation",
			c.LeaseDuration, c.Detector.DeadAfter)
	}
	return nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// Event records something the controller did, for logging and for tests.
type Event struct {
	Kind   string // "promoted", "read_only", "node_dead", "node_suspect", "node_alive"
	Shard  core.ShardID
	Node   cluster.NodeID
	Epoch  core.Epoch
	Detail string
}

func (e Event) String() string {
	if e.Detail != "" {
		return fmt.Sprintf("%s shard=%d node=%s epoch=%d: %s", e.Kind, e.Shard, e.Node, e.Epoch, e.Detail)
	}
	return fmt.Sprintf("%s shard=%d node=%s epoch=%d", e.Kind, e.Shard, e.Node, e.Epoch)
}

// applied is the placement this controller has already acted on, so it can
// tell a newly-committed decision from one it has already installed.
type applied struct {
	epoch  core.Epoch
	leader cluster.NodeID
	ro     bool
}

// Controller decides who leads each shard, and performs failover.
type Controller struct {
	cfg     Config
	det     *cluster.Detector
	clock   cluster.Clock
	promote Promoter
	store   Store

	mu       sync.Mutex
	m        *cluster.ShardMap // cached view of the store, refreshed each tick
	last     map[core.ShardID]applied
	roReason map[core.ShardID]string
	// pendingRO records the epoch a read-only has already been proposed for,
	// so an asynchronous commit does not collect one proposal per tick.
	pendingRO map[core.ShardID]core.Epoch
	reports   map[cluster.NodeID]map[core.ShardID]ShardReport
	leases    map[core.ShardID]cluster.Lease
	failover  map[core.ShardID]time.Time // when the leader was declared dead
}

// New returns a controller managing the given map with a local store -- the
// single-controller behaviour, which cannot detect its own isolation.
func New(m *cluster.ShardMap, cfg Config, clock cluster.Clock, p Promoter) (*Controller, error) {
	return NewWithStore(NewLocalStore(m), cfg, clock, p)
}

// NewWithStore returns a controller backed by an explicit store.
func NewWithStore(store Store, cfg Config, clock cluster.Clock, p Promoter) (*Controller, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = cluster.SystemClock{}
	}
	m := store.Map()
	det := cluster.NewDetector(cfg.Detector, clock)
	for n := range m.Nodes {
		det.Register(n)
	}
	c := &Controller{
		cfg: cfg, det: det, clock: clock, promote: p, store: store, m: m,
		last:      make(map[core.ShardID]applied, len(m.Shards)),
		roReason:  make(map[core.ShardID]string),
		pendingRO: make(map[core.ShardID]core.Epoch),
		reports:   make(map[cluster.NodeID]map[core.ShardID]ShardReport),
		leases:    make(map[core.ShardID]cluster.Lease),
		failover:  make(map[core.ShardID]time.Time),
	}
	// Seed the applied view with the initial placement. Without this the first
	// tick would treat every shard's founding leader as a fresh promotion and
	// re-install an epoch nobody changed.
	for s, p := range m.Shards {
		c.last[s] = applied{epoch: p.Epoch, leader: p.Leader, ro: p.ReadOnly}
	}
	c.renewLeasesLocked()
	return c, nil
}

// Mode reports how decisions become cluster state: "local" or "raft".
func (c *Controller) Mode() string { return c.store.Mode() }

// Map returns a snapshot of the current placement.
func (c *Controller) Map() *cluster.ShardMap {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshLocked()
	return c.m.Clone()
}

// refreshLocked re-reads the placement from the store. Caller holds c.mu.
func (c *Controller) refreshLocked() { c.m = c.store.Map() }

// Lease returns the current lease for a shard.
func (c *Controller) Lease(s core.ShardID) cluster.Lease {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leases[s]
}

// Detector exposes the failure detector, for metrics and assertions.
func (c *Controller) Detector() *cluster.Detector { return c.det }

// Heartbeat records liveness and per-shard progress.
func (c *Controller) Heartbeat(hb Heartbeat) {
	c.det.Heartbeat(hb.Node)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reports[hb.Node] == nil {
		c.reports[hb.Node] = make(map[core.ShardID]ShardReport)
	}
	for s, r := range hb.Shards {
		c.reports[hb.Node][s] = r
	}
	c.refreshLocked()
	// A live leader's lease is renewed by its own heartbeat. A leader that
	// cannot reach the controller stops being renewed and its lease lapses,
	// which is precisely the mechanism that stops it serving stale reads.
	c.renewLeasesLocked()
}

// Tick advances failure detection and performs any failover it implies.
//
// The sequence is deliberate:
//
//  1. apply anything that has COMMITTED since last tick, installing epochs on
//     the replicas the log says now lead;
//  2. decide what should change, given the freshly applied map;
//  3. PROPOSE those changes -- which may be refused;
//  4. apply again, so a store that commits synchronously takes effect now.
//
// Deciding from a stale map, or installing an epoch before the decision
// commits, are both ways to fence a healthy leader on the strength of a
// decision that never happened.
func (c *Controller) Tick() []Event {
	transitions := c.det.Tick()
	c.mu.Lock()
	defer c.mu.Unlock()

	c.refreshLocked()
	events := c.applyLocked()

	var pending []raft.Command
	for _, t := range transitions {
		c.store.SetNodeState(t.Node, t.To)
		c.m.Nodes[t.Node] = t.To
		switch t.To {
		case cluster.NodeSuspect:
			events = append(events, Event{Kind: "node_suspect", Node: t.Node})
		case cluster.NodeAlive:
			events = append(events, Event{Kind: "node_alive", Node: t.Node})
		case cluster.NodeDead:
			events = append(events, Event{Kind: "node_dead", Node: t.Node})
			pending = append(pending, c.planFailoverLocked(t.Node)...)
		}
	}

	if len(pending) > 0 {
		if err := c.store.Propose(pending); err != nil {
			// The decision was refused. NOTHING changed -- not the map, not the
			// epoch on any replica, not a lease. This event is the whole
			// difference between the two stores, and it is the reason a
			// partitioned controller is harmless rather than catastrophic.
			events = append(events, Event{
				Kind: "not_decider", Detail: err.Error(),
			})
		} else {
			c.refreshLocked()
			events = append(events, c.applyLocked()...)
		}
	}

	c.renewLeasesLocked()
	return events
}

// applyLocked installs anything newly committed and returns what it did.
//
// This is where a decision becomes real: the epoch is written to the replica
// here, on APPLY, not at the moment the controller decided. Caller holds c.mu.
func (c *Controller) applyLocked() []Event {
	var events []Event
	var follow []raft.Command

	// Deterministic order, so the event stream is reproducible.
	shards := make([]core.ShardID, 0, len(c.m.Shards))
	for s := range c.m.Shards {
		shards = append(shards, s)
	}
	sort.Slice(shards, func(i, j int) bool { return shards[i] < shards[j] })

	for _, s := range shards {
		p := c.m.Shards[s]
		prev, seen := c.last[s]
		if seen && prev.epoch == p.Epoch && prev.leader == p.Leader && prev.ro == p.ReadOnly {
			continue
		}
		switch {
		case p.ReadOnly:
			delete(c.leases, s)
			events = append(events, Event{
				Kind: "read_only", Shard: s, Epoch: p.Epoch, Detail: c.roReason[s],
			})
		case p.Leader != "" && seen && p.Epoch > prev.epoch:
			// The epoch bump IS the fence. Installing it is what makes the old
			// leader's replication messages carry a lower epoch and be refused.
			if err := c.promote(s, p.Leader, p.Epoch); err != nil {
				c.roReason[s] = "promotion failed: " + err.Error()
				// Propose the read-only once per epoch. Without this guard a
				// store whose commit is asynchronous gets one proposal per
				// tick until it lands.
				if c.pendingRO[s] != p.Epoch {
					c.pendingRO[s] = p.Epoch
					follow = append(follow, raft.Command{
						Kind: raft.CmdReadOnly, Shard: s, Epoch: p.Epoch,
					})
				}
				continue // not recorded as applied; the read-only will be
			}
			c.failover[s] = c.clock.Now()
			// The new leader gets NO lease until it heartbeats as leader at the
			// new epoch. Granting one here hands out overlapping authority
			// inside the very window this mechanism exists to protect.
			delete(c.leases, s)
			events = append(events, Event{Kind: "promoted", Shard: s, Node: p.Leader, Epoch: p.Epoch})
		}
		c.last[s] = applied{epoch: p.Epoch, leader: p.Leader, ro: p.ReadOnly}
	}

	// Propose, but do NOT recurse to apply. Under a Raft store the read-only
	// decision has not committed yet, so re-applying here would see the same
	// un-applied epoch bump, fail to promote again, and propose again --
	// forever. Recursion here was an infinite loop, found by a test hanging.
	// The decision lands on a later tick, which is what asynchronous commit
	// means.
	if len(follow) > 0 {
		if err := c.store.Propose(follow); err != nil {
			events = append(events, Event{Kind: "not_decider", Detail: err.Error()})
		}
	}
	return events
}

// planFailoverLocked works out what should change when a node dies. It is a
// pure decision: it reads the map and returns commands, mutating nothing.
//
// That purity is what lets the same logic sit behind a store that applies
// everything and a store that can refuse.
func (c *Controller) planFailoverLocked(dead cluster.NodeID) []raft.Command {
	var cmds []raft.Command
	for _, s := range c.m.ShardsLedBy(dead) {
		p := c.m.Shards[s]
		cand := c.candidatesLocked(s, dead)

		if len(cand) == 0 {
			// No replica is known to have been in sync at the moment of
			// failure. Promoting one anyway would silently discard every write
			// it missed, and the client was told those writes succeeded.
			//
			// Unavailability is the correct answer. A database that returns
			// wrong data is worse than one that returns an error.
			c.roReason[s] = "no in-sync replica survived; refusing to promote"
			cmds = append(cmds, raft.Command{
				Kind: raft.CmdReadOnly, Shard: s, Epoch: p.Epoch + 1,
			})
			continue
		}

		// Most-caught-up first, ties broken by ID so the choice is
		// deterministic and therefore testable.
		winner := cand[0]
		replicas := remove(append([]cluster.NodeID(nil), p.Replicas...), winner)
		if !contains(replicas, dead) {
			replicas = append(replicas, dead)
		}
		sort.Slice(replicas, func(i, j int) bool { return replicas[i] < replicas[j] })

		cmds = append(cmds,
			raft.Command{Kind: raft.CmdSetISR, Shard: s, ISR: c.reports[dead][s].ISR},
			raft.Command{
				Kind: raft.CmdPromote, Shard: s, Node: winner,
				Epoch: p.Epoch + 1, Replicas: replicas,
			},
		)
	}
	return cmds
}

// candidatesLocked returns promotion-eligible replicas, most-caught-up first.
//
// Eligibility is the whole safety argument: a replica is eligible only if the
// dead leader's last heartbeat listed it as in-sync. Everything an in-sync
// replica holds is committed, because the leader could not have acknowledged
// anything above it without this replica acknowledging first.
func (c *Controller) candidatesLocked(s core.ShardID, dead cluster.NodeID) []cluster.NodeID {
	lastISR := c.reports[dead][s].ISR
	type cand struct {
		id      cluster.NodeID
		applied core.LSN
	}
	var out []cand
	for _, r := range c.m.Shards[s].Replicas {
		if r == dead || !contains(lastISR, r) {
			continue
		}
		if c.det.State(r) != cluster.NodeAlive {
			continue
		}
		out = append(out, cand{id: r, applied: c.reports[r][s].Applied})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].applied != out[j].applied {
			return out[i].applied > out[j].applied
		}
		return out[i].id < out[j].id
	})
	ids := make([]cluster.NodeID, len(out))
	for i, c := range out {
		ids[i] = c.id
	}
	return ids
}

// renewLeasesLocked extends the lease of every shard whose leader is alive.
func (c *Controller) renewLeasesLocked() {
	now := c.clock.Now()
	for s, p := range c.m.Shards {
		if p.Leader == "" || p.ReadOnly {
			delete(c.leases, s)
			continue
		}
		if c.det.State(p.Leader) != cluster.NodeAlive {
			// Do not renew a suspect or dead leader. Letting the existing lease
			// run out is exactly how the stale-read window closes on its own,
			// with no message required.
			continue
		}
		// A freshly promoted leader holds NO lease until it has heartbeated as
		// leader at the new epoch. Granting one at the instant of promotion
		// would hand out overlapping authority inside the very window this
		// mechanism exists to protect: the old leader's lease may still be
		// running, and it has no way to know it was replaced.
		if r, ok := c.reports[p.Leader][s]; !ok || !r.IsLeader || r.Epoch != p.Epoch {
			continue
		}
		c.leases[s] = cluster.Lease{
			Holder:  p.Leader,
			Epoch:   p.Epoch,
			Expires: now.Add(c.cfg.LeaseDuration),
		}
	}
}

// FailoverAt returns when a shard last failed over, for measuring the window.
func (c *Controller) FailoverAt(s core.ShardID) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.failover[s]
	return t, ok
}

func contains(s []cluster.NodeID, x cluster.NodeID) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func remove(s []cluster.NodeID, x cluster.NodeID) []cluster.NodeID {
	out := s[:0]
	for _, v := range s {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
