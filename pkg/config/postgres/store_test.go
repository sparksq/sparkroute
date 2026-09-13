// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestMigrationDefinesImmutableRevisionAndActivationTables(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/001_config.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_config_revisions",
		"CREATE TABLE IF NOT EXISTS llm_config_state",
		"CREATE TABLE IF NOT EXISTS llm_config_activations",
		"llm_config_revisions_immutable",
		"BEFORE UPDATE OR DELETE",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration does not contain %q", required)
		}
	}
}

func TestMutationValidation(t *testing.T) {
	t.Parallel()

	if err := validateMetadata("", ""); err == nil {
		t.Fatal("validateMetadata() actor error = nil")
	}
	if err := validateVersion("not-a-revision"); err == nil {
		t.Fatal("validateVersion() error = nil")
	}
}

func TestActivationHistoryValidation(t *testing.T) {
	t.Parallel()

	store := &Store{}
	for name, query := range map[string]ActivationQuery{
		"negative limit": {
			Limit: -1,
		},
		"excessive limit": {
			Limit: maxActivationLimit + 1,
		},
		"negative generation": {
			BeforeGeneration: -1,
		},
		"invalid revision": {
			Revision: "not-a-revision",
		},
	} {
		query := query
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := store.ListActivations(context.Background(), query); err == nil {
				t.Fatal("ListActivations() error = nil")
			}
		})
	}
}

func TestPostgresConfigurationStoreIntegration(t *testing.T) {
	url := os.Getenv("SPARKROUTE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("SPARKROUTE_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, Options{
		URL:                   url,
		AutoMigrate:           true,
		ExpectedPostgresMajor: 17,
		MaxConnections:        4,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	seed := fmt.Sprintf("%d", time.Now().UnixNano())
	firstDocument := testDocument("first-" + seed)
	firstVersion, created, err := store.Publish(ctx, firstDocument, PublishOptions{
		Actor:  "integration-test",
		Reason: "publish first",
	})
	if err != nil {
		t.Fatalf("Publish() first error = %v", err)
	}
	if !created {
		t.Fatal("Publish() first created = false")
	}
	publishedDocument, revision, err := store.GetRevision(ctx, firstVersion)
	if err != nil {
		t.Fatalf("GetRevision() error = %v", err)
	}
	if revision.Version != firstVersion ||
		revision.CreatedBy != "integration-test" ||
		publishedDocument.VirtualModels[0].Name != firstDocument.VirtualModels[0].Name {
		t.Fatalf("GetRevision() = document:%#v revision:%#v", publishedDocument, revision)
	}
	if _, _, err := store.GetRevision(
		ctx,
		config.Version(strings.Repeat("f", 64)),
	); !errors.Is(err, ErrRevisionNotFound) {
		t.Fatalf("GetRevision() missing error = %v", err)
	}
	replayedVersion, created, err := store.Publish(ctx, firstDocument, PublishOptions{
		Actor: "integration-test",
	})
	if err != nil {
		t.Fatalf("Publish() replay error = %v", err)
	}
	if created || replayedVersion != firstVersion {
		t.Fatalf("Publish() replay = version:%s created:%v", replayedVersion, created)
	}
	firstActivation, err := store.Activate(ctx, firstVersion, ActivateOptions{
		Actor:  "integration-test",
		Reason: "activate first",
	})
	if err != nil {
		t.Fatalf("Activate() first error = %v", err)
	}
	if firstActivation.Version != firstVersion || firstActivation.Generation < 1 {
		t.Fatalf("Activate() first = %#v", firstActivation)
	}
	loaded, loadedVersion, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load() first error = %v", err)
	}
	if loadedVersion != firstVersion ||
		loaded.VirtualModels[0].Name != firstDocument.VirtualModels[0].Name {
		t.Fatalf("Load() first = version:%s document:%#v", loadedVersion, loaded)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	changes, err := store.Watch(watchCtx)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	secondDocument := testDocument("second-" + seed)
	secondActivation, created, err := store.PublishAndActivate(
		ctx,
		secondDocument,
		PublishOptions{Actor: "integration-test", Reason: "publish second"},
		ActivateOptions{
			Actor:          "integration-test",
			Reason:         "activate second",
			ExpectedActive: &firstVersion,
		},
	)
	if err != nil {
		t.Fatalf("PublishAndActivate() error = %v", err)
	}
	if !created ||
		secondActivation.PreviousVersion != firstVersion ||
		secondActivation.Generation != firstActivation.Generation+1 {
		t.Fatalf("PublishAndActivate() = %#v, created:%v", secondActivation, created)
	}
	select {
	case changed := <-changes:
		if changed != secondActivation.Version {
			t.Fatalf("Watch() version = %s, want %s", changed, secondActivation.Version)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Watch() timed out")
	}
	stopWatch()

	if _, err := store.Activate(ctx, firstVersion, ActivateOptions{
		Actor:          "integration-test",
		ExpectedActive: &firstVersion,
	}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("Activate() conflict error = %v", err)
	}
	secondVersion := secondActivation.Version
	rollback, err := store.Activate(ctx, firstVersion, ActivateOptions{
		Actor:          "integration-test",
		Reason:         "rollback",
		ExpectedActive: &secondVersion,
	})
	if err != nil {
		t.Fatalf("Activate() rollback error = %v", err)
	}
	if rollback.PreviousVersion != secondVersion ||
		rollback.Generation != secondActivation.Generation+1 {
		t.Fatalf("Activate() rollback = %#v", rollback)
	}
	history, err := store.ListActivations(ctx, ActivationQuery{
		Revision: firstVersion,
		Limit:    1,
	})
	if err != nil {
		t.Fatalf("ListActivations() first page error = %v", err)
	}
	if len(history.Activations) != 1 ||
		history.Activations[0].Generation != rollback.Generation ||
		history.NextBeforeGeneration != rollback.Generation {
		t.Fatalf("ListActivations() first page = %#v", history)
	}
	olderHistory, err := store.ListActivations(ctx, ActivationQuery{
		Revision:         firstVersion,
		BeforeGeneration: history.NextBeforeGeneration,
		Limit:            1,
	})
	if err != nil {
		t.Fatalf("ListActivations() second page error = %v", err)
	}
	if len(olderHistory.Activations) != 1 ||
		olderHistory.Activations[0].Generation != firstActivation.Generation ||
		olderHistory.NextBeforeGeneration != 0 {
		t.Fatalf("ListActivations() second page = %#v", olderHistory)
	}
	revisions, err := store.ListRevisions(ctx, 10)
	if err != nil {
		t.Fatalf("ListRevisions() error = %v", err)
	}
	if len(revisions) < 2 {
		t.Fatalf("ListRevisions() count = %d", len(revisions))
	}
	if _, err := store.pool.Exec(
		ctx,
		"UPDATE llm_config_revisions SET document_json = document_json WHERE revision_id = $1",
		firstVersion,
	); err == nil {
		t.Fatal("immutable revision UPDATE error = nil")
	}
}

func testDocument(suffix string) config.Document {
	provider := "provider-" + suffix
	deployment := "deployment-" + suffix
	model := "model-" + suffix
	return config.Document{
		Providers: []config.Provider{{
			Name:    provider,
			Type:    "openai_compatible",
			BaseURL: "https://example.com/v1",
		}},
		Deployments: []config.Deployment{{
			Name:     deployment,
			Provider: provider,
			Model:    "upstream",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: model,
			Pools: []config.RoutingPool{{
				Priority: 0,
				Targets: []config.WeightedTarget{{
					Deployment: deployment,
					Weight:     1,
				}},
			}},
		}},
	}
}
