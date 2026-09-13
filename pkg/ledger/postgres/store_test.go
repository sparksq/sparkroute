// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/ledger/ledgertest"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/responsesstate/responsesstatetest"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func TestParseTimescaleVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		version   string
		wantMajor int
		wantMinor int
		wantError bool
	}{
		{version: "2.17.0", wantMajor: 2, wantMinor: 17},
		{version: "2.23.1-dev", wantMajor: 2, wantMinor: 23},
		{version: "3.0.0", wantMajor: 3, wantMinor: 0},
		{version: "invalid", wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.version, func(t *testing.T) {
			t.Parallel()
			major, minor, err := parseTimescaleVersion(test.version)
			if test.wantError {
				if err == nil {
					t.Fatal("parseTimescaleVersion() error = nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTimescaleVersion() error = %v", err)
			}
			if major != test.wantMajor || minor != test.wantMinor {
				t.Fatalf("version = %d.%d, want %d.%d", major, minor, test.wantMajor, test.wantMinor)
			}
		})
	}
}

func TestMigrationsUseTimeInclusiveKeysAndTimescaleOverlay(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/002_ledger.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"PRIMARY KEY (started_at, request_id)",
		"PRIMARY KEY (attempt_started_at, request_id, attempt_no)",
		"PRIMARY KEY (occurred_at, event_id)",
		"CREATE TABLE IF NOT EXISTS llm_request_lookup",
		"CREATE TABLE IF NOT EXISTS llm_attempt_lookup",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration does not contain %q", required)
		}
	}
	overlay, err := migrationFiles.ReadFile("migrations/timescaledb/002_hypertables.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	for _, required := range []string{"create_hypertable", "chunk_time_interval => INTERVAL '1 day'"} {
		if !strings.Contains(string(overlay), required) {
			t.Fatalf("TimescaleDB migration does not contain %q", required)
		}
	}
	for _, deferredFeature := range []string{
		"continuous aggregate",
		"add_compression_policy",
		"enable_columnstore",
		"add_columnstore_policy",
	} {
		if strings.Contains(strings.ToLower(sql), deferredFeature) {
			t.Fatalf("migration unexpectedly uses deferred feature %q", deferredFeature)
		}
	}
}

func TestCommonMigrationsContainNoTimescaleSQL(t *testing.T) {
	t.Parallel()

	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", entry.Name(), err)
		}
		sql := strings.ToLower(string(content))
		for _, timescaleOnly := range []string{
			"create extension if not exists timescaledb",
			"create_hypertable(",
			"drop_chunks(",
		} {
			if strings.Contains(sql, timescaleOnly) {
				t.Fatalf("common migration %s uses %q", entry.Name(), timescaleOnly)
			}
		}
	}
}

func TestParseBackend(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]Backend{
		"":            BackendPostgres,
		" postgres ":  BackendPostgres,
		"TIMESCALEDB": BackendTimescaleDB,
	} {
		got, err := ParseBackend(input)
		if err != nil || got != want {
			t.Fatalf("ParseBackend(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseBackend("sqlite"); err == nil {
		t.Fatal("ParseBackend(sqlite) error = nil")
	}
}

func TestReaderMigrationAddsFilterAlignedSeekIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/003_reader_indexes.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"llm_requests_virtual_model_seek_idx",
		"llm_requests_outcome_seek_idx",
		"llm_attempts_request_seek_idx",
		"llm_attempts_provider_seek_idx",
		"llm_attempts_deployment_seek_idx",
		"llm_attempts_outcome_seek_idx",
		"attempt_started_at DESC",
		"request_id DESC",
		"attempt_no DESC",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("reader migration does not contain %q", required)
		}
	}
}

func TestIdentityMigrationAddsTenantSeekIndex(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/004_request_identity.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"principal_id",
		"principal_roles_json JSONB",
		"tenant_id",
		"attribution_json JSONB",
		"llm_requests_tenant_seek_idx",
		"tenant_id, started_at DESC, request_id DESC",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("identity migration does not contain %q", required)
		}
	}
}

func TestAggregateMigrationAddsFinalRouteIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/008_aggregate_indexes.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"llm_requests_final_provider_started_idx",
		"final_provider, started_at DESC",
		"llm_requests_final_deployment_started_idx",
		"final_deployment, started_at DESC",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("aggregate migration does not contain %q", required)
		}
	}
}

func TestRuntimeEventMigrationAddsFilterAlignedIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/009_runtime_event_indexes.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"model_runtime_events_virtual_model_occurred_idx",
		"model_runtime_events_outcome_occurred_idx",
		"occurred_at DESC, event_id DESC",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("runtime-event migration does not contain %q", required)
		}
	}
}

func TestResponseAffinityMigrationAddsBoundedRoutingState(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/005_response_affinity.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_response_affinities",
		"PRIMARY KEY (owner_scope, response_id)",
		"virtual_model TEXT NOT NULL",
		"deployment TEXT NOT NULL",
		"expires_at TIMESTAMPTZ NOT NULL",
		"llm_response_affinities_expiry_idx",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("response affinity migration does not contain %q", required)
		}
	}
}

func TestResourceAffinityMigrationAddsDurableRoutingState(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/006_resource_affinity.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_resource_affinities",
		"PRIMARY KEY (owner_scope, resource_kind, resource_id)",
		"resource_kind TEXT NOT NULL",
		"deleted_at TIMESTAMPTZ",
		"expires_at TIMESTAMPTZ",
		"llm_resource_affinities_expiry_idx",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("resource affinity migration does not contain %q", required)
		}
	}
}

func TestFileIndexMigrationAddsCallerScopedSeekIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/010_file_index.sql")
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

func TestLegacySavedTraceMigrationDelegatesSchemaOwnership(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/007_saved_traces.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	if !strings.Contains(sql, "independently reusable saved-trace PostgreSQL package") {
		t.Fatal("legacy migration does not document delegated schema ownership")
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Fatal("legacy migration still creates the saved-trace table")
	}
}

func TestPromptCacheAffinityMigrationIsContentFree(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/011_prompt_cache_affinity.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_prompt_cache_affinities",
		"scope_digest TEXT NOT NULL",
		"prefix_digest TEXT NOT NULL",
		"expires_at TIMESTAMPTZ NOT NULL",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("prompt-cache affinity migration does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"prompt TEXT", "request_body", "response_body"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("prompt-cache affinity migration contains content field %q", forbidden)
		}
	}
}

func TestPIIConversationMigrationStoresOnlyEncryptedValues(t *testing.T) {
	t.Parallel()
	content, err := migrationFiles.ReadFile("migrations/012_pii_conversation_mappings.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"CREATE TABLE IF NOT EXISTS llm_pii_conversation_mappings",
		"lookup_digest BYTEA NOT NULL",
		"ciphertext BYTEA NOT NULL",
		"absolute_expires_at TIMESTAMPTZ NOT NULL",
		"UNIQUE (tenant_id, principal_id, conversation_id, token)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("PII mapping migration does not contain %q", required)
		}
	}
	for _, forbidden := range []string{"original TEXT", "plaintext", "secret_key", "master_key"} {
		if strings.Contains(sql, forbidden) {
			t.Fatalf("PII mapping migration contains forbidden field %q", forbidden)
		}
	}
}

