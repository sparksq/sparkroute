// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package modelrouter

import "testing"

func TestDeploymentMetadataOverridesPricesSurvivesRefreshAndCanBeRemoved(t *testing.T) {
	policy := DefaultRoutingPolicy()
	policy.Models["local"] = ModelMetadata{Enabled: true, InputPrice: 99, SizeB: 100, Weight: 3, Priority: 7}
	router, err := NewRouterManager(policy)
	if err != nil {
		t.Fatal(err)
	}
	zero, size, discoveredPrice := 0.0, 8.0, 50.0
	fields := map[string]DiscoveredModelMetadata{"local": {InputPrice: &zero, OutputPrice: &zero, SizeB: &size}}
	if err := router.ReplaceDeploymentMetadata(fields); err != nil {
		t.Fatal(err)
	}
	size = 999 // callers cannot mutate the compiled snapshot
	if err := router.ReplaceDiscoveredMetadata(DiscoveredMetadataSnapshot{Source: "live", Models: map[string]DiscoveredModelMetadata{"local": {InputPrice: &discoveredPrice}}}); err != nil {
		t.Fatal(err)
	}
	got := router.runtime.Load().policy.Models["local"]
	if got.InputPrice != 0 || got.SizeB != 8 || got.Weight != 3 || got.Priority != 7 {
		t.Fatalf("effective: %+v", got)
	}
	if router.policy.Models["local"].InputPrice != 99 {
		t.Fatal("mutated durable policy")
	}
	// Installing a routing edit retains deployment attributes.
	router.installPolicyLocked(policy)
	if router.runtime.Load().policy.Models["local"].InputPrice != 0 {
		t.Fatal("policy update lost zero price")
	}
	if err := router.ReplaceDeploymentMetadata(nil); err != nil {
		t.Fatal(err)
	}
	if router.runtime.Load().policy.Models["local"].InputPrice != 99 {
		t.Fatal("removal retained stale metadata")
	}
	invalid := -1.0
	if err := router.ReplaceDeploymentMetadata(map[string]DiscoveredModelMetadata{"local": {InputPrice: &invalid}}); err == nil {
		t.Fatal("accepted negative price")
	}
}
