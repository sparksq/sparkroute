package savedtrace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueryCursorMetadataAndJSONLExport(t *testing.T) {
	t.Parallel()

	start := time.Unix(100, 0).UTC()
	records := []Record{
		testRecord("newer", start.Add(time.Second), "tenant-a", "workspace-a"),
		testRecord("matching", start, "tenant-a", "workspace-a"),
		testRecord("other", start.Add(-time.Second), "tenant-b", "workspace-b"),
	}
	store := &memoryStore{records: records}
	query := Query{
		TenantID: "tenant-a",
		Metadata: map[string]string{"sparkroute.workspace": "workspace-a"},
		Limit:    1,
	}
	page, err := store.List(context.Background(), query)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].RequestID != "newer" ||
		page.NextCursor == "" {
		t.Fatalf("first page = %#v", page)
	}
	query.Cursor = page.NextCursor
	page, err = store.List(context.Background(), query)
	if err != nil {
		t.Fatalf("List() second error = %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].RequestID != "matching" {
		t.Fatalf("second page = %#v", page)
	}

	var output bytes.Buffer
	exported, err := ExportJSONL(
		context.Background(),
		store,
		Query{TenantID: "tenant-a"},
		&output,
		0,
	)
	if err != nil || exported != 2 {
		t.Fatalf("ExportJSONL() = %d, %v", exported, err)
	}
	if strings.Count(strings.TrimSpace(output.String()), "\n") != 1 ||
		!strings.Contains(output.String(), `"body":"request"`) {
		t.Fatalf("export = %q", output.String())
	}
}

func TestExportJSONLOpensAndClosesRecordSnapshot(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	store := &datasetSnapshotStore{records: []Record{
		testRecord("request-c", started.Add(2*time.Second), "tenant-a", "workspace-a"),
		testRecord("request-b", started.Add(time.Second), "tenant-a", "workspace-a"),
		testRecord("request-a", started, "tenant-a", "workspace-a"),
	}}
	var output bytes.Buffer
	exported, err := ExportJSONL(context.Background(), store, Query{}, &output, 2)
	if err != nil {
		t.Fatalf("ExportJSONL() error = %v", err)
	}
	if exported != 2 || store.opens != 1 || store.visits != 1 || store.closes != 1 ||
		store.lists != 0 || store.legacyVisits != 0 {
		t.Fatalf(
			"export/calls = %d / opens=%d visits=%d closes=%d lists=%d legacy=%d",
			exported,
			store.opens,
			store.visits,
			store.closes,
			store.lists,
			store.legacyVisits,
		)
	}
}

func TestAsyncRecorderDrainsBeforeClose(t *testing.T) {
	t.Parallel()

	store := &lockedMemoryStore{}
	recorder, err := NewAsyncRecorder(store, AsyncOptions{QueueCapacity: 4})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	recorder.Record(testRecord(
		"request-a",
		time.Unix(100, 0).UTC(),
		"tenant-a",
		"workspace-a",
	))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := store.count(); got != 1 {
		t.Fatalf("stored records = %d", got)
	}
	if got := recorder.Stats(); got.Accepted != 1 || got.Persisted != 1 || got.Pending != 0 || got.Lost != 0 {
		t.Fatalf("stats = %#v", got)
	}
	recorder.Record(testRecord(
		"late",
		time.Unix(101, 0).UTC(),
		"tenant-a",
		"workspace-a",
	))
	if recorder.Lost() != 1 {
		t.Fatalf("lost = %d, want 1", recorder.Lost())
	}
}

