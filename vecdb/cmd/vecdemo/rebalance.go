package main

import (
	"fmt"

	"github.com/eskhakha/vecdb/internal/rebalance"
)

// rebalanceStory runs the shard-move simulation twice: once with the naive
// cutover that loses data, once with the pause that does not.
//
// The pair is the point. A demo of only the correct protocol makes the pause
// phase look like gratuitous downtime somebody should optimise away.
func (c *demo) rebalanceStory() {
	fmt.Println("== moving a shard while it is being written to ==")
	fmt.Println()
	fmt.Println("snapshot -> catch-up -> pause -> cutover -> cleanup")
	fmt.Println()

	// ---- 1. the naive version: cut over as soon as the copy exists ----
	naive := rebalance.NewMove(0, "node-1", "node-3", 20)
	naive.UnsafeCutover = true
	naive.ShipBatch = 1
	naive.SnapshotTicks = 3
	for i := 0; naive.Phase != rebalance.PhaseDone && i < 100; i++ {
		_, _ = naive.Write(fmt.Sprintf("w%d", i))
		naive.Step()
	}
	lost := naive.Lost()
	fmt.Printf("naive cutover      : %d writes acknowledged, %d LOST\n", len(naive.Acked()), len(lost))
	if len(lost) > 0 {
		fmt.Printf("                     first lost record: LSN %d (%s)\n", lost[0].LSN, lost[0].ID)
		fmt.Println("                     the destination was promoted while still behind,")
		fmt.Println("                     so everything above its cursor simply vanished.")
	}
	fmt.Println()

	// ---- 2. the correct version, same load ----
	safe := rebalance.NewMove(0, "node-1", "node-3", 20)
	safe.ShipBatch = 4
	safe.SnapshotTicks = 12
	var acked, refused, pauseTicks int
	for i := 0; safe.Phase != rebalance.PhaseDone && i < 300; i++ {
		for w := 0; w < 2; w++ {
			if _, err := safe.Write(fmt.Sprintf("w%d-%d", i, w)); err != nil {
				refused++
			} else {
				acked++
			}
		}
		if safe.Phase == rebalance.PhasePause {
			pauseTicks++
		}
		safe.Step()
		if err := safe.Check(); err != nil {
			fatal(c.log, "invariant violated mid-move", err)
		}
	}
	fmt.Printf("staged cutover     : %d writes acknowledged, %d LOST\n", acked, len(safe.Lost()))
	fmt.Printf("                     %d writes refused during a %d-tick write pause\n", refused, pauseTicks)
	fmt.Printf("                     epoch %d -> %d at cutover; the old owner is fenced\n", 1, safe.Epoch)
	fmt.Println("                     an honest refusal beats an ack the system cannot keep.")
	fmt.Println()

	// ---- 3. failure during the move costs nothing ----
	ab := rebalance.NewMove(0, "node-1", "node-3", 20)
	ab.ShipBatch = 4
	ab.SnapshotTicks = 6
	for i := 0; ab.Phase != rebalance.PhaseCatchUp && i < 100; i++ {
		_, _ = ab.Write(fmt.Sprintf("w%d", i))
		ab.Step()
	}
	ab.Dst.Down = true
	ab.Step()
	_, err := ab.Write("immediately-after-abort")
	fmt.Printf("destination dies   : phase=%s reason=%q\n", ab.Phase, ab.Reason)
	fmt.Printf("                     %d acknowledged writes lost, writes resume on %s (err=%v)\n",
		len(ab.Lost()), ab.Writer, err)
	fmt.Println("                     the source is untouched until the cutover commits,")
	fmt.Println("                     which is exactly why cleanup runs last.")
	fmt.Println()
}
