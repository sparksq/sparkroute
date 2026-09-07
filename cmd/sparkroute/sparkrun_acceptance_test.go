// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
)

type acceptanceUpstream struct {
	server *httptest.Server

	mu     sync.Mutex
	calls  map[string]int
	models map[string]int
}

func newAcceptanceUpstream(t *testing.T) *acceptanceUpstream {
	t.Helper()
	upstream := &acceptanceUpstream{
		calls: make(map[string]int), models: make(map[string]int),
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var envelope map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
			http.Error(writer, "invalid fixture request", http.StatusBadRequest)
			return
		}
		var model string
		_ = json.Unmarshal(envelope["model"], &model)
		upstream.mu.Lock()
		upstream.calls[request.URL.Path]++
		upstream.models[model]++
		upstream.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/chat/completions":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": "chatcmpl-acceptance", "object": "chat.completion",
				"model": model, "choices": []any{},
				"usage": map[string]int{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
			})
		case "/v1/embeddings":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"object": "list", "model": model,
				"data": []any{map[string]any{
					"object": "embedding", "index": 0, "embedding": []float64{0.25, 0.75},
				}},
				"usage": map[string]int{"prompt_tokens": 1, "total_tokens": 1},
			})
		case "/v1/responses":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": "resp-acceptance-next", "object": "response",
				"model": model, "status": "completed", "output": []any{},
				"usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
			})
		default:
			http.Error(writer, "unexpected fixture path", http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *acceptanceUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func (u *acceptanceUpstream) modelCount(model string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.models[model]
}

func TestSparkrunRuntimeEndToEndAcceptance(t *testing.T) {
	upstream := newAcceptanceUpstream(t)
	statePath := newAcceptanceBridgeFixture(t, upstream.server.URL, acceptanceBridgeState{
		WarmVisible: true, EnsureDelayMilliseconds: 250,
	})
	stateStore := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	if err := stateStore.Bind(context.Background(), responsesstate.Affinity{
		Scope:         acceptanceCallerScope(),
		ResponseID:    "resp-cold-prior",
		VirtualModel:  "cold",
		Provider:      "local",
		Deployment:    "cold-deployment",
		UpstreamModel: "cold-upstream",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("bind cold response affinity: %v", err)
	}

	document, router := acceptanceRoutingDocument(true, 32, 1<<20, 10*time.Second, 150*time.Millisecond)
	generation, cancel := buildAcceptanceRuntime(t, document, router, stateStore)
	defer func() {
		cancel()
		generation.close()
	}()

	waitAcceptance(t, 10*time.Second, func() bool {
		state := mustAcceptanceBridgeState(t, statePath)
		_, metadataReady := router.DiscoveredMetadata().Effective["warm"]
		return state.DiscoverCalls > 0 && metadataReady
	}, "initial discovered endpoint and metadata")

	bridgeBeforeWarm := mustAcceptanceBridgeState(t, statePath)
	chat := serveAcceptanceJSON(
		t,
		generation.data,
		"/v1/chat/completions",
		`{"model":"warm","messages":[{"role":"user","content":"warm chat"}]}`,
	)
	if chat.Code != http.StatusOK {
		t.Fatalf("warm chat status/body = %d %s", chat.Code, chat.Body)
	}
	embedding := serveAcceptanceJSON(
		t,
		generation.data,
		"/v1/embeddings",
		`{"model":"warm","input":"warm embedding"}`,
	)
	if embedding.Code != http.StatusOK {
		t.Fatalf("warm embedding status/body = %d %s", embedding.Code, embedding.Body)
	}
	bridgeAfterWarm := mustAcceptanceBridgeState(t, statePath)
	if bridgeAfterWarm.EnsureCalls != bridgeBeforeWarm.EnsureCalls ||
		bridgeAfterWarm.StopCalls != bridgeBeforeWarm.StopCalls {
		t.Fatalf(
			"warm inference entered bridge activation path: before=%#v after=%#v",
			bridgeBeforeWarm,
			bridgeAfterWarm,
		)
	}

	const concurrentRequests = 8
	type responseResult struct {
		status int
		body   string
	}
	results := make(chan responseResult, concurrentRequests)
	start := make(chan struct{})
	for range concurrentRequests {
		go func() {
			<-start
			response := serveAcceptanceJSONNoTest(
				generation.data,
				"/v1/responses",
				`{"model":"auto","previous_response_id":"resp-cold-prior","input":"continue","store":false}`,
			)
			results <- responseResult{status: response.Code, body: response.Body.String()}
		}()
	}
	close(start)
	for range concurrentRequests {
		select {
		case result := <-results:
			if result.status != http.StatusOK || !responseModelIs(result.body, "auto") {
				t.Fatalf("cold response status/body = %d %s", result.status, result.body)
			}
		case <-time.After(15 * time.Second):
			t.Fatal("concurrent cold-start request timed out")
		}
	}
	bridgeAfterCold := mustAcceptanceBridgeState(t, statePath)
	if bridgeAfterCold.EnsureCalls != 1 ||
		bridgeAfterCold.LastRecipe != "cold-recipe" ||
		bridgeAfterCold.LastRecipeRevision != "coldrev00001" {
		t.Fatalf("activation bridge state = %#v", bridgeAfterCold)
	}
	if upstream.modelCount("cold-upstream") != concurrentRequests {
		t.Fatalf(
			"cold upstream requests = %d, want %d",
			upstream.modelCount("cold-upstream"),
			concurrentRequests,
		)
	}
	if _, exists := router.DiscoveredMetadata().Effective["cold"]; !exists {
		t.Fatal("cold activation metadata was not published")
	}

	waitAcceptance(t, 10*time.Second, func() bool {
		state := mustAcceptanceBridgeState(t, statePath)
		_, coldMetadata := router.DiscoveredMetadata().Effective["cold"]
		return state.StopCalls == 1 && !state.ColdActive && !coldMetadata
	}, "idle stop and metadata removal")

	discoverBeforeRemoval := mustAcceptanceBridgeState(t, statePath).DiscoverCalls
	if _, err := updateAcceptanceBridgeState(statePath, func(state *acceptanceBridgeState) error {
		state.WarmVisible = false
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitAcceptance(t, 15*time.Second, func() bool {
		state := mustAcceptanceBridgeState(t, statePath)
		_, warmMetadata := router.DiscoveredMetadata().Effective["warm"]
		return state.DiscoverCalls > discoverBeforeRemoval && !warmMetadata
	}, "discovered endpoint disappearance")
	missing := serveAcceptanceJSON(
		t,
		generation.data,
		"/v1/chat/completions",
		`{"model":"warm","messages":[{"role":"user","content":"gone"}]}`,
	)
	if missing.Code != http.StatusServiceUnavailable ||
		!strings.Contains(missing.Body.String(), `"code":"endpoint_not_ready"`) {
		t.Fatalf("missing endpoint status/body = %d %s", missing.Code, missing.Body)
	}
	if upstream.count("/v1/chat/completions") != 1 ||
		upstream.count("/v1/embeddings") != 1 ||
		upstream.count("/v1/responses") != concurrentRequests {
		t.Fatalf("unexpected upstream counts: %#v", upstream.calls)
	}
}

func TestSparkrunRuntimeBoundsColdStartAndPropagatesCancellation(t *testing.T) {
	upstream := newAcceptanceUpstream(t)
	statePath := newAcceptanceBridgeFixture(t, upstream.server.URL, acceptanceBridgeState{
		EnsureDelayMilliseconds: 350,
	})
	document, _ := acceptanceRoutingDocument(false, 2, 4096, 10*time.Second, 80*time.Millisecond)
	generation, cancelRuntime := buildAcceptanceRuntime(t, document, nil, nil)
	defer func() {
		cancelRuntime()
		generation.close()
	}()

	smallBody := `{"model":"cold","messages":[{"role":"user","content":"queue"}]}`
	type requestResult struct {
		status int
		body   string
	}
	results := make(chan requestResult, 2)
	for range 2 {
		go func() {
			response := serveAcceptanceJSONNoTest(
				generation.data, "/v1/chat/completions", smallBody,
			)
			results <- requestResult{status: response.Code, body: response.Body.String()}
		}()
	}
	waitAcceptance(t, 10*time.Second, func() bool {
		return mustAcceptanceBridgeState(t, statePath).EnsureCalls == 1
	}, "cold-start owner")
	queueFull := serveAcceptanceJSON(
		t, generation.data, "/v1/chat/completions", smallBody,
	)
	if queueFull.Code != http.StatusServiceUnavailable ||
		!strings.Contains(queueFull.Body.String(), `"code":"cold_start_queue_full"`) {
		t.Fatalf("queue-full status/body = %d %s", queueFull.Code, queueFull.Body)
	}
	for range 2 {
		result := <-results
		if result.status != http.StatusOK {
			t.Fatalf("queued request status/body = %d %s", result.status, result.body)
		}
	}
	waitAcceptance(t, 10*time.Second, func() bool {
		return mustAcceptanceBridgeState(t, statePath).StopCalls == 1 &&
			acceptanceRuntimeOffline(generation, document)
	}, "first idle stop")

	largeBody := fmt.Sprintf(
		`{"model":"cold","messages":[{"role":"user","content":"%s"}]}`,
		strings.Repeat("x", 3000),
	)
	largeDone := make(chan requestResult, 1)
	go func() {
		response := serveAcceptanceJSONNoTest(
			generation.data, "/v1/chat/completions", largeBody,
		)
		largeDone <- requestResult{status: response.Code, body: response.Body.String()}
	}()
	waitAcceptance(t, 10*time.Second, func() bool {
		return mustAcceptanceBridgeState(t, statePath).EnsureCalls == 2
	}, "second cold-start owner")
	queueBytes := serveAcceptanceJSON(
		t, generation.data, "/v1/chat/completions", largeBody,
	)
	if queueBytes.Code != http.StatusServiceUnavailable ||
		!strings.Contains(queueBytes.Body.String(), `"code":"cold_start_queue_bytes_exceeded"`) {
		t.Fatalf("queue-bytes status/body = %d %s", queueBytes.Code, queueBytes.Body)
	}
	if result := <-largeDone; result.status != http.StatusOK {
		t.Fatalf("large owner status/body = %d %s", result.status, result.body)
	}
	waitAcceptance(t, 10*time.Second, func() bool {
		return mustAcceptanceBridgeState(t, statePath).StopCalls == 2 &&
			acceptanceRuntimeOffline(generation, document)
	}, "second idle stop")

	if _, err := updateAcceptanceBridgeState(statePath, func(state *acceptanceBridgeState) error {
		state.EnsureDelayMilliseconds = 1000
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requestContext, cancelRequest := context.WithCancel(context.Background())
	cancelled := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(smallBody),
		).WithContext(requestContext)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		generation.data.ServeHTTP(response, request)
		cancelled <- response
	}()
	waitAcceptance(t, 10*time.Second, func() bool {
		return mustAcceptanceBridgeState(t, statePath).EnsureCalls == 3
	}, "cancellable activation")
	cancelRequest()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled cold-start request did not return")
	}
	if mustAcceptanceBridgeState(t, statePath).ColdActive {
		t.Fatal("cancelled bridge process activated the workload")
	}
}

func TestSparkrunRuntimeRejectsActivationTimeoutAndRecipeMismatch(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		upstream := newAcceptanceUpstream(t)
		statePath := newAcceptanceBridgeFixture(t, upstream.server.URL, acceptanceBridgeState{
			EnsureDelayMilliseconds: 500,
		})
		document, _ := acceptanceRoutingDocument(false, 4, 1<<20, 100*time.Millisecond, 0)
		generation, cancel := buildAcceptanceRuntime(t, document, nil, nil)
		defer func() {
			cancel()
			generation.close()
		}()
		response := serveAcceptanceJSON(
			t,
			generation.data,
			"/v1/chat/completions",
			`{"model":"cold","messages":[{"role":"user","content":"timeout"}]}`,
		)
		if response.Code != http.StatusGatewayTimeout ||
			!strings.Contains(response.Body.String(), `"code":"activation_timeout"`) {
			t.Fatalf("timeout status/body = %d %s", response.Code, response.Body)
		}
		if upstream.count("/v1/chat/completions") != 0 ||
			mustAcceptanceBridgeState(t, statePath).ColdActive {
			t.Fatal("timed-out activation reached inference or became active")
		}
	})

	t.Run("recipe_revision", func(t *testing.T) {
		upstream := newAcceptanceUpstream(t)
		statePath := newAcceptanceBridgeFixture(t, upstream.server.URL, acceptanceBridgeState{
			BadRecipeRevision: true,
		})
		document, _ := acceptanceRoutingDocument(false, 4, 1<<20, 10*time.Second, 0)
		generation, cancel := buildAcceptanceRuntime(t, document, nil, nil)
		defer func() {
			cancel()
			generation.close()
		}()
		response := serveAcceptanceJSON(
			t,
			generation.data,
			"/v1/chat/completions",
			`{"model":"cold","messages":[{"role":"user","content":"mismatch"}]}`,
		)
		if response.Code != http.StatusServiceUnavailable ||
			!strings.Contains(response.Body.String(), `"code":"activation_failed"`) {
			t.Fatalf("recipe mismatch status/body = %d %s", response.Code, response.Body)
		}
		if upstream.count("/v1/chat/completions") != 0 {
			t.Fatal("recipe-mismatched endpoint reached inference")
		}
		state := mustAcceptanceBridgeState(t, statePath)
		if state.EnsureCalls != 1 || state.LastRecipeRevision != "coldrev00001" {
			t.Fatalf("recipe mismatch bridge state = %#v", state)
		}
	})
}

func newAcceptanceBridgeFixture(
	t *testing.T,
	serverURL string,
	initial acceptanceBridgeState,
) string {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	host, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	initial.Host = host
	initial.Port = port
	statePath := t.TempDir() + "/bridge-state.json"
	if _, err := updateAcceptanceBridgeState(statePath, func(state *acceptanceBridgeState) error {
		*state = initial
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(acceptanceBridgeStateEnvironment, statePath)
	return statePath
}

func acceptanceRoutingDocument(
	includeWarm bool,
	maxQueuedWaiters int,
	maxQueuedBodyBytes int64,
	activationTimeout time.Duration,
	idleTTL time.Duration,
) (config.Document, *modelrouter.RouterManager) {
	provider := config.Provider{Name: "local", Type: "openai_compatible"}
	coldCapabilities := []config.Capability{
		config.CapabilityResponses,
		config.CapabilityStoredCompletion,
	}
	document := config.Document{
		Providers: []config.Provider{provider},
		Deployments: []config.Deployment{{
			Name: "cold-deployment", Provider: "local", Model: "cold-upstream",
			Capabilities: coldCapabilities,
			EndpointSource: config.EndpointSource{
				Type: config.EndpointSourceActivatable, Controller: "sparkrun",
				Revision: "cold-binding-v1", Recipe: "cold-recipe",
				RecipeRevision: "coldrev00001", ClusterCandidates: []string{"local-cluster"},
				ActivationTimeout: config.Duration(activationTimeout),
				IdleTTL:           config.Duration(idleTTL),
				MaxQueuedWaiters:  maxQueuedWaiters, MaxQueuedBodyBytes: maxQueuedBodyBytes,
				ColdStart: config.ColdStartWait,
			},
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "cold",
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
				Deployment: "cold-deployment", Weight: 100,
			}}}},
		}},
	}
	policy := modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 1,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{"cold"}},
		},
		Models: map[string]modelrouter.ModelMetadata{"cold": {Enabled: true}},
	}
	if includeWarm {
		document.Deployments = append(document.Deployments, config.Deployment{
			Name: "warm-deployment", Provider: "local", Model: "warm-upstream",
			Capabilities: []config.Capability{config.CapabilitySingleVectorEmbedding},
			EndpointSource: config.EndpointSource{
				Type: config.EndpointSourceDiscovered, Controller: "sparkrun",
			},
		})
		document.VirtualModels = append(document.VirtualModels, config.VirtualModel{
			Name: "warm", Aliases: []string{"default"},
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
				Deployment: "warm-deployment", Weight: 100,
			}}}},
		})
		policy.VirtualModels["auto"] = modelrouter.VirtualModel{
			Strategy: "round_robin", Models: []string{"warm", "cold"},
		}
		policy.Models["warm"] = modelrouter.ModelMetadata{Enabled: true}
	}
	document.ModelRouting = &policy
	router, err := modelrouter.NewRouterManager(policy)
	if err != nil {
		panic(err)
	}
	return document, router
}

