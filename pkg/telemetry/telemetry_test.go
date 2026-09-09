// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package telemetry

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const incomingTraceparent = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"

type testConventions struct{}

func (testConventions) Request(RequestStart) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("test.request", "present")}
}

func (testConventions) Attempt(AttemptStart) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String("test.attempt", "present")}
}

func TestInstrumentationTraceTopologyPropagationAndMetrics(t *testing.T) {
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	t.Cleanup(func() {
		if err := tracerProvider.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown tracer provider: %v", err)
		}
	})
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() {
		if err := meterProvider.Shutdown(context.Background()); err != nil {
			t.Fatalf("shutdown meter provider: %v", err)
		}
	})
	instrumentation, err := New(Options{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
		Propagator:     propagation.TraceContext{},
		Conventions:    testConventions{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	headers := http.Header{"Traceparent": []string{incomingTraceparent}}
	requestContext, request := instrumentation.StartRequest(
		context.Background(),
		headers,
		RequestStart{
			RequestID:      "request-high-cardinality",
			Protocol:       "openai",
			Operation:      "chat_completions",
			ConfigRevision: "revision-high-cardinality",
			Identity: identity.Identity{
				Principal: identity.Principal{
					ID:      "client-high-cardinality",
					Type:    "machine",
					Tenant:  "tenant-high-cardinality",
					Subject: "service-account:high-cardinality",
					Roles:   []string{"inference"},
				},
				Attribution: identity.Attribution{
					identity.AttributeTenant:    "tenant-high-cardinality",
					identity.AttributeWorkspace: "workspace-high-cardinality",
				},
			},
		},
	)
	request.SetStream(true)
	attemptContext, attempt := request.StartAttempt(AttemptStart{
		VirtualModel:         "assistant",
		Provider:             "provider-a",
		Deployment:           "deployment-a",
		UpstreamModel:        "real-model",
		UpstreamProtocol:     "anthropic",
		NativeProtocol:       true,
		RequiredCapabilities: []string{"tools", "vision"},
		CircuitState:         "half_open",
		HalfOpenProbe:        true,
		ActiveAtAdmission:    1,
		RetryReason:          "upstream_http_503",
		RetryDelay:           250 * time.Millisecond,
		Attempt:              1,
		PoolPriority:         0,
	})
	instrumentation.TargetAdmissionChanged("deployment-a", 1)
	instrumentation.TargetAdmissionRejected(routing.AdmissionRejection{
		Deployment: "deployment-a",
		Reason:     routing.AdmissionConcurrencyLimit,
	})
	instrumentation.TargetCircuitChanged(routing.CircuitTransition{
		Deployment:    "deployment-a",
		From:          routing.CircuitClosed,
		To:            routing.CircuitOpen,
		Reason:        "consecutive_failures",
		EjectionCount: 1,
	})
	instrumentation.RetryBudgetActiveChanged("assistant", 1)
	instrumentation.RetryBudgetRejected(routing.RetryBudgetRejection{
		VirtualModel:   "assistant",
		ActiveRequests: 10,
		ActiveRetries:  2,
		AllowedRetries: 2,
	})
	instrumentation.RecordRetryDelay(
		requestContext,
		"assistant",
		"upstream_http_503",
		250*time.Millisecond,
	)
	request.SetModelRouting(ModelRoutingSelection{
		Router: "fox-sparkroute-native", Strategy: "stage_router",
		SelectedVirtualModel: "assistant", DecisionSource: "dimensions", Tier: "capable",
		Duration: 2 * time.Millisecond, Stage: true, Score: 0.7, Confidence: 0.7,
		Severity: 0.7, Exploring: 1,
	})

	propagated := http.Header{}
	instrumentation.Inject(attemptContext, propagated)
	extracted := propagation.TraceContext{}.Extract(
		context.Background(),
		propagation.HeaderCarrier(propagated),
	)
	if got, want := trace.SpanContextFromContext(extracted).SpanID(),
		trace.SpanContextFromContext(attemptContext).SpanID(); got != want {
		t.Fatalf("propagated span ID = %s, want attempt span ID %s", got, want)
	}

	started := time.Now()
	inputTokens, outputTokens, totalTokens := int64(17), int64(5), int64(22)
	cachedTokens, cacheCreationTokens, reasoningTokens := int64(11), int64(3), int64(2)
	timeToFirstByte := 10 * time.Millisecond
	usage := ledger.TokenUsage{
		InputTokens:          &inputTokens,
		OutputTokens:         &outputTokens,
		TotalTokens:          &totalTokens,
		CachedInputTokens:    &cachedTokens,
		CacheCreationTokens:  &cacheCreationTokens,
		ReasoningTokens:      &reasoningTokens,
		Completeness:         ledger.UsageComplete,
		NormalizationVersion: "openai-chat-v1",
	}
	attempt.End(ledger.AttemptRecord{
		RequestID:     "request-high-cardinality",
		Attempt:       1,
		StartedAt:     started,
		CompletedAt:   started.Add(25 * time.Millisecond),
		Provider:      "provider-a",
		Deployment:    "deployment-a",
		UpstreamModel: "real-model",
		HTTPStatus:    http.StatusOK,
		Outcome:       ledger.OutcomeSuccess,
		Latency:       25 * time.Millisecond,
		Usage:         usage,
	})
	instrumentation.TargetAdmissionChanged("deployment-a", -1)
	instrumentation.RetryBudgetActiveChanged("assistant", -1)
	request.End(ledger.RequestRecord{
		RequestID:              "request-high-cardinality",
		StartedAt:              started,
		CompletedAt:            started.Add(30 * time.Millisecond),
		Protocol:               "openai",
		Operation:              "chat_completions",
		Stream:                 true,
		RequestedModel:         "alias",
		VirtualModel:           "assistant",
		ResponsePresentedModel: "assistant",
		FinalProvider:          "provider-a",
		FinalDeployment:        "deployment-a",
		FinalUpstreamModel:     "real-model",
		AttemptCount:           1,
		HTTPStatus:             http.StatusOK,
		Outcome:                ledger.OutcomeSuccess,
		Latency:                30 * time.Millisecond,
		TimeToFirstByte:        &timeToFirstByte,
		Usage:                  usage,
	})

	ended := spanRecorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("ended span count = %d, want 2", len(ended))
	}
	attemptSpan := spanNamed(t, ended, "llm.gateway.upstream.chat")
	requestSpan := spanNamed(t, ended, "llm.gateway.chat_completions")
	if got, want := requestSpan.SpanContext().TraceID().String(),
		"0af7651916cd43dd8448eb211c80319c"; got != want {
		t.Fatalf("request trace ID = %s, want %s", got, want)
	}
	if got, want := requestSpan.Parent().SpanID().String(),
		"b7ad6b7169203331"; got != want {
		t.Fatalf("request parent span ID = %s, want %s", got, want)
	}
	if got, want := attemptSpan.Parent().SpanID(), requestSpan.SpanContext().SpanID(); got != want {
		t.Fatalf("attempt parent span ID = %s, want request span ID %s", got, want)
	}
	assertSpanAttribute(t, requestSpan.Attributes(), "test.request", "present")
	assertSpanAttribute(
		t,
		requestSpan.Attributes(),
		"llm.gateway.client.principal.id",
		"client-high-cardinality",
	)
	assertSpanAttribute(
		t,
		requestSpan.Attributes(),
		identity.AttributeWorkspace,
		"workspace-high-cardinality",
	)
	assertSpanStringSliceAttribute(
		t,
		requestSpan.Attributes(),
		"llm.gateway.client.principal.roles",
		[]string{"inference"},
	)
	assertSpanAttribute(t, attemptSpan.Attributes(), "test.attempt", "present")
	assertSpanAttribute(t, attemptSpan.Attributes(), "gen_ai.request.model", "real-model")
	assertSpanAttribute(
		t,
		attemptSpan.Attributes(),
		"llm.gateway.upstream.protocol",
		"anthropic",
	)
	assertSpanBoolAttribute(
		t,
		attemptSpan.Attributes(),
		"llm.gateway.upstream.protocol_native",
		true,
	)
	assertSpanAttribute(t, attemptSpan.Attributes(), "llm.gateway.circuit.state", "half_open")
	assertSpanAttribute(
		t,
		attemptSpan.Attributes(),
		"llm.gateway.retry.reason",
		"upstream_http_503",
	)
	assertSpanInt64Attribute(
		t,
		attemptSpan.Attributes(),
		"llm.gateway.retry.delay_ms",
		250,
	)
	assertSpanStringSliceAttribute(
		t,
		attemptSpan.Attributes(),
		"llm.gateway.required_capabilities",
		[]string{"tools", "vision"},
	)
	assertSpanInt64Attribute(t, attemptSpan.Attributes(), "gen_ai.usage.input_tokens", 17)
	assertSpanInt64Attribute(t, requestSpan.Attributes(), "llm.gateway.time_to_first_byte_ms", 10)
	assertSpanAttribute(t, requestSpan.Attributes(), "llm.gateway.model_router.strategy", "stage_router")
	assertSpanAttribute(t, requestSpan.Attributes(), "llm.gateway.model_router.decision_source", "dimensions")

	var metrics metricdata.ResourceMetrics
	if err := metricReader.Collect(requestContext, &metrics); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.requests", ""); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempts", ""); got != 1 {
		t.Fatalf("attempt count = %d, want 1", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.active_requests", ""); got != 0 {
		t.Fatalf("active request count = %d, want 0", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.active_streams", ""); got != 0 {
		t.Fatalf("active stream count = %d, want 0", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.target.active_requests", ""); got != 0 {
		t.Fatalf("target active request count = %d, want 0", got)
	}
	if got := metricInt64Sum(
		t,
		metrics,
		"llm.gateway.target.admission_rejections",
		"",
	); got != 1 {
		t.Fatalf("target admission rejection count = %d, want 1", got)
	}
	if got := metricInt64Sum(
		t,
		metrics,
		"llm.gateway.target.circuit_transitions",
		"",
	); got != 1 {
		t.Fatalf("circuit transition count = %d, want 1", got)
	}
	if got := metricInt64Sum(
		t,
		metrics,
		"llm.gateway.target.circuit_ejections",
		"",
	); got != 1 {
		t.Fatalf("circuit ejection count = %d, want 1", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.retry.active", ""); got != 0 {
		t.Fatalf("active retry count = %d, want 0", got)
	}
	if got := metricInt64Sum(
		t,
		metrics,
		"llm.gateway.retry.budget_rejections",
		"",
	); got != 1 {
		t.Fatalf("retry budget rejection count = %d, want 1", got)
	}
	if got := metricHistogramCount(t, metrics, "llm.gateway.retry.delay"); got != 1 {
		t.Fatalf("retry delay histogram count = %d, want 1", got)
	}
	if got := metricHistogramCount(t, metrics, "llm.gateway.request.time_to_first_byte"); got != 1 {
		t.Fatalf("first-byte histogram count = %d, want 1", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.model_router.decisions", ""); got != 1 {
		t.Fatalf("model-router decision count = %d, want 1", got)
	}
	for _, name := range []string{
		"llm.gateway.model_router.duration",
		"llm.gateway.stage_router.score",
		"llm.gateway.stage_router.confidence",
		"llm.gateway.stage_router.severity",
		"llm.gateway.stage_router.spinning",
		"llm.gateway.stage_router.exploring",
		"llm.gateway.stage_router.production_intensity",
	} {
		if got := metricHistogramCount(t, metrics, name); got != 1 {
			t.Errorf("%s histogram count = %d, want 1", name, got)
		}
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempt.tokens", "input"); got != 17 {
		t.Fatalf("input token count = %d, want 17", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempt.tokens", "output"); got != 5 {
		t.Fatalf("output token count = %d, want 5", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempt.tokens", "cached_input"); got != 11 {
		t.Fatalf("cached input token count = %d, want 11", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempt.tokens", "cache_creation"); got != 3 {
		t.Fatalf("cache creation token count = %d, want 3", got)
	}
	if got := metricInt64Sum(t, metrics, "llm.gateway.attempt.tokens", "reasoning"); got != 2 {
		t.Fatalf("reasoning token count = %d, want 2", got)
	}
	assertNoMetricAttribute(t, metrics, "llm.gateway.request.id")
	assertNoMetricAttribute(t, metrics, "llm.gateway.client.tenant.id")
	assertNoMetricAttribute(t, metrics, identity.AttributeWorkspace)
}

func metricHistogramCount(
	t *testing.T,
	metrics metricdata.ResourceMetrics,
	name string,
) uint64 {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			histogram, ok := current.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %s is %T, want float64 histogram", name, current.Data)
			}
			var count uint64
			for _, point := range histogram.DataPoints {
				count += point.Count
			}
			return count
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func spanNamed(
	t *testing.T,
	spans []sdktrace.ReadOnlySpan,
	name string,
) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func assertSpanAttribute(
	t *testing.T,
	attrs []attribute.KeyValue,
	key string,
	want string,
) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if got := attr.Value.AsString(); got != want {
				t.Fatalf("span attribute %s = %q, want %q", key, got, want)
			}
			return
		}
	}
	t.Fatalf("span attribute %s not found", key)
}

func assertSpanInt64Attribute(
	t *testing.T,
	attrs []attribute.KeyValue,
	key string,
	want int64,
) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if got := attr.Value.AsInt64(); got != want {
				t.Fatalf("span attribute %s = %d, want %d", key, got, want)
			}
			return
		}
	}
	t.Fatalf("span attribute %s not found", key)
}

func assertSpanBoolAttribute(
	t *testing.T,
	attrs []attribute.KeyValue,
	key string,
	want bool,
) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			if got := attr.Value.AsBool(); got != want {
				t.Fatalf("span attribute %s = %t, want %t", key, got, want)
			}
			return
		}
	}
	t.Fatalf("span attribute %s not found", key)
}

