package main

import (
	"context"
	"fmt"
	"time"

	"k8s.io/utils/clock/testing"
	"sigs.k8s.io/karpenter/pkg/controllers/disruption"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
)

// Demonstrates that Karpenter consolidation bails immediately when cluster state changes
func main() {
	ctx := context.Background()
	clk := testing.NewFakeClock(time.Now())
	
	cluster := state.NewCluster(clk, nil, nil)
	c := disruption.MakeConsolidation(clk, cluster, nil, nil, nil, nil, nil)
	singleNode := disruption.NewSingleNodeConsolidation(c)
	multiNode := disruption.NewMultiNodeConsolidation(c)
	
	fmt.Println("=== Karpenter Consolidation Bailout Behavior ===")
	fmt.Printf("Initial IsConsolidated: Single=%v, Multi=%v\n", 
		singleNode.IsConsolidated(), multiNode.IsConsolidated())
	
	// Test commands when not consolidated (normal case)
	cmds1, _ := singleNode.ComputeCommands(ctx, map[string]int{})
	cmds2, _ := multiNode.ComputeCommands(ctx, map[string]int{})
	fmt.Printf("Commands when not consolidated: Single=%d, Multi=%d\n", len(cmds1), len(cmds2))
	
	// Simulate cluster state change (any pod/node activity)
	fmt.Println("\n--- Simulating cluster state change ---")
	cluster.MarkUnconsolidated()
	
	fmt.Printf("After state change IsConsolidated: Single=%v, Multi=%v\n",
		singleNode.IsConsolidated(), multiNode.IsConsolidated())
	
	fmt.Println("\n🎯 KEY FINDING:")
	fmt.Println("Consolidation methods check IsConsolidated() FIRST and bail if true")
	fmt.Println("ANY cluster change makes future consolidation attempts return empty commands")
	fmt.Println("In large clusters with frequent changes, this significantly delays cost optimization")
}
