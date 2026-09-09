// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func TestChatCompletionsModelVisibilityAccess(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		calls.Add(1)
		writeChatResponse(w, "upstream-model", "visible")
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL + "/v1")
	document.VirtualModels = append(document.VirtualModels, []config.VirtualModel{
		{
			Name:       "hidden-model",
			Aliases:    []string{"hidden-alias"},
			Visibility: config.ModelVisibilityHidden,
			Pools:      document.VirtualModels[0].Pools,
		},
		{
			Name:       "internal-model",
			Aliases:    []string{"internal-alias"},
			Visibility: config.ModelVisibilityInternal,
			Pools:      document.VirtualModels[0].Pools,
		},
	}...)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	hidden := serveChat(t, handler, `{"model":"hidden-alias","messages":[]}`)
	if hidden.Code != http.StatusOK {
		t.Fatalf("hidden status = %d; body=%s", hidden.Code, hidden.Body)
	}
	internal := serveChat(t, handler, `{"model":"internal-alias","messages":[]}`)
	if internal.Code != http.StatusNotFound {
		t.Fatalf("internal status = %d; body=%s", internal.Code, internal.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestChatCompletionsPreGuardrailAllowsAndRecordsInternalCall(t *testing.T) {
	t.Parallel()

	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		primaryCalls.Add(1)
		writeChatResponse(w, "upstream-model", "primary")
	}))
	defer primary.Close()
	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailUpstreamRequest(t, request)
		if input.Model != "upstream-guard" ||
			!strings.Contains(input.LastContent, `"version":2`) ||
			!strings.Contains(input.LastContent, `"protocol":"openai"`) ||
			!strings.Contains(input.LastContent, `"operation":"chat_completions"`) ||
			!strings.Contains(input.LastContent, `"guardrail":"request-safety"`) ||
			!strings.Contains(input.LastContent, `"phase":"pre"`) ||
			!strings.Contains(input.LastContent, `"private input"`) {
			t.Errorf("guardrail input = %#v", input)
		}
		writeGuardrailVerdict(w, "upstream-guard", `{"action":"allow"}`)
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:  "request-safety",
		Model: "safety",
	}}
	// Guardrail subrequests deliberately bypass guardrails on their own model.
	document.VirtualModels[1].Guardrails.Pre = []config.Guardrail{{
		Name:  "bounded-recursion",
		Model: "safety",
	}}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[{"role":"user","content":"private input"}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if primaryCalls.Load() != 1 || guardCalls.Load() != 1 {
		t.Fatalf(
			"calls = primary:%d guard:%d, want 1/1",
			primaryCalls.Load(),
			guardCalls.Load(),
		)
	}
	records := recorder.snapshot()
	var requestModels []string
	for _, record := range records {
		if record.Request != nil {
			requestModels = append(requestModels, record.Request.VirtualModel)
		}
	}
	if len(requestModels) != 2 ||
		requestModels[0] != "safety" ||
		requestModels[1] != "public" {
		t.Fatalf("ledger request models = %v", requestModels)
	}
}

