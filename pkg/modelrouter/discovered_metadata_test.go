// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestDiscoveredMetadataEnrichesRuntimeWithoutMutatingPolicy(t *testing.T) {
	t.Parallel()

	router := discoveredMetadataTestRouter(t, map[string]ModelMetadata{
		"alpha": {Enabled: true},
		"beta":  {Enabled: true},
	})
	router.ObserveRoute("alpha", 25*time.Millisecond, 200)
	if err := router.ReplaceDiscoveredMetadata(DiscoveredMetadataSnapshot{
		Source: "sparkrun:generation-a",
		Models: map[string]DiscoveredModelMetadata{
			"alpha": {SizeB: floatPointer(70), Context: intPointer(65_536), Tags: []string{"local", "chat"}},
			"beta":  {SizeB: floatPointer(7), Context: intPointer(32_768), Tags: []string{"chat"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	decision, err := router.Route(context.Background(), Input{
		RequestedModel: "auto",
		Candidates:     []Candidate{{Name: "alpha"}, {Name: "beta"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.VirtualModel != "beta" {
		t.Fatalf("selected model = %q, want beta", decision.VirtualModel)
	}
	policy := router.Policy()
	if policy.Revision != 7 || policy.Models["alpha"].SizeB != 0 || policy.Models["beta"].Context != 0 {
		t.Fatalf("authored policy was mutated: %#v", policy)
	}
	if got := router.Observations()["alpha"].Requests; got != 1 {
		t.Fatalf("observation requests = %d, want 1", got)
	}
	plan, err := router.Plan("auto", NormalizedRequest{}, []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Models["alpha"].Context != 65_536 || plan.Models["beta"].SizeB != 7 {
		t.Fatalf("effective runtime metadata = %#v", plan.Models)
	}
}

func TestDiscoveredMetadataPrecedenceAggregationAndRemoval(t *testing.T) {
	t.Parallel()

	router := discoveredMetadataTestRouter(t, map[string]ModelMetadata{
		"alpha": {Enabled: true, SizeB: 13, Tags: []string{"operator"}},
		"beta":  {Enabled: true, DiscoveryDisabled: true},
	})
	observed := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	for _, snapshot := range []DiscoveredMetadataSnapshot{
		{
			Source: "source-b", ObservedAt: observed.Add(time.Minute),
			Models: map[string]DiscoveredModelMetadata{
				"alpha": {SizeB: floatPointer(70), InputPrice: floatPointer(2), Context: intPointer(16_384), Tags: []string{"gpu"}},
				"beta":  {SizeB: floatPointer(4), InputPrice: floatPointer(0)},
			},
		},
		{
			Source: "source-a", ObservedAt: observed,
			Models: map[string]DiscoveredModelMetadata{
				"alpha": {SizeB: floatPointer(60), InputPrice: floatPointer(1), Context: intPointer(32_768), Tags: []string{"chat", "gpu"}},
			},
		},
	} {
		if err := router.ReplaceDiscoveredMetadata(snapshot); err != nil {
			t.Fatal(err)
		}
	}
	state := router.DiscoveredMetadata()
	if state.Generation != 2 || len(state.Sources) != 2 || state.Sources[0].Source != "source-a" {
		t.Fatalf("metadata state = %#v", state)
	}
	effective := state.Effective["alpha"]
	if effective.SizeB == nil || *effective.SizeB != 70 ||
		effective.InputPrice == nil || *effective.InputPrice != 2 ||
		effective.Context == nil || *effective.Context != 16_384 ||
		len(effective.Tags) != 2 || effective.Tags[0] != "chat" || effective.Tags[1] != "gpu" {
		t.Fatalf("effective discovery metadata = %#v", effective)
	}
	plan, err := router.Plan("auto", NormalizedRequest{}, []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Models["alpha"].SizeB != 13 || plan.Models["alpha"].InputPrice != 2 ||
		len(plan.Models["alpha"].Tags) != 1 || plan.Models["alpha"].Tags[0] != "operator" {
		t.Fatalf("operator precedence was not preserved: %#v", plan.Models["alpha"])
	}
	if plan.Models["beta"].SizeB != 0 || plan.Models["beta"].InputPrice != 0 {
		t.Fatalf("discovery-disabled metadata was enriched: %#v", plan.Models["beta"])
	}
	if err := router.RemoveDiscoveredMetadata("source-b"); err != nil {
		t.Fatal(err)
	}
	state = router.DiscoveredMetadata()
	if state.Generation != 3 || len(state.Sources) != 1 || *state.Effective["alpha"].SizeB != 60 {
		t.Fatalf("metadata state after removal = %#v", state)
	}
}

func TestDiscoveredMetadataRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	router := discoveredMetadataTestRouter(t, map[string]ModelMetadata{
		"alpha": {Enabled: true},
		"beta":  {Enabled: true},
	})
	for _, snapshot := range []DiscoveredMetadataSnapshot{
		{Source: "", Models: map[string]DiscoveredModelMetadata{}},
		{Source: "source", Models: map[string]DiscoveredModelMetadata{"alpha": {Context: intPointer(-1)}}},
		{Source: "source", Models: map[string]DiscoveredModelMetadata{"alpha": {SizeB: floatPointer(math.NaN())}}},
		{Source: "source", Models: map[string]DiscoveredModelMetadata{"alpha": {Tags: []string{""}}}},
	} {
		if err := router.ReplaceDiscoveredMetadata(snapshot); err == nil {
			t.Fatalf("invalid snapshot was accepted: %#v", snapshot)
		}
	}
	if state := router.DiscoveredMetadata(); state.Generation != 0 || len(state.Sources) != 0 {
		t.Fatalf("invalid metadata changed state: %#v", state)
	}
}

func discoveredMetadataTestRouter(
	t *testing.T,
	metadata map[string]ModelMetadata,
) *RouterManager {
	t.Helper()
	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 7, DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {Strategy: "smallest", Models: []string{"alpha", "beta"}},
		},
		Models: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func floatPointer(value float64) *float64 { return &value }
func intPointer(value int) *int           { return &value }
