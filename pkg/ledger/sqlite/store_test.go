package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/ledger/ledgertest"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/responsesstate/responsesstatetest"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func TestReaderAndRetentionConformance(t *testing.T) {
	t.Parallel()

	store, err := Open(context.Background(), Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	ledgertest.RunConformance(t, store, nil)
	responsesstatetest.RunFileStoreConformance(t, store)
}

func TestFileIndexMigrationAddsCallerScopedSeekIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/009_file_index.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_file_index",
		"PRIMARY KEY (owner_scope, file_id)",
		"REFERENCES llm_resource_affinities",
		"llm_file_index_scope_created_idx",
		"llm_file_index_scope_purpose_created_idx",
		"INSERT INTO llm_file_index",
		"WHERE resource_kind = 'file'",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("file index migration does not contain %q", required)
		}
	}
}

func TestAggregateMigrationAddsFinalRouteIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/007_aggregate_indexes.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"llm_requests_final_provider_started_idx",
		"final_provider, started_at_unix_ns DESC",
		"llm_requests_final_deployment_started_idx",
		"final_deployment, started_at_unix_ns DESC",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("aggregate migration does not contain %q", required)
		}
	}
}

func TestRuntimeEventMigrationAddsHistoryAndSeekIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/008_runtime_events.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS model_runtime_events",
		"event_id TEXT PRIMARY KEY",
		"occurred_at_unix_ns INTEGER NOT NULL",
		"model_runtime_events_controller_seek_idx",
		"model_runtime_events_virtual_model_seek_idx",
		"model_runtime_events_deployment_seek_idx",
		"model_runtime_events_outcome_seek_idx",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("runtime-event migration does not contain %q", required)
		}
	}
}

func TestStorePersistsIdempotentRequestAndAttempt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	now := time.Now().UTC()
	zero := int64(0)
	input := int64(12)
	request := ledger.NewRequestRecord(ledger.RequestRecord{
		RequestID:      "request-1",
		StartedAt:      now,
		CompletedAt:    now.Add(time.Second),
		Protocol:       "openai",
		Operation:      "chat_completions",
		RequestedModel: "default",
		VirtualModel:   "public",
		AttemptCount:   1,
		HTTPStatus:     200,
		Outcome:        ledger.OutcomeSuccess,
		Latency:        time.Second,
		Usage: ledger.TokenUsage{
			InputTokens:          &input,
			OutputTokens:         &zero,
			Completeness:         ledger.UsagePartial,
			NormalizationVersion: "test/v1",
			Raw:                  []byte(`{"prompt_tokens":12,"completion_tokens":0}`),
		},
	})
	attempt := ledger.NewAttemptRecord(ledger.AttemptRecord{
		RequestID:     "request-1",
		Attempt:       1,
		StartedAt:     now,
		CompletedAt:   now.Add(time.Second),
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream-model",
		HTTPStatus:    200,
		Outcome:       ledger.OutcomeSuccess,
		Latency:       time.Second,
		Usage:         request.Request.Usage,
	})
	records := []ledger.Record{attempt, request}
	if err := store.AppendBatch(ctx, records); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if err := store.AppendBatch(ctx, records); err != nil {
		t.Fatalf("idempotent AppendBatch() error = %v", err)
	}

	var requestCount, attemptCount int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_requests").Scan(&requestCount); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_attempts").Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if requestCount != 1 || attemptCount != 1 {
		t.Fatalf("counts = requests:%d attempts:%d", requestCount, attemptCount)
	}
	var gotInput, gotOutput, gotTotal sql.NullInt64
	if err := store.db.QueryRowContext(
		ctx,
		"SELECT input_tokens, output_tokens, total_tokens FROM llm_requests WHERE request_id = ?",
		"request-1",
	).Scan(&gotInput, &gotOutput, &gotTotal); err != nil {
		t.Fatalf("query usage: %v", err)
	}
	if !gotInput.Valid || gotInput.Int64 != 12 ||
		!gotOutput.Valid || gotOutput.Int64 != 0 ||
		gotTotal.Valid {
		t.Fatalf("usage = input:%#v output:%#v total:%#v", gotInput, gotOutput, gotTotal)
	}
}

