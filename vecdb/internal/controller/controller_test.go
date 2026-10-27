package controller

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/replication"
	"github.com/eskhakha/vecdb/internal/store"
	"github.com/eskhakha/vecdb/internal/wal"
)

const dim = 8

// ---------- detector ----------

func TestDetectorTransitions(t *testing.T) {
	clk := cluster.NewManualClock()
	d := cluster.NewDetector(cluster.DetectorConfig{
		SuspectAfter: 1500 * time.Millisecond,
		DeadAfter:    5 * time.Second,
	}, clk)
	d.Register("n1")

	if got := d.Tick(); len(got) != 0 {
		t.Fatalf("fresh node produced transitions: %v", got)
	}
	clk.Advance(2 * time.Second)
	tr := d.Tick()
	if len(tr) != 1 || tr[0].To != cluster.NodeSuspect {
		t.Fatalf("after 2s got %v, want one alive->suspect", tr)
	}
	// Ticking again must not re-report: promotion must fire on the edge, not
	// on every tick that finds the node still down.
	if got := d.Tick(); len(got) != 0 {
		t.Fatalf("repeat tick re-reported: %v", got)
	}
	clk.Advance(4 * time.Second)
	tr = d.Tick()
	if len(tr) != 1 || tr[0].To != cluster.NodeDead {
		t.Fatalf("after 6s got %v, want suspect->dead", tr)
	}
}

// TestDetectorRevivesOnHeartbeat: the detector is allowed to be wrong, so a
// node it declared dead must be able to come back.
func TestDetectorRevivesOnHeartbeat(t *testing.T) {
	clk := cluster.NewManualClock()
	d := cluster.NewDetector(cluster.DetectorConfig{
		SuspectAfter: time.Second, DeadAfter: 2 * time.Second,
	}, clk)
	d.Register("n1")
	clk.Advance(3 * time.Second)
	d.Tick()
	if d.State("n1") != cluster.NodeDead {
		t.Fatal("node should be dead")
	}
	d.Heartbeat("n1")
	tr := d.Tick()
	if len(tr) != 1 || tr[0].To != cluster.NodeAlive {
		t.Fatalf("heartbeat after death gave %v, want dead->alive", tr)
	}
}

// TestLeaseMustBeShorterThanDeadAfter pins the invariant that makes leases
// work. If a lease can outlive the failure threshold, a deposed leader's lease
// is still valid when its replacement is installed -- which is the exact window
// leases exist to close.
func TestLeaseMustBeShorterThanDeadAfter(t *testing.T) {
	m, _ := cluster.NewShardMap(1, []cluster.NodeID{"a", "b"}, 2)
	_, err := New(m, Config{
		Detector:      cluster.DetectorConfig{DeadAfter: 2 * time.Second},
		LeaseDuration: 5 * time.Second,
	}, cluster.NewManualClock(), func(core.ShardID, cluster.NodeID, core.Epoch) error { return nil })
	if err == nil {
		t.Fatal("a lease longer than the failure threshold was accepted")
	}
	if !strings.Contains(err.Error(), "shorter than DeadAfter") {
		t.Fatalf("unhelpful error: %v", err)
	}
}

// ---------- failover ----------

