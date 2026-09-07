package savedtrace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestExportDatasetWritesDeepSpecRowsAndCompleteManifest(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	end := started.Add(2 * time.Second)
	record := testRecord("request-a", started, "tenant-a", "workspace-a")
	record.HTTPStatus = 200
	record.CaptureOutcome = CaptureComplete
	record.CaptureSessionID = "capture-a"
	record.ConversationID = "conversation-a"
	record.Request.Body = `{"model":"chat","messages":[{"role":"user","content":"hello"}]}`
	record.Response.Body = `{"id":"response-a","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	completed := end.Add(time.Second)
	store := &datasetMemoryStore{
		memoryStore: memoryStore{records: []Record{record}},
		sessions: []CaptureSession{{
			ID: "capture-a", StartedAt: started.Add(-time.Second), UpdatedAt: completed,
			CompletedAt: &completed, Mode: "block", Accepted: 1, Persisted: 1,
		}},
	}
	var output bytes.Buffer
	manifest, err := ExportDataset(context.Background(), store, Query{
		StartedAtOrAfter: started.Add(-time.Second), StartedAtBefore: end,
	}, &output, DatasetOptions{Projection: ProjectionDeepSpec, RequireComplete: true})
	if err != nil {
		t.Fatalf("ExportDataset() error = %v", err)
	}
	if !manifest.CaptureComplete || manifest.MatchingRecords != 1 || manifest.DatasetRecords != 1 {
		t.Fatalf("manifest = %#v", manifest)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	files := make(map[string][]byte)
	for _, file := range archive.File {
		reader, openErr := file.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		files[file.Name], err = io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Contains(files["dataset.jsonl"], []byte(`"conversations"`)) ||
		!bytes.Contains(files["dataset.jsonl"], []byte(`"conversation_id":"conversation-a"`)) {
		t.Fatalf("dataset = %s", files["dataset.jsonl"])
	}
	var archived DatasetManifest
	if err := json.Unmarshal(files["manifest.json"], &archived); err != nil {
		t.Fatal(err)
	}
	if !archived.CaptureComplete || archived.ContentPolicyDigest == "" {
		t.Fatalf("archived manifest = %#v", archived)
	}
}

func TestExportDatasetRequireCompleteRejectsLossBeforeWriting(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	end := started.Add(2 * time.Second)
	record := testRecord("request-a", started, "tenant-a", "workspace-a")
	record.CaptureSessionID = "capture-a"
	record.CaptureOutcome = CaptureComplete
	completed := end.Add(time.Second)
	store := &datasetMemoryStore{
		memoryStore: memoryStore{records: []Record{record}},
		sessions: []CaptureSession{{
			ID: "capture-a", StartedAt: started.Add(-time.Second), UpdatedAt: completed,
			CompletedAt: &completed, Mode: "drop", Accepted: 1, Persisted: 1,
			Lost: 4, QueueFull: 4,
		}},
	}
	var output bytes.Buffer
	manifest, err := ExportDataset(context.Background(), store, Query{
		StartedAtOrAfter: started.Add(-time.Second), StartedAtBefore: end,
	}, &output, DatasetOptions{RequireComplete: true})
	if !errors.Is(err, ErrIncompleteDataset) {
		t.Fatalf("ExportDataset() error = %v", err)
	}
	if manifest.CaptureComplete || output.Len() != 0 {
		t.Fatalf("manifest/output = %#v / %d", manifest, output.Len())
	}
}

func TestExportDatasetUsesRecordVisitorAndPreservesLimit(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	store := &datasetVisitorStore{
		records: []Record{
			testRecord("request-c", started.Add(2*time.Second), "tenant-a", "workspace-a"),
			testRecord("request-b", started.Add(time.Second), "tenant-a", "workspace-a"),
			testRecord("request-a", started, "tenant-a", "workspace-a"),
		},
	}
	var output bytes.Buffer
	manifest, err := ExportDataset(context.Background(), store, Query{}, &output, DatasetOptions{
		Projection: ProjectionCanonical, MaximumRecords: 2,
	})
	if err != nil {
		t.Fatalf("ExportDataset() error = %v", err)
	}
	if store.visits != 2 || store.lists != 0 {
		t.Fatalf("store calls: visits=%d lists=%d, want visits=2 lists=0", store.visits, store.lists)
	}
	if manifest.MatchingRecords != 2 || manifest.DatasetRecords != 2 || !manifest.RecordLimitApplied {
		t.Fatalf("manifest = %#v", manifest)
	}
	archive, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	for _, file := range archive.File {
		if file.Name != "records.jsonl" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(reader)
		var requestIDs []string
		for {
			var record Record
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			requestIDs = append(requestIDs, record.RequestID)
		}
		_ = reader.Close()
		if len(requestIDs) != 2 || requestIDs[0] != "request-c" || requestIDs[1] != "request-b" {
			t.Fatalf("dataset request IDs = %#v", requestIDs)
		}
		return
	}
	t.Fatal("records.jsonl is missing")
}

func TestExportDatasetOpensOneReplayableRecordSnapshot(t *testing.T) {
	t.Parallel()
	started := time.Unix(100, 0).UTC()
	store := &datasetSnapshotStore{records: []Record{
		testRecord("request-c", started.Add(2*time.Second), "tenant-a", "workspace-a"),
		testRecord("request-b", started.Add(time.Second), "tenant-a", "workspace-a"),
		testRecord("request-a", started, "tenant-a", "workspace-a"),
	}}
	var output bytes.Buffer
	manifest, err := ExportDataset(context.Background(), store, Query{}, &output, DatasetOptions{
		Projection: ProjectionCanonical, MaximumRecords: 2,
	})
	if err != nil {
		t.Fatalf("ExportDataset() error = %v", err)
	}
	if store.opens != 1 || store.visits != 2 || store.closes != 1 ||
		store.lists != 0 || store.legacyVisits != 0 {
		t.Fatalf(
			"snapshot calls: opens=%d visits=%d closes=%d lists=%d legacy=%d",
			store.opens,
			store.visits,
			store.closes,
			store.lists,
			store.legacyVisits,
		)
	}
	if manifest.MatchingRecords != 2 || manifest.DatasetRecords != 2 ||
		!manifest.RecordLimitApplied {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestDatasetManifestValuesAreBounded(t *testing.T) {
	t.Parallel()
	values := map[string]struct{}{}
	count, byteCount := maximumDatasetManifestValues-1, 0
	if err := addDatasetManifestValue(values, "first", &count, &byteCount); err != nil {
		t.Fatalf("addDatasetManifestValue() at limit: %v", err)
	}
	if err := addDatasetManifestValue(values, "first", &count, &byteCount); err != nil {
		t.Fatalf("addDatasetManifestValue() duplicate: %v", err)
	}
	if err := addDatasetManifestValue(values, "overflow", &count, &byteCount); err == nil {
		t.Fatal("addDatasetManifestValue() accepted value beyond cardinality limit")
	}

	values = map[string]struct{}{}
	count, byteCount = 0, maximumDatasetManifestValueBytes
	if err := addDatasetManifestValue(values, "overflow", &count, &byteCount); err == nil {
		t.Fatal("addDatasetManifestValue() accepted value beyond byte limit")
	}
}

func TestAsyncRecorderPersistsCompletedCaptureSession(t *testing.T) {
	t.Parallel()
	store := &sessionRecorderStore{}
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		SessionID: "capture-test", BatchSize: 2, BatchInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder.Record(testRecord("request-a", time.Unix(100, 0).UTC(), "tenant", "workspace"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.records) != 1 || store.records[0].CaptureSessionID != "capture-test" {
		t.Fatalf("records = %#v", store.records)
	}
	if len(store.sessions) == 0 {
		t.Fatal("no capture sessions")
	}
	final := store.sessions[len(store.sessions)-1]
	if final.CompletedAt == nil || final.Accepted != 1 || final.Persisted != 1 || final.Pending != 0 || final.Lost != 0 {
		t.Fatalf("final session = %#v", final)
	}
}

type datasetMemoryStore struct {
	memoryStore
	sessions []CaptureSession
}

type datasetVisitorStore struct {
	records []Record
	visits  int
	lists   int
}

type datasetSnapshotStore struct {
	records      []Record
	opens        int
	visits       int
	closes       int
	lists        int
	legacyVisits int
}

type datasetRecordSnapshot struct {
	store   *datasetSnapshotStore
	records []Record
	limited bool
	closed  bool
}

func (s *datasetSnapshotStore) List(context.Context, Query) (Page, error) {
	s.lists++
	return Page{}, errors.New("List must not be called when RecordSnapshotReader is available")
}

func (s *datasetSnapshotStore) VisitRecords(context.Context, Query, int, func(Record) error) (bool, error) {
	s.legacyVisits++
	return false, errors.New("RecordVisitor must not be called when RecordSnapshotReader is available")
}

func (s *datasetSnapshotStore) OpenRecordSnapshot(
	_ context.Context,
	_ Query,
	maximum int,
) (RecordSnapshot, error) {
	s.opens++
	records := s.records
	limited := maximum > 0 && len(records) > maximum
	if limited {
		records = records[:maximum]
	}
	return &datasetRecordSnapshot{store: s, records: records, limited: limited}, nil
}

func (s *datasetRecordSnapshot) Visit(_ context.Context, visit func(Record) error) error {
	if s.closed {
		return errors.New("snapshot is closed")
	}
	s.store.visits++
	for _, record := range s.records {
		if err := visit(record); err != nil {
			return err
		}
	}
	return nil
}

func (s *datasetRecordSnapshot) Limited() bool { return s.limited }

func (s *datasetRecordSnapshot) Close() error {
	if !s.closed {
		s.closed = true
		s.store.closes++
	}
	return nil
}

func (s *datasetVisitorStore) List(context.Context, Query) (Page, error) {
	s.lists++
	return Page{}, errors.New("List must not be called when RecordVisitor is available")
}

func (s *datasetVisitorStore) VisitRecords(_ context.Context, _ Query, maximum int, visit func(Record) error) (bool, error) {
	s.visits++
	for index, record := range s.records {
		if maximum > 0 && index >= maximum {
			return true, nil
		}
		if err := visit(record); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (s *datasetMemoryStore) UpsertCaptureSession(_ context.Context, session CaptureSession) error {
	s.sessions = append(s.sessions, session)
	return nil
}

func (s *datasetMemoryStore) ListCaptureSessions(_ context.Context, query CaptureSessionQuery) ([]CaptureSession, error) {
	var sessions []CaptureSession
	for _, session := range s.sessions {
		if !query.StartedBefore.IsZero() && !session.StartedAt.Before(query.StartedBefore) {
			continue
		}
		if !query.UpdatedAfter.IsZero() && session.UpdatedAt.Before(query.UpdatedAfter) {
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, nil
}

type sessionRecorderStore struct {
	mu       sync.Mutex
	records  []Record
	sessions []CaptureSession
}

func (s *sessionRecorderStore) Append(ctx context.Context, record Record) error {
	return s.AppendTraceBatch(ctx, []Record{record})
}

func (s *sessionRecorderStore) AppendTraceBatch(_ context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
	return nil
}

func (*sessionRecorderStore) List(context.Context, Query) (Page, error) { return Page{}, nil }
func (*sessionRecorderStore) Close() error                              { return nil }
func (s *sessionRecorderStore) UpsertCaptureSession(_ context.Context, session CaptureSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = append(s.sessions, session)
	return nil
}
func (*sessionRecorderStore) ListCaptureSessions(context.Context, CaptureSessionQuery) ([]CaptureSession, error) {
	return nil, nil
}