func TestChatCompletionsPreGuardrailBlocksBeforePrimary(t *testing.T) {
	t.Parallel()

	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		primaryCalls.Add(1)
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"block","reason":"policy matched"}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:  "request-safety",
		Model: "safety",
	}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{"model":"public","messages":[]}`)
	assertOpenAIErrorCode(t, response, http.StatusBadRequest, "guardrail_blocked")
	if primaryCalls.Load() != 0 {
		t.Fatalf("primary calls = %d, want 0", primaryCalls.Load())
	}
	if strings.Contains(response.Body.String(), "policy matched") {
		t.Fatal("model-provided guardrail reason leaked to the caller")
	}
}

func TestChatCompletionsPreGuardrailReplacesRequest(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		writeChatResponse(w, "upstream-model", "primary")
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"replace","replacement":{"model":"public","stream":false,"messages":[{"role":"user","content":"[REDACTED]"}]}}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:             "request-pii",
		Model:            "safety",
		AllowReplacement: true,
	}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[{"role":"user","content":"SSN 123"}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	upstream := <-captured
	if string(upstream["model"]) != `"upstream-model"` ||
		!strings.Contains(string(upstream["messages"]), "[REDACTED]") ||
		strings.Contains(string(upstream["messages"]), "SSN 123") {
		t.Fatalf("primary upstream body = %#v", upstream)
	}
}

func TestChatCompletionsGuardrailFailureModes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		mode          config.GuardrailFailureMode
		wantStatus    int
		wantPrimary   int64
		wantErrorCode string
	}{
		{
			name:          "closed by default",
			wantStatus:    http.StatusBadGateway,
			wantPrimary:   0,
			wantErrorCode: "guardrail_failed",
		},
		{
			name:        "open",
			mode:        config.GuardrailFailOpen,
			wantStatus:  http.StatusOK,
			wantPrimary: 1,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var primaryCalls atomic.Int64
			primary := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				primaryCalls.Add(1)
				writeChatResponse(w, "upstream-model", "primary")
			}))
			defer primary.Close()
			guard := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				writeGuardrailVerdict(w, "upstream-guard", `not JSON`)
			}))
			defer guard.Close()
			document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
			document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
				Name:        "request-safety",
				Model:       "safety",
				FailureMode: test.mode,
			}}
			handler, err := NewDataHandler(document, DataOptions{})
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveChat(t, handler, `{"model":"public","messages":[]}`)
			if response.Code != test.wantStatus {
				t.Fatalf(
					"status = %d, want %d; body=%s",
					response.Code,
					test.wantStatus,
					response.Body,
				)
			}
			if test.wantErrorCode != "" {
				assertOpenAIErrorCode(
					t,
					response,
					test.wantStatus,
					test.wantErrorCode,
				)
			}
			if primaryCalls.Load() != test.wantPrimary {
				t.Fatalf(
					"primary calls = %d, want %d",
					primaryCalls.Load(),
					test.wantPrimary,
				)
			}
		})
	}
}

func TestChatCompletionsGuardrailUsesNormalFallback(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeChatResponse(w, "upstream-model", "primary")
	}))
	defer primary.Close()
	var failedCalls atomic.Int64
	failedGuard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		failedCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failedGuard.Close()
	var healthyCalls atomic.Int64
	healthyGuard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		healthyCalls.Add(1)
		writeGuardrailVerdict(w, "upstream-guard-b", `{"action":"allow"}`)
	}))
	defer healthyGuard.Close()

	document := guardrailDocument(primary.URL+"/v1", failedGuard.URL+"/v1")
	document.Providers = append(document.Providers, config.Provider{
		Name:    "guard-provider-b",
		Type:    "openai_compatible",
		BaseURL: healthyGuard.URL + "/v1",
	})
	document.Deployments = append(document.Deployments, config.Deployment{
		Name:     "guard-deployment-b",
		Provider: "guard-provider-b",
		Model:    "upstream-guard-b",
	})
	document.VirtualModels[1].Pools[0].Targets = append(
		document.VirtualModels[1].Pools[0].Targets,
		config.WeightedTarget{Deployment: "guard-deployment-b", Weight: 100},
	)
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:  "request-safety",
		Model: "safety",
	}}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:    zeroPicker{},
		RetryDelayPicker: zeroPicker{},
		RetrySleeper:     &recordingRetrySleeper{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{"model":"public","messages":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if failedCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf(
			"guard calls = failed:%d healthy:%d, want 1/1",
			failedCalls.Load(),
			healthyCalls.Load(),
		)
	}
}

func TestChatCompletionsPostGuardrailReplacesOnlyChoices(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("ETag", `"upstream-body"`)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-primary",
			"object":  "chat.completion",
			"model":   "upstream-model",
			"created": 123,
			"choices": []any{map[string]any{
				"index":   0,
				"message": map[string]any{"role": "assistant", "content": "SSN 123"},
			}},
			"usage": map[string]any{
				"prompt_tokens":     5,
				"completion_tokens": 2,
				"total_tokens":      7,
			},
		})
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		input := readGuardrailUpstreamRequest(t, request)
		if !strings.Contains(input.LastContent, `"phase":"post"`) ||
			!strings.Contains(input.LastContent, "SSN 123") {
			t.Errorf("post guard input = %#v", input)
		}
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"replace","replacement":{"model":"spoofed","usage":{"total_tokens":999},"choices":[{"index":0,"message":{"role":"assistant","content":"[REDACTED]"}}]}}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:             "response-pii",
		Model:            "safety",
		AllowReplacement: true,
	}}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{"model":"public","messages":[]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if values := response.Header().Values("X-Request-Id"); len(values) != 1 {
		t.Fatalf("request ID header values = %v, want one", values)
	}
	if response.Header().Get("ETag") != "" {
		t.Fatalf("stale ETag = %q", response.Header().Get("ETag"))
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["id"]) != `"chatcmpl-primary"` ||
		string(body["model"]) != `"public"` ||
		!strings.Contains(string(body["choices"]), "[REDACTED]") ||
		strings.Contains(string(body["choices"]), "SSN 123") ||
		!strings.Contains(string(body["usage"]), `"total_tokens":7`) {
		t.Fatalf("post-guarded response = %s", response.Body)
	}
	records := recorder.snapshot()
	var outer *ledger.RequestRecord
	for _, record := range records {
		if record.Request != nil && record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if outer == nil || outer.Usage.TotalTokens == nil || *outer.Usage.TotalTokens != 7 {
		t.Fatalf("outer usage record = %#v", outer)
	}
}

func TestChatCompletionsPostGuardrailBlocksAfterSuccessfulPrimary(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeChatResponse(w, "upstream-model", "unsafe response")
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(w, "upstream-guard", `{"action":"block"}`)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:  "response-safety",
		Model: "safety",
	}}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{"model":"public","messages":[]}`)
	assertOpenAIErrorCode(t, response, http.StatusBadRequest, "guardrail_blocked")
	records := recorder.snapshot()
	var primaryAttempt *ledger.AttemptRecord
	var outer *ledger.RequestRecord
	for _, record := range records {
		if record.Attempt != nil && record.Attempt.UpstreamModel == "upstream-model" {
			primaryAttempt = record.Attempt
		}
		if record.Request != nil && record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if primaryAttempt == nil || primaryAttempt.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("primary attempt = %#v", primaryAttempt)
	}
	if outer == nil ||
		outer.Outcome != ledger.OutcomeRejected ||
		outer.FailureClass != "guardrail_blocked" {
		t.Fatalf("outer request = %#v", outer)
	}
}

