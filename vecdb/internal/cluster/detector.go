package cluster

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// NodeID names a node.
type NodeID string

// NodeState is the failure detector's opinion about a node.
//
// "Opinion" is the right word. Fischer, Lynch and Paterson proved that no
// deterministic algorithm solves consensus in an asynchronous system with even
// one crash fault, so every practical detector is a timeout, and every timeout
// is a guess. This type is a guess with a name.
type NodeState int

const (
	// NodeAlive: heartbeats are arriving.
	NodeAlive NodeState = iota
	// NodeSuspect: heartbeats are late. Stop routing new work here, but do not
	// take irreversible action -- the node may simply be slow.
	NodeSuspect
	// NodeDead: heartbeats are long overdue. Irreversible action (promotion)
	// is now allowed, in the knowledge that this may be wrong.
	NodeDead
)

func (s NodeState) String() string {
	switch s {
	case NodeAlive:
		return "alive"
	case NodeSuspect:
		return "suspect"
	case NodeDead:
		return "dead"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// DetectorConfig sets the two thresholds that define the trade.
type DetectorConfig struct {
	// SuspectAfter is how long without a heartbeat before a node is suspected.
	// Cheap to be wrong here: suspicion is reversible.
	SuspectAfter time.Duration
	// DeadAfter is how long before a node is declared dead and its shards may
	// be failed over.
	//
	// This single value IS the write-unavailability window on a leader crash,
	// and it is also the false-positive rate on a slow node. Those two cannot
	// both be optimised, which is FLP showing up as an engineering constraint.
	DeadAfter time.Duration
}

func (c *DetectorConfig) setDefaults() {
	if c.SuspectAfter <= 0 {
		c.SuspectAfter = 1500 * time.Millisecond
	}
	if c.DeadAfter <= 0 {
		c.DeadAfter = 5 * time.Second
	}
}

// Transition records a state change, so callers can act on edges rather than
// polling levels.
type Transition struct {
	Node NodeID
	From NodeState
	To   NodeState
	At   time.Time
}

func (t Transition) String() string {
	return fmt.Sprintf("%s %s->%s", t.Node, t.From, t.To)
}

// Detector is a timeout-based failure detector.
//
// It deliberately is NOT phi-accrual. Phi-accrual adapts a threshold to
// observed latency variance, which is valuable on a real network and worth
// nothing on an in-process one: it would be a statistical model with nothing to
// model, and a second thing to debug when a failover misbehaves.
type Detector struct {
	cfg   DetectorConfig
	clock Clock

	mu    sync.Mutex
	last  map[NodeID]time.Time
	state map[NodeID]NodeState
}

// NewDetector returns a detector with no nodes registered.
func NewDetector(cfg DetectorConfig, clock Clock) *Detector {
	cfg.setDefaults()
	if clock == nil {
		clock = SystemClock{}
	}
	return &Detector{
		cfg: cfg, clock: clock,
		last:  make(map[NodeID]time.Time),
		state: make(map[NodeID]NodeState),
	}
}

// Register adds a node, initially alive.
func (d *Detector) Register(id NodeID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last[id] = d.clock.Now()
	d.state[id] = NodeAlive
}

// Heartbeat records liveness. A heartbeat from a node currently believed dead
// revives it: the detector was wrong, which is expected and must be survivable.
func (d *Detector) Heartbeat(id NodeID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.last[id] = d.clock.Now()
}

// Tick re-evaluates every node and returns the state changes.
//
// Returning transitions rather than levels matters: promotion must happen once,
// on the alive->dead edge, not on every tick that finds a node still dead.
func (d *Detector) Tick() []Transition {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.clock.Now()
	var out []Transition
	ids := make([]NodeID, 0, len(d.state))
	for id := range d.state {
		ids = append(ids, id)
	}
	// Sorted so a tick that changes several nodes reports them deterministically.
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	for _, id := range ids {
		silent := now.Sub(d.last[id])
		want := NodeAlive
		switch {
		case silent >= d.cfg.DeadAfter:
			want = NodeDead
		case silent >= d.cfg.SuspectAfter:
			want = NodeSuspect
		}
		if got := d.state[id]; got != want {
			out = append(out, Transition{Node: id, From: got, To: want, At: now})
			d.state[id] = want
		}
	}
	return out
}

// State returns the current opinion about a node.
func (d *Detector) State(id NodeID) NodeState {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state[id]
}

// Alive lists nodes currently believed alive, sorted.
func (d *Detector) Alive() []NodeID {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []NodeID
	for id, s := range d.state {
		if s == NodeAlive {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
