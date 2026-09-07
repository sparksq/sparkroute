// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestNativeStrategies(t *testing.T) {
	t.Parallel()

	metadata := map[string]ModelMetadata{
		"small": {Enabled: true, Weight: 1, Priority: 1, SizeB: 7, InputPrice: 1, OutputPrice: 1},
		"large": {Enabled: true, Weight: 2, Priority: 2, SizeB: 70, InputPrice: 5, OutputPrice: 5},
	}
	tests := []struct {
		strategy string
		want     string
	}{
		{strategy: "smallest", want: "small"},
		{strategy: "largest", want: "large"},
		{strategy: "lowest_cost", want: "small"},
		{strategy: "balanced", want: "small"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.strategy, func(t *testing.T) {
			t.Parallel()
			router := testRouter(t, test.strategy, metadata)
			decision, err := router.Route(context.Background(), Input{
				RequestedModel: "auto",
				Candidates:     []Candidate{{Name: "small"}, {Name: "large"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			if decision.VirtualModel != test.want {
				t.Fatalf("selected = %q, want %q", decision.VirtualModel, test.want)
			}
		})
	}
}

func TestRoundRobinWeightedRoundRobinAndRandom(t *testing.T) {
	t.Parallel()

	metadata := map[string]ModelMetadata{
		"alpha": {Enabled: true, Weight: 1},
		"beta":  {Enabled: true, Weight: 2},
	}
	candidates := []string{"alpha", "beta"}
	roundRobin := testRouter(t, "round_robin", metadata)
	for index, want := range []string{"alpha", "beta", "alpha", "beta"} {
		decision, err := roundRobin.Select("auto", NormalizedRequest{}, candidates, false)
		if err != nil || decision.ResolvedModel != want {
			t.Fatalf("round robin %d = %+v, %v; want %q", index, decision, err, want)
		}
	}
	weighted := testRouter(t, "weighted_round_robin", metadata)
	for index, want := range []string{"alpha", "beta", "beta", "alpha"} {
		decision, err := weighted.Select("auto", NormalizedRequest{}, candidates, false)
		if err != nil || decision.ResolvedModel != want {
			t.Fatalf("weighted round robin %d = %+v, %v; want %q", index, decision, err, want)
		}
	}
	random := testRouter(t, "random", metadata)
	for index := 0; index < 20; index++ {
		decision, err := random.Select("auto", NormalizedRequest{}, candidates, false)
		if err != nil || (decision.ResolvedModel != "alpha" && decision.ResolvedModel != "beta") {
			t.Fatalf("random %d = %+v, %v", index, decision, err)
		}
	}
}

func TestFastestUsesCompleteGatewayObservations(t *testing.T) {
	t.Parallel()

	router := testRouter(t, "fastest", map[string]ModelMetadata{
		"alpha": {Enabled: true},
		"beta":  {Enabled: true},
	})
	router.ObserveRoute("alpha", 80*time.Millisecond, 200)
	router.ObserveRoute("beta", 10*time.Millisecond, 200)
	decision, err := router.Select(
		"auto", NormalizedRequest{}, []string{"alpha", "beta"}, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ResolvedModel != "beta" {
		t.Fatalf("selected = %q, want beta", decision.ResolvedModel)
	}
}

func TestKeywordAliasAndPreset(t *testing.T) {
	t.Parallel()

	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 9, DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {
				Strategy: "smallest", Models: []string{"small", "large"}, Aliases: []string{"smart"},
				Kwargs: map[string]map[string]any{"high": {"strategy": "largest"}},
			},
		},
		Models: map[string]ModelMetadata{
			"small": {Enabled: true, SizeB: 7},
			"large": {Enabled: true, SizeB: 70},
		},
		KeywordRules: []KeywordRule{{
			Name: "code", Keywords: []string{"write code"}, Strategy: "largest", Models: []string{"small", "large"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := router.Select(
		"smart", NormalizedRequest{Text: "Please WRITE CODE"}, []string{"small", "large"}, false,
	)
	if err != nil || decision.ResolvedModel != "large" || decision.MatchedRule != "code" {
		t.Fatalf("keyword decision = %+v, %v", decision, err)
	}
	decision, err = router.Select(
		"smart:high", NormalizedRequest{}, []string{"small", "large"}, false,
	)
	if err != nil || decision.ResolvedModel != "large" || decision.RoutingPreset != "high" {
		t.Fatalf("preset decision = %+v, %v", decision, err)
	}
}

func TestRouteDecisionCarriesPresetProviderPriority(t *testing.T) {
	t.Parallel()

	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 4, DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {
				Strategy: "round_robin", Models: []string{"logical"},
				Kwargs: map[string]map[string]any{
					"private": {"provider_priority": []string{"second", "first"}},
				},
			},
		},
		Models: map[string]ModelMetadata{"logical": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision, err := router.Route(context.Background(), Input{
		RequestedModel: "auto:private",
		Candidates:     []Candidate{{Name: "logical"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if decision.VirtualModel != "logical" ||
		strings.Join(decision.ProviderPriority, ",") != "second,first" {
		t.Fatalf("route decision = %#v", decision)
	}
	decision.ProviderPriority[0] = "mutated"
	next, err := router.Route(context.Background(), Input{
		RequestedModel: "auto:private",
		Candidates:     []Candidate{{Name: "logical"}},
	})
	if err != nil || next.ProviderPriority[0] != "second" {
		t.Fatalf("provider priority was not isolated: %#v, %v", next, err)
	}
}

func testRouter(t *testing.T, strategy string, models map[string]ModelMetadata) *RouterManager {
	t.Helper()
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 1, DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{"auto": {Strategy: strategy, Models: names}},
		Models:        models,
	})
	if err != nil {
		t.Fatal(err)
	}
	return router
}
