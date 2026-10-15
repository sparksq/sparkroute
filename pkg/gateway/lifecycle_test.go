// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

type gatewayRuntimeController struct {
	registry *endpointregistry.Memory
	endpoint endpointregistry.Endpoint

	mu       sync.Mutex
	ensures  int
	releases int
}

func (c *gatewayRuntimeController) EnsureReady(
	ctx context.Context,
	binding lifecycle.Binding,
	_ lifecycle.RequestFeatures,
) (lifecycle.Lease, error) {
	c.mu.Lock()
	c.ensures++
	id := fmt.Sprintf("lease-%d", c.ensures)
	c.mu.Unlock()
	if binding.FencingToken > 0 {
		c.endpoint.BindingRevision = binding.Revision
		c.endpoint.FencingToken = binding.FencingToken
	}
	if err := c.registry.Register(ctx, c.endpoint); err != nil {
		return lifecycle.Lease{}, err
	}
	return lifecycle.Lease{ID: id, Endpoint: c.endpoint}, nil
}

func (c *gatewayRuntimeController) Release(
	context.Context,
	lifecycle.Lease,
	lifecycle.RequestOutcome,
) error {
	c.mu.Lock()
	c.releases++
	c.mu.Unlock()
	return nil
}

func (c *gatewayRuntimeController) Status(
	context.Context,
	lifecycle.Binding,
) (lifecycle.Status, error) {
	endpoint := c.endpoint
	return lifecycle.Status{
		State: endpointregistry.StateReady, Endpoint: &endpoint, UpdatedAt: time.Now(),
	}, nil
}

func (*gatewayRuntimeController) Stop(
	context.Context,
	lifecycle.Binding,
	lifecycle.StopReason,
) error {
	return nil
}

