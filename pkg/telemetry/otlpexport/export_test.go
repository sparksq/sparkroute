// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package otlpexport

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestFromEnvironmentUsesGenericEndpointForTracesOnly(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otel")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-sparkroute-experiment=tenant%2Fa")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "authorization=Bearer%20test")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "")

	runtime, err := FromEnvironment(context.Background(), "test-service", "test-version")
	if err != nil {
		t.Fatalf("FromEnvironment() error = %v", err)
	}
	if runtime.TracerProvider == nil {
		t.Fatal("expected trace provider")
	}
	if runtime.MeterProvider != nil {
		t.Fatal("generic endpoint must not implicitly enable metrics")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestFromEnvironmentDisabledWithoutAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "malformed-but-unused")

	runtime, err := FromEnvironment(context.Background(), "test-service", "test-version")
	if err != nil {
		t.Fatalf("FromEnvironment() error = %v", err)
	}
	if runtime.TracerProvider != nil || runtime.MeterProvider != nil {
		t.Fatal("expected disabled providers")
	}
}

func TestSignalEndpoint(t *testing.T) {
	for _, test := range []struct {
		name string
		base string
		want string
	}{
		{name: "root", base: "https://collector.example", want: "https://collector.example/v1/traces"},
		{
			name: "prefix",
			base: "https://collector.example/otel",
			want: "https://collector.example/otel/v1/traces",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := signalEndpoint(test.base, "/v1/traces")
			if err != nil {
				t.Fatalf("signalEndpoint() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("signalEndpoint() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestTraceExporterSendsOTLPHTTPWithHeadersAndAttributes(t *testing.T) {
	type capturedRequest struct {
		path        string
		contentType string
		experiment  string
		body        []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read OTLP request body: %v", err)
		}
		captured <- capturedRequest{
			path:        request.URL.Path,
			contentType: request.Header.Get("Content-Type"),
			experiment:  request.Header.Get("X-SparkRoute-Experiment"),
			body:        body,
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "none")

	runtime, err := New(context.Background(), Options{
		ServiceName:    "gateway-test",
		ServiceVersion: "test",
		TraceEndpoint:  server.URL + "/v1/traces",
		TraceHeaders:   map[string]string{"X-SparkRoute-Experiment": "tenant-a"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tracer := runtime.TracerProvider.Tracer("test")
	_, span := tracer.Start(context.Background(), "gateway-attempt", trace.WithSpanKind(trace.SpanKindClient))
	span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", 17))
	span.End()
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OTLP trace export")
	}
	if request.path != "/v1/traces" {
		t.Fatalf("OTLP path = %q, want /v1/traces", request.path)
	}
	if request.contentType != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", request.contentType)
	}
	if request.experiment != "tenant-a" {
		t.Fatalf("experiment header = %q, want tenant-a", request.experiment)
	}
	if !bytes.Contains(request.body, []byte("gen_ai.usage.input_tokens")) {
		t.Fatal("OTLP protobuf does not contain token-usage attribute")
	}
}

func TestMetricExporterSendsOTLPHTTP(t *testing.T) {
	type capturedRequest struct {
		path        string
		contentType string
		body        []byte
	}
	captured := make(chan capturedRequest, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read OTLP request body: %v", err)
		}
		captured <- capturedRequest{
			path:        request.URL.Path,
			contentType: request.Header.Get("Content-Type"),
			body:        body,
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_COMPRESSION", "none")

	runtime, err := New(context.Background(), Options{
		ServiceName:    "gateway-test",
		ServiceVersion: "test",
		MetricEndpoint: server.URL + "/v1/metrics",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	meter := runtime.MeterProvider.Meter("test")
	counter, err := meter.Int64Counter("llm.gateway.test.counter")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(context.Background(), 3)
	forceFlusher, ok := runtime.MeterProvider.(interface {
		ForceFlush(context.Context) error
	})
	if !ok {
		t.Fatal("metric provider does not implement ForceFlush")
	}
	if err := forceFlusher.ForceFlush(context.Background()); err != nil {
		t.Fatalf("ForceFlush() error = %v", err)
	}

	var request capturedRequest
	select {
	case request = <-captured:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OTLP metric export")
	}
	if request.path != "/v1/metrics" {
		t.Fatalf("OTLP path = %q, want /v1/metrics", request.path)
	}
	if request.contentType != "application/x-protobuf" {
		t.Fatalf("Content-Type = %q, want application/x-protobuf", request.contentType)
	}
	if !bytes.Contains(request.body, []byte("llm.gateway.test.counter")) {
		t.Fatal("OTLP protobuf does not contain the metric")
	}
	if err := runtime.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestMergeHeaders(t *testing.T) {
	got, err := mergeHeaders(
		map[string]string{"shared": "one", "override": "old"},
		"override=new,x-name=tenant%2Fa",
	)
	if err != nil {
		t.Fatalf("mergeHeaders() error = %v", err)
	}
	want := map[string]string{
		"Shared":   "one",
		"Override": "new",
		"X-Name":   "tenant/a",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeHeaders() = %#v, want %#v", got, want)
	}
}

func TestParseHeadersRejectsMalformedInput(t *testing.T) {
	for _, value := range []string{
		"missing-value",
		"=empty-key",
		"bad%20name=value",
		"bad=%zz",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseHeaders(value); err == nil {
				t.Fatalf("parseHeaders(%q) unexpectedly succeeded", value)
			}
		})
	}
}
