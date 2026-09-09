package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func TestSparkrunExclusionPersistsAcrossReconcileRestartAndRestore(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "gateway.db")
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "operator", ""); err != nil {
		t.Fatal(err)
	}
	_, revision, _ := store.Load(ctx)
	generated := generatedDocument()
	result, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, generated, managed.ReplaceOptions{ExpectedActive: revision, Actor: "sparkrun"})
	if err != nil {
		t.Fatal(err)
	}
	revision = result.Current.Version
	operator := config.Document{SparkrunOverrides: &config.SparkrunOverrides{ExcludedDeployments: []string{"generated"}}}
	candidate, validation, err := store.BuildCandidate(ctx, managed.OwnerOperator, operator, revision)
	if err != nil || !validation.Valid || len(candidate.Deployments) != 0 || len(candidate.VirtualModels) != 0 {
		t.Fatalf("candidate: %#v, %v", candidate, err)
	}
	before, _, _ := store.Load(ctx)
	if len(before.VirtualModels) != 1 {
		t.Fatal("validation changed stored state")
	}
	result, err = store.ReplaceSet(ctx, managed.OwnerOperator, operator, managed.ReplaceOptions{ExpectedActive: revision, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), managed.ReplaceOptions{ExpectedActive: revision, Actor: "stale-browser"}); !errors.Is(err, managed.ErrRevisionConflict) {
		t.Fatalf("stale save: %v", err)
	}
	revision = result.Current.Version
	// The source remains intact and may change during the next proxy sync.
	generated.Deployments[0].Title = "new discovery title"
	if _, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, generated, managed.ReplaceOptions{ExpectedActive: revision, Actor: "sparkrun"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	current, revision, err := store.Load(ctx)
	if err != nil || len(current.Deployments) != 0 || len(current.VirtualModels) != 0 {
		t.Fatalf("restored process resurrected entry: %#v %v", current, err)
	}
	set, err := store.GetSet(ctx, managed.OwnerSparkrun)
	if err != nil || len(set.Document.Deployments) != 1 || set.Document.Deployments[0].Title != "new discovery title" {
		t.Fatalf("source was altered: %#v %v", set, err)
	}
	// It remains a valid exclusion even if discovery temporarily reports nothing.
	result, err = store.ReplaceSet(ctx, managed.OwnerSparkrun, managed.EmptyDocument(), managed.ReplaceOptions{ExpectedActive: revision, Actor: "sparkrun"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.ReplaceSet(ctx, managed.OwnerSparkrun, generated, managed.ReplaceOptions{ExpectedActive: result.Current.Version, Actor: "sparkrun"})
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.ReplaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), managed.ReplaceOptions{ExpectedActive: result.Current.Version, Actor: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	current, _, err = store.Load(ctx)
	if err != nil || len(current.VirtualModels) != 1 || current.Deployments[0].Title != "new discovery title" {
		t.Fatalf("restore: %#v %v", current, err)
	}
}
