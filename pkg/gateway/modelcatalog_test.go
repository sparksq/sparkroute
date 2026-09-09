// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/modelcatalog"
)

func TestModelCatalogRoutesUnknownTenantModelThroughNormalDataPlane(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer catalog-secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-catalog","object":"chat.completion","created":1,
			"model":"upstream-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"catalog"},"finish_reason":"stop"}]
		}`)
	}))
	defer upstream.Close()

	requestKey := modelcatalog.Request{TenantID: "tenant-a", RequestedModel: "tenant-model"}
	entry := gatewayCatalogEntry("tenant-model", upstream.URL+"/v1")
	entry.Providers[0].Auth = config.ProviderAuth{
		Type: config.AuthBearer, Credential: credentials.Ref("env://CATALOG_SECRET"),
	}
	resolver := modelcatalog.NewMemoryResolver(map[modelcatalog.Request]modelcatalog.Entry{
		requestKey: entry,
	})
	directory := gatewayCatalogDirectory(t, resolver, upstream)
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(proxyDocument("https://central.example/v1"), DataOptions{
		ModelCatalog: directory,
		ModelCatalogCredentials: staticCatalogCredentialSource{
			"env://CATALOG_SECRET": "catalog-secret",
		},
		Ledger: recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	serve := func(tenant string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"tenant-model","messages":[{"role":"user","content":"hello"}]}`),
		)
		request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
			Principal:   identity.Principal{ID: "principal", Tenant: tenant},
			Attribution: identity.Attribution{identity.AttributeTenant: tenant},
		}))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := serve("tenant-a"); response.Code != http.StatusOK {
		t.Fatalf("tenant-a status = %d; body = %q", response.Code, response.Body.String())
	}
	if response := serve("tenant-a"); response.Code != http.StatusOK {
		t.Fatalf("cached tenant-a status = %d; body = %q", response.Code, response.Body.String())
	}
	if response := serve("tenant-b"); response.Code != http.StatusNotFound {
		t.Fatalf("tenant-b status = %d; body = %q", response.Code, response.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2", got)
	}
	var requestRecord *ledger.RequestRecord
	for _, record := range recorder.snapshot() {
		if record.Kind == ledger.KindRequest && record.Request != nil {
			requestRecord = record.Request
			break
		}
	}
	if requestRecord == nil || requestRecord.TenantID != "tenant-a" ||
		!strings.HasPrefix(requestRecord.ConfigRevision, "catalog:test:revision-1:") {
		t.Fatalf("catalog request record = %#v", requestRecord)
	}
	stats := directory.Stats()
	if stats.Misses != 2 || stats.PositiveHits != 1 || stats.NotFound != 1 {
		t.Fatalf("directory stats = %#v", stats)
	}
}

func TestModelCatalogCentralModelAlwaysWins(t *testing.T) {
	t.Parallel()

	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-central","object":"chat.completion","created":1,
			"model":"upstream-model",
			"choices":[{"index":0,"message":{"role":"assistant","content":"central"},"finish_reason":"stop"}]
		}`)
	}))
	defer central.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("tenant catalog shadowed a central model")
	}))
	defer blocked.Close()
	resolver := modelcatalog.NewMemoryResolver(map[modelcatalog.Request]modelcatalog.Entry{
		{TenantID: "tenant-a", RequestedModel: "central-alias"}: gatewayCatalogEntry("central-alias", blocked.URL+"/v1"),
	})
	directory := gatewayCatalogDirectory(t, resolver, blocked)
	document := proxyDocument(central.URL + "/v1")
	document.VirtualModels[0].Aliases = []string{"central-alias"}
	handler, err := NewDataHandler(document, DataOptions{ModelCatalog: directory})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"central-alias","messages":[{"role":"user","content":"hello"}]}`,
	))
	request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
		Principal: identity.Principal{Tenant: "tenant-a"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "central") {
		t.Fatalf("status = %d; body = %q", response.Code, response.Body.String())
	}
	if stats := directory.Stats(); stats.Lookups != 0 {
		t.Fatalf("catalog stats = %#v, want no lookup", stats)
	}
}