type harness struct {
	clk    *cluster.ManualClock
	ctrl   *Controller
	tr     *replication.InProcTransport
	leader *replication.Leader
	fols   map[cluster.NodeID]*replication.Follower
	newLd  map[core.ShardID]*replication.Leader
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	clk := cluster.NewManualClock()
	tr := replication.NewInProcTransport(replication.FaultConfig{})
	rcfg := replication.Config{MinISR: 2, SyncTimeout: time.Second, Clock: clk}

	open := func() *store.Shard {
		sh, err := store.Open(store.Config{
			Dir: t.TempDir(), Dim: dim, Metric: core.Cosine,
			Sync: wal.SyncNever, SegmentSize: 1 << 20, Replicated: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		return sh
	}
	ld, err := replication.NewLeader("node-1", open(), tr, 1, rcfg)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		clk: clk, tr: tr, leader: ld,
		fols:  map[cluster.NodeID]*replication.Follower{},
		newLd: map[core.ShardID]*replication.Leader{},
	}
	for _, id := range []cluster.NodeID{"node-2", "node-3"} {
		f := replication.NewFollower(id, open())
		tr.Register(id, f)
		ld.AddReplica(id)
		h.fols[id] = f
	}
	m, err := cluster.NewShardMap(1, []cluster.NodeID{"node-1", "node-2", "node-3"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := New(m, cfg, clk, func(s core.ShardID, to cluster.NodeID, e core.Epoch) error {
		f, ok := h.fols[to]
		if !ok {
			return fmt.Errorf("no follower %s", to)
		}
		nl, err := replication.Promote(to, f, tr, e, rcfg)
		if err != nil {
			return err
		}
		h.newLd[s] = nl
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h.ctrl = ctrl
	return h
}

// beat sends a heartbeat from the leader and each follower.
func (h *harness) beat() {
	epoch, applied, isr := h.leader.Report()
	h.ctrl.Heartbeat(Heartbeat{Node: "node-1", Shards: map[core.ShardID]ShardReport{
		0: {Epoch: epoch, Applied: applied, IsLeader: true, ISR: isr},
	}})
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
}

func (h *harness) put(id string) (core.LSN, error) {
	rng := rand.New(rand.NewSource(int64(len(id))))
	e := make([]float32, dim)
	for i := range e {
		e[i] = rng.Float32()
	}
	return h.leader.Put(context.Background(), &core.Vector{ID: id, Embedding: e})
}

func defaultCfg() Config {
	return Config{
		Detector: cluster.DetectorConfig{
			SuspectAfter: 1500 * time.Millisecond,
			DeadAfter:    5 * time.Second,
		},
		LeaseDuration: 2 * time.Second,
	}
}

// TestAutomaticFailover: the leader stops heartbeating, the detector declares
// it dead, and the controller promotes the most-caught-up in-sync replica.
func TestAutomaticFailover(t *testing.T) {
	h := newHarness(t, defaultCfg())
	acked := map[string]core.LSN{}
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("id-%d", i)
		lsn, err := h.put(id)
		if err != nil {
			t.Fatal(err)
		}
		acked[id] = lsn
	}
	h.beat()
	if p := h.ctrl.Map().Shards[0]; p.Leader != "node-1" || p.Epoch != 1 {
		t.Fatalf("initial placement wrong: %+v", p)
	}

	// The leader dies. Followers keep heartbeating.
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	h.clk.Advance(2 * time.Second)
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	if ev := h.ctrl.Tick(); !hasKind(ev, "node_suspect") {
		t.Fatalf("expected suspicion at 2s, got %v", ev)
	}
	h.clk.Advance(4 * time.Second)
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	events := h.ctrl.Tick()
	if !hasKind(events, "promoted") {
		t.Fatalf("no promotion after the failure threshold: %v", events)
	}

	p := h.ctrl.Map().Shards[0]
	if p.Leader == "node-1" {
		t.Fatal("the dead node is still leader")
	}
	if p.Epoch != 2 {
		t.Fatalf("epoch = %d after failover, want 2: without the bump the old leader is unfenced", p.Epoch)
	}
	if p.ReadOnly {
		t.Fatal("shard went read-only despite an in-sync survivor")
	}
	// Every acknowledged write must be on the new leader.
	nl := h.newLd[0]
	for id, want := range acked {
		got, err := nl.Shard().Get(id)
		if err != nil {
			t.Fatalf("acknowledged write %s lost in automatic failover: %v", id, err)
		}
		if got.Version != want {
			t.Fatalf("%s: version %d, acknowledged as %d", id, got.Version, want)
		}
	}
}

// TestNoInSyncReplicaGoesReadOnly is the rule that separates a database from a
// cache. With no replica known to have been in sync, promoting one would
// silently discard writes the client was told had succeeded.
func TestNoInSyncReplicaGoesReadOnly(t *testing.T) {
	h := newHarness(t, defaultCfg())
	if _, err := h.put("a"); err != nil {
		t.Fatal(err)
	}
	// Heartbeat a leader whose in-sync set is EMPTY: no follower is eligible.
	h.ctrl.Heartbeat(Heartbeat{Node: "node-1", Shards: map[core.ShardID]ShardReport{
		0: {Epoch: 1, Applied: 1, IsLeader: true, ISR: nil},
	}})
	for id := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{0: {Epoch: 1}}})
	}
	h.clk.Advance(6 * time.Second)
	for id := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{0: {Epoch: 1}}})
	}
	events := h.ctrl.Tick()
	if hasKind(events, "promoted") {
		t.Fatal("promoted a replica that was never in sync: acknowledged writes can be lost")
	}
	if !hasKind(events, "read_only") {
		t.Fatalf("expected the shard to go read-only, got %v", events)
	}
	p := h.ctrl.Map().Shards[0]
	if !p.ReadOnly || p.Leader != "" {
		t.Fatalf("placement should be leaderless and read-only, got %+v", p)
	}
}