func TestAsyncRecorderBlocksInsteadOfDroppingWhenFull(t *testing.T) {
	t.Parallel()

	store := newBlockingStore()
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 1,
		Workers:       1,
		Overflow:      OverflowBlock,
	})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	recorder.Record(testRecord("one", time.Unix(100, 0).UTC(), "tenant", "workspace"))
	<-store.started
	recorder.Record(testRecord("two", time.Unix(101, 0).UTC(), "tenant", "workspace"))
	returned := make(chan struct{})
	go func() {
		recorder.Record(testRecord("three", time.Unix(102, 0).UTC(), "tenant", "workspace"))
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("Record() returned while the queue and worker were blocked")
	case <-time.After(25 * time.Millisecond):
	}
	close(store.release)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("Record() did not resume after persistence made progress")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := store.count(); got != 3 {
		t.Fatalf("stored records = %d, want 3", got)
	}
	if got := recorder.Stats(); got.Accepted != 3 || got.Persisted != 3 || got.Pending != 0 || got.Lost != 0 {
		t.Fatalf("stats = %#v", got)
	}
}

func TestAsyncRecorderDropPolicyRemainsExplicitlyAvailable(t *testing.T) {
	t.Parallel()

	store := newBlockingStore()
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 1,
		Workers:       1,
		Overflow:      OverflowDrop,
	})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	recorder.Record(testRecord("one", time.Unix(100, 0).UTC(), "tenant", "workspace"))
	<-store.started
	recorder.Record(testRecord("two", time.Unix(101, 0).UTC(), "tenant", "workspace"))
	recorder.Record(testRecord("three", time.Unix(102, 0).UTC(), "tenant", "workspace"))
	if got := recorder.Stats(); got.Accepted != 2 || got.QueueFull != 1 || got.Lost != 1 {
		t.Fatalf("stats before release = %#v", got)
	}
	close(store.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestAsyncRecorderUsesConfiguredWorkers(t *testing.T) {
	t.Parallel()

	store := newBlockingStore()
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 8,
		Workers:       4,
		Overflow:      OverflowBlock,
	})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	for index := range 4 {
		recorder.Record(testRecord(
			string(rune('a'+index)),
			time.Unix(int64(100+index), 0).UTC(),
			"tenant",
			"workspace",
		))
	}
	deadline := time.After(time.Second)
	for store.startedCount.Load() < 4 {
		select {
		case <-store.started:
		case <-deadline:
			t.Fatalf("started workers = %d, want 4", store.startedCount.Load())
		}
	}
	close(store.release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := store.maxConcurrent.Load(); got != 4 {
		t.Fatalf("maximum concurrent writes = %d, want 4", got)
	}
}

func TestAsyncRecorderUsesBatchStoreGroupCommit(t *testing.T) {
	t.Parallel()

	store := &batchMemoryStore{}
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 128,
		Workers:       1,
		Overflow:      OverflowBlock,
		BatchSize:     100,
		BatchInterval: time.Second,
	})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	for index := range 100 {
		recorder.Record(testRecord(
			fmt.Sprintf("request-%d", index),
			time.Unix(int64(100+index), 0).UTC(),
			"tenant",
			"workspace",
		))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if records, batches := store.counts(); records != 100 || batches != 1 {
		t.Fatalf("records, batches = %d, %d; want 100, 1", records, batches)
	}
	if got := recorder.Stats(); got.Accepted != 100 || got.Persisted != 100 || got.Lost != 0 {
		t.Fatalf("stats = %#v", got)
	}
}

