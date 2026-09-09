package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func presetOptions(t *testing.T, store *Store) managed.ReplaceOptions {
	t.Helper()
	catalog, err := store.ListPresets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return managed.ReplaceOptions{ExpectedActive: catalog.ActiveRevision, ExpectedPresetsRevision: &catalog.PresetsRevision, Actor: "operator"}
}

func presetDocument() config.Document {
	return config.Document{Providers: []config.Provider{{Name: "local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8001/v1"}},
		Deployments:   []config.Deployment{{Name: "local", Provider: "local", Model: "model"}},
		VirtualModels: []config.VirtualModel{{Name: "chat", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "local", Weight: 1}}}}}}}
}

func TestPresetsSaveSwitchAndResumeOnlyOperatorConfiguration(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.sqlite")
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, generatedDocument(), presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	generated, _ := store.GetSet(ctx, managed.OwnerSparkrun)
	copy, err := store.SavePreset(ctx, "Work", presetOptions(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, presetDocument(), presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetPreset(ctx, copy.ID)
	if err != nil || len(saved.Document.VirtualModels) != 1 {
		t.Fatalf("active preset was not updated: %#v, %v", saved, err)
	}
	def, _ := store.GetPreset(ctx, managed.DefaultPreset)
	if len(def.Document.VirtualModels) != 0 {
		t.Fatal("saving Work changed Default")
	}
	if _, err := store.ActivatePreset(ctx, managed.DefaultPreset, presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	doc, _, _ := store.Load(ctx)
	if len(doc.VirtualModels) != 1 || doc.VirtualModels[0].Name != "generated" {
		t.Fatalf("Default: %#v", doc)
	}
	if _, err := store.ActivatePreset(ctx, copy.ID, presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := store.GetSet(ctx, managed.OwnerSparkrun)
	if !reflect.DeepEqual(generated, unchanged) {
		t.Fatal("switching changed generated configuration")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := store.ListPresets(ctx)
	if err != nil || catalog.ActivePreset != copy.ID || len(catalog.Presets) != 2 {
		t.Fatalf("resume: %#v, %v", catalog, err)
	}
	doc, _, err = store.Load(ctx)
	if err != nil || len(doc.VirtualModels) != 2 {
		t.Fatalf("resumed config: %#v, %v", doc, err)
	}
}

func TestPresetSelectionFencesStaleEditsEvenForEqualDocuments(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	old := presetOptions(t, store)
	copy, err := store.SavePreset(ctx, "Copy", old)
	if err != nil {
		t.Fatal(err)
	}
	current := presetOptions(t, store)
	if current.ExpectedActive != old.ExpectedActive {
		t.Fatal("copy changed configuration hash")
	}
	if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, presetDocument(), old); !errors.Is(err, managed.ErrPresetConflict) {
		t.Fatalf("stale save: %v", err)
	}
	if _, err := store.ActivatePreset(ctx, managed.DefaultPreset, old); !errors.Is(err, managed.ErrPresetConflict) {
		t.Fatalf("stale switch: %v", err)
	}
	if _, err := store.ActivatePreset(ctx, managed.DefaultPreset, current); err != nil {
		t.Fatal(err)
	}
	if err := store.RenamePreset(ctx, copy.ID, "Renamed", current); !errors.Is(err, managed.ErrPresetConflict) {
		t.Fatalf("stale rename: %v", err)
	}
	if err := store.RenamePreset(ctx, copy.ID, "Renamed", presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	if err := store.DeletePreset(ctx, copy.ID, presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetPreset(ctx, copy.ID); !errors.Is(err, managed.ErrPresetNotFound) {
		t.Fatalf("deleted preset: %v", err)
	}
}

func TestPresetsRejectInvalidManagementAndSwitchAtomically(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if _, err := store.Initialize(ctx, presetDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", " Default", "default", "Bad\nname", strings.Repeat("x", 65)} {
		if _, err := store.SavePreset(ctx, name, presetOptions(t, store)); !errors.Is(err, managed.ErrInvalidPreset) {
			t.Fatalf("name %q: %v", name, err)
		}
	}
	if err := store.RenamePreset(ctx, "default", "Renamed", presetOptions(t, store)); !errors.Is(err, managed.ErrInvalidPreset) {
		t.Fatalf("rename Default: %v", err)
	}
	copy, err := store.SavePreset(ctx, "Work", presetOptions(t, store))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"default", copy.ID} {
		if err := store.DeletePreset(ctx, id, presetOptions(t, store)); !errors.Is(err, managed.ErrInvalidPreset) {
			t.Fatalf("delete %s: %v", id, err)
		}
	}
	if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	// Generated inventory now owns names used by the stored Default snapshot.
	if _, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, presetDocument(), presetOptions(t, store)); err != nil {
		t.Fatal(err)
	}
	before, _ := store.ListPresets(ctx)
	if _, err := store.ActivatePreset(ctx, "default", presetOptions(t, store)); !errors.Is(err, managed.ErrInvalidConfiguration) {
		t.Fatalf("invalid merged preset: %v", err)
	}
	after, _ := store.ListPresets(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed activation changed preset state")
	}
	op, _ := store.GetSet(ctx, managed.OwnerOperator)
	if len(op.Document.Providers) != 0 {
		t.Fatal("failed activation changed operator")
	}
}

func TestPresetMigrationSeedsExistingOperator(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.sqlite")
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Initialize(ctx, presetDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	// Model an existing managed-set database before presets were introduced.
	if _, err := store.db.ExecContext(ctx, "DROP TABLE gateway_config_preset_state; DROP TABLE gateway_config_presets; DELETE FROM gateway_config_schema_migrations WHERE version = '004_operator_presets'"); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	store, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	catalog, err := store.ListPresets(ctx)
	if err != nil || catalog.ActivePreset != "default" || len(catalog.Presets) != 1 {
		t.Fatalf("migration: %#v, %v", catalog, err)
	}
	preset, err := store.GetPreset(ctx, "default")
	if err != nil || len(preset.Document.VirtualModels) != 1 {
		t.Fatalf("migrated preset: %#v, %v", preset, err)
	}
}
