// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"testing"
)

func TestDeploymentMetadataReducesFallbacksAndAliasesWithoutInventingValues(t *testing.T) {
	small, large, free, paid, short, long := 8.0, 70.0, 0.0, 2.0, 8192, 65536
	document := Document{
		Deployments: []Deployment{
			{Name: "a", ModelMetadata: &modelrouter.DiscoveredModelMetadata{SizeB: &small, Context: &long, InputPrice: &free, Tags: []string{"local"}}},
			{Name: "b", ModelMetadata: &modelrouter.DiscoveredModelMetadata{SizeB: &large, Context: &short, InputPrice: &paid, Tags: []string{"local", "coding"}}},
			{Name: "unknown"}},
		VirtualModels: []VirtualModel{{Name: "coding", Aliases: []string{"code"}, Pools: []RoutingPool{{Targets: []WeightedTarget{{Deployment: "a"}}}, {Targets: []WeightedTarget{{Deployment: "b"}, {Deployment: "unknown"}}}}}},
	}
	got := document.DeploymentMetadata()
	for _, name := range []string{"coding", "code"} {
		fields := got[name]
		if *fields.SizeB != 70 || *fields.Context != 8192 || *fields.InputPrice != 2 || fields.OutputPrice != nil || len(fields.Tags) != 2 {
			t.Fatalf("%s: %+v", name, fields)
		}
	}
	if len(document.Deployments[0].ModelMetadata.Tags) != 1 {
		t.Fatal("mutated deployment")
	}
}