// TestMostCaughtUpReplicaWins: promotion is deterministic and prefers the
// replica with the highest applied position.
func TestMostCaughtUpReplicaWins(t *testing.T) {
	h := newHarness(t, defaultCfg())
	h.ctrl.Heartbeat(Heartbeat{Node: "node-1", Shards: map[core.ShardID]ShardReport{
		0: {Epoch: 1, Applied: 100, IsLeader: true, ISR: []cluster.NodeID{"node-2", "node-3"}},
	}})
	h.ctrl.Heartbeat(Heartbeat{Node: "node-2", Shards: map[core.ShardID]ShardReport{0: {Applied: 90}}})
	h.ctrl.Heartbeat(Heartbeat{Node: "node-3", Shards: map[core.ShardID]ShardReport{0: {Applied: 100}}})

	h.clk.Advance(6 * time.Second)
	h.ctrl.Heartbeat(Heartbeat{Node: "node-2", Shards: map[core.ShardID]ShardReport{0: {Applied: 90}}})
	h.ctrl.Heartbeat(Heartbeat{Node: "node-3", Shards: map[core.ShardID]ShardReport{0: {Applied: 100}}})
	events := h.ctrl.Tick()

	var promoted cluster.NodeID
	for _, e := range events {
		if e.Kind == "promoted" {
			promoted = e.Node
		}
	}
	if promoted != "node-3" {
		t.Fatalf("promoted %s, want node-3 (applied 100 vs node-2's 90)", promoted)
	}
}

// ---------- the split-brain window, demonstrated then closed ----------

// TestSplitBrainWindowWithoutLeases DELIBERATELY ASSERTS BROKEN BEHAVIOUR.
//
// A partitioned leader is declared dead and replaced. It has not learned this,
// because learning requires a message and it is partitioned. Epoch fencing
// already stops it corrupting the new leader's log -- that takes a message,
// which is refused. But it can still answer a read out of its own memory,
// because reading requires talking to nobody.
//
// This test pins that hole so it is visible rather than assumed away. The very
// next test closes it with a lease.
func TestSplitBrainWindowWithoutLeases(t *testing.T) {
	h := newHarness(t, defaultCfg())
	if _, err := h.put("key"); err != nil {
		t.Fatal(err)
	}
	h.beat()

	// The leader is partitioned but very much alive.
	h.tr.Partition("node-1", true)
	h.clk.Advance(6 * time.Second)
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	if ev := h.ctrl.Tick(); !hasKind(ev, "promoted") {
		t.Fatalf("expected promotion, got %v", ev)
	}
	if h.ctrl.Map().Shards[0].Epoch != 2 {
		t.Fatal("epoch was not bumped")
	}

	// The old leader still answers a plain read. This is the hole.
	if _, err := h.leader.Shard().Get("key"); err != nil {
		t.Fatalf("expected the deposed leader to still serve a stale read "+
			"(that is the window this test documents), got %v", err)
	}

	// But it can no longer ACKNOWLEDGE a write: synchronous replication plus
	// MinISR means it cannot satisfy its own in-sync requirement while
	// partitioned. The exposure is stale reads, not divergent histories.
	if _, err := h.put("written-by-zombie"); err == nil {
		t.Fatal("a partitioned leader acknowledged a write; the exposure is much worse than stale reads")
	}
}

