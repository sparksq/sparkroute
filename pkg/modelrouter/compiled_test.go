// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"fmt"
	"sync"
	"testing"
)

func compiledRoutingTestPolicy() RoutingPolicy {
	return RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 7, DefaultVirtualModel: "auto",
		Models: map[string]ModelMetadata{
			"alpha": {Enabled: true, Weight: 1},
			"beta":  {Enabled: true, Weight: 2},
		},
		VirtualModels: map[string]VirtualModel{
			"auto": {
				Strategy: "round_robin", Models: []string{"alpha", "beta"}, Aliases: []string{"smart"},
				Kwargs: map[string]map[string]any{"high": {"strategy": "largest", "models": []any{"beta"}, "nested": map[string]any{"integer": int64(9007199254740993)}}},
			},
		},
		KeywordRules: []KeywordRule{{Name: "beta-rule", Keywords: []string{"BETA"}, Strategy: "largest", Models: []string{"beta"}}},
	}
}

func TestCompiledRoutingSnapshotIsIsolatedAndIndexed(t *testing.T) {
	policy := compiledRoutingTestPolicy()
	router, err := NewRouterManager(policy)
	if err != nil {
		t.Fatal(err)
	}
	policy.VirtualModels["auto"] = VirtualModel{Strategy: "smallest", Models: []string{"alpha"}}
	policy.Models["beta"] = ModelMetadata{Enabled: false}

	plan, err := router.Plan("smart:high", NormalizedRequest{}, []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.VirtualModel != "auto" || plan.VirtualAlias != "smart" || plan.RoutingPreset != "high" || plan.Strategy != "largest" || len(plan.Eligible) != 1 || plan.Eligible[0] != "beta" {
		t.Fatalf("unexpected indexed plan: %+v", plan)
	}
	nested := plan.RouterKwargs["nested"].(map[string]any)
	if integer, ok := nested["integer"].(int64); !ok || integer != 9007199254740993 {
		t.Fatalf("kwargs clone lost scalar type/value: %#v", nested["integer"])
	}
	if !router.OwnsVirtualRequest("smart:missing") || router.OwnsVirtualRequest("physical:missing") {
		t.Fatal("compiled virtual ownership index is incorrect")
	}
}

func TestCompiledRouterConcurrentPublishPlanAndMetrics(t *testing.T) {
	router, err := NewRouterManager(compiledRoutingTestPolicy())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				plan, planErr := router.Plan("smart", NormalizedRequest{Text: fmt.Sprintf("request %d", i)}, []string{"alpha", "beta"})
				if planErr != nil {
					t.Errorf("plan: %v", planErr)
					return
				}
				decision, resolveErr := router.ResolveNative(plan, false)
				if resolveErr != nil {
					t.Errorf("resolve: %v", resolveErr)
					return
				}
				handle := router.Begin(decision.ResolvedModel)
				router.End(handle, 200)
			}
		}(worker)
	}
	for revision := 0; revision < 20; revision++ {
		current := router.Policy()
		if err := router.SetPolicy(current, current.Revision); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func BenchmarkRouterPlanCompiled(b *testing.B) {
	router, err := NewRouterManager(compiledRoutingTestPolicy())
	if err != nil {
		b.Fatal(err)
	}
	available := []string{"alpha", "beta"}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := router.Plan("smart:high", NormalizedRequest{Text: "ordinary request"}, available); err != nil {
				b.Fatal(err)
			}
		}
	})
}
