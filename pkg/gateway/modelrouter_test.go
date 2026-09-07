// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
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
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

type escapingProviderStateRouter struct{}

func (escapingProviderStateRouter) OwnsVirtualRequest(requested string) bool {
	return requested == "auto"
}

func (escapingProviderStateRouter) Route(
	_ context.Context,
	_ modelrouter.Input,
) (modelrouter.Decision, error) {
	return modelrouter.Decision{
		VirtualModel: "first",
		Annotations:  map[string]string{"selector": "auto"},
	}, nil
}

func TestNativeModelRouterDrivesChatAndEmbeddingExecution(t *testing.T) {
	t.Parallel()

	var chatCalls atomic.Int64
	chatUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		chatCalls.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("chat upstream path = %q", request.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-routed", "model": "chat-physical", "choices": []any{},
		})
	}))
	defer chatUpstream.Close()

	var embeddingCalls atomic.Int64
	embeddingUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		embeddingCalls.Add(1)
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("embedding upstream path = %q", request.URL.Path)
		}
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read embedding request: %v", err)
		}
		if !strings.Contains(string(raw), `"model":"embedding-physical"`) {
			t.Errorf("embedding upstream body = %s", raw)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{},
			"model":  "embedding-physical",
			"usage": map[string]any{
				"prompt_tokens": 1,
				"total_tokens":  1,
			},
		})
	}))
	defer embeddingUpstream.Close()

	document := config.Document{
		Providers: []config.Provider{
			{Name: "chat-provider", Type: "openai_compatible", BaseURL: chatUpstream.URL + "/v1"},
			{Name: "embedding-provider", Type: "openai_compatible", BaseURL: embeddingUpstream.URL + "/v1"},
		},
		Deployments: []config.Deployment{
			{Name: "chat-deployment", Provider: "chat-provider", Model: "chat-physical"},
			{
				Name: "embedding-deployment", Provider: "embedding-provider", Model: "embedding-physical",
				Capabilities: []config.Capability{config.CapabilitySingleVectorEmbedding},
			},
		},
		VirtualModels: []config.VirtualModel{
			{Name: "general", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "chat-deployment", Weight: 100}}}}},
			{Name: "vectors", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "embedding-deployment", Weight: 100}}}}},
		},
	}
	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version:             modelrouter.RoutingPolicyVersion,
		Revision:            7,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{"general", "vectors"}},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"general": {Enabled: true},
			"vectors": {Enabled: true},
		},
		KeywordRules: []modelrouter.KeywordRule{
			{Name: "chat", Keywords: []string{"code"}, Strategy: "round_robin", Models: []string{"general"}},
		},
	})
	if err != nil {
		t.Fatalf("NewRouterManager() error = %v", err)
	}
	policy := router.Policy()
	document.ModelRouting = &policy
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	models := httptest.NewRecorder()
	handler.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"auto"`) {
		t.Fatalf("models status/body = %d %s", models.Code, models.Body)
	}

	chat := httptest.NewRecorder()
	chatRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"write code"}]}`),
	)
	chatRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(chat, chatRequest)
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status/body = %d %s", chat.Code, chat.Body)
	}
	assertResponseModel(t, chat.Body.Bytes(), "auto")

	embedding := httptest.NewRecorder()
	embeddingRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(`{"model":"auto","input":"embedding this text"}`),
	)
	embeddingRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(embedding, embeddingRequest)
	if embedding.Code != http.StatusOK {
		t.Fatalf("embedding status/body = %d %s", embedding.Code, embedding.Body)
	}
	assertResponseModel(t, embedding.Body.Bytes(), "auto")

	if chatCalls.Load() != 1 || embeddingCalls.Load() != 1 {
		t.Fatalf("upstream calls = chat %d, embedding %d", chatCalls.Load(), embeddingCalls.Load())
	}
	records := recorder.snapshot()
	var routedRequests int
	for _, record := range records {
		if record.Request == nil {
			continue
		}
		routedRequests++
		if record.Request.RequestedModel != "auto" {
			t.Fatalf("requested model = %q", record.Request.RequestedModel)
		}
		if record.Request.VirtualModel != "general" && record.Request.VirtualModel != "vectors" {
			t.Fatalf("selected virtual model = %q", record.Request.VirtualModel)
		}
	}
	if routedRequests != 2 {
		t.Fatalf("routed request records = %d, want 2", routedRequests)
	}
}

