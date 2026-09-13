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

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func TestMigrationUsesPortablePostgreSQL(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/001_saved_traces.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := strings.ToLower(string(content))
	for _, required := range []string{
		"create table if not exists llm_saved_traces",
		"metadata_json jsonb",
		"llm_saved_traces_tenant_started_idx",
		"using gin (metadata_json)",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("saved-trace migration does not contain %q", required)
		}
	}
	for _, extension := range []string{"timescaledb", "create_hypertable", "drop_chunks"} {
		if strings.Contains(sql, extension) {
			t.Fatalf("saved-trace migration unexpectedly uses %q", extension)
		}
	}
	datasetMigration, err := migrationFiles.ReadFile("migrations/002_trace_datasets.sql")
	if err != nil {
		t.Fatal(err)
	}
	datasetSQL := strings.ToLower(string(datasetMigration))
	for _, required := range []string{"capture_outcome", "capture_session_id", "llm_saved_trace_capture_sessions"} {
		if !strings.Contains(datasetSQL, required) {
			t.Fatalf("dataset migration does not contain %q", required)
		}
	}
}

func TestStoreIntegration(t *testing.T) {
	url := os.Getenv("SPARKROUTE_TEST_TRACE_POSTGRES_URL")
	if url == "" {
		t.Skip("SPARKROUTE_TEST_TRACE_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, Options{
		URL:                   url,
		AutoMigrate:           true,
		ExpectedPostgresMajor: 17,
		MaxConnections:        2,
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	record := savedtrace.Record{
		Version:          savedtrace.SchemaVersion,
		RequestID:        fmt.Sprintf("trace-integration-%d", now.UnixNano()),
		StartedAt:        now,
		CompletedAt:      now.Add(time.Second),
		TenantID:         "tenant-a",
		ConversationID:   "conversation-a",
		CaptureSessionID: "capture-a",
		Metadata:         map[string]string{"sparkroute.experiment": "plain-postgres"},
		Protocol:         "openai",
		Operation:        "responses",
		Outcome:          "success",
		CaptureOutcome:   savedtrace.CaptureComplete,
		Request:          savedtrace.Payload{Body: `{"input":"hello"}`},
		Response:         savedtrace.Payload{Body: `{"output":"world"}`},
	}
	if err := store.Append(ctx, record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	page, err := store.List(ctx, savedtrace.Query{
		TenantID: "tenant-a",
		Metadata: map[string]string{"sparkroute.experiment": "plain-postgres"},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].RequestID != record.RequestID {
		t.Fatalf("List() = %#v", page)
	}
	completed := now.Add(2 * time.Second)
	if err := store.UpsertCaptureSession(ctx, savedtrace.CaptureSession{
		ID: "capture-a", StartedAt: now.Add(-time.Second), UpdatedAt: completed,
		CompletedAt: &completed, Mode: "block", Accepted: 1, Persisted: 1,
	}); err != nil {
		t.Fatal(err)
	}
	sessions, err := store.ListCaptureSessions(ctx, savedtrace.CaptureSessionQuery{
		StartedBefore: completed.Add(time.Second), UpdatedAfter: now,
	})
	if err != nil || len(sessions) == 0 {
		t.Fatalf("capture sessions = %#v, %v", sessions, err)
	}
}
