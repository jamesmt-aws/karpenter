/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package disruption

import (
	"context"
	"math"

	disruptionutils "sigs.k8s.io/karpenter/pkg/utils/disruption"
)

// Test-side port of the Balanced consolidation policy's score
// functions from jamesmt-aws/karpenter:balanced-impl-pr's
// pkg/controllers/disruption/balanced.go.
//
// Differences from the source:
//
//   - The consolidation threshold (k) is passed as a direct parameter
//     to ScoreMoveK rather than read from a NodePool spec field. The
//     full Balanced policy adds a NodePool API field that we do not
//     bring in here, since the corpus harness only needs the math.
//   - The function is exported as ScoreMoveK to avoid shadowing
//     ScoreMove if it is later cherry-picked from balanced-impl-pr.
//
// Used by the corpus harness to test whether a feasible probe would
// have been rejected by the score gate, and to construct a
// score-gated brute-force oracle for Shape D investigation.

// BalancedScoreResult holds the result of scoring a consolidation move.
type BalancedScoreResult struct {
	Score              float64
	SavingsFraction    float64
	DisruptionFraction float64
	Approved           bool
	Threshold          float64
	K                  int32
}

// NodePoolTotals holds the precomputed totals needed by the scoring function.
type NodePoolTotals struct {
	TotalCost           float64
	TotalDisruptionCost float64
}

// ComputeNodePoolTotalsForCorpus precomputes the totals a NodePool needs
// to score moves. Mirrors balanced-impl-pr's computeNodePoolTotals.
// Exported for use by the corpus harness; the balanced-impl-pr
// equivalent is package-private.
func ComputeNodePoolTotalsForCorpus(ctx context.Context, allCandidates []*Candidate) map[string]NodePoolTotals {
	totalsMap := map[string]NodePoolTotals{}
	for _, c := range allCandidates {
		price := CandidatePrice(c)
		if price == 0 {
			continue
		}
		name := c.NodePool.Name
		totals := totalsMap[name]
		totals.TotalCost += price
		totals.TotalDisruptionCost += 1.0
		for _, p := range c.reschedulablePods {
			evictionCost := disruptionutils.EvictionCost(ctx, p)
			totals.TotalDisruptionCost += math.Max(0, evictionCost)
		}
		totalsMap[name] = totals
	}
	return totalsMap
}

// ComputeMoveDisruptionCostForCorpus computes the disruption cost of
// a move (a candidate set being jointly removed). Per-node base of
// 1.0 plus sum(max(0, EvictionCost(pod))).
func ComputeMoveDisruptionCostForCorpus(ctx context.Context, candidates []*Candidate) float64 {
	cost := float64(len(candidates))
	for _, c := range candidates {
		for _, p := range c.reschedulablePods {
			evictionCost := disruptionutils.EvictionCost(ctx, p)
			cost += math.Max(0, evictionCost)
		}
	}
	return cost
}

// ProbeFeasibilityForCorpus calls computeConsolidation once on the
// given candidate set and reports whether the result is one the
// production algorithm would treat as feasible. Mirrors the validity
// check in binarySearchPrefix: Delete is always feasible, Replace
// is feasible only when filterOutSameInstanceType leaves a non-empty
// replacement option list. Used by the corpus harness to verify
// algorithm decisions against a direct probe.
func ProbeFeasibilityForCorpus(ctx context.Context, c consolidation, candidates ...*Candidate) (bool, error) {
	cmd, err := c.computeConsolidation(ctx, candidates...)
	if err != nil {
		return false, err
	}
	if cmd.Decision() == DeleteDecision {
		return true, nil
	}
	if cmd.Decision() == ReplaceDecision {
		filtered, err := filterOutSameInstanceType(cmd.Replacements[0], candidates)
		if err != nil {
			return false, nil
		}
		return len(filtered.InstanceTypeOptions) > 0, nil
	}
	return false, nil
}

// ScoreMoveK scores a consolidation move at threshold k. A move is
// approved when score >= 1/k.
func ScoreMoveK(savings float64, disruptionCost float64, totals NodePoolTotals, k int32) BalancedScoreResult {
	if totals.TotalCost <= 0 {
		return BalancedScoreResult{Score: 0, Approved: false, K: k}
	}
	savingsFraction := savings / totals.TotalCost
	if savings <= 0 {
		return BalancedScoreResult{
			Score:           0,
			SavingsFraction: savingsFraction,
			Approved:        false,
			K:               k,
		}
	}
	if totals.TotalDisruptionCost <= 0 {
		return BalancedScoreResult{
			Score:           math.Inf(1),
			SavingsFraction: savingsFraction,
			Approved:        savings > 0,
			K:               k,
		}
	}
	disruptionFraction := disruptionCost / totals.TotalDisruptionCost
	score := savingsFraction / disruptionFraction
	threshold := 1.0 / float64(k)
	return BalancedScoreResult{
		Score:              score,
		SavingsFraction:    savingsFraction,
		DisruptionFraction: disruptionFraction,
		Approved:           score >= threshold,
		Threshold:          threshold,
		K:                  k,
	}
}