func TestChatCompletionsRejectsStreamingWithPostGuardrailBeforeCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls.Add(1)
	}))
	defer upstream.Close()
	document := guardrailDocument(upstream.URL+"/v1", upstream.URL+"/v1")
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:  "response-safety",
		Model: "safety",
	}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[],"stream":true}`,
	)
	assertOpenAIErrorCode(t, response, http.StatusBadRequest, "unsupported_feature")
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsStreamingPostGuardrailReleasesAllowedWindowsIncrementally(
	t *testing.T,
) {
	t.Parallel()

	firstContent := strings.Repeat("a", 1200)
	releasePrimary := make(chan struct{})
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatStreamChunk(w, firstContent, nil)
		w.(http.Flusher).Flush()
		<-releasePrimary
		writeChatStreamChunk(w, "second", nil)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer primary.Close()

	inputs := make(chan guardrailInput, 2)
	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		upstreamRequest := readGuardrailUpstreamRequest(t, request)
		var input guardrailInput
		if err := json.Unmarshal([]byte(upstreamRequest.LastContent), &input); err != nil {
			t.Errorf("decode stream guardrail input: %v", err)
		}
		inputs <- input
		writeGuardrailVerdict(w, "upstream-guard", `{"action":"allow"}`)
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Post: []config.Guardrail{{
			Name:  "response-safety",
			Model: "safety",
		}},
		Stream: &config.GuardrailStreamPolicy{
			WindowBytes:  1 << 10,
			ContextBytes: 4 << 10,
		},
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	gateway := httptest.NewServer(handler)
	defer gateway.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		gateway.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := gateway.Client().Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	reader := bufio.NewReader(response.Body)
	firstLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first screened window: %v", err)
	}
	if !strings.Contains(firstLine, firstContent) ||
		!strings.Contains(firstLine, `"model":"public"`) {
		t.Fatalf("first streamed line = %q", firstLine)
	}
	if guardCalls.Load() != 1 {
		t.Fatalf("guard calls before upstream release = %d, want 1", guardCalls.Load())
	}
	close(releasePrimary)
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if !bytes.Contains(remaining, []byte(`"content":"second"`)) ||
		!bytes.Contains(remaining, []byte("[DONE]")) {
		t.Fatalf("remaining stream = %q", remaining)
	}

	firstInput := <-inputs
	secondInput := <-inputs
	if firstInput.Phase != guardrailPhasePostStream ||
		firstInput.Stream == nil ||
		firstInput.Stream.Sequence != 0 ||
		firstInput.Stream.Final ||
		len(firstInput.Stream.ApprovedContext) != 0 ||
		len(firstInput.Stream.Pending) != 1 {
		t.Fatalf("first guardrail input = %#v", firstInput)
	}
	if secondInput.Stream == nil ||
		secondInput.Stream.Sequence != 1 ||
		!secondInput.Stream.Final ||
		len(secondInput.Stream.ApprovedContext) != 1 ||
		len(secondInput.Stream.Pending) != 1 {
		t.Fatalf("second guardrail input = %#v", secondInput)
	}
	if !bytes.Contains(
		secondInput.Stream.ApprovedContext[0],
		[]byte(firstContent),
	) {
		t.Fatalf(
			"approved context = %s",
			secondInput.Stream.ApprovedContext[0],
		)
	}
}

func TestChatCompletionsStreamingPostGuardrailBlockWithholdsPendingWindow(
	t *testing.T,
) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatStreamChunk(w, "private response", map[string]any{
			"prompt_tokens":     3,
			"completion_tokens": 5,
			"total_tokens":      8,
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"block","reason":"sensitive"}`,
		)
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Post: []config.Guardrail{{
			Name:  "response-safety",
			Model: "safety",
		}},
		Stream: &config.GuardrailStreamPolicy{},
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[],"stream":true}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed 200", response.Code)
	}
	if response.Body.Len() != 0 {
		t.Fatalf("blocked pending output leaked: %q", response.Body.Bytes())
	}

	var primaryAttempt *ledger.AttemptRecord
	var outer *ledger.RequestRecord
	for _, record := range recorder.snapshot() {
		if record.Attempt != nil &&
			record.Attempt.UpstreamModel == "upstream-model" {
			primaryAttempt = record.Attempt
		}
		if record.Request != nil && record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if primaryAttempt == nil ||
		primaryAttempt.Outcome != ledger.OutcomeSuccess ||
		primaryAttempt.Usage.TotalTokens == nil ||
		*primaryAttempt.Usage.TotalTokens != 8 {
		t.Fatalf("primary attempt = %#v", primaryAttempt)
	}
	if outer == nil ||
		outer.HTTPStatus != http.StatusOK ||
		outer.Outcome != ledger.OutcomeRejected ||
		outer.FailureClass != "guardrail_blocked" ||
		outer.Usage.TotalTokens == nil ||
		*outer.Usage.TotalTokens != 8 {
		t.Fatalf("outer request = %#v", outer)
	}
}

