// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func sharedProviderDocument() config.Document {
	return config.Document{Providers: []config.Provider{{Name: "sparkrun", Type: "sparkrun"}}}
}

func sharedRecipeDocument() config.Document {
	document := sharedProviderDocument()
	document.Deployments = []config.Deployment{{Name: "stable-id", Provider: "sparkrun", Model: "model",
		EndpointSource: config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun", Revision: "stable-revision", Recipe: "catalog:recipe"}}}
	document.VirtualModels = []config.VirtualModel{{Name: "model", Aliases: []string{"ds4f"}, Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "stable-id", Weight: 1}}}}}}
	return document
}

func TestSharedProviderWorksBeforeAndAfterPluginSync(t *testing.T) {
	for _, pluginFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "recipe-first", true: "plugin-first"}[pluginFirst], func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			if _, err := store.Initialize(ctx, managed.EmptyDocument(), "test", ""); err != nil {
				t.Fatal(err)
			}
			_, revision, _ := store.Load(ctx)
			if pluginFirst {
				result, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, sharedProviderDocument(), managed.ReplaceOptions{ExpectedActive: revision, Actor: "plugin"})
				if err != nil {
					t.Fatal(err)
				}
				revision = result.Current.Version
			}
			draft := sharedRecipeDocument()
			if pluginFirst {
				draft.Providers = nil
			}
			candidate, validation, err := store.BuildCandidate(ctx, managed.OwnerOperator, draft, revision)
			if err != nil || len(candidate.Providers) != 1 {
				t.Fatal(candidate, err)
			}
			// Validate must not persist the fallback provider or the deployment.
			_, still, _ := store.Load(ctx)
			if still != revision {
				t.Fatal("validation changed storage")
			}
			result, err := store.ReplaceSet(ctx, managed.OwnerOperator, draft, managed.ReplaceOptions{ExpectedActive: revision, Actor: "operator"})
			if err != nil || result.Current.Version != validation.CandidateRevision || result.Set.Revision != validation.SetRevision {
				t.Fatal(result, validation, err)
			}
			revision = result.Current.Version
			operator, _ := store.GetSet(ctx, managed.OwnerOperator)
			generated, _ := store.GetSet(ctx, managed.OwnerSparkrun)
			if len(operator.Document.Providers) != 0 || !reflect.DeepEqual(generated.Document.Providers, sharedProviderDocument().Providers) {
				t.Fatal("shared provider did not acquire the read-only owner", operator, generated)
			}
			for _, inventory := range []config.Document{sharedProviderDocument(), managed.EmptyDocument(), sharedProviderDocument()} {
				result, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, inventory, managed.ReplaceOptions{ExpectedActive: revision, Actor: "plugin"})
				if err != nil || result.Changed || result.Current.Version != revision {
					t.Fatal("sync should preserve the shared provider and be idempotent", result, err)
				}
				loaded, _, err := store.Load(ctx)
				if err != nil || len(loaded.Providers) != 1 || loaded.Deployments[0].Provider != "sparkrun" {
					t.Fatal(loaded, err)
				}
			}
			if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, draft, managed.ReplaceOptions{ExpectedActive: still, Actor: "stale"}); !errors.Is(err, ErrRevisionConflict) {
				t.Fatal("stale write accepted", err)
			}
		})
	}
}

func TestPluginSyncMigratesExistingDefaultProviderAtomically(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "test", ""); err != nil {
		t.Fatal(err)
	}
	legacy := sharedRecipeDocument()
	legacy.Providers[0].Name = "sparkrun:operator"
	legacy.Deployments[0].Provider = "sparkrun:operator"
	sets := map[managed.Owner]config.Document{managed.OwnerOperator: legacy, managed.OwnerSparkrun: sharedProviderDocument()}
	// Seed the exact fragments and merged revision an older gateway stored.
	for owner, fragment := range sets {
		raw, revision, err := managed.FragmentRevision(fragment)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, "UPDATE gateway_config_managed_sets SET document_json = ?, revision_id = ? WHERE owner = ?", raw, revision, owner); err != nil {
			t.Fatal(err)
		}
	}
	merged, err := managed.Merge(sets)
	if err != nil {
		t.Fatal(err)
	}
	raw, revision, err := config.EncodeCanonical(merged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE gateway_config_current SET document_json = ?, revision_id = ? WHERE singleton = 1", raw, revision); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Load(ctx); err != nil {
		t.Fatal("legacy database no longer loads", err)
	}
	validation, err := store.ValidateSet(ctx, managed.OwnerSparkrun, sharedProviderDocument(), revision)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, _, _ := store.Load(ctx)
	if len(unchanged.Providers) != 2 {
		t.Fatal("validation migrated storage")
	}
	result, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, sharedProviderDocument(), managed.ReplaceOptions{ExpectedActive: revision, Actor: "plugin"})
	if err != nil || !result.Changed || result.Current.Version != validation.CandidateRevision {
		t.Fatal(result, err)
	}
	loaded, _, err := store.Load(ctx)
	if err != nil || len(loaded.Providers) != 1 || !reflect.DeepEqual(loaded.Deployments, sharedRecipeDocument().Deployments) || !reflect.DeepEqual(loaded.VirtualModels, legacy.VirtualModels) {
		t.Fatal("migration changed binding identity or routing", loaded, err)
	}
	operator, _ := store.GetSet(ctx, managed.OwnerOperator)
	if len(operator.Document.Providers) != 0 || operator.Document.Deployments[0].Provider != "sparkrun" {
		t.Fatal("migration did not persist the normalized operator fragment", operator)
	}
	// A failed write cannot partially persist a provider ownership change.
	broken := sharedProviderDocument()
	broken.Providers[0].BaseURL = "http://localhost:8000/v1"
	if _, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, broken, managed.ReplaceOptions{ExpectedActive: result.Current.Version, Actor: "plugin"}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatal("accepted a conflicting provider", err)
	}
	if _, after, err := store.Load(ctx); err != nil || after != result.Current.Version {
		t.Fatal("failed write changed configuration", after, err)
	}
}
