// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package managed

import (
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func TestMergeRejectsMultipleModelRoutingOwners(t *testing.T) {
	t.Parallel()

	policy := modelrouter.DefaultRoutingPolicy()
	_, err := Merge(map[Owner]config.Document{
		OwnerOperator: {ModelRouting: &policy},
		OwnerSparkrun: {ModelRouting: &policy},
	})
	if err == nil || !strings.Contains(err.Error(), "model_routing") {
		t.Fatalf("Merge() error = %v", err)
	}
}

func TestMergeConcatenatesOwnersAndValidatesWholeDocument(t *testing.T) {
	t.Parallel()

	merged, err := Merge(map[Owner]config.Document{
		OwnerOperator: {
			CapabilityDefaults: config.CapabilityDefaults{
				Unknown: config.UnknownCapabilityReject,
			},
			VirtualModels: []config.VirtualModel{{
				Name: "public", Pools: []config.RoutingPool{{
					Priority: 0, Targets: []config.WeightedTarget{{Deployment: "generated", Weight: 1}},
				}},
			}},
		},
		OwnerSparkrun: {
			Providers: []config.Provider{{
				Name: "sparkrun", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1",
				CapabilityDefaults: config.CapabilityDefaults{
					Unknown: config.UnknownCapabilityTry,
				},
			}},
			Deployments: []config.Deployment{{
				Name: "generated", Provider: "sparkrun", Model: "upstream",
			}},
		},
	})
	if err != nil {
		t.Fatalf("Merge() error = %v", err)
	}
	if len(merged.Providers) != 1 || len(merged.Deployments) != 1 || len(merged.VirtualModels) != 1 {
		t.Fatalf("Merge() = %#v", merged)
	}
	if merged.CapabilityDefaults.Unknown != config.UnknownCapabilityReject ||
		merged.Providers[0].CapabilityDefaults.Unknown != config.UnknownCapabilityTry {
		t.Fatalf("merged capability defaults = %#v / %#v", merged.CapabilityDefaults, merged.Providers[0].CapabilityDefaults)
	}
}

func TestMergeRejectsCapabilityDefaultCollision(t *testing.T) {
	t.Parallel()

	_, err := Merge(map[Owner]config.Document{
		OwnerOperator: {
			CapabilityDefaults: config.CapabilityDefaults{Unknown: config.UnknownCapabilityReject},
		},
		OwnerSparkrun: {
			CapabilityDefaults: config.CapabilityDefaults{Unknown: config.UnknownCapabilityTry},
		},
	})
	if err == nil ||
		!strings.Contains(err.Error(), `capability_defaults owned by "sparkrun"`) ||
		!strings.Contains(err.Error(), `capability_defaults owned by "operator"`) {
		t.Fatalf("Merge() error = %v", err)
	}
}

func TestMergeRejectsCrossOwnerCollision(t *testing.T) {
	t.Parallel()

	provider := config.Provider{Name: "duplicate", Type: "openai_compatible", BaseURL: "http://127.0.0.1/v1"}
	_, err := Merge(map[Owner]config.Document{
		OwnerOperator: {Providers: []config.Provider{provider}},
		OwnerSparkrun: {Providers: []config.Provider{provider}},
	})
	if err == nil ||
		!strings.Contains(err.Error(), `provider "duplicate"`) ||
		!strings.Contains(err.Error(), `owned by "sparkrun"`) ||
		!strings.Contains(err.Error(), `owned by "operator"`) {
		t.Fatalf("Merge() error = %v", err)
	}
}

func TestMergeNamesAliasCollisionOwners(t *testing.T) {
	t.Parallel()

	_, err := Merge(map[Owner]config.Document{
		OwnerOperator: {
			VirtualModels: []config.VirtualModel{{Name: "fast"}},
		},
		OwnerSparkrun: {
			VirtualModels: []config.VirtualModel{{
				Name: "generated", Aliases: []string{"fast"},
			}},
		},
	})
	if err == nil ||
		!strings.Contains(err.Error(), `alias "fast" for virtual model "generated"`) ||
		!strings.Contains(err.Error(), `owned by "sparkrun"`) ||
		!strings.Contains(err.Error(), `virtual model "fast" owned by "operator"`) {
		t.Fatalf("Merge() error = %v", err)
	}
}

func TestMergeNamesCrossOwnerReferrer(t *testing.T) {
	t.Parallel()

	_, err := Merge(map[Owner]config.Document{
		OwnerOperator: {
			VirtualModels: []config.VirtualModel{{
				Name: "operator-model",
				Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
					Deployment: "removed-generated", Weight: 1,
				}}}},
			}},
		},
		OwnerSparkrun: EmptyDocument(),
	})
	if err == nil ||
		!strings.Contains(err.Error(), `virtual model "operator-model" owned by "operator"`) ||
		!strings.Contains(err.Error(), `missing deployment "removed-generated"`) {
		t.Fatalf("Merge() error = %v", err)
	}
}