func TestChatCompletionsStreamingPostGuardrailBlockPreservesOnlyApprovedPrefix(
	t *testing.T,
) {
	t.Parallel()

	firstContent := strings.Repeat("safe", 300)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatStreamChunk(w, firstContent, nil)
		writeChatStreamChunk(w, "blocked second window", nil)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()
	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		if guardCalls.Add(1) == 1 {
			writeGuardrailVerdict(w, "upstream-guard", `{"action":"allow"}`)
			return
		}
		writeGuardrailVerdict(w, "upstream-guard", `{"action":"block"}`)
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Post: []config.Guardrail{{
			Name:  "response-safety",
			Model: "safety",
		}},
		Stream: &config.GuardrailStreamPolicy{
			WindowBytes: 1 << 10,
		},
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[],"stream":true}`,
	)
	if !bytes.Contains(response.Body.Bytes(), []byte(firstContent)) ||
		bytes.Contains(response.Body.Bytes(), []byte("blocked second window")) ||
		bytes.Contains(response.Body.Bytes(), []byte("[DONE]")) {
		t.Fatalf("screened response = %q", response.Body.Bytes())
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guard calls = %d, want 2", guardCalls.Load())
	}
	var outer *ledger.RequestRecord
	for _, record := range recorder.snapshot() {
		if record.Request != nil && record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if outer == nil ||
		outer.Outcome != ledger.OutcomeRejected ||
		outer.FailureClass != "guardrail_blocked" {
		t.Fatalf("outer request = %#v", outer)
	}
}

func TestChatCompletionsStreamingPostGuardrailReplacementUsesFailureMode(
	t *testing.T,
) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		failureMode config.GuardrailFailureMode
		wantBody    bool
		wantOutcome ledger.Outcome
		wantFailure string
	}{
		{
			name:        "fail open",
			failureMode: config.GuardrailFailOpen,
			wantBody:    true,
			wantOutcome: ledger.OutcomeSuccess,
		},
		{
			name:        "fail closed",
			failureMode: config.GuardrailFailClosed,
			wantOutcome: ledger.OutcomeUpstreamError,
			wantFailure: "guardrail_failed",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			primary := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeChatStreamChunk(w, "screen me", nil)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer primary.Close()
			guard := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				writeGuardrailVerdict(
					w,
					"upstream-guard",
					`{"action":"replace","replacement":{"choices":[]}}`,
				)
			}))
			defer guard.Close()

			document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
			document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
				Post: []config.Guardrail{{
					Name:             "response-safety",
					Model:            "safety",
					FailureMode:      test.failureMode,
					AllowReplacement: true,
				}},
				Stream: &config.GuardrailStreamPolicy{},
			}
			recorder := &collectingRecorder{}
			handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveChat(
				t,
				handler,
				`{"model":"public","messages":[],"stream":true}`,
			)
			if got := response.Body.Len() != 0; got != test.wantBody {
				t.Fatalf("response body = %q", response.Body.Bytes())
			}
			var outer *ledger.RequestRecord
			for _, record := range recorder.snapshot() {
				if record.Request != nil &&
					record.Request.VirtualModel == "public" {
					outer = record.Request
				}
			}
			if outer == nil ||
				outer.Outcome != test.wantOutcome ||
				outer.FailureClass != test.wantFailure {
				t.Fatalf("outer request = %#v", outer)
			}
		})
	}
}

func TestChatCompletionsStreamingPostGuardrailCancellationLeaksNoWindow(
	t *testing.T,
) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeChatStreamChunk(w, "pending response", nil)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer primary.Close()
	guardEntered := make(chan struct{}, 1)
	releaseGuard := make(chan struct{})
	guard := httptest.NewServer(http.HandlerFunc(func(
		_ http.ResponseWriter,
		request *http.Request,
	) {
		select {
		case guardEntered <- struct{}{}:
		default:
		}
		select {
		case <-request.Context().Done():
		case <-releaseGuard:
		}
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Post: []config.Guardrail{{
			Name:  "response-safety",
			Model: "safety",
		}},
		Stream: &config.GuardrailStreamPolicy{},
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[],"stream":true}`),
	).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() {
		handler.ServeHTTP(response, request)
		close(completed)
	}()
	select {
	case <-guardEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("guardrail call did not start")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(3 * time.Second):
		t.Fatal("gateway did not stop after caller cancellation")
	}
	close(releaseGuard)
	if response.Body.Len() != 0 {
		t.Fatalf("cancelled pending output leaked: %q", response.Body.Bytes())
	}
	var primaryAttempt *ledger.AttemptRecord
	var outer *ledger.RequestRecord
	for _, record := range recorder.snapshot() {
		if record.Attempt != nil &&
			record.Attempt.UpstreamModel == "upstream-model" {
			primaryAttempt = record.Attempt
		}
		if record.Request != nil && record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if primaryAttempt == nil ||
		primaryAttempt.Outcome != ledger.OutcomeClientCancelled {
		t.Fatalf("primary attempt = %#v", primaryAttempt)
	}
	if outer == nil ||
		outer.Outcome != ledger.OutcomeClientCancelled ||
		outer.FailureClass != "client_cancelled" {
		t.Fatalf("outer request = %#v", outer)
	}
}

