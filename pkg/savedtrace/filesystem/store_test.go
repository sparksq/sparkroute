package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func TestStoreRoundTripAndFilters(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	record := savedtrace.Record{
		Version:     savedtrace.SchemaVersion,
		RequestID:   "request-a",
		StartedAt:   time.Unix(100, 0).UTC(),
		CompletedAt: time.Unix(101, 0).UTC(),
		TenantID:    "tenant-a",
		Metadata:    map[string]string{"sparkroute.task_id": "task-a"},
		Protocol:    "openai",
		Operation:   "responses",
		Outcome:     "success",
		Request:     savedtrace.Payload{Body: `{"input":"hello"}`},
		Response:    savedtrace.Payload{Body: `{"output":[]}`},
	}
	if err := store.Append(context.Background(), record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	page, err := store.List(context.Background(), savedtrace.Query{
		TenantID: "tenant-a",
		Metadata: map[string]string{"sparkroute.task_id": "task-a"},
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].Request.Body != record.Request.Body {
		t.Fatalf("page = %#v", page)
	}
	page, err = store.List(context.Background(), savedtrace.Query{
		Metadata: map[string]string{"sparkroute.task_id": "different"},
	})
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("filtered page = %#v, %v", page, err)
	}
	if info, err := os.Stat(root); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("root permissions = %v, %v", info.Mode().Perm(), err)
	}
}

func TestExportJSONLScansJournalOnceAndExportsEveryRecord(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "traces")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer store.Close()
	const count = 37
	records := make([]savedtrace.Record, 0, count)
	for index := 0; index < count; index++ {
		started := time.Unix(1_000+int64(index), 0).UTC()
		records = append(records, savedtrace.Record{
			Version: savedtrace.SchemaVersion, RequestID: fmt.Sprintf("request-%03d", index),
			StartedAt: started, CompletedAt: started.Add(time.Millisecond),
			Protocol: "openai", Operation: "chat_completions", Outcome: "success",
		})
	}
	if err := store.AppendTraceBatch(context.Background(), records); err != nil {
		t.Fatalf("AppendTraceBatch() error = %v", err)
	}
	var output bytes.Buffer
	exported, err := savedtrace.ExportJSONL(
		context.Background(), store, savedtrace.Query{}, &output, 0,
	)
	if err != nil {
		t.Fatalf("ExportJSONL() error = %v", err)
	}
	if exported != count {
		t.Fatalf("ExportJSONL() count = %d, want %d", exported, count)
	}
	decoder := json.NewDecoder(&output)
	for index := count - 1; index >= 0; index-- {
		var record savedtrace.Record
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode record %d: %v", index, err)
		}
		if want := fmt.Sprintf("request-%03d", index); record.RequestID != want {
			t.Fatalf("record request ID = %q, want %q", record.RequestID, want)
		}
	}
	if err := decoder.Decode(&savedtrace.Record{}); !errors.Is(err, io.EOF) {
		t.Fatalf("trailing decode error = %v", err)
	}
}

