package savedtrace

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegressionMatrixTraceBackpressureRetainsEveryRecord(t *testing.T) {
	t.Parallel()
	const producers = 16
	const recordsPerProducer = 2048
	const expected = producers * recordsPerProducer
	store := &saturatingBatchStore{identifiers: make(map[string]struct{}), delay: 200 * time.Microsecond}
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 256, Workers: 1, Overflow: OverflowBlock,
		BatchSize: 256, BatchInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(100, 0).UTC()
	var wait sync.WaitGroup
	wait.Add(producers)
	for producer := range producers {
		go func() {
			defer wait.Done()
			for index := range recordsPerProducer {
				recorder.Record(testRecord(
					fmt.Sprintf("trace-%d-%d", producer, index), started, "tenant", "workspace",
				))
			}
		}()
	}
	wait.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		t.Fatal(err)
	}
	stats := recorder.Stats()
	if stats.Accepted != expected || stats.Persisted != expected || stats.Pending != 0 || stats.Lost != 0 ||
		stats.QueueFull != 0 || stats.StoreFailures != 0 {
		t.Fatalf("recorder stats = %#v", stats)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.identifiers) != expected {
		t.Fatalf("unique records = %d, want %d", len(store.identifiers), expected)
	}
}

func BenchmarkRegressionMatrixTraceBackpressure(b *testing.B) {
	store := &batchMemoryStore{}
	recorder, err := NewAsyncRecorder(store, AsyncOptions{
		QueueCapacity: 256, Workers: 1, Overflow: OverflowBlock,
		BatchSize: 256, BatchInterval: time.Millisecond,
	})
	if err != nil {
		b.Fatal(err)
	}
	started := time.Unix(100, 0).UTC()
	var identifier atomic.Uint64
	b.ReportAllocs()
	b.SetBytes(int64(len("request") + len("response")))
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			recorder.Record(testRecord(
				fmt.Sprintf("benchmark-%d", identifier.Add(1)), started, "tenant", "workspace",
			))
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := recorder.Close(ctx); err != nil {
		b.Fatal(err)
	}
	records, _ := store.counts()
	if records != b.N {
		b.Fatalf("persisted records = %d, want %d", records, b.N)
	}
}

type saturatingBatchStore struct {
	mu          sync.Mutex
	identifiers map[string]struct{}
	delay       time.Duration
}

func (s *saturatingBatchStore) Append(ctx context.Context, record Record) error {
	return s.AppendTraceBatch(ctx, []Record{record})
}

func (s *saturatingBatchStore) AppendTraceBatch(ctx context.Context, records []Record) error {
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range records {
		s.identifiers[record.RequestID] = struct{}{}
	}
	return nil
}

func (*saturatingBatchStore) List(context.Context, Query) (Page, error) { return Page{}, nil }
func (*saturatingBatchStore) Close() error                              { return nil }