func guardrailDocument(primaryURL, guardURL string) config.Document {
	document := proxyDocument(primaryURL)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "guard-provider",
		Type:    "openai_compatible",
		BaseURL: guardURL,
	})
	document.Deployments = append(document.Deployments, config.Deployment{
		Name:     "guard-deployment",
		Provider: "guard-provider",
		Model:    "upstream-guard",
	})
	document.VirtualModels = append(document.VirtualModels, config.VirtualModel{
		Name:       "safety",
		Visibility: config.ModelVisibilityInternal,
		Pools: []config.RoutingPool{{
			Targets: []config.WeightedTarget{{
				Deployment: "guard-deployment",
				Weight:     100,
			}},
		}},
	})
	return document
}

func writeChatStreamChunk(
	w http.ResponseWriter,
	content string,
	usage map[string]any,
) {
	payload := map[string]any{
		"id":     "chatcmpl-stream",
		"object": "chat.completion.chunk",
		"model":  "upstream-model",
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	}
	if usage != nil {
		payload["usage"] = usage
	}
	raw, _ := json.Marshal(payload)
	_, _ = w.Write(append(append([]byte("data: "), raw...), []byte("\n\n")...))
}

func serveChat(
	t *testing.T,
	handler http.Handler,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func writeChatResponse(
	w http.ResponseWriter,
	model string,
	content string,
) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     "chatcmpl-test",
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":   0,
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})
}

