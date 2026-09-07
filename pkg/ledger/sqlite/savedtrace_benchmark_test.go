package sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func BenchmarkRegressionMatrixTraceSQLiteGroupCommit(b *testing.B) {
	directory := b.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		b.Fatal(err)
	}
	store, err := Open(context.Background(), Options{Path: filepath.Join(directory, "traces.db")})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	const batchSize = 256
	started := time.Unix(100, 0).UTC()
	var sequence int
	b.ReportAllocs()
	for b.Loop() {
		records := make([]savedtrace.Record, batchSize)
		for index := range records {
			sequence++
			records[index] = benchmarkSQLiteTrace(fmt.Sprintf("trace-%d", sequence), started)
		}
		if err := store.AppendTraceBatch(context.Background(), records); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(batchSize, "records/batch")
}

func benchmarkSQLiteTrace(identifier string, started time.Time) savedtrace.Record {
	return savedtrace.Record{
		Version: savedtrace.SchemaVersion, RequestID: identifier,
		StartedAt: started, CompletedAt: started.Add(time.Second),
		Protocol: "openai", Operation: "responses", Outcome: "success",
		Request:  savedtrace.Payload{Body: `{"input":"benchmark"}`},
		Response: savedtrace.Payload{Body: `{"output":"benchmark"}`},
	}
}
