package main

import (
	"fmt"
	"time"

	"github.com/eskhakha/vecdb/internal/cluster"
	"github.com/eskhakha/vecdb/internal/controller"
	"github.com/eskhakha/vecdb/internal/core"
	"github.com/eskhakha/vecdb/internal/raft"
)

// splitbrainStory runs the same partition twice: once with a single controller
// that owns the shard map, once with the map behind a replicated log.
//
// The pair is the point. A consensus layer shown only in its working state
// looks like ceremony; shown against the failure it removes, it is obviously
// load-bearing.
func (c *demo) splitbrainStory() {
	const (
		leader  = cluster.NodeID("node-1")
		replica = cluster.NodeID("node-2")
		spare   = cluster.NodeID("node-3")
	)
	nodes := []cluster.NodeID{leader, replica, spare}
	cfg := controller.Config{
		Detector:      cluster.DetectorConfig{SuspectAfter: 1500 * time.Millisecond, DeadAfter: 5 * time.Second},
		LeaseDuration: 2 * time.Second,
	}

	fmt.Println("== a controller that has been partitioned away from a HEALTHY leader ==")
	fmt.Println()
	fmt.Println("node-1 leads shard 0 and is perfectly fine.")
	fmt.Println("The controller simply cannot reach it -- and from inside a partition,")
	fmt.Println("\"I cannot reach node-1\" and \"node-1 is dead\" are the same observation.")
	fmt.Println()

	// ---- 1. single controller ----
	m, err := cluster.NewShardMap(1, nodes, 2)
	if err != nil {
		fatal(c.log, "shard map", err)
	}
	m.Shards[0] = cluster.ShardPlacement{Epoch: 1, Leader: leader, Replicas: []cluster.NodeID{replica}}

	clk := cluster.NewManualClock()
	var localPromoted []cluster.NodeID
	local, err := controller.New(m, cfg, clk, func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
		localPromoted = append(localPromoted, to)
		return nil
	})
	if err != nil {
		fatal(c.log, "local controller", err)
	}
	beat(local, leader, true, replica)
	beat(local, replica, false, replica)
	local.Tick()
	for i := 0; i < 8; i++ {
		clk.Advance(time.Second)
		beat(local, replica, false, replica)
		local.Tick()
	}
	p := local.Map().Shards[0]
	fmt.Printf("single controller  : promoted %v, map now leader=%s epoch=%d\n", localPromoted, p.Leader, p.Epoch)
	fmt.Println("                     node-1 is still alive, still serving, and still")
	fmt.Println("                     believes it leads. That is two leaders.")
	fmt.Println()

	// ---- 2. the same thing, behind a replicated log ----
	sim := raft.NewSim(5, 7)
	stores := map[raft.NodeID]*controller.RaftStore{}
	for _, id := range sim.IDs() {
		stores[id] = controller.NewRaftStore(sim.Node(id), sim, nodes)
	}
	var rl *raft.Node
	for i := 0; i < 400 && rl == nil; i++ {
		sim.Step()
		rl = sim.AnyLeader()
	}
	if rl == nil {
		fatal(c.log, "raft", fmt.Errorf("no leader elected"))
	}
	if err := stores[rl.ID()].Propose([]raft.Command{{
		Kind: raft.CmdPromote, Shard: 0, Node: leader, Epoch: 1,
		Replicas: []cluster.NodeID{replica},
	}}); err != nil {
		fatal(c.log, "seed placement", err)
	}
	sim.RunFor(60)

	// Isolate the controller's raft node into a 2-of-5 minority.
	old := sim.AnyLeader().ID()
	minority := []raft.NodeID{old}
	var majority []raft.NodeID
	for _, id := range sim.IDs() {
		if id != old {
			majority = append(majority, id)
		}
	}
	minority = append(minority, majority[0])
	majority = majority[1:]
	sim.Partition(minority)

	clk2 := cluster.NewManualClock()
	var raftPromoted []cluster.NodeID
	isolated, err := controller.NewWithStore(stores[old], cfg, clk2,
		func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
			raftPromoted = append(raftPromoted, to)
			return nil
		})
	if err != nil {
		fatal(c.log, "raft controller", err)
	}
	beat(isolated, leader, true, replica)
	beat(isolated, replica, false, replica)
	isolated.Tick()

	refused := 0
	for i := 0; i < 12; i++ {
		clk2.Advance(time.Second)
		beat(isolated, replica, false, replica)
		for _, e := range isolated.Tick() {
			if e.Kind == "not_decider" {
				refused++
			}
		}
		sim.RunFor(10)
	}
	rp := stores[old].Map().Shards[0]
	fmt.Printf("raft-backed        : promoted %v, refused %d times\n", raftPromoted, refused)
	fmt.Printf("                     map on the isolated node itself: leader=%s epoch=%d\n", rp.Leader, rp.Epoch)
	fmt.Println("                     it decided exactly the same thing and could not commit it,")
	fmt.Println("                     so the decision never happened -- not even locally.")
	fmt.Println()

	// ---- 3. the majority side is unaffected and can still act ----
	var mLeader *raft.Node
	for i := 0; i < 600 && mLeader == nil; i++ {
		sim.Step()
		mLeader = sim.LeaderIn(majority)
	}
	if mLeader == nil {
		fatal(c.log, "raft", fmt.Errorf("majority elected no leader"))
	}
	clk3 := cluster.NewManualClock()
	var majPromoted []cluster.NodeID
	healthy, err := controller.NewWithStore(stores[mLeader.ID()], cfg, clk3,
		func(_ core.ShardID, to cluster.NodeID, _ core.Epoch) error {
			majPromoted = append(majPromoted, to)
			return nil
		})
	if err != nil {
		fatal(c.log, "majority controller", err)
	}
	beat(healthy, leader, true, replica)
	beat(healthy, replica, false, replica)
	healthy.Tick()
	for i := 0; i < 12; i++ {
		clk3.Advance(time.Second)
		beat(healthy, replica, false, replica)
		healthy.Tick()
		sim.RunFor(10)
	}
	fmt.Printf("majority side      : %s became raft leader and promoted %v\n", mLeader.ID(), majPromoted)
	fmt.Println("                     availability is preserved -- consensus removes the")
	fmt.Println("                     wrong decision, not the ability to decide.")
	fmt.Println()

	sim.Heal()
	sim.RunFor(400)
	hp := stores[old].Map().Shards[0]
	fmt.Printf("after healing      : the isolated node converged on leader=%s epoch=%d\n", hp.Leader, hp.Epoch)
	fmt.Println("                     its own uncommitted decision was discarded, and it")
	fmt.Println("                     adopted the majority's -- which is the same decision,")
	fmt.Println("                     made by someone entitled to make it.")
	fmt.Println()
}

func beat(c *controller.Controller, n cluster.NodeID, isLeader bool, isr cluster.NodeID) {
	r := controller.ShardReport{Epoch: 1, Applied: 100, IsLeader: isLeader}
	if isLeader {
		r.ISR = []cluster.NodeID{isr}
	}
	c.Heartbeat(controller.Heartbeat{Node: n, Shards: map[core.ShardID]controller.ShardReport{0: r}})
}
