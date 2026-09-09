// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryStore struct {
	mu        sync.Mutex
	batches   [][]Record
	failures  int
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	closed    bool
}

func (s *memoryStore) AppendBatch(_ context.Context, records []Record) error {
	if s.started != nil {
		s.startOnce.Do(func() { close(s.started) })
	}
	if s.release != nil {
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures > 0 {
		s.failures--
		return errors.New("temporary store failure")
	}
	copied := append([]Record(nil), records...)
	s.batches = append(s.batches, copied)
	return nil
}

func (s *memoryStore) Health(context.Context) error { return nil }

func (s *memoryStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

func TestAsyncWriterBatchesAndFlushesOnClose(t *testing.T) {
	t.Parallel()

	store := &memoryStore{}
	writer, err := NewAsyncWriter(store, AsyncOptions{
		QueueCapacity:     8,
		MaxBatchRecords:   2,
		MaxBatchBytes:     1 << 20,
		FlushInterval:     time.Hour,
		WriteTimeout:      time.Second,
		MaxRetries:        1,
		InitialRetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAsyncWriter() error = %v", err)
	}
	writer.Record(testAttemptRecord("request-1", 1))
	writer.Record(testAttemptRecord("request-1", 2))
	writer.Record(testRequestRecord("request-1"))
	closeWriter(t, writer)

	stats := writer.Stats()
	if stats.Enqueued != 3 || stats.Written != 3 || stats.Lost != 0 || stats.Batches != 2 {
		t.Fatalf("stats = %#v", stats)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.batches) != 2 || len(store.batches[0]) != 2 || len(store.batches[1]) != 1 {
		t.Fatalf("batch sizes = %v", batchSizes(store.batches))
	}
	if !store.closed {
		t.Fatal("store was not closed")
	}
}

func TestAsyncWriterFailsOpenWhenQueueIsFull(t *testing.T) {
	t.Parallel()

	store := &memoryStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	writer, err := NewAsyncWriter(store, AsyncOptions{
		QueueCapacity:     1,
		MaxBatchRecords:   1,
		MaxBatchBytes:     1 << 20,
		FlushInterval:     time.Hour,
		WriteTimeout:      time.Second,
		MaxRetries:        1,
		InitialRetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAsyncWriter() error = %v", err)
	}
	writer.Record(testAttemptRecord("request-1", 1))
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("store write did not start")
	}
	writer.Record(testAttemptRecord("request-1", 2))
	writer.Record(testRequestRecord("request-1"))
	if stats := writer.Stats(); stats.Lost != 1 {
		t.Fatalf("lost = %d, want 1", stats.Lost)
	}
	close(store.release)
	closeWriter(t, writer)
	if stats := writer.Stats(); stats.Written != 2 || stats.Lost != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestAsyncWriterRetriesStoreFailures(t *testing.T) {
	t.Parallel()

	store := &memoryStore{failures: 2}
	writer, err := NewAsyncWriter(store, AsyncOptions{
		QueueCapacity:     1,
		MaxBatchRecords:   1,
		MaxBatchBytes:     1 << 20,
		FlushInterval:     time.Hour,
		WriteTimeout:      time.Second,
		MaxRetries:        2,
		InitialRetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewAsyncWriter() error = %v", err)
	}
	writer.Record(testRequestRecord("request-1"))
	closeWriter(t, writer)
	stats := writer.Stats()
	if stats.Retries != 2 || stats.Written != 1 || stats.Lost != 0 {
		t.Fatalf("stats = %#v", stats)
	}
}

func TestAsyncWriterRejectsInvalidRecord(t *testing.T) {
	t.Parallel()

	writer, err := NewAsyncWriter(DiscardStore{}, AsyncOptions{})
	if err != nil {
		t.Fatalf("NewAsyncWriter() error = %v", err)
	}
	writer.Record(Record{Kind: KindRequest})
	closeWriter(t, writer)
	if stats := writer.Stats(); stats.Enqueued != 0 || stats.Lost != 1 {
		t.Fatalf("stats = %#v", stats)
	}
}

func testRequestRecord(id string) Record {
	now := time.Now()
	return NewRequestRecord(RequestRecord{
		RequestID:   id,
		StartedAt:   now,
		CompletedAt: now,
		Protocol:    "openai",
		Operation:   "chat_completions",
		Outcome:     OutcomeSuccess,
		Usage:       TokenUsage{Completeness: UsageMissing},
	})
}

func testAttemptRecord(id string, attempt int) Record {
	now := time.Now()
	return NewAttemptRecord(AttemptRecord{
		RequestID:     id,
		Attempt:       attempt,
		StartedAt:     now,
		CompletedAt:   now,
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "model",
		Outcome:       OutcomeSuccess,
		Usage:         TokenUsage{Completeness: UsageMissing},
	})
}

func closeWriter(t *testing.T, writer *AsyncWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := writer.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func batchSizes(batches [][]Record) []int {
	result := make([]int, len(batches))
	for index, batch := range batches {
		result[index] = len(batch)
	}
	return result
}
