// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.
package sparkrun

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

type updateCatalog struct {
	catalogFixture
	untrusted   bool
	unavailable bool
	model       string
}

func (f *updateCatalog) Catalog(ctx context.Context, operation string, args map[string]any, result any) error {
	if f.unavailable {
		return errors.New("catalog unavailable")
	}
	if err := f.catalogFixture.Catalog(ctx, operation, args, result); err != nil {
		return err
	}
	if details, ok := result.(*RecipeDetails); ok {
		details.Trusted = !f.untrusted
		if f.model != "" {
			details.Model = f.model
		}
	}
	return nil
}

func updateFixture(t *testing.T, owner managed.Owner) (*configsqlite.Store, *updateCatalog, config.Document, *Workloads) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := configsqlite.Open(ctx, configsqlite.Options{Path: filepath.Join(root, "config.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := &updateCatalog{catalogFixture: catalogFixture{revision: "old", settings: RecipeSettings{RequestProfiles: profileImplementations(map[string]map[string]map[string]json.RawMessage{"low": {"chat_completions": {"temperature": json.RawMessage(`0.2`)}}})}}}
	doc, _, _, err := PrepareRecipeDraft(ctx, f, managed.EmptyDocument(), managed.EmptyDocument(), RecipeDraft{Reference: "catalog:123", RecipeRevision: "old", Name: "coding", Aliases: []string{"code"}, Cluster: "lab", Overrides: map[string]string{"max_model_len": "4096"}})
	if err != nil {
		t.Fatal(err)
	}
	doc.Deployments[0].Title = "Custom title"
	doc.VirtualModels[1].Profile.Patch.IngressOverrides = config.RequestParameters{"chat_completions": {"temperature": json.RawMessage(`0.5`)}}
	if _, err = store.Initialize(ctx, doc, "test", ""); err != nil {
		t.Fatal(err)
	}
	if owner == managed.OwnerSparkrun {
		// Bootstrap puts supplied entities in operator. Move this test fixture into
		// the generated owner through one valid intermediate empty configuration.
		_, revision, _ := store.Load(ctx)
		if _, err = store.ReplaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), managed.ReplaceOptions{ExpectedActive: revision, Actor: "test"}); err != nil {
			t.Fatal(err)
		}
		_, revision, _ = store.Load(ctx)
		if _, err = store.ReplaceSet(ctx, owner, doc, managed.ReplaceOptions{ExpectedActive: revision, Actor: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	f.revision = "new"
	f.settings.RequestProfiles["low"].IngressOverrides["chat_completions"]["temperature"] = json.RawMessage(`0.7`)
	w := NewWorkloads()
	t.Cleanup(w.Close)
	return store, f, doc, w
}

func TestRecipeAutoUpdatePreservesRoutesAndAppliesOnlyAfterReceiptRemoved(t *testing.T) {
	for _, owner := range []managed.Owner{managed.OwnerOperator, managed.OwnerSparkrun} {
		t.Run(string(owner), func(t *testing.T) {
			ctx := context.Background()
			store, f, before, w := updateFixture(t, owner)
			targets, _ := lifecycle.TargetsFromDocument(before)
			for _, phase := range []string{"running", "sleeping", "warming", "unknown", "offline"} {
				inspect := func(context.Context) (WorkloadReport, error) {
					return WorkloadReport{Complete: true, Workloads: []WorkloadInfo{{RecipeRevision: "old", ClusterName: "lab", LifecycleState: phase}}}, nil
				}
				if err := RefreshRecipeBindings(ctx, store, f, w, inspect); err != nil {
					t.Fatal(err)
				}
				set, _ := store.GetSet(ctx, owner)
				if set.Document.Deployments[0].EndpointSource.RecipeRevision != "old" {
					t.Fatalf("updated %s workload", phase)
				}
			}
			if err := RefreshRecipeBindings(ctx, store, f, w, func(context.Context) (WorkloadReport, error) { return WorkloadReport{Complete: true}, nil }); err != nil {
				t.Fatal(err)
			}
			set, _ := store.GetSet(ctx, owner)
			after := set.Document
			source := after.Deployments[0].EndpointSource
			if source.RecipeRevision != "new" || source.Revision == before.Deployments[0].EndpointSource.Revision {
				t.Fatal(source)
			}
			if !reflect.DeepEqual(before.VirtualModels, after.VirtualModels) || !reflect.DeepEqual(before.Deployments[0].EndpointSource.Overrides, source.Overrides) || after.Deployments[0].Name != before.Deployments[0].Name || after.Deployments[0].Title != "Custom title" {
				t.Fatal("operator routing/policy changed", after)
			}
			if !w.bindingRetired(targets[0].Binding) {
				t.Fatal("old generation can still admit retired binding")
			}
			revision := set.Revision
			if err := RefreshRecipeBindings(ctx, store, f, w, func(context.Context) (WorkloadReport, error) {
				t.Fatal("unchanged recipe inspected jobs")
				return WorkloadReport{Complete: true}, nil
			}); err != nil {
				t.Fatal(err)
			}
			set, _ = store.GetSet(ctx, owner)
			if revision != set.Revision {
				t.Fatal("idempotent refresh created a revision")
			}
		})
	}
}

func TestRecipeAutoUpdateFailsClosed(t *testing.T) {
	for _, reason := range []string{"untrusted", "unavailable", "model changed", "inventory unavailable", "incomplete inventory", "active request", "concurrent edit", "recipe moved"} {
		t.Run(reason, func(t *testing.T) {
			ctx := context.Background()
			store, f, before, w := updateFixture(t, managed.OwnerOperator)
			targets, _ := lifecycle.TargetsFromDocument(before)
			switch reason {
			case "untrusted":
				f.untrusted = true
			case "unavailable":
				f.unavailable = true
			case "model changed":
				f.model = "other/model"
			case "active request":
				w.observe(targets[0].Binding, Endpoint{WorkloadID: "job", ClusterName: "lab"}, nil, 0, true)
			}
			inspect := func(context.Context) (WorkloadReport, error) {
				switch reason {
				case "incomplete inventory":
					return WorkloadReport{}, nil
				case "inventory unavailable":
					return WorkloadReport{Complete: true}, errors.New("offline")
				case "concurrent edit":
					set, _ := store.GetSet(ctx, managed.OwnerOperator)
					_, revision, _ := store.Load(ctx)
					set.Document.Deployments[0].Title = "concurrent title"
					if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, set.Document, managed.ReplaceOptions{ExpectedActive: revision, Actor: "test"}); err != nil {
						t.Fatal(err)
					}
				case "recipe moved":
					f.revision = "newer"
				}
				return WorkloadReport{Complete: true}, nil
			}
			err := RefreshRecipeBindings(ctx, store, f, w, inspect)
			if reason != "active request" && err == nil {
				t.Fatal("expected failed refresh")
			}
			set, _ := store.GetSet(ctx, managed.OwnerOperator)
			if set.Document.Deployments[0].EndpointSource.RecipeRevision != "old" {
				t.Fatal("unsafe update")
			}
			if reason == "concurrent edit" && set.Document.Deployments[0].Title != "concurrent title" {
				t.Fatal("lost concurrent edit")
			}
			if w.bindingRetired(targets[0].Binding) {
				t.Fatal("failed update retired live binding")
			}
		})
	}
}

func TestRecipeAutoUpdateIgnoresUnrelatedPlacement(t *testing.T) {
	ctx := context.Background()
	store, f, _, w := updateFixture(t, managed.OwnerOperator)
	err := RefreshRecipeBindings(ctx, store, f, w, func(context.Context) (WorkloadReport, error) {
		return WorkloadReport{Complete: true, Workloads: []WorkloadInfo{{RecipeRevision: "old", ClusterName: "other"}, {RecipeRevision: "different", ClusterName: "lab"}, {JobID: "legacy-without-fingerprint"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	set, _ := store.GetSet(ctx, managed.OwnerOperator)
	if set.Document.Deployments[0].EndpointSource.RecipeRevision != "new" {
		t.Fatal("unrelated job blocked update")
	}
}