// TestLeaseClosesTheStaleReadWindow is the same scenario with StrongRead. The
// controller stops renewing a leader it cannot reach, the lease lapses on its
// own with no message required, and the read is refused BEFORE the replacement
// is installed. The diff between this test and the previous one is the entire
// value of leases.
func TestLeaseClosesTheStaleReadWindow(t *testing.T) {
	h := newHarness(t, defaultCfg())
	if _, err := h.put("key"); err != nil {
		t.Fatal(err)
	}
	h.beat()
	h.leader.GrantLease(h.ctrl.Lease(0))

	// While healthy and heartbeating, strong reads work.
	if _, err := h.leader.StrongRead("key"); err != nil {
		t.Fatalf("healthy leader could not serve a strong read: %v", err)
	}

	// Partition. The controller stops renewing; the lease (2s) lapses well
	// before the failure threshold (5s) is even reached.
	h.tr.Partition("node-1", true)
	h.clk.Advance(3 * time.Second)
	if _, err := h.leader.StrongRead("key"); !errors.Is(err, replication.ErrNoLease) {
		t.Fatalf("strong read after lease expiry = %v, want ErrNoLease", err)
	}

	// And it is still refused after the replacement is installed.
	h.clk.Advance(3 * time.Second)
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	if ev := h.ctrl.Tick(); !hasKind(ev, "promoted") {
		t.Fatalf("expected promotion, got %v", ev)
	}
	if _, err := h.leader.StrongRead("key"); !errors.Is(err, replication.ErrNoLease) {
		t.Fatal("the deposed leader served a strong read after its replacement was installed")
	}
}

// TestLeaseIsNotGrantedToTheNewLeaderImmediately: handing the replacement a
// lease at the instant of promotion would create overlapping authority in
// exactly the window this mechanism protects.
func TestLeaseIsNotGrantedToTheNewLeaderImmediately(t *testing.T) {
	h := newHarness(t, defaultCfg())
	if _, err := h.put("key"); err != nil {
		t.Fatal(err)
	}
	h.beat()
	h.clk.Advance(6 * time.Second)
	for id, f := range h.fols {
		h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
			0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
		}})
	}
	h.ctrl.Tick()
	if l := h.ctrl.Lease(0); l.Holder != "" {
		t.Fatalf("a lease was granted at the moment of promotion: %+v", l)
	}
	// It appears only once the new leader heartbeats.
	nl := h.newLd[0]
	epoch, applied, isr := nl.Report()
	h.ctrl.Heartbeat(Heartbeat{Node: nl.ID, Shards: map[core.ShardID]ShardReport{
		0: {Epoch: epoch, Applied: applied, IsLeader: true, ISR: isr},
	}})
	if l := h.ctrl.Lease(0); l.Holder != nl.ID {
		t.Fatalf("new leader has no lease after heartbeating: %+v", l)
	}
}

// TestFailoverWindowIsMeasured: the write-unavailability window is a number the
// system reports, not a claim. It is bounded below by the failure threshold,
// because no detector beats its own timeout.
func TestFailoverWindowIsMeasured(t *testing.T) {
	cfg := defaultCfg()
	h := newHarness(t, cfg)
	if _, err := h.put("a"); err != nil {
		t.Fatal(err)
	}
	h.beat()
	died := h.clk.Now()

	for elapsed := time.Duration(0); elapsed < 7*time.Second; elapsed += time.Second {
		h.clk.Advance(time.Second)
		for id, f := range h.fols {
			h.ctrl.Heartbeat(Heartbeat{Node: id, Shards: map[core.ShardID]ShardReport{
				0: {Epoch: f.Shard().Epoch(), Applied: f.Shard().AppliedLSN()},
			}})
		}
		if hasKind(h.ctrl.Tick(), "promoted") {
			break
		}
	}
	at, ok := h.ctrl.FailoverAt(0)
	if !ok {
		t.Fatal("no failover recorded")
	}
	window := at.Sub(died)
	if window < cfg.Detector.DeadAfter {
		t.Fatalf("failover window %s is shorter than the failure threshold %s; "+
			"the detector cannot beat its own timeout", window, cfg.Detector.DeadAfter)
	}
	t.Logf("measured write-unavailability window: %s (failure threshold %s)",
		window, cfg.Detector.DeadAfter)
}

func hasKind(events []Event, kind string) bool {
	for _, e := range events {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
