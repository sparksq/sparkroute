// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
	tracefilesystem "github.com/sparksq/sparkroute/pkg/savedtrace/filesystem"
)

func TestTraceExportCommandFiltersFilesystemRecords(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := tracefilesystem.Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	started := time.Unix(100, 0).UTC()
	for _, tenant := range []string{"tenant-a", "tenant-b"} {
		record := savedtrace.Record{
			Version:     savedtrace.SchemaVersion,
			RequestID:   "request-" + tenant,
			StartedAt:   started,
			CompletedAt: started.Add(time.Second),
			TenantID:    tenant,
			Metadata:    map[string]string{"sparkroute.workspace": "workspace-a"},
			Protocol:    "openai",
			Operation:   "responses",
			Outcome:     "success",
			Request:     savedtrace.Payload{Body: "request-" + tenant},
			Response:    savedtrace.Payload{Body: "response-" + tenant},
		}
		if err := store.Append(context.Background(), record); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	var output bytes.Buffer
	err = run(
		context.Background(),
		[]string{
			"traces", "export",
			"-storage", "filesystem",
			"-filesystem", root,
			"-tenant", "tenant-a",
			"-metadata", "sparkroute.workspace=workspace-a",
		},
		&output,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if !strings.Contains(output.String(), `"tenant_id":"tenant-a"`) ||
		strings.Contains(output.String(), `"tenant_id":"tenant-b"`) {
		t.Fatalf("export = %q", output.String())
	}
}