func TestStoreBatchUsesOneJournalAndSurvivesReopen(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	started := time.Unix(100, 0).UTC()
	records := []savedtrace.Record{
		filesystemTestRecord("request-a", started),
		filesystemTestRecord("request-b", started.Add(time.Nanosecond)),
		filesystemTestRecord("request-c", started.Add(2*time.Nanosecond)),
	}
	if err := store.AppendBatch(context.Background(), records); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	journal := filepath.Join(root, "1970", "01", "01", journalName)
	file, err := os.Open(journal)
	if err != nil {
		t.Fatalf("Open(journal) error = %v", err)
	}
	decoder := json.NewDecoder(file)
	decoded := 0
	for {
		var record savedtrace.Record
		if err := decoder.Decode(&record); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		decoded++
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if decoded != len(records) {
		t.Fatalf("journal records = %d, want %d", decoded, len(records))
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	page, err := reopened.List(context.Background(), savedtrace.Query{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != len(records) || page.Records[0].RequestID != "request-c" {
		t.Fatalf("page = %#v", page)
	}
}

func TestOpenRepairsOnlyIncompleteJournalTail(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := store.AppendBatch(context.Background(), []savedtrace.Record{
		filesystemTestRecord("request-a", time.Unix(100, 0).UTC()),
		filesystemTestRecord("request-b", time.Unix(101, 0).UTC()),
	}); err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	journal := filepath.Join(root, "1970", "01", "01", journalName)
	file, err := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"request_id":"partial`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	page, err := reopened.List(context.Background(), savedtrace.Query{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 2 {
		t.Fatalf("records after repair = %d, want 2", len(page.Records))
	}
}

func filesystemTestRecord(requestID string, started time.Time) savedtrace.Record {
	return savedtrace.Record{
		Version:     savedtrace.SchemaVersion,
		RequestID:   requestID,
		StartedAt:   started,
		CompletedAt: started.Add(time.Second),
		Protocol:    "openai",
		Operation:   "responses",
		Outcome:     "success",
	}
}

func TestStorePaginatesEqualTimestampsByRequestID(t *testing.T) {
	t.Parallel()

	store, err := Open(filepath.Join(t.TempDir(), "traces"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	started := time.Unix(100, 0).UTC()
	for _, requestID := range []string{"request-a", "request-b"} {
		record := savedtrace.Record{
			Version:     savedtrace.SchemaVersion,
			RequestID:   requestID,
			StartedAt:   started,
			CompletedAt: started.Add(time.Second),
			Protocol:    "openai",
			Operation:   "responses",
			Outcome:     "success",
		}
		if err := store.Append(context.Background(), record); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	query := savedtrace.Query{Limit: 1}
	page, err := store.List(context.Background(), query)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 1 ||
		page.Records[0].RequestID != "request-b" ||
		page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	query.Cursor = page.NextCursor
	page, err = store.List(context.Background(), query)
	if err != nil {
		t.Fatalf("List() second error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].RequestID != "request-a" {
		t.Fatalf("second page = %#v", page)
	}
}

func TestRecordSnapshotExternalMergeIsOrderedReplayableAndBounded(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := OpenWithOptions(root, Options{
		ExportMemoryBytes: 3 * indexEntrySize,
		ExportDiskBytes:   1 << 20,
		ExportMergeFanIn:  2,
		ExportOpenFiles:   1,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions() error = %v", err)
	}
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	order := []int{9, 1, 14, 3, 12, 5, 18, 7, 0, 16, 2, 11, 4, 19, 6, 15, 8, 17, 10, 13}
	records := make([]savedtrace.Record, 0, len(order))
	for _, index := range order {
		record := filesystemTestRecord(
			fmt.Sprintf("request-%02d", index),
			base.Add(time.Duration(index/2)*time.Nanosecond),
		)
		if index%3 == 0 || index%3 == 1 {
			record.TenantID = "tenant-a"
		} else {
			record.TenantID = "tenant-b"
		}
		records = append(records, record)
	}
	if err := store.AppendTraceBatch(context.Background(), records); err != nil {
		t.Fatalf("AppendTraceBatch() error = %v", err)
	}
	matching := make([]savedtrace.Record, 0)
	for _, record := range records {
		if record.TenantID == "tenant-a" {
			matching = append(matching, record)
		}
	}
	sort.Slice(matching, func(left, right int) bool {
		if matching[left].StartedAt.Equal(matching[right].StartedAt) {
			return matching[left].RequestID > matching[right].RequestID
		}
		return matching[left].StartedAt.After(matching[right].StartedAt)
	})
	cursor, err := savedtrace.NextCursor(matching[1])
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.OpenRecordSnapshot(context.Background(), savedtrace.Query{
		TenantID: "tenant-a",
		Cursor:   cursor,
	}, 5)
	if err != nil {
		t.Fatalf("OpenRecordSnapshot() error = %v", err)
	}
	concrete := snapshot.(*recordSnapshot)
	workspaceInfo, err := os.Stat(concrete.workspace)
	if err != nil {
		t.Fatalf("stat snapshot workspace: %v", err)
	}
	if workspaceInfo.Mode().Perm() != 0o700 {
		t.Fatalf("snapshot workspace permissions = %v", workspaceInfo.Mode().Perm())
	}
	indexInfo, err := os.Stat(concrete.indexPath)
	if err != nil {
		t.Fatalf("stat snapshot index: %v", err)
	}
	if indexInfo.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot index permissions = %v", indexInfo.Mode().Perm())
	}
	if !snapshot.Limited() {
		t.Fatal("snapshot Limited() = false, want true")
	}

	newer := filesystemTestRecord("request-new", base.Add(time.Hour))
	newer.TenantID = "tenant-a"
	if err := store.Append(context.Background(), newer); err != nil {
		t.Fatalf("Append(newer) error = %v", err)
	}
	want := matching[2:7]
	for replay := 0; replay < 2; replay++ {
		var requestIDs []string
		if err := snapshot.Visit(context.Background(), func(record savedtrace.Record) error {
			requestIDs = append(requestIDs, record.RequestID)
			return nil
		}); err != nil {
			t.Fatalf("Visit(%d) error = %v", replay, err)
		}
		if len(requestIDs) != len(want) {
			t.Fatalf("Visit(%d) IDs = %#v", replay, requestIDs)
		}
		for index := range want {
			if requestIDs[index] != want[index].RequestID {
				t.Fatalf("Visit(%d) IDs = %#v, want index %d = %q", replay, requestIDs, index, want[index].RequestID)
			}
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := snapshot.Visit(cancelled, func(savedtrace.Record) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Visit() error = %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if _, err := os.Stat(concrete.workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot workspace remains after Close(): %v", err)
	}
	if err := snapshot.Visit(context.Background(), func(savedtrace.Record) error { return nil }); !errors.Is(err, ErrSnapshotClosed) {
		t.Fatalf("Visit() after Close() error = %v", err)
	}
	page, err := store.List(context.Background(), savedtrace.Query{TenantID: "tenant-a"})
	if err != nil {
		t.Fatalf("List() after append error = %v", err)
	}
	if len(page.Records) == 0 || page.Records[0].RequestID != newer.RequestID {
		t.Fatalf("List() does not include post-snapshot append: %#v", page)
	}
}

func TestRecordSnapshotFailsClosedAtDiskBudgetAndCleansWork(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	store, err := OpenWithOptions(root, Options{
		ExportMemoryBytes: 2 * indexEntrySize,
		ExportDiskBytes:   5 * indexEntrySize,
		ExportMergeFanIn:  2,
	})
	if err != nil {
		t.Fatalf("OpenWithOptions() error = %v", err)
	}
	records := make([]savedtrace.Record, 6)
	for index := range records {
		records[index] = filesystemTestRecord(
			fmt.Sprintf("request-%d", index),
			time.Unix(int64(100+index), 0).UTC(),
		)
	}
	if err := store.AppendTraceBatch(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenRecordSnapshot(context.Background(), savedtrace.Query{}, 0); !errors.Is(err, ErrExportDiskBudget) {
		t.Fatalf("OpenRecordSnapshot() error = %v, want disk budget", err)
	}
	entries, err := os.ReadDir(store.exportRoot())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len(exportSnapshotPrefix) && entry.Name()[:len(exportSnapshotPrefix)] == exportSnapshotPrefix {
			t.Fatalf("failed snapshot work remains: %q", entry.Name())
		}
	}
}

func TestOpenRemovesOnlyKnownStaleExportSnapshots(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "traces")
	if err := os.MkdirAll(filepath.Join(root, exportWorkDirectory, exportSnapshotPrefix+"stale"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, exportWorkDirectory, exportSnapshotPrefix+"stale", "run.idx"),
		[]byte("stale"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	staleDirectory := filepath.Join(root, exportWorkDirectory, exportSnapshotPrefix+"stale")
	staleAt := time.Now().Add(-staleExportWorkAge - time.Hour)
	if err := os.Chtimes(staleDirectory, staleAt, staleAt); err != nil {
		t.Fatal(err)
	}
	recentDirectory := filepath.Join(root, exportWorkDirectory, exportSnapshotPrefix+"recent")
	if err := os.Mkdir(recentDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, exportWorkDirectory, "operator-marker")
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.exportRoot(), exportSnapshotPrefix+"stale")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale snapshot was not removed: %v", err)
	}
	if _, err := os.Stat(recentDirectory); err != nil {
		t.Fatalf("recent snapshot work was removed: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("unrecognized export-work entry was removed: %v", err)
	}
}