func TestSelectorPresetProviderPriorityOrdersExecutionAndFailover(t *testing.T) {
	t.Parallel()

	var preferredCalls atomic.Int64
	preferred := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		preferredCalls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "temporarily unavailable"},
		})
	}))
	defer preferred.Close()
	var ordinaryCalls atomic.Int64
	ordinary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ordinaryCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-provider-priority", "model": "ordinary-upstream", "choices": []any{},
		})
	}))
	defer ordinary.Close()

	document := config.Document{
		Providers: []config.Provider{
			{Name: "ordinary", Type: "openai_compatible", BaseURL: ordinary.URL + "/v1"},
			// Matching is case-insensitive because SparkRoute provider IDs are
			// normalized while gateway provider names are case-preserving.
			{Name: "Preferred", Type: "openai_compatible", BaseURL: preferred.URL + "/v1"},
		},
		Deployments: []config.Deployment{
			{Name: "ordinary-deployment", Provider: "ordinary", Model: "ordinary-upstream"},
			{Name: "preferred-deployment", Provider: "Preferred", Model: "preferred-upstream"},
		},
		VirtualModels: []config.VirtualModel{{
			Name: "logical",
			Pools: []config.RoutingPool{
				{Priority: 0, Targets: []config.WeightedTarget{{Deployment: "ordinary-deployment", Weight: 100}}},
				{Priority: 10, Targets: []config.WeightedTarget{{Deployment: "preferred-deployment", Weight: 100}}},
			},
		}},
	}
	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 8, DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {
				Strategy: "round_robin", Models: []string{"logical"},
				Kwargs: map[string]map[string]any{
					"private": {"provider_priority": []string{"preferred", "ordinary"}},
				},
			},
		},
		Models: map[string]modelrouter.ModelMetadata{"logical": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := router.Policy()
	document.ModelRouting = &policy
	ledgerRecorder := &collectingRecorder{}
	traceRecorder := &savedTraceCollector{}
	handler, err := NewDataHandler(document, DataOptions{
		Ledger:       ledgerRecorder,
		SavedTraces:  traceRecorder,
		RetrySleeper: &recordingRetrySleeper{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"auto:private","messages":[{"role":"user","content":"hello"}]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", response.Code, response.Body)
	}
	assertResponseModel(t, response.Body.Bytes(), "auto:private")
	if preferredCalls.Load() != 1 || ordinaryCalls.Load() != 1 {
		t.Fatalf("upstream calls = preferred %d, ordinary %d", preferredCalls.Load(), ordinaryCalls.Load())
	}
	records := ledgerRecorder.snapshot()
	if len(records) != 3 || records[0].Attempt == nil || records[1].Attempt == nil || records[2].Request == nil ||
		records[0].Attempt.Provider != "Preferred" || !records[0].Attempt.Retried ||
		records[1].Attempt.Provider != "ordinary" || records[1].Attempt.Retried ||
		records[2].Request.FinalProvider != "ordinary" || records[2].Request.AttemptCount != 2 {
		t.Fatalf("provider-priority ledger records = %#v", records)
	}
	traces := traceRecorder.snapshot()
	if len(traces) != 1 || traces[0].RequestedModel != "auto:private" ||
		traces[0].VirtualModel != "logical" || traces[0].FinalProvider != "ordinary" ||
		traces[0].AttemptCount != 2 {
		t.Fatalf("provider-priority saved trace = %#v", traces)
	}
}

func TestSelectorProviderStatePinsResponseAndFileLogicalModel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "unexpected", "object": "response", "model": "upstream-first",
			"output": []any{},
		})
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		switch request.URL.Path {
		case "/v1/responses":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_next", "object": "response", "model": "upstream-second",
				"output": []any{},
			})
		case "/v1/chat/completions":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "chat_next", "object": "chat.completion", "model": "upstream-second",
				"choices": []any{},
			})
		default:
			t.Errorf("unexpected upstream path %q", request.URL.Path)
		}
	}))
	defer second.Close()

	document := selectorStateDocument(first.URL+"/v1", second.URL+"/v1")
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	scope := responsesCallerScope(identity.Identity{})
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope: scope, ResponseID: "resp_prior", VirtualModel: "second",
		Provider: "provider-second", Deployment: "deployment-second",
		UpstreamModel: "upstream-second", BoundAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Bind(response) error = %v", err)
	}
	if err := state.BindResource(context.Background(), responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: "file_second",
		},
		VirtualModel: "second", Provider: "provider-second",
		Deployment: "deployment-second", UpstreamModel: "upstream-second",
		BoundAt: now,
	}); err != nil {
		t.Fatalf("BindResource(file) error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"auto","previous_response_id":"resp_prior","input":"continue","store":false}`,
		identity.Identity{},
	)
	if response.Code != http.StatusOK {
		t.Fatalf("Responses status/body = %d %s", response.Code, response.Body)
	}
	assertResponseModel(t, response.Body.Bytes(), "auto")

	chat := serveGatewayRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/chat/completions",
		`{"model":"auto","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_second"}}]}]}`,
		"application/json",
	)
	if chat.Code != http.StatusOK {
		t.Fatalf("Chat status/body = %d %s", chat.Code, chat.Body)
	}
	assertResponseModel(t, chat.Body.Bytes(), "auto")
	if firstCalls.Load() != 0 || secondCalls.Load() != 2 {
		t.Fatalf(
			"upstream calls = first %d, second %d",
			firstCalls.Load(), secondCalls.Load(),
		)
	}
}