func TestRecordArgumentsPreserveNullAndZeroTokens(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 7, 28, 12, 0, 0, 123456789, time.FixedZone("test", 3600))
	completed := started.Add(time.Second)
	zero := int64(0)
	input := int64(10)
	record := ledger.RequestRecord{
		RequestID:      "request-1",
		StartedAt:      started,
		CompletedAt:    completed,
		PrincipalID:    "client-a",
		PrincipalRoles: []string{"inference"},
		TenantID:       "tenant-a",
		Attribution:    map[string]string{"sparkroute.tenant": "tenant-a"},
		Protocol:       "openai",
		Operation:      "chat_completions",
		Outcome:        ledger.OutcomeSuccess,
		Latency:        time.Second,
		Usage: ledger.TokenUsage{
			InputTokens:  &input,
			OutputTokens: &zero,
			Completeness: ledger.UsagePartial,
		},
	}
	args, err := requestArgs(record, normalizePostgresTime(started))
	if err != nil {
		t.Fatalf("requestArgs() error = %v", err)
	}
	if len(args) != 38 {
		t.Fatalf("argument count = %d, want 38", len(args))
	}
	gotStarted, ok := args[0].(time.Time)
	if !ok {
		t.Fatalf("started argument type = %T", args[0])
	}
	if gotStarted.Location() != time.UTC || gotStarted.Nanosecond() != 123456000 {
		t.Fatalf("normalized start = %s", gotStarted.Format(time.RFC3339Nano))
	}
	if args[25] != int64(10) || args[26] != int64(0) || args[27] != nil {
		t.Fatalf("token arguments = input:%#v output:%#v total:%#v", args[25], args[26], args[27])
	}
}

