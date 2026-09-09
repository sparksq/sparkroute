// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type responseSequencePicker struct {
	values []int64
}

func (p *responseSequencePicker) Pick(int64) (int64, error) {
	value := p.values[0]
	p.values = p.values[1:]
	return value, nil
}

var _ routing.WeightedPicker = (*responseSequencePicker)(nil)

type failingResponsesState struct{}

func (failingResponsesState) Resolve(
	context.Context,
	string,
	string,
) (responsesstate.Affinity, bool, error) {
	return responsesstate.Affinity{}, false, nil
}

func (failingResponsesState) Bind(
	context.Context,
	responsesstate.Affinity,
) error {
	return fmt.Errorf("state store unavailable")
}

func TestResponsesPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() { _ = request.Body.Close() }()
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		captured <- capturedUpstreamRequest{Path: request.URL.Path, Body: body}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":        "resp_1",
			"object":    "response",
			"model":     "upstream-model",
			"status":    "completed",
			"output":    []any{},
			"new_field": map[string]any{"preserved": true},
			"usage": map[string]any{
				"input_tokens":  13,
				"output_tokens": 5,
				"total_tokens":  18,
				"input_tokens_details": map[string]any{
					"cached_tokens":      3,
					"cache_write_tokens": 2,
				},
				"output_tokens_details": map[string]any{"reasoning_tokens": 1},
			},
		})
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		responsesDocument(upstream.URL+"/v1"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{
			"model":"default",
			"input":"hello",
			"store":false,
			"future_request_field":{"preserve":true}
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	gotRequest := <-captured
	if gotRequest.Path != "/v1/responses" {
		t.Fatalf("upstream path = %q", gotRequest.Path)
	}
	if string(gotRequest.Body["model"]) != `"upstream-model"` ||
		string(gotRequest.Body["future_request_field"]) != `{"preserve":true}` ||
		string(gotRequest.Body["store"]) != "false" {
		t.Fatalf("upstream body = %#v", gotRequest.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["model"]) != `"public"` ||
		string(body["new_field"]) != `{"preserved":true}` {
		t.Fatalf("response body = %s", response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 || records[0].Attempt == nil || records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	if records[1].Request.Operation != "responses" ||
		records[1].Request.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("request record = %#v", records[1].Request)
	}
	assertTokenValue(t, "responses input", records[1].Request.Usage.InputTokens, 13)
	assertTokenValue(t, "responses cached", records[0].Attempt.Usage.CachedInputTokens, 3)
	assertTokenValue(t, "responses cache write", records[0].Attempt.Usage.CacheCreationTokens, 2)
	assertTokenValue(t, "responses reasoning", records[1].Request.Usage.ReasoningTokens, 1)
}

func TestResponsesStreamingTypedEventsAndUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\n")
		_, _ = io.WriteString(w, `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"upstream-model","status":"in_progress"}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","sequence_number":1,"delta":"hello","future":true}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","model":"upstream-model","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`+"\n\n")
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		responsesDocument(upstream.URL+"/v1"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":false,"stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	stream := response.Body.String()
	if strings.Count(stream, `"model":"public"`) != 2 ||
		strings.Contains(stream, `"model":"upstream-model"`) ||
		!strings.Contains(stream, "event: response.output_text.delta\n") ||
		!strings.Contains(stream, `"future":true`) {
		t.Fatalf("stream = %s", stream)
	}
	records := recorder.snapshot()
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	assertTokenValue(t, "stream responses input", records[0].Attempt.Usage.InputTokens, 4)
	assertTokenValue(t, "stream responses output", records[1].Request.Usage.OutputTokens, 2)
}

func TestResponsesStreamingBindsStoredResponse(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_stream","model":"upstream-model"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_stream","model":"upstream-model","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":true,"stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "resp_stream") {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	_, found, err := state.Resolve(
		context.Background(),
		responsesCallerScope(identity.Identity{}),
		"resp_stream",
	)
	if err != nil || !found {
		t.Fatalf("Resolve(resp_stream) found = %v, error = %v", found, err)
	}
}

func TestResponsesDoesNotExposeUnboundStoredResponse(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_must_not_leak", "object": "response", "model": "upstream-model",
		})
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
	)
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: failingResponsesState{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		strings.Contains(response.Body.String(), "resp_must_not_leak") {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "state_affinity_unavailable" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestResponsesAffinityIsCallerScoped(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_next", "object": "response", "model": "upstream-model",
		})
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	owner := identity.Identity{Principal: identity.Principal{
		ID: "client-a", Tenant: "tenant-a", Type: "machine", Subject: "client-a",
	}}
	now := time.Now().UTC()
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope:         responsesCallerScope(owner),
		ResponseID:    "resp_private",
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream-model",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":false,"previous_response_id":"resp_private"}`),
	)
	request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
		Principal: identity.Principal{
			ID: "client-b", Tenant: "tenant-b", Type: "machine", Subject: "client-b",
		},
	}))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestResponsesRejectsDeferredPromptStateBeforeUpstream(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	handler, err := NewDataHandler(responsesDocument(upstream.URL+"/v1"), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	requests := []string{
		`{"model":"public","input":"hello","store":false,"prompt":{"id":"pmpt_1"}}`,
	}
	for _, raw := range requests {
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("request %s status = %d; body=%s", raw, response.Code, response.Body)
			continue
		}
		var body openAIErrorEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
			body.Error.Code != "unsupported_feature" {
			t.Errorf("request %s error = %#v, %v", raw, body, err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestResponsesRejectsNullBooleanControls(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{
		`{"model":"public","input":"hello","store":null}`,
		`{"model":"public","input":"hello","background":null}`,
	} {
		if _, _, _, err := decodeResponsesRequest([]byte(raw)); err == nil {
			t.Errorf("decodeResponsesRequest(%s) error = nil", raw)
		}
	}
}

func TestResponsesStoredStatePinsFollowUp(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "unexpected", "object": "response", "model": "first",
		})
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		call := secondCalls.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		responseID := "resp_1"
		if call == 2 {
			if string(body["previous_response_id"]) != `"resp_1"` {
				t.Errorf("follow-up body = %#v", body)
			}
			responseID = "resp_2"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": responseID, "object": "response", "model": "upstream-second",
			"status": "completed", "output": []any{},
		})
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityStoredCompletion,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:  &responseSequencePicker{values: []int64{150, 0}},
		ResponsesState: state,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	serve := func(raw string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(raw),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	initial := serve(`{"model":"public","input":"hello"}`)
	if initial.Code != http.StatusOK {
		t.Fatalf("initial status = %d; body=%s", initial.Code, initial.Body)
	}
	followUp := serve(
		`{"model":"public","input":"again","previous_response_id":"resp_1"}`,
	)
	if followUp.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d; body=%s", followUp.Code, followUp.Body)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 2 {
		t.Fatalf("upstream calls = (%d, %d), want (0, 2)", firstCalls.Load(), secondCalls.Load())
	}
	affinity, found, err := state.Resolve(
		context.Background(),
		responsesCallerScope(identity.Identity{}),
		"resp_2",
	)
	if err != nil || !found || affinity.Deployment != "deployment-second" {
		t.Fatalf("resp_2 affinity = %#v, %v, %v", affinity, found, err)
	}
}

func TestResponsesStoredRequestDoesNotRetry(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityStoredCompletion,
		)
	}
	handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestResponsesProviderHostedToolDoesNotRetry(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityProviderTools,
		)
	}
	handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":false,"tools":[{"type":"web_search"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestResponsesFallsBackToChatWithoutExplicitDeploymentCapability(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q, want /v1/chat/completions", request.URL.Path)
		}
		defer func() { _ = request.Body.Close() }()
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		if _, ok := body["messages"]; !ok || string(body["model"]) != `"upstream-model"` {
			t.Errorf("translated upstream request = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl_1","object":"chat.completion","model":"upstream-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`)
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL + "/v1")
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(`{"model":"public","input":"hello","store":false}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body["object"]) != `"response"` ||
		string(body["model"]) != `"public"` {
		t.Fatalf("translated response = %s", response.Body)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1", calls.Load())
	}
}

func TestResponsesRequiresStateHostedToolAndBackgroundCapabilities(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, raw := range []string{
		`{"model":"public","input":"hello"}`,
		`{"model":"public","input":"hello","store":false,"tools":[{"type":"web_search"}]}`,
		`{"model":"public","input":"hello","store":false,"background":true}`,
	} {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(raw),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Errorf("request %s status = %d; body=%s", raw, response.Code, response.Body)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestDetectResponsesCapabilities(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"input":[{"role":"developer","content":[
			{"type":"input_image","image_url":"https://example.com/image.png"},
			{"type":"input_video","video_url":"data:video/mp4;base64,eA=="},
			{"type":"input_file","file_data":"ZmlsZQ=="}
		]}],
		"tools":[{"type":"function","name":"lookup"}],
		"parallel_tool_calls":true,
		"text":{"format":{"type":"json_schema","name":"answer","schema":{}}},
		"reasoning":{"effort":"high"},
		"top_logprobs":2,
		"service_tier":"priority"
	}`), &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := detectResponsesCapabilities(envelope, true)
	if err != nil {
		t.Fatalf("detectResponsesCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityDeveloperMessages,
		config.CapabilityFileInput,
		config.CapabilityLogprobs,
		config.CapabilityParallelTools,
		config.CapabilityReasoning,
		config.CapabilityResponses,
		config.CapabilityServiceTier,
		config.CapabilityStoredCompletion,
		config.CapabilityStructuredOutputs,
		config.CapabilityTools,
		config.CapabilityVision,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestDetectResponsesCapabilitiesInToolOutput(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"input":[{
			"type":"function_call_output",
			"call_id":"call_1",
			"output":[{"type":"input_image","image_url":"https://example.com/image.png"}]
		}]
	}`), &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := detectResponsesCapabilities(envelope, false)
	if err != nil {
		t.Fatalf("detectResponsesCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityResponses,
		config.CapabilityStoredCompletion,
		config.CapabilityTools,
		config.CapabilityVision,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestDetectResponsesProviderHostedToolCapability(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"store":false,
		"tools":[{"type":"web_search"}],
		"tool_choice":{"type":"web_search"}
	}`), &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := detectResponsesCapabilities(envelope, false)
	if err != nil {
		t.Fatalf("detectResponsesCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityProviderTools,
		config.CapabilityResponses,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func responsesDocument(baseURL string) config.Document {
	document := proxyDocument(baseURL)
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
	)
	return document
}
