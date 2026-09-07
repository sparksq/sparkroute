package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func TestManagedSetCurrentConfigurationAndWatch(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	initialized, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", "initial")
	if err != nil || !initialized {
		t.Fatalf("Initialize() = %v, %v", initialized, err)
	}
	if initialized, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", "again"); err != nil || initialized {
		t.Fatalf("second Initialize() = %v, %v", initialized, err)
	}
	_, active, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	validation, err := store.ValidateSet(ctx, managed.OwnerSparkrun, generatedDocument(), active)
	if err != nil || !validation.Valid || validation.ActiveRevision != active ||
		validation.CandidateRevision == active {
		t.Fatalf("ValidateSet() = %#v, %v", validation, err)
	}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	changes, err := store.Watch(watchCtx)
	if err != nil {
		t.Fatal(err)
	}

	generated := generatedDocument()
	result, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, generated, managed.ReplaceOptions{
		ExpectedActive: active, Actor: "sparkrun-agent", Reason: "discover",
	})
	if err != nil || !result.Changed || result.Current.Version == active || result.Current.UpdatedBy != "sparkrun-agent" {
		t.Fatalf("ReplaceSet() = %#v, %v", result, err)
	}
	select {
	case version := <-changes:
		if version != result.Current.Version {
			t.Fatalf("Watch() = %s, want %s", version, result.Current.Version)
		}
	case <-time.After(time.Second):
		t.Fatal("Watch() did not report the current configuration")
	}
	document, version, err := store.Load(ctx)
	if err != nil || version != result.Current.Version || len(document.VirtualModels) != 1 {
		t.Fatalf("Load() = %#v, %s, %v", document, version, err)
	}
	set, err := store.GetSet(ctx, managed.OwnerSparkrun)
	if err != nil || set.UpdatedBy != "sparkrun-agent" || len(set.Document.Deployments) != 1 {
		t.Fatalf("GetSet() = %#v, %v", set, err)
	}
	for _, table := range []string{
		"gateway_config_revisions", "gateway_config_activations",
		"gateway_config_managed_set_events",
	} {
		var exists bool
		err := store.db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)
		`, table).Scan(&exists)
		if err != nil || exists {
			t.Fatalf("history table %s exists = %v, %v", table, exists, err)
		}
	}

	unchanged, err := store.ReplaceSet(ctx, managed.OwnerSparkrun, generated, managed.ReplaceOptions{
		ExpectedActive: version, Actor: "sparkrun-agent", Reason: "repeat",
	})
	if err != nil || unchanged.Changed {
		t.Fatalf("unchanged ReplaceSet() = %#v, %v", unchanged, err)
	}
}

func generatedDocument() config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name: "sparkrun", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "generated", Provider: "sparkrun", Model: "upstream",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "generated", Pools: []config.RoutingPool{{
				Priority: 0, Targets: []config.WeightedTarget{{Deployment: "generated", Weight: 1}},
			}},
		}},
	}
}

func TestReplaceSetRejectsConflictAndInvalidMergedDocument(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	_, active, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReplaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), managed.ReplaceOptions{
		ExpectedActive: config.Version(strings.Repeat("0", 64)), Actor: "operator",
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	_, err = store.ValidateSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), config.Version(strings.Repeat("0", 64)))
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("validation conflict error = %v", err)
	}
	invalid := config.Document{Deployments: []config.Deployment{{
		Name: "broken", Provider: "missing", Model: "upstream",
	}}}
	_, err = store.ReplaceSet(ctx, managed.OwnerSparkrun, invalid, managed.ReplaceOptions{
		ExpectedActive: active, Actor: "sparkrun-agent",
	})
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid error = %v", err)
	}
	_, err = store.ValidateSet(ctx, managed.OwnerSparkrun, invalid, active)
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("invalid validation error = %v", err)
	}
	_, after, err := store.Load(ctx)
	if err != nil || after != active {
		t.Fatalf("active after invalid replacement = %s, %v", after, err)
	}
}

func TestReplaceSetReportsCrossOwnerDeletionReferrer(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	_, active, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := store.ReplaceSet(
		ctx,
		managed.OwnerSparkrun,
		generatedDocument(),
		managed.ReplaceOptions{ExpectedActive: active, Actor: "sparkrun-agent"},
	)
	if err != nil {
		t.Fatal(err)
	}
	operator := config.Document{VirtualModels: []config.VirtualModel{{
		Name: "operator-model",
		Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
			Deployment: "generated", Weight: 1,
		}}}},
	}}}
	operatorResult, err := store.ReplaceSet(
		ctx,
		managed.OwnerOperator,
		operator,
		managed.ReplaceOptions{
			ExpectedActive: generated.Current.Version,
			Actor:          "operator",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.ReplaceSet(
		ctx,
		managed.OwnerSparkrun,
		managed.EmptyDocument(),
		managed.ReplaceOptions{
			ExpectedActive: operatorResult.Current.Version,
			Actor:          "sparkrun-agent",
		},
	)
	if !errors.Is(err, ErrInvalidConfiguration) ||
		!strings.Contains(err.Error(), `virtual model "operator-model" owned by "operator"`) ||
		!strings.Contains(err.Error(), `missing deployment "generated"`) {
		t.Fatalf("cross-owner deletion error = %v", err)
	}
	_, after, loadErr := store.Load(ctx)
	if loadErr != nil || after != operatorResult.Current.Version {
		t.Fatalf("active after blocked deletion = %s, %v", after, loadErr)
	}
}

func TestLegacyHistoryIsCopiedWithoutDeletingIt(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", "initial"); err != nil {
		t.Fatal(err)
	}
	document, version, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := config.EncodeCanonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		DELETE FROM gateway_config_current;
		CREATE TABLE gateway_config_revisions (
			revision_id TEXT PRIMARY KEY,
			document_json TEXT NOT NULL
		);
		CREATE TABLE gateway_config_state (
			singleton INTEGER PRIMARY KEY,
			active_revision_id TEXT NOT NULL,
			activated_at TEXT NOT NULL,
			activated_by TEXT NOT NULL,
			reason TEXT
		);
	`); err != nil {
		t.Fatal(err)
	}
	now := formatTime(time.Now().UTC())
	if _, err := store.db.ExecContext(ctx,
		"INSERT INTO gateway_config_revisions (revision_id, document_json) VALUES (?, ?)",
		version, raw,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO gateway_config_state (
			singleton, active_revision_id, activated_at, activated_by, reason
		) VALUES (1, ?, ?, 'legacy-operator', 'legacy current state')
	`, version, now); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyCurrentConfiguration(ctx, store.db); err != nil {
		t.Fatal(err)
	}
	loaded, loadedVersion, err := store.Load(ctx)
	if err != nil || loadedVersion != version || len(loaded.Providers) != 0 {
		t.Fatalf("Load() after legacy copy = %#v, %s, %v", loaded, loadedVersion, err)
	}
	var retained int
	if err := store.db.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM gateway_config_revisions",
	).Scan(&retained); err != nil || retained != 1 {
		t.Fatalf("retained legacy revisions = %d, %v", retained, err)
	}
}

func TestStoreRequiresPrivateFilesystem(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), Options{Path: filepath.Join(directory, "config.db")})
	if err == nil || !strings.Contains(err.Error(), "must not grant") {
		t.Fatalf("Open() error = %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), Options{Path: filepath.Join(directory, "config.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