func TestSelectorProviderStateRejectsConflictingReferences(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	document := selectorStateDocument(upstream.URL+"/v1", upstream.URL+"/v1")
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	scope := responsesCallerScope(identity.Identity{})
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope: scope, ResponseID: "resp_second", VirtualModel: "second",
		Provider: "provider-second", Deployment: "deployment-second",
		UpstreamModel: "upstream-second", BoundAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.BindResource(context.Background(), responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: "file_first",
		},
		VirtualModel: "first", Provider: "provider-first",
		Deployment: "deployment-first", UpstreamModel: "upstream-first", BoundAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatal(err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"auto","previous_response_id":"resp_second","store":false,"input":[{"type":"input_file","file_id":"file_first"}]}`,
		identity.Identity{},
	)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), `"code":"state_affinity_model_mismatch"`) ||
		calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
}

func TestGatewayRejectsRouterWhichEscapesProviderStatePin(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	document := selectorStateDocument(upstream.URL+"/v1", upstream.URL+"/v1")
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope: responsesCallerScope(identity.Identity{}), ResponseID: "resp_second",
		VirtualModel: "second", Provider: "provider-second",
		Deployment: "deployment-second", UpstreamModel: "upstream-second",
		BoundAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
		ModelRouter:    escapingProviderStateRouter{},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"auto","previous_response_id":"resp_second","input":"continue","store":false}`,
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"state_affinity_model_unavailable"`) ||
		calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
}

func selectorStateDocument(firstBaseURL, secondBaseURL string) config.Document {
	capabilities := []config.Capability{
		config.CapabilityResponses,
		config.CapabilityStoredCompletion,
		config.CapabilityFiles,
		config.CapabilityFileInput,
	}
	document := config.Document{
		Providers: []config.Provider{
			{Name: "provider-first", Type: "openai_compatible", BaseURL: firstBaseURL},
			{Name: "provider-second", Type: "openai_compatible", BaseURL: secondBaseURL},
		},
		Deployments: []config.Deployment{
			{Name: "deployment-first", Provider: "provider-first", Model: "upstream-first", Capabilities: capabilities},
			{Name: "deployment-second", Provider: "provider-second", Model: "upstream-second", Capabilities: capabilities},
		},
		VirtualModels: []config.VirtualModel{
			{Name: "first", Aliases: []string{"default"}, Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "deployment-first", Weight: 100}}}}},
			{Name: "second", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "deployment-second", Weight: 100}}}}},
		},
	}
	document.ModelRouting = &modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 12,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{"first", "second"}},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"first": {Enabled: true}, "second": {Enabled: true},
		},
	}
	return document
}

func assertResponseModel(t *testing.T, raw []byte, want string) {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var got string
	if err := json.Unmarshal(body["model"], &got); err != nil {
		t.Fatalf("decode response model: %v", err)
	}
	if got != want {
		t.Fatalf("response model = %q, want %q", got, want)
	}
}