func TestDynamicEndpointColdStartForChatAndEmbeddings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		request    string
		capability config.Capability
		upstream   string
		response   string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			request:  `{"model":"virtual","messages":[{"role":"user","content":"hello"}]}`,
			upstream: "/v1/chat/completions",
			response: `{"id":"chat-1","object":"chat.completion","model":"upstream","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		},
		{
			name: "embeddings", path: "/v1/embeddings",
			request:    `{"model":"virtual","input":"hello"}`,
			capability: config.CapabilitySingleVectorEmbedding,
			upstream:   "/v1/embeddings",
			response:   `{"object":"list","model":"upstream","data":[{"object":"embedding","index":0,"embedding":[0.25]}],"usage":{"prompt_tokens":1,"total_tokens":1}}`,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var upstreamPath string
			var upstreamModel string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				upstreamPath = request.URL.Path
				var envelope map[string]json.RawMessage
				if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
					t.Errorf("decode upstream body: %v", err)
				}
				_ = json.Unmarshal(envelope["model"], &upstreamModel)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer upstream.Close()

			document := dynamicGatewayDocument(test.capability)
			registry := endpointregistry.NewMemory()
			controller := &gatewayRuntimeController{
				registry: registry,
				endpoint: endpointregistry.Endpoint{
					ID: "endpoint", Target: "deployment", BaseURL: upstream.URL + "/v1",
					Controller: "controller", Protocol: "openai", ServedModels: []string{"upstream"},
					State: endpointregistry.StateReady,
				},
			}
			targets, err := lifecycle.TargetsFromDocument(document)
			if err != nil {
				t.Fatal(err)
			}
			coordinator, err := lifecycle.NewAdmissionCoordinator(targets, lifecycle.AdmissionOptions{
				Registry: registry, Controllers: map[string]lifecycle.Controller{"controller": controller},
				Authorizer: lifecycle.EndpointAuthorizerFunc(
					func(context.Context, endpointregistry.Endpoint) error { return nil },
				),
				InstanceID: "test-gateway", PollInterval: 10 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewDataHandler(document, DataOptions{Lifecycle: coordinator})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.request))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if upstreamPath != test.upstream || upstreamModel != "upstream" {
				t.Fatalf("upstream path/model = %q/%q", upstreamPath, upstreamModel)
			}
			controller.mu.Lock()
			ensures, releases := controller.ensures, controller.releases
			controller.mu.Unlock()
			if ensures != 1 || releases != 1 {
				t.Fatalf("controller ensures/releases = %d/%d, want 1/1", ensures, releases)
			}
		})
	}
}

func TestDynamicEndpointRequiresLifecycleCoordinator(t *testing.T) {
	t.Parallel()

	_, err := NewDataHandler(dynamicGatewayDocument(""), DataOptions{})
	if err == nil {
		t.Fatal("NewDataHandler() error = nil")
	}
}

func dynamicGatewayDocument(capability config.Capability) config.Document {
	capabilities := []config.Capability(nil)
	if capability != "" {
		capabilities = []config.Capability{capability}
	}
	return config.Document{
		Providers: []config.Provider{{Name: "provider", Type: "openai_compatible"}},
		Deployments: []config.Deployment{{
			Name: "deployment", Provider: "provider", Model: "upstream",
			Capabilities: capabilities,
			EndpointSource: config.EndpointSource{
				Type: config.EndpointSourceActivatable, Controller: "controller",
				Revision: "binding-revision", Recipe: "recipe",
				ActivationTimeout: config.Duration(time.Second),
				MaxQueuedWaiters:  2, MaxQueuedBodyBytes: 1 << 20,
				ColdStart: config.ColdStartWait,
			},
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "virtual", RequiredCapabilities: capabilities,
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
				Deployment: "deployment", Weight: 1,
			}}}},
		}},
	}
}

func TestPreferRunningRoutesToWarmFallbackBeforeColdActivation(t *testing.T) {
	enabled, disabled := true, false
	for _, test := range []struct {
		name                    string
		prefer                  *bool
		busy, missing, failWarm bool
		coldCalls, warmCalls    int
	}{
		{name: "explicit opt-out keeps priority", prefer: &disabled, coldCalls: 1},
		{name: "default prefers running fallback", warmCalls: 1},
		{name: "running fallback", prefer: &enabled, warmCalls: 1},
		{name: "busy fallback starts primary", prefer: &enabled, busy: true, coldCalls: 1},
		{name: "no running targets starts primary", prefer: &enabled, missing: true, coldCalls: 1},
		{name: "failed warm target falls back to cold priority", prefer: &enabled, failWarm: true, coldCalls: 1, warmCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			coldCalls, warmCalls := 0, 0
			reply := func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"reply","object":"chat.completion","model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}
			cold := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); coldCalls++; mu.Unlock(); reply(w) }))
			defer cold.Close()
			warm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				warmCalls++
				mu.Unlock()
				if test.failWarm {
					w.WriteHeader(503)
					return
				}
				reply(w)
			}))
			defer warm.Close()
			document := dynamicGatewayDocument("")
			document.Deployments = append(document.Deployments, config.Deployment{Name: "warm", Provider: "provider", Model: "upstream", EndpointSource: config.EndpointSource{Type: config.EndpointSourceDiscovered, Controller: "controller"}})
			document.VirtualModels[0].Selection.PreferRunning = test.prefer
			document.VirtualModels[0].Pools = append(document.VirtualModels[0].Pools, config.RoutingPool{Priority: 1, Targets: []config.WeightedTarget{{Deployment: "warm", Weight: 1}}})
			registry := endpointregistry.NewMemory()
			endpoint := endpointregistry.Endpoint{ID: "cold-endpoint", Target: "deployment", BaseURL: cold.URL + "/v1", Controller: "controller", Protocol: "openai", ServedModels: []string{"upstream"}, State: endpointregistry.StateReady}
			controller := &gatewayRuntimeController{registry: registry, endpoint: endpoint}
			if !test.missing {
				endpoint.ID = "warm-endpoint"
				endpoint.Target = "warm"
				endpoint.BaseURL = warm.URL + "/v1"
				endpoint.MaxConcurrency = 1
				if test.busy {
					endpoint.ActiveRequests = 1
				}
				if err := registry.Register(context.Background(), endpoint); err != nil {
					t.Fatal(err)
				}
			}
			targets, err := lifecycle.TargetsFromDocument(document)
			if err != nil {
				t.Fatal(err)
			}
			coordinator, err := lifecycle.NewAdmissionCoordinator(targets, lifecycle.AdmissionOptions{Registry: registry, Controllers: map[string]lifecycle.Controller{"controller": controller}, Authorizer: lifecycle.EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil })})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewDataHandler(document, DataOptions{Lifecycle: coordinator})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"virtual","messages":[{"role":"user","content":"hello"}]}`))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if coldCalls != test.coldCalls || warmCalls != test.warmCalls {
				t.Fatalf("cold/warm requests = %d/%d; want %d/%d", coldCalls, warmCalls, test.coldCalls, test.warmCalls)
			}
			controller.mu.Lock()
			defer controller.mu.Unlock()
			if controller.ensures != test.coldCalls {
				t.Fatalf("cold activation calls = %d, want %d", controller.ensures, test.coldCalls)
			}
		})
	}
}
