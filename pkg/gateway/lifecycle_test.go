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