func TestFragmentRevisionNormalizesNilCollections(t *testing.T) {
	t.Parallel()

	raw, revision, err := FragmentRevision(config.Document{})
	if err != nil {
		t.Fatal(err)
	}
	if len(revision) != 64 || string(raw) != `{"providers":[],"deployments":[],"virtual_models":[]}` {
		t.Fatalf("FragmentRevision() = %s, %s", raw, revision)
	}
}

func TestOperatorPolicyAssignmentsSurviveGeneratedInventoryChurn(t *testing.T) {
	ref := "personal"
	operator := config.Document{PIIProfiles: map[string]config.PIIPolicy{"personal": {Entities: []config.PIIEntity{config.PIIEntityEmail}}}, ModelPolicies: map[string]config.ModelPolicyAssignment{"coding": {PIIProfile: &ref}}}
	generated := config.Document{
		Providers:     []config.Provider{{Name: "sparkrun", Type: "openai", BaseURL: "http://127.0.0.1:8000/v1"}},
		Deployments:   []config.Deployment{{Name: "generated", Provider: "sparkrun", Model: "upstream"}},
		VirtualModels: []config.VirtualModel{{Name: "coding", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "generated", Weight: 1}}}}}},
	}
	original, _, _ := FragmentRevision(generated)
	for _, inventory := range []config.Document{generated, EmptyDocument(), generated} {
		merged, err := Merge(map[Owner]config.Document{OwnerOperator: operator, OwnerSparkrun: inventory})
		if err != nil {
			t.Fatal(err)
		}
		if *merged.ModelPolicies["coding"].PIIProfile != "personal" {
			t.Fatal("assignment lost")
		}
		effective, err := merged.ResolveModelPolicies()
		if err != nil {
			t.Fatal(err)
		}
		if len(inventory.VirtualModels) > 0 && effective.VirtualModels[0].Privacy.PII.Entities[0] != config.PIIEntityEmail {
			t.Fatal("returned model did not inherit policy")
		}
		merged.PIIProfiles["personal"].Entities[0] = config.PIIEntityPhone
		*merged.ModelPolicies["coding"].PIIProfile = "changed"
		if operator.PIIProfiles["personal"].Entities[0] != config.PIIEntityEmail || ref != "personal" {
			t.Fatal("merged policy aliases owner storage")
		}
	}
	after, _, _ := FragmentRevision(generated)
	if string(original) != string(after) {
		t.Fatal("generated fragment mutated")
	}
	_, err := Merge(map[Owner]config.Document{OwnerSparkrun: operator})
	if err == nil || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("allowed generated policy ownership: %v", err)
	}
	delete(operator.PIIProfiles, "personal")
	if _, err = Merge(map[Owner]config.Document{OwnerOperator: operator}); err == nil {
		t.Fatal("allowed deleting referenced dormant profile")
	}
}