func TestPostgresStoreIntegration(t *testing.T) {
	url := os.Getenv("SPARKROUTE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("SPARKROUTE_TEST_POSTGRES_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	backendName := os.Getenv("SPARKROUTE_TEST_POSTGRES_BACKEND")
	if backendName == "" {
		backendName = string(BackendPostgres)
	}
	backend, err := ParseBackend(backendName)
	if err != nil {
		t.Fatalf("ParseBackend() error = %v", err)
	}
	store, err := Open(ctx, Options{
		URL:                   url,
		AutoMigrate:           true,
		Backend:               backend,
		ExpectedPostgresMajor: 17,
		MaxConnections:        2,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	}()

	now := normalizePostgresTime(time.Now())
	requestID := fmt.Sprintf("integration-%d", now.UnixNano())
	request := ledger.NewRequestRecord(ledger.RequestRecord{
		RequestID:   requestID,
		StartedAt:   now,
		CompletedAt: now.Add(time.Millisecond),
		Protocol:    "openai",
		Operation:   "chat_completions",
		Outcome:     ledger.OutcomeSuccess,
		Latency:     time.Millisecond,
		Usage:       ledger.TokenUsage{Completeness: ledger.UsageMissing},
	})
	attempt := ledger.NewAttemptRecord(ledger.AttemptRecord{
		RequestID:     requestID,
		Attempt:       1,
		StartedAt:     now,
		CompletedAt:   now.Add(time.Millisecond),
		Provider:      "integration",
		Deployment:    "integration",
		UpstreamModel: "integration",
		Outcome:       ledger.OutcomeSuccess,
		Latency:       time.Millisecond,
		Usage:         ledger.TokenUsage{Completeness: ledger.UsageMissing},
	})
	records := []ledger.Record{attempt, request}
	if err := store.AppendBatch(ctx, records); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if err := store.AppendBatch(ctx, records); err != nil {
		t.Fatalf("idempotent AppendBatch() error = %v", err)
	}

	var requestCount, attemptCount int
	if err := store.pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM llm_requests WHERE request_id = $1",
		requestID,
	).Scan(&requestCount); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if err := store.pool.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM llm_attempts WHERE request_id = $1",
		requestID,
	).Scan(&attemptCount); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if requestCount != 1 || attemptCount != 1 {
		t.Fatalf("counts = requests:%d attempts:%d", requestCount, attemptCount)
	}
	affinity := responsesstate.Affinity{
		Scope:         "scope-a",
		ResponseID:    requestID + "-response",
		VirtualModel:  "public",
		Provider:      "integration",
		Deployment:    "integration",
		UpstreamModel: "integration",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}
	if err := store.Bind(ctx, affinity); err != nil {
		t.Fatalf("Bind() response affinity error = %v", err)
	}
	got, found, err := store.Resolve(ctx, affinity.Scope, affinity.ResponseID)
	if err != nil || !found || !responsesstate.SameRoute(got, affinity) {
		t.Fatalf("Resolve() response affinity = %#v, %v, %v", got, found, err)
	}
	resource := responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope:      "scope-a",
			Kind:       responsesstate.ResourceFile,
			ResourceID: requestID + "-file",
		},
		VirtualModel:  "public",
		Provider:      "integration",
		Deployment:    "integration",
		UpstreamModel: "integration",
		BoundAt:       now,
	}
	if err := store.BindResource(ctx, resource); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	itemResource := resource
	itemResource.Kind = responsesstate.ResourceItem
	itemResource.ResourceID = requestID + "-item"
	itemResource.ExpiresAt = now.Add(time.Hour)
	if err := store.BindResource(ctx, itemResource); err != nil {
		t.Fatalf("BindResource() item error = %v", err)
	}
	gotResource, found, err := store.ResolveResource(ctx, resource.ResourceKey)
	if err != nil || !found ||
		!responsesstate.SameResourceRoute(gotResource, resource) ||
		!gotResource.ExpiresAt.IsZero() {
		t.Fatalf(
			"ResolveResource() = %#v, %v, %v",
			gotResource,
			found,
			err,
		)
	}
	gotItem, found, err := store.ResolveResource(ctx, itemResource.ResourceKey)
	if err != nil || !found ||
		!responsesstate.SameResourceRoute(gotItem, itemResource) ||
		gotItem.ExpiresAt.IsZero() {
		t.Fatalf(
			"ResolveResource() item = %#v, %v, %v",
			gotItem,
			found,
			err,
		)
	}
	durableItem := itemResource
	durableItem.BoundAt = now.Add(time.Minute)
	durableItem.ExpiresAt = time.Time{}
	if err := store.BindResource(ctx, durableItem); err != nil {
		t.Fatalf("BindResource() durable item error = %v", err)
	}
	gotItem, found, err = store.ResolveResource(ctx, itemResource.ResourceKey)
	if err != nil || !found || !gotItem.ExpiresAt.IsZero() {
		t.Fatalf(
			"ResolveResource() durable item = %#v, %v, %v",
			gotItem,
			found,
			err,
		)
	}
	deletedAt := now.Add(time.Second)
	if err := store.TombstoneResource(
		ctx,
		resource.ResourceKey,
		deletedAt,
		deletedAt.Add(time.Hour),
	); err != nil {
		t.Fatalf("TombstoneResource() error = %v", err)
	}
	gotResource, found, err = store.ResolveResource(ctx, resource.ResourceKey)
	if err != nil || !found || !gotResource.Deleted() ||
		!gotResource.ExpiresAt.After(gotResource.DeletedAt) {
		t.Fatalf(
			"ResolveResource() tombstone = %#v, %v, %v",
			gotResource,
			found,
			err,
		)
	}
	traceRecord := savedtrace.Record{
		Version:         savedtrace.SchemaVersion,
		RequestID:       requestID + "-trace",
		StartedAt:       now,
		CompletedAt:     now.Add(time.Second),
		TenantID:        "tenant-integration",
		Metadata:        map[string]string{"sparkroute.experiment": "integration"},
		Protocol:        "openai",
		Operation:       "responses",
		VirtualModel:    "public",
		FinalDeployment: "integration",
		Outcome:         "success",
		Request:         savedtrace.Payload{Body: `{"input":"prompt"}`},
		Response:        savedtrace.Payload{Body: `{"output":[]}`},
	}
	if err := store.Append(ctx, traceRecord); err != nil {
		t.Fatalf("Append() saved trace error = %v", err)
	}
	tracePage, err := store.List(ctx, savedtrace.Query{
		TenantID:   "tenant-integration",
		Deployment: "integration",
		Metadata:   map[string]string{"sparkroute.experiment": "integration"},
	})
	if err != nil ||
		len(tracePage.Records) != 1 ||
		tracePage.Records[0].RequestID != traceRecord.RequestID ||
		tracePage.Records[0].Request.Body != traceRecord.Request.Body {
		t.Fatalf("List() saved traces = %#v, %v", tracePage, err)
	}

	replica, err := Open(ctx, Options{
		URL:                   url,
		Backend:               backend,
		ExpectedPostgresMajor: 17,
		MaxConnections:        2,
	})
	if err != nil {
		t.Fatalf("Open() second replica error = %v", err)
	}
	defer func() {
		if err := replica.Close(); err != nil {
			t.Errorf("Close(replica) error = %v", err)
		}
	}()
	promptRoute := promptcache.Route{
		Provider: "integration", Deployment: "integration",
		UpstreamModel: "integration", UpstreamProtocol: "openai",
	}
	promptPrefix := promptcache.Prefix{
		Digest: requestID + "-prefix", Bytes: 4096, Segments: 2,
	}
	if err := store.Record(ctx, promptcache.Observation{
		ScopeDigest: requestID + "-scope", VirtualModel: "public",
		Operation: "chat_completions", ConfigRevision: "revision-a",
		Prefixes: []promptcache.Prefix{promptPrefix}, Route: promptRoute,
		LastSuccess: now, ExpiresAt: now.Add(time.Minute),
	}); err != nil {
		t.Fatalf("Record() prompt-cache affinity error = %v", err)
	}
	promptMatch, found, err := replica.Lookup(ctx, promptcache.Query{
		ScopeDigest: requestID + "-scope", VirtualModel: "public",
		Operation: "chat_completions", ConfigRevision: "revision-a",
		Prefixes:   []promptcache.Prefix{promptPrefix},
		Candidates: []promptcache.Route{promptRoute}, Now: now.Add(time.Second),
	})
	if err != nil || !found || promptMatch.Route != promptRoute ||
		promptMatch.Prefix.Digest != promptPrefix.Digest {
		t.Fatalf("Lookup() cross-replica prompt-cache affinity = %#v, %v, %v", promptMatch, found, err)
	}
	replicaScope := requestID + "-replica-scope"
	replicaFile := responsesstate.FileRecord{
		Scope: replicaScope, ID: requestID + "-replica-file", Object: "file",
		Bytes: 42, CreatedAt: now.Unix(), Filename: "shared.txt", Purpose: "user_data",
	}
	replicaAffinity := responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope: replicaScope, Kind: responsesstate.ResourceFile, ResourceID: replicaFile.ID,
		},
		VirtualModel: "public", Provider: "integration",
		Deployment: "integration", UpstreamModel: "integration", BoundAt: now,
	}
	if err := store.BindFile(ctx, replicaAffinity, replicaFile); err != nil {
		t.Fatalf("BindFile() first replica error = %v", err)
	}
	replicaPage, err := replica.ListFiles(ctx, responsesstate.FileQuery{Scope: replicaScope})
	if err != nil || len(replicaPage.Records) != 1 || replicaPage.Records[0] != replicaFile {
		t.Fatalf("ListFiles() second replica = %#v, %v", replicaPage, err)
	}
	deletedReplicaFileAt := now.Add(2 * time.Second)
	if err := replica.TombstoneResource(
		ctx,
		replicaAffinity.ResourceKey,
		deletedReplicaFileAt,
		deletedReplicaFileAt.Add(time.Hour),
	); err != nil {
		t.Fatalf("TombstoneResource() second replica error = %v", err)
	}
	replicaPage, err = store.ListFiles(ctx, responsesstate.FileQuery{Scope: replicaScope})
	if err != nil || len(replicaPage.Records) != 0 {
		t.Fatalf("ListFiles() first replica after delete = %#v, %v", replicaPage, err)
	}

	ledgertest.RunConformance(t, store, normalizePostgresTime)
	responsesstatetest.RunFileStoreConformance(t, store)
}
