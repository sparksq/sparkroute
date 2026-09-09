// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gatewaytelemetry "github.com/sparksq/sparkroute/pkg/telemetry"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestChatCompletionsCreatesAttemptChildAndPropagatesIt(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	t.Cleanup(func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown tracer provider: %v", err)
		}
	})

	upstreamTraceparent := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamTraceparent <- request.Header.Get("Traceparent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			w,
			`{"id":"chatcmpl-1","model":"upstream-model","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`,
		)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(proxyDocument(upstream.URL+"/v1"), DataOptions{
		Telemetry: gatewaytelemetry.Options{
			TracerProvider: tracerProvider,
			Propagator:     propagation.TraceContext{},
		},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(
		"Traceparent",
		"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", response.Code, response.Body.String())
	}

	ended := spanRecorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("ended span count = %d, want 2", len(ended))
	}
	var requestSpan, attemptSpan sdktrace.ReadOnlySpan
	for _, span := range ended {
		switch span.Name() {
		case "llm.gateway.chat_completions":
			requestSpan = span
		case "llm.gateway.upstream.chat":
			attemptSpan = span
		}
	}
	if requestSpan == nil || attemptSpan == nil {
		t.Fatalf("missing request or attempt span: %#v", ended)
	}
	if got, want := attemptSpan.Parent().SpanID(), requestSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("attempt parent = %s, want request span %s", got, want)
	}
	carrier := propagation.HeaderCarrier(http.Header{
		"Traceparent": []string{<-upstreamTraceparent},
	})
	upstreamContext := propagation.TraceContext{}.Extract(context.Background(), carrier)
	if got, want := trace.SpanContextFromContext(upstreamContext).SpanID(),
		attemptSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("upstream parent = %s, want attempt span %s", got, want)
	}
}
