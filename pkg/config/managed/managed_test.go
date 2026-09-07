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