func TestSavedTraceRoundTripAndMetadataFiltering(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	started := time.Unix(100, 123).UTC()
	record := savedtrace.Record{
		Version:          savedtrace.SchemaVersion,
		RequestID:        "request-trace",
		StartedAt:        started,
		CompletedAt:      started.Add(time.Second),
		TenantID:         "tenant-a",
		ConversationID:   "conversation-a",
		ResponseID:       "response-a",
		SessionID:        "session-a",
		CaptureSessionID: "capture-a",
		Metadata:         map[string]string{"sparkroute.task_id": "task-a"},
		Protocol:         "openai",
		Operation:        "embeddings",
		VirtualModel:     "embedding",
		FinalDeployment:  "embedding-a",
		Outcome:          "success",
		CaptureOutcome:   savedtrace.CaptureComplete,
		Request:          savedtrace.Payload{Body: `{"input":"prompt"}`},
		Response:         savedtrace.Payload{Body: `{"data":[]}`},
	}
	if err := store.Append(ctx, record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if err := store.Append(ctx, record); err != nil {
		t.Fatalf("idempotent Append() error = %v", err)
	}
	page, err := store.List(ctx, savedtrace.Query{
		TenantID:   "tenant-a",
		Operation:  "embeddings",
		Deployment: "embedding-a",
		Metadata:   map[string]string{"sparkroute.task_id": "task-a"},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 1 ||
		page.Records[0].Request.Body != record.Request.Body ||
		page.Records[0].Metadata["sparkroute.task_id"] != "task-a" ||
		page.Records[0].ConversationID != "conversation-a" ||
		page.Records[0].CaptureSessionID != "capture-a" {
		t.Fatalf("page = %#v", page)
	}
	page, err = store.List(ctx, savedtrace.Query{
		Metadata: map[string]string{"sparkroute.task_id": "different"},
	})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("nonmatching page = %#v, %v", page, err)
	}
	second := record
	second.RequestID = "request-trace-batch"
	second.StartedAt = second.StartedAt.Add(time.Second)
	second.CompletedAt = second.CompletedAt.Add(time.Second)
	if err := store.AppendTraceBatch(ctx, []savedtrace.Record{second}); err != nil {
		t.Fatalf("AppendTraceBatch() error = %v", err)
	}
	completed := started.Add(3 * time.Second)
	session := savedtrace.CaptureSession{
		ID: "capture-a", StartedAt: started.Add(-time.Second), UpdatedAt: completed,
		CompletedAt: &completed, Mode: "block", Accepted: 2, Persisted: 2,
	}
	if err := store.UpsertCaptureSession(ctx, session); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListCaptureSessions(ctx, savedtrace.CaptureSessionQuery{
		StartedBefore: completed.Add(time.Second), UpdatedAfter: started,
	})
	if err != nil || len(sessions) != 1 || sessions[0].Persisted != 2 {
		t.Fatalf("capture sessions = %#v, %v", sessions, err)
	}
}

func TestStoreBatchIsTransactional(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, err := Open(ctx, Options{Path: ":memory:"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()

	now := time.Now()
	valid := ledger.NewRequestRecord(ledger.RequestRecord{
		RequestID:   "request-1",
		StartedAt:   now,
		CompletedAt: now,
		Protocol:    "openai",
		Operation:   "chat_completions",
		Outcome:     ledger.OutcomeSuccess,
		Usage:       ledger.TokenUsage{Completeness: ledger.UsageMissing},
	})
	invalid := ledger.Record{Kind: ledger.KindAttempt}
	if err := store.AppendBatch(ctx, []ledger.Record{valid, invalid}); err == nil {
		t.Fatal("AppendBatch() error = nil, want validation error")
	}
	var count int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM llm_requests").Scan(&count); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if count != 0 {
		t.Fatalf("request count = %d, want transaction rollback", count)
	}
}

func TestStoreCreatesPrivateFile(t *testing.T) {
	t.Parallel()

	parent := filepath.Join(t.TempDir(), "ledger")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	path := filepath.Join(parent, "usage.db")
	store, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file permissions = %#o, want 0600", got)
	}
}

func TestStoreRejectsSharedParentDirectory(t *testing.T) {
	t.Parallel()

	parent := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	_, err := Open(context.Background(), Options{
		Path: filepath.Join(parent, "usage.db"),
	})
	if err == nil {
		t.Fatal("Open() error = nil, want private-directory error")
	}
}

func TestResponseAffinityPersistsAndRejectsConflict(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	path := filepath.Join(parent, "affinity.db")
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	now := time.Now().UTC()
	affinity := responsesstate.Affinity{
		Scope:         "scope-a",
		ResponseID:    "resp_persisted",
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}
	if err := store.Bind(ctx, affinity); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	store, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer store.Close()
	got, found, err := store.Resolve(ctx, affinity.Scope, affinity.ResponseID)
	if err != nil || !found || !responsesstate.SameRoute(got, affinity) {
		t.Fatalf("Resolve() = %#v, %v, %v", got, found, err)
	}
	conflict := affinity
	conflict.Deployment = "other"
	if err := store.Bind(ctx, conflict); !errors.Is(err, responsesstate.ErrConflict) {
		t.Fatalf("conflicting Bind() error = %v", err)
	}
}

func TestResourceAffinityPersistsAndTombstones(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	path := filepath.Join(parent, "resource-affinity.db")
	ctx := context.Background()
	store, err := Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	now := time.Now().UTC()
	affinity := responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope:      "scope-a",
			Kind:       responsesstate.ResourceFile,
			ResourceID: "file_persisted",
		},
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream",
		BoundAt:       now,
	}
	fileRecord := responsesstate.FileRecord{
		Scope: affinity.Scope, ID: affinity.ResourceID, Object: "file",
		Bytes: 12, CreatedAt: now.Unix(), Filename: "persisted.txt", Purpose: "user_data",
	}
	if err := store.BindFile(ctx, affinity, fileRecord); err != nil {
		t.Fatalf("BindFile() error = %v", err)
	}
	itemAffinity := affinity
	itemAffinity.Kind = responsesstate.ResourceItem
	itemAffinity.ResourceID = "item_persisted"
	itemAffinity.ExpiresAt = now.Add(time.Hour)
	if err := store.BindResource(ctx, itemAffinity); err != nil {
		t.Fatalf("BindResource() item error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	store, err = Open(ctx, Options{Path: path})
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	defer store.Close()
	got, found, err := store.ResolveResource(ctx, affinity.ResourceKey)
	if err != nil || !found || !responsesstate.SameResourceRoute(got, affinity) {
		t.Fatalf("ResolveResource() = %#v, %v, %v", got, found, err)
	}
	files, err := store.ListFiles(ctx, responsesstate.FileQuery{Scope: affinity.Scope})
	if err != nil || len(files.Records) != 1 || files.Records[0] != fileRecord {
		t.Fatalf("ListFiles() = %#v, %v", files, err)
	}
	gotItem, found, err := store.ResolveResource(ctx, itemAffinity.ResourceKey)
	if err != nil || !found ||
		!responsesstate.SameResourceRoute(gotItem, itemAffinity) {
		t.Fatalf("ResolveResource() item = %#v, %v, %v", gotItem, found, err)
	}
	durableItem := itemAffinity
	durableItem.BoundAt = now.Add(time.Minute)
	durableItem.ExpiresAt = time.Time{}
	if err := store.BindResource(ctx, durableItem); err != nil {
		t.Fatalf("BindResource() durable item error = %v", err)
	}
	gotItem, found, err = store.ResolveResource(ctx, itemAffinity.ResourceKey)
	if err != nil || !found || !gotItem.ExpiresAt.IsZero() {
		t.Fatalf(
			"ResolveResource() durable item = %#v, %v, %v",
			gotItem,
			found,
			err,
		)
	}
	deletedAt := time.Now().UTC()
	if err := store.TombstoneResource(
		ctx,
		affinity.ResourceKey,
		deletedAt,
		deletedAt.Add(time.Hour),
	); err != nil {
		t.Fatalf("TombstoneResource() error = %v", err)
	}
	files, err = store.ListFiles(ctx, responsesstate.FileQuery{Scope: affinity.Scope})
	if err != nil || len(files.Records) != 0 {
		t.Fatalf("ListFiles() after tombstone = %#v, %v", files, err)
	}
	got, found, err = store.ResolveResource(ctx, affinity.ResourceKey)
	if err != nil || !found || !got.Deleted() {
		t.Fatalf("tombstone ResolveResource() = %#v, %v, %v", got, found, err)
	}
	if err := store.BindResource(ctx, affinity); !errors.Is(err, responsesstate.ErrConflict) {
		t.Fatalf("BindResource() over tombstone error = %v", err)
	}
}

func TestSeekPaginationMigrationBackfillsExistingRows(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	path := filepath.Join(parent, "legacy.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() legacy file error = %v", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	initial, err := migrationFiles.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if _, err := db.Exec(string(initial)); err != nil {
		t.Fatalf("apply legacy migration: %v", err)
	}
	started := "2026-07-28T12:00:00.123456789Z"
	if _, err := db.Exec(`
		INSERT INTO llm_requests (
			request_id, started_at, completed_at, protocol, operation, stream,
			attempt_count, outcome, latency_ns, usage_completeness
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, "legacy-request", started, started, "openai", "chat_completions", 0,
		0, string(ledger.OutcomeSuccess), 0, string(ledger.UsageMissing)); err != nil {
		t.Fatalf("insert legacy request: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO ledger_schema_migrations (version, applied_at) VALUES (?, ?)",
		"001_initial",
		started,
	); err != nil {
		t.Fatalf("record legacy migration: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(context.Background(), Options{Path: path})
	if err != nil {
		t.Fatalf("Open() migrated store error = %v", err)
	}
	defer store.Close()
	var startedNS int64
	if err := store.db.QueryRowContext(
		context.Background(),
		"SELECT started_at_unix_ns FROM llm_requests WHERE request_id = ?",
		"legacy-request",
	).Scan(&startedNS); err != nil {
		t.Fatalf("read migrated seek key: %v", err)
	}
	if startedNS == 0 {
		t.Fatal("migrated seek key = 0")
	}
	page, err := store.ListRequests(context.Background(), ledger.RequestQuery{})
	if err != nil {
		t.Fatalf("ListRequests() migrated store error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].RequestID != "legacy-request" {
		t.Fatalf("ListRequests() migrated records = %#v", page.Records)
	}
}