func buildAcceptanceRuntime(
	t *testing.T,
	document config.Document,
	router modelrouter.Router,
	state responsesstate.Store,
) (*runtimeGeneration, context.CancelFunc) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	generation, err := buildRuntimeGeneration(document, "acceptance-v1", runtimeBuildOptions{
		Context:             ctx,
		Logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		Telemetry:           &otlpexport.Runtime{},
		ResponsesState:      state,
		ModelRouter:         router,
		SparkrunCommand:     executable,
		SparkrunEndpointTTL: 10 * time.Second,
		SparkrunReconcile:   5 * time.Second,
		SparkrunStopTimeout: time.Second,
	})
	if err != nil {
		cancel()
		t.Fatalf("build acceptance runtime: %v", err)
	}
	return generation, cancel
}

func acceptanceRuntimeOffline(
	generation *runtimeGeneration,
	document config.Document,
) bool {
	if generation.sparkrun == nil {
		return false
	}
	targets, err := lifecycle.TargetsFromDocument(document)
	if err != nil {
		return false
	}
	for _, target := range targets {
		if target.Deployment != "cold-deployment" {
			continue
		}
		status, statusErr := generation.sparkrun.Status(
			context.Background(), target.Binding,
		)
		return statusErr == nil && status.State == endpointregistry.StateOffline
	}
	return false
}

func serveAcceptanceJSON(
	t *testing.T,
	handler http.Handler,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	return serveAcceptanceJSONNoTest(handler, path, body)
}

func serveAcceptanceJSONNoTest(
	handler http.Handler,
	path string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func responseModelIs(body string, want string) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return false
	}
	var model string
	return json.Unmarshal(envelope["model"], &model) == nil && model == want
}

func acceptanceCallerScope() string {
	principal := identity.Identity{}.Principal
	// Keep this test helper in lock-step with gateway.responsesCallerScope
	// without exposing caller hashes or provider-owned IDs to the bridge.
	raw := "v1\x00" + principal.Tenant + "\x00" + principal.ID
	return fmt.Sprintf("%x", sha256Sum([]byte(raw)))
}

func sha256Sum(value []byte) [32]byte {
	// Isolated for an obvious audit point in this cross-package acceptance test.
	return sha256.Sum256(value)
}

func waitAcceptance(
	t *testing.T,
	timeout time.Duration,
	condition func() bool,
	description string,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", description)
}
