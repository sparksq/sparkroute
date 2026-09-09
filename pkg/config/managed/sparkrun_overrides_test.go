// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package managed

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func exclusionFixture() config.Document {
	model := func(name string, targets ...string) config.VirtualModel {
		pool := config.RoutingPool{Priority: 3}
		for _, target := range targets {
			pool.Targets = append(pool.Targets, config.WeightedTarget{Deployment: target, Weight: 17})
		}
		return config.VirtualModel{Name: name, Aliases: []string{name + "-alias"}, Pools: []config.RoutingPool{pool}}
	}
	return config.Document{
		Providers:     []config.Provider{{Name: "sparkrun", Type: "openai", BaseURL: "http://127.0.0.1:8000/v1"}},
		Deployments:   []config.Deployment{{Name: "stopped", Provider: "sparkrun", Model: "upstream"}, {Name: "kept", Provider: "sparkrun", Model: "other"}},
		VirtualModels: []config.VirtualModel{model("stopped", "stopped"), model("stopped:low", "stopped"), model("shared", "stopped", "kept")},
	}
}

func TestExclusionsRemoveGeneratedNamesAndKeepOtherTargetsWithoutMutatingFragments(t *testing.T) {
	generated := exclusionFixture()
	operator := config.Document{SparkrunOverrides: &config.SparkrunOverrides{ExcludedDeployments: []string{"stopped"}}}
	before, _ := json.Marshal(generated)
	merged, err := Merge(map[Owner]config.Document{OwnerOperator: operator, OwnerSparkrun: generated})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Deployments) != 1 || merged.Deployments[0].Name != "kept" || len(merged.VirtualModels) != 1 || merged.VirtualModels[0].Name != "shared" {
		t.Fatalf("unexpected entities: %#v", merged)
	}
	pool := merged.VirtualModels[0].Pools[0]
	if pool.Priority != 3 || len(pool.Targets) != 1 || pool.Targets[0].Deployment != "kept" || pool.Targets[0].Weight != 17 {
		t.Fatalf("other target changed: %#v", pool)
	}
	merged.SparkrunOverrides.ExcludedDeployments[0] = "mutated-result"
	merged.VirtualModels[0].Pools[0].Targets[0].Weight = 99
	after, _ := json.Marshal(generated)
	if string(before) != string(after) || operator.SparkrunOverrides.ExcludedDeployments[0] != "stopped" {
		t.Fatal("merge mutated an owner fragment")
	}
}

func TestExclusionsReportOperatorReferencesInsteadOfDroppingThem(t *testing.T) {
	generated := exclusionFixture()
	operator := config.Document{SparkrunOverrides: &config.SparkrunOverrides{ExcludedDeployments: []string{"stopped"}}, VirtualModels: []config.VirtualModel{{Name: "user-profile", Pools: generated.VirtualModels[0].Pools}}}
	_, err := Merge(map[Owner]config.Document{OwnerOperator: operator, OwnerSparkrun: generated})
	if err == nil || !strings.Contains(err.Error(), `"user-profile"`) || !strings.Contains(err.Error(), `missing deployment "stopped"`) {
		t.Fatalf("dependency error: %v", err)
	}
	operator.VirtualModels = nil
	policy := modelrouter.DefaultRoutingPolicy()
	policy.Models["stopped"] = modelrouter.ModelMetadata{Enabled: true}
	operator.ModelRouting = &policy
	_, err = Merge(map[Owner]config.Document{OwnerOperator: operator, OwnerSparkrun: generated})
	if err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Fatalf("routing dependency error: %v", err)
	}
}

func TestExclusionsAreOperatorOwnedAndNeverRemoveOperatorDeployments(t *testing.T) {
	generated := exclusionFixture()
	generated.SparkrunOverrides = &config.SparkrunOverrides{ExcludedDeployments: []string{"stopped"}}
	if _, err := Merge(map[Owner]config.Document{OwnerSparkrun: generated}); err == nil || !strings.Contains(err.Error(), "managed by the operator") {
		t.Fatalf("generated owner accepted overrides: %v", err)
	}
	operator := generated
	merged, err := Merge(map[Owner]config.Document{OwnerOperator: operator})
	if err != nil || !reflect.DeepEqual(merged.Deployments, operator.Deployments) {
		t.Fatalf("operator deployment removed: %v", err)
	}
	for _, excluded := range [][]string{{""}, {"a", "a"}, {"a\nb"}, make([]string, 4097)} {
		operator.SparkrunOverrides.ExcludedDeployments = excluded
		if _, err := Merge(map[Owner]config.Document{OwnerOperator: operator}); err == nil {
			t.Fatalf("accepted invalid exclusions: %d entries", len(excluded))
		}
	}
}