func TestAsyncRecorderReportsStoreFailureOnClose(t *testing.T) {
	t.Parallel()

	recorder, err := NewAsyncRecorder(failingStore{}, AsyncOptions{})
	if err != nil {
		t.Fatalf("NewAsyncRecorder() error = %v", err)
	}
	recorder.Record(testRecord("failed", time.Unix(100, 0).UTC(), "tenant", "workspace"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err == nil {
		t.Fatal("Close() error = nil")
	}
	if got := recorder.Stats(); got.Accepted != 1 || got.Persisted != 0 || got.Pending != 0 || got.StoreFailures != 1 {
		t.Fatalf("stats = %#v", got)
	}
}

func TestRecordValidateAcceptsEncodedAWSEventStream(t *testing.T) {
	t.Parallel()

	record := testRecord(
		"binary",
		time.Unix(100, 0).UTC(),
		"tenant-a",
		"workspace-a",
	)
	record.Response = Payload{
		ContentType: "application/vnd.amazon.eventstream",
		Body:        Base64BodyPrefix + "AAECAw==",
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	record.Response.Body = Base64BodyPrefix + "not-base64"
	if err := record.Validate(); err == nil {
		t.Fatal("Validate() error = nil for invalid base64")
	}
}

func testRecord(id string, started time.Time, tenant, workspace string) Record {
	return Record{
		Version:     SchemaVersion,
		RequestID:   id,
		StartedAt:   started,
		CompletedAt: started.Add(time.Second),
		TenantID:    tenant,
		Metadata:    map[string]string{"sparkroute.workspace": workspace},
		Protocol:    "openai",
		Operation:   "chat_completions",
		Outcome:     "success",
		Request:     Payload{ContentType: "application/json", Body: "request"},
		Response:    Payload{ContentType: "application/json", Body: "response"},
	}
}

type memoryStore struct {
	records []Record
}

func (s *memoryStore) Append(_ context.Context, record Record) error {
	s.records = append(s.records, record)
	return nil
}

func (s *memoryStore) List(_ context.Context, query Query) (Page, error) {
	plan, err := PlanQuery(&query)
	if err != nil {
		return Page{}, err
	}
	matches := make([]Record, 0)
	for _, record := range s.records {
		if Matches(record, query, plan) {
			matches = append(matches, record)
		}
	}
	page := Page{Records: matches}
	if len(matches) > plan.Limit {
		page.Records = matches[:plan.Limit]
		page.NextCursor, err = NextCursor(page.Records[plan.Limit-1])
	}
	return page, err
}

func (*memoryStore) Close() error { return nil }

type lockedMemoryStore struct {
	mu      sync.Mutex
	records []Record
}

type blockingStore struct {
	mu            sync.Mutex
	records       []Record
	started       chan struct{}
	release       chan struct{}
	startedCount  atomic.Int64
	concurrent    atomic.Int64
	maxConcurrent atomic.Int64
}

func newBlockingStore() *blockingStore {
	return &blockingStore{
		started: make(chan struct{}, 16),
		release: make(chan struct{}),
	}
}

func (s *blockingStore) Append(ctx context.Context, record Record) error {
	s.startedCount.Add(1)
	current := s.concurrent.Add(1)
	defer s.concurrent.Add(-1)
	for {
		maximum := s.maxConcurrent.Load()
		if current <= maximum || s.maxConcurrent.CompareAndSwap(maximum, current) {
			break
		}
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	return nil
}

func (*blockingStore) List(context.Context, Query) (Page, error) { return Page{}, nil }
func (*blockingStore) Close() error                              { return nil }
func (s *blockingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

type failingStore struct{}

func (failingStore) Append(context.Context, Record) error { return errors.New("write failed") }
func (failingStore) List(context.Context, Query) (Page, error) {
	return Page{}, nil
}
func (failingStore) Close() error { return nil }

type batchMemoryStore struct {
	mu      sync.Mutex
	records int
	batches int
}

func (s *batchMemoryStore) Append(ctx context.Context, record Record) error {
	return s.AppendTraceBatch(ctx, []Record{record})
}

func (s *batchMemoryStore) AppendTraceBatch(_ context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records += len(records)
	s.batches++
	return nil
}

func (*batchMemoryStore) List(context.Context, Query) (Page, error) { return Page{}, nil }
func (*batchMemoryStore) Close() error                              { return nil }
func (s *batchMemoryStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.records, s.batches
}

func (s *lockedMemoryStore) Append(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

func (s *lockedMemoryStore) List(context.Context, Query) (Page, error) {
	return Page{}, nil
}

func (s *lockedMemoryStore) Close() error { return nil }

func (s *lockedMemoryStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}
