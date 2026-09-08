package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	credentialbuiltin "github.com/sparksq/sparkroute/pkg/credentials/builtin"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
)

func TestMMProjectionGenerationEnableUpdateDisableAndInherit(t *testing.T) {
	t.Setenv("TEST_MMBRIDGE_A", "token-a")
	t.Setenv("TEST_MMBRIDGE_B", "token-b")
	bridge := func(token string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("bridge received wrong credential")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/v1/models":
				io.WriteString(w, `{"data":[{"id":"vision"}]}`)
			case "/v1/mmprojection/capabilities":
				io.WriteString(w, `{"name":"pilco-mmbridge","projection_api":1,"endpoints":["/v1/chat/completions","/v1/responses"]}`)
			default:
				t.Errorf("unexpected bridge path: %s", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}
	a, b := bridge("token-a"), bridge("token-b")
	defer a.Close()
	defer b.Close()
	startupDoc := managed.EmptyDocument()
	startupDoc.MMProjection = &config.MMProjectionConfig{Enabled: true, URL: a.URL + "/v1", TokenRef: "env://TEST_MMBRIDGE_A", AnalyzerModel: "startup"}
	registry, err := credentialbuiltin.NewRegistry(startupDoc, credentialbuiltin.Options{})
	if err != nil {
		t.Fatal(err)
	}
	options := runtimeBuildOptions{Context: context.Background(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: &otlpexport.Runtime{}, AdminEnabled: true, AllowInsecureAdmin: true}
	closeStartup, err := configureGenerationMMProjection(startupDoc, &options, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer closeStartup()
	options.MMProjectionCredentials = registry
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"test","object":"chat.completion","model":"vision","choices":[{"index":0,"message":{"role":"assistant","content":"analyzed"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	build := func(setting *config.MMProjectionConfig) *runtimeGeneration {
		document := managed.EmptyDocument()
		document.MMProjection = setting
		document.Providers = []config.Provider{{Name: "provider", Type: "openai", BaseURL: upstream.URL + "/v1"}}
		document.Deployments = []config.Deployment{{Name: "vision", Provider: "provider", Model: "vision"}}
		for _, name := range []string{"first", "second", "startup"} {
			document.VirtualModels = append(document.VirtualModels, config.VirtualModel{Name: name, Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "vision", Weight: 100}}}}})
		}
		g, err := buildRuntimeGeneration(document, "test", options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(g.close)
		return g
	}
	probe := func(g *runtimeGeneration, model string) {
		r := httptest.NewRecorder()
		g.admin.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/v1/mm-projection/probe", nil))
		var status mmprojection.Status
		if err := json.Unmarshal(r.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if r.Code != 200 || status.State != "healthy" || status.AnalyzerModel != model {
			t.Fatalf("probe = %d %s", r.Code, r.Body.String())
		}
	}
	first := build(&config.MMProjectionConfig{Enabled: true, URL: a.URL + "/v1", TokenRef: "env://TEST_MMBRIDGE_A", AnalyzerModel: "first"})
	probe(first, "first")
	second := build(&config.MMProjectionConfig{Enabled: true, URL: b.URL + "/v1", TokenRef: "env://TEST_MMBRIDGE_B", AnalyzerModel: "second", TimeoutMS: 120000})
	probe(second, "second")
	probe(first, "first") // Overlapping generations retain their URL, token, and analyzer.
	disabled := build(&config.MMProjectionConfig{})
	r := httptest.NewRecorder()
	disabled.admin.ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var state struct {
		MMProjection mmprojection.Status `json:"mm_projection"`
	}
	if err := json.Unmarshal(r.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.MMProjection.Configured || state.MMProjection.State != "disabled" {
		t.Fatalf("disabled status = %+v", state.MMProjection)
	}
	probe(build(nil), "startup") // Removing the override restores startup settings.

	// Analyzer callbacks authenticate against the current generation's token.
	for _, tc := range []struct {
		g            *runtimeGeneration
		token, model string
		code         int
	}{
		{first, "token-a", "first", http.StatusOK},
		{second, "token-b", "second", http.StatusOK},
		{first, "token-a", "second", http.StatusNotFound},
		{second, "token-a", "second", http.StatusUnauthorized},
		{second, "token-b", "first", http.StatusNotFound},
		{disabled, "token-b", "second", http.StatusNotFound},
	} {
		r := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, mmprojection.AnalyzerChatPath, strings.NewReader(`{"model":"`+tc.model+`","messages":[{"role":"user","content":"test"}]}`))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Content-Type", "application/json")
		tc.g.data.ServeHTTP(r, req)
		if r.Code != tc.code {
			t.Fatalf("callback status=%d, want=%d: %s", r.Code, tc.code, r.Body.String())
		}
	}
}

func TestMMProjectionRejectsUnconfiguredCredentialSchemeBeforePublish(t *testing.T) {
	doc := managed.EmptyDocument()
	doc.MMProjection = &config.MMProjectionConfig{Enabled: true, URL: "http://localhost:8100/v1", TokenRef: "missing://bridge"}
	if err := validateDocumentForIntegration(doc, credentialbuiltin.Options{}, false); err == nil {
		t.Fatal("accepted unavailable credential scheme")
	}
	doc.MMProjection.Enabled = false
	if err := validateDocumentForIntegration(doc, credentialbuiltin.Options{}, false); err != nil {
		t.Fatal(err)
	}
}