func TestModelCatalogRoutesSingleVectorEmbedding(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"object":"list",
			"model":"upstream-embedding-model",
			"data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],
			"usage":{"prompt_tokens":2,"total_tokens":2}
		}`)
	}))
	defer upstream.Close()
	entry := gatewayCatalogEntry("tenant-embedding", upstream.URL+"/v1")
	entry.Deployments[0].Model = "upstream-embedding-model"
	entry.Deployments[0].Capabilities = []config.Capability{
		config.CapabilitySingleVectorEmbedding,
	}
	directory := gatewayCatalogDirectory(
		t,
		modelcatalog.NewMemoryResolver(map[modelcatalog.Request]modelcatalog.Entry{
			{TenantID: "tenant-a", RequestedModel: "tenant-embedding"}: entry,
		}),
		upstream,
	)
	handler, err := NewDataHandler(proxyDocument("https://central.example/v1"), DataOptions{
		ModelCatalog: directory,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(`{"model":"tenant-embedding","input":"hello"}`),
	)
	request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
		Principal: identity.Principal{Tenant: "tenant-a"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "tenant-embedding") {
		t.Fatalf("status = %d; body = %q", response.Code, response.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1", got)
	}
}

func TestCatalogRequestedModelRecognizesAllModelBearingIngressShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		method string
		path   string
		body   string
		want   string
	}{
		{http.MethodPost, "/v1/chat/completions", `{"model":"chat-model"}`, "chat-model"},
		{http.MethodPost, "/v1/responses", `{"model":"response-model"}`, "response-model"},
		{http.MethodPost, "/v1/embeddings", `{"model":"embedding-model"}`, "embedding-model"},
		{http.MethodPost, "/v1/messages", `{"model":"anthropic-model"}`, "anthropic-model"},
		{http.MethodPost, "/v1beta/models/gemini-model:generateContent", `{}`, "gemini-model"},
		{http.MethodPost, "/model/bedrock-model/converse", `{}`, "bedrock-model"},
		{http.MethodGet, "/v1/models/listed-model", "", "listed-model"},
	}
	for _, test := range tests {
		request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		model, ok := catalogRequestedModel(request, defaultMaxRequestBytes)
		if !ok || model != test.want {
			t.Errorf("catalogRequestedModel(%q) = %q, %v", test.path, model, ok)
		}
		if test.body != "" && !strings.Contains(test.path, "/models/") &&
			!strings.HasPrefix(test.path, "/model/") {
			replayed, err := io.ReadAll(request.Body)
			if err != nil || string(replayed) != test.body {
				t.Errorf("replayed body for %q = %q, %v", test.path, replayed, err)
			}
		}
	}
}

func TestModelCatalogUnavailableUsesBoundedProtocolError(t *testing.T) {
	t.Parallel()

	directory := gatewayCatalogDirectory(t, unavailableCatalogResolver{}, nil)
	handler, err := NewDataHandler(proxyDocument("https://central.example/v1"), DataOptions{
		ModelCatalog: directory,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"tenant-model","messages":[]}`),
	)
	request = request.WithContext(identity.WithContext(context.Background(), identity.Identity{
		Principal: identity.Principal{Tenant: "tenant-a"},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), "model_catalog_unavailable") ||
		strings.Contains(response.Body.String(), "sensitive") {
		t.Fatalf("status = %d; body = %q", response.Code, response.Body.String())
	}
}

type unavailableCatalogResolver struct{}

func (unavailableCatalogResolver) Resolve(context.Context, modelcatalog.Request) (modelcatalog.Entry, error) {
	return modelcatalog.Entry{}, errors.New("sensitive backend details")
}

func gatewayCatalogDirectory(
	t *testing.T,
	resolver modelcatalog.Resolver,
	upstream *httptest.Server,
) *modelcatalog.Directory {
	t.Helper()
	host := "127.0.0.1"
	if upstream == nil {
		host = "catalog.example"
	}
	directory, err := modelcatalog.NewDirectory(modelcatalog.Options{
		Resolver: resolver,
		SourceID: "test",
		Policy: modelcatalog.Policy{
			AllowedProviderTypes:     []string{"openai_compatible"},
			AllowedEndpointSchemes:   []string{"http", "https"},
			AllowedEndpointHosts:     []string{host},
			AllowedCredentialSchemes: []string{"env"},
			UnknownCapabilityDefault: config.UnknownCapabilityReject,
		},
	})
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	return directory
}

type staticCatalogCredentialSource map[credentials.Ref]string

func (s staticCatalogCredentialSource) Resolve(
	_ context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	value, exists := s[ref]
	if !exists {
		return credentials.Material{}, errors.New("not found")
	}
	return credentials.Material{Value: []byte(value)}, nil
}

func gatewayCatalogEntry(model, baseURL string) modelcatalog.Entry {
	return modelcatalog.Entry{
		SourceID: "test",
		Revision: "revision-1",
		VirtualModel: config.VirtualModel{
			Name: model,
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{Deployment: "catalog-deployment", Weight: 1}},
			}},
		},
		Providers: []config.Provider{{
			Name: "catalog-provider", Type: "openai_compatible", BaseURL: baseURL,
		}},
		Deployments: []config.Deployment{{
			Name: "catalog-deployment", Provider: "catalog-provider", Model: "upstream-model",
		}},
	}
}