func assertSpanStringSliceAttribute(
	t *testing.T,
	attrs []attribute.KeyValue,
	key string,
	want []string,
) {
	t.Helper()
	for _, attr := range attrs {
		if string(attr.Key) == key {
			got := attr.Value.AsStringSlice()
			if len(got) != len(want) {
				t.Fatalf("span attribute %s = %v, want %v", key, got, want)
			}
			for index := range want {
				if got[index] != want[index] {
					t.Fatalf("span attribute %s = %v, want %v", key, got, want)
				}
			}
			return
		}
	}
	t.Fatalf("span attribute %s not found", key)
}

func metricInt64Sum(
	t *testing.T,
	metrics metricdata.ResourceMetrics,
	name string,
	tokenType string,
) int64 {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name != name {
				continue
			}
			sum, ok := current.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %s is %T, want int64 sum", name, current.Data)
			}
			var result int64
			for _, point := range sum.DataPoints {
				if tokenType != "" {
					value, exists := point.Attributes.Value(
						attribute.Key("llm.gateway.token.type"),
					)
					if !exists || value.AsString() != tokenType {
						continue
					}
				}
				result += point.Value
			}
			return result
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func assertNoMetricAttribute(
	t *testing.T,
	metrics metricdata.ResourceMetrics,
	forbidden string,
) {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			switch data := current.Data.(type) {
			case metricdata.Sum[int64]:
				for _, point := range data.DataPoints {
					if _, exists := point.Attributes.Value(attribute.Key(forbidden)); exists {
						t.Fatalf("metric %s contains forbidden attribute %s", current.Name, forbidden)
					}
				}
			case metricdata.Histogram[float64]:
				for _, point := range data.DataPoints {
					if _, exists := point.Attributes.Value(attribute.Key(forbidden)); exists {
						t.Fatalf("metric %s contains forbidden attribute %s", current.Name, forbidden)
					}
				}
			}
		}
	}
}