func writeGuardrailVerdict(
	w http.ResponseWriter,
	model string,
	verdict string,
) {
	writeJSON(w, http.StatusOK, map[string]any{
		"id":     "chatcmpl-guard",
		"object": "chat.completion",
		"model":  model,
		"choices": []any{map[string]any{
			"index":   0,
			"message": map[string]any{"role": "assistant", "content": verdict},
		}},
	})
}

type guardrailUpstreamRequest struct {
	Model       string
	LastContent string
}

func readGuardrailUpstreamRequest(
	t *testing.T,
	request *http.Request,
) guardrailUpstreamRequest {
	t.Helper()
	var body struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Errorf("decode guardrail upstream request: %v", err)
		return guardrailUpstreamRequest{}
	}
	result := guardrailUpstreamRequest{Model: body.Model}
	if len(body.Messages) != 0 {
		result.LastContent = body.Messages[len(body.Messages)-1].Content
	}
	return result
}

func readJSONEnvelope(
	t *testing.T,
	reader io.Reader,
) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.NewDecoder(reader).Decode(&envelope); err != nil {
		t.Errorf("decode JSON envelope: %v", err)
	}
	return envelope
}

func assertOpenAIErrorCode(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	code string,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf(
			"status = %d, want %d; body=%s",
			response.Code,
			status,
			response.Body,
		)
	}
	var failure openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if failure.Error.Code != code {
		t.Fatalf("error code = %q, want %q", failure.Error.Code, code)
	}
}

func TestSelectorRetainsSelectedModelAfterPreGuardrail(t *testing.T) {
	for _, verdict := range []string{`{"action":"allow"}`, `{"action":"replace","replacement":{"model":"auto","messages":[{"role":"user","content":"safe"}]}}`} {
		t.Run(verdict, func(t *testing.T) {
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeChatResponse(w, "upstream-model", "ok") }))
			defer primary.Close()
			guard := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeChatResponse(w, "upstream-guard", verdict) }))
			defer guard.Close()
			document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
			document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{Name: "check", Model: "safety", AllowReplacement: true}}
			policy := modelrouter.DefaultRoutingPolicy()
			policy.VirtualModels["auto"] = modelrouter.VirtualModel{Strategy: "smallest", Models: []string{"public"}}
			policy.Models["public"] = modelrouter.ModelMetadata{Enabled: true}
			document.ModelRouting = &policy
			handler, err := NewDataHandler(document, DataOptions{})
			if err != nil {
				t.Fatal(err)
			}
			response := serveChat(t, handler, `{"model":"auto","messages":[{"role":"user","content":"input"}]}`)
			if response.Code != http.StatusOK {
				t.Fatalf("selector after guardrail: %d %s", response.Code, response.Body)
			}
		})
	}
}
