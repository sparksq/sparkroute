package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
)

func traceTestOptions() runtimeBuildOptions {
	return runtimeBuildOptions{Context: context.Background(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), TraceStores: newTraceStores("", "", nil), Telemetry: &otlpexport.Runtime{}}
}
func TestTraceGenerationsShareStoreDrainAndDisable(t *testing.T) {
	for _, storage := range []string{"filesystem", "database"} {
		t.Run(storage, func(t *testing.T) {
			base := traceTestOptions()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			document := config.Document{Observability: &config.Observability{SavedTraces: &config.SavedTraceConfig{Enabled: true, Storage: storage, Path: filepath.Join(dir, "payloads")}}}
			first := base
			closeFirst, err := configureGenerationObservability(document, &first)
			if err != nil {
				t.Fatal(err)
			}
			second := base
			document.Observability.SavedTraces.QueueCapacity = 2
			closeSecond, err := configureGenerationObservability(document, &second)
			if err != nil {
				t.Fatal(err)
			}
			if first.TraceReader != second.TraceReader {
				t.Fatal("overlapping generations opened separate stores")
			}
			now := time.Now().UTC()
			first.SavedTraces.Record(savedtrace.Record{Version: 1, RequestID: "before-save", StartedAt: now, CompletedAt: now, Protocol: "openai", Operation: "chat_completions", Outcome: "success", CaptureOutcome: savedtrace.CaptureComplete, Request: savedtrace.Payload{Body: "test prompt"}})
			closeFirst() // Drains the retiring generation without closing the shared store.
			page, err := second.TraceReader.List(context.Background(), savedtrace.Query{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Records) != 1 {
				t.Fatal("retired request was lost", page)
			}
			disabled := base
			document.Observability.SavedTraces.Enabled = false
			closeDisabled, err := configureGenerationObservability(document, &disabled)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDisabled()
			if disabled.SavedTraces != nil || disabled.TraceReader != nil {
				t.Fatal("capture stayed enabled")
			}
			closeSecond()
			if len(base.TraceStores.stores) != 0 {
				t.Fatal("store was not released")
			}
		})
	}
}
func TestManagedOTLPExportUsesHeaderEnvironmentAndDisabledMeansNoSpans(t *testing.T) {
	received := make(chan string, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer collector.Close()
	t.Setenv("SPARKROUTE_TEST_TRACE_AUTH", "Bearer test-only-secret")
	document := config.Document{Observability: &config.Observability{OTLPTraces: &config.OTLPTraceConfig{Enabled: true, Endpoint: collector.URL + "/v1/traces", HeaderEnv: map[string]string{"Authorization": "SPARKROUTE_TEST_TRACE_AUTH"}}}}
	options := traceTestOptions()
	closeEnabled, err := configureGenerationObservability(document, &options)
	if err != nil {
		t.Fatal(err)
	}
	_, span := options.Telemetry.TracerProvider.Tracer("test").Start(context.Background(), "qa")
	span.End()
	closeEnabled()
	select {
	case header := <-received:
		if header != "Bearer test-only-secret" {
			t.Fatal("missing collector authentication")
		}
	case <-time.After(time.Second):
		t.Fatal("no exported span")
	}
	document.Observability.OTLPTraces.Enabled = false
	closeDisabled, err := configureGenerationObservability(document, &options)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDisabled()
	_, span = options.Telemetry.TracerProvider.Tracer("test").Start(context.Background(), "disabled")
	if span.IsRecording() {
		t.Fatal("disabled export recorded a span")
	}
	span.End()
	document.Observability.OTLPTraces.Enabled = true
	t.Setenv("SPARKROUTE_TEST_TRACE_AUTH", "")
	if err := validateTraceEnvironment(document.Observability); err == nil || strings.Contains(err.Error(), "test-only-secret") {
		t.Fatal("missing environment credential was accepted or leaked", err)
	}
}
