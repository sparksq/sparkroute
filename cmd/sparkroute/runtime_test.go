// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	credentialbuiltin "github.com/sparksq/sparkroute/pkg/credentials/builtin"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
)

func TestRuntimeSlotDrainsRetiredGeneration(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	release := make(chan struct{})
	var closed atomic.Int64
	first := &runtimeGeneration{
		data: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			writer.WriteHeader(http.StatusNoContent)
		}),
		closeFn: func() { closed.Add(1) },
	}
	second := &runtimeGeneration{
		data: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusCreated)
		}),
	}
	slot := newRuntimeSlot(first)
	defer slot.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		response := httptest.NewRecorder()
		slot.dataHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusNoContent {
			t.Errorf("old generation status = %d", response.Code)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("old request did not start")
	}
	slot.replace(second)
	if closed.Load() != 0 {
		t.Fatal("retired generation closed before active request drained")
	}
	response := httptest.NewRecorder()
	slot.dataHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusCreated {
		t.Fatalf("new generation status = %d", response.Code)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("old request did not drain")
	}
	if closed.Load() != 1 {
		t.Fatalf("retired generation close count = %d", closed.Load())
	}
}

func TestValidateRuntimeDocumentAcceptsBuiltinPIIInOSSCommand(t *testing.T) {
	t.Parallel()
	document := config.Document{
		Providers: []config.Provider{{
			Name: "local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "local-default", Provider: "local", Model: "upstream-model",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "local-default",
			Privacy: &config.PrivacyPolicy{PII: &config.PIIPolicy{
				Entities: []config.PIIEntity{config.PIIEntityEmail},
			}},
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
				Deployment: "local-default", Weight: 100,
			}}}},
		}},
	}
	err := validateRuntimeDocument(document, credentialbuiltin.Options{})
	if err != nil {
		t.Fatalf("validateRuntimeDocument() error = %v", err)
	}
}

func TestBuildRuntimeGenerationDisabledSavedTracesDoesNotPanic(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"created":1,
			"model":"upstream-model",
			"choices":[{
				"index":0,
				"message":{"role":"assistant","content":"ready"},
				"finish_reason":"stop"
			}]
		}`)
	}))
	defer upstream.Close()

	document := config.Document{
		Providers: []config.Provider{{
			Name: "local", Type: "openai_compatible", BaseURL: upstream.URL + "/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "local-default", Provider: "local", Model: "upstream-model",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "local-default",
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{
				Deployment: "local-default", Weight: 100,
			}}}},
		}},
	}
	var disabledRecorder *savedtrace.AsyncRecorder
	generation, err := buildRuntimeGeneration(document, "revision-a", runtimeBuildOptions{
		Context:     context.Background(),
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ledger:      ledger.DiscardRecorder{},
		Telemetry:   &otlpexport.Runtime{},
		SavedTraces: disabledRecorder,
	})
	if err != nil {
		t.Fatalf("buildRuntimeGeneration() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"local-default",
			"messages":[{"role":"user","content":"ready?"}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	generation.data.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"response status = %d; body = %q",
			response.Code,
			response.Body.String(),
		)
	}
}

type notifyingManagedStore struct {
	managed.Store
	ready chan struct{}
}

func (s notifyingManagedStore) Watch(ctx context.Context) (<-chan config.Version, error) {
	changes, err := s.Store.Watch(ctx)
	if err == nil {
		close(s.ready)
	}
	return changes, err
}

func TestManagedConfigurationReconcilerAppliesStaticGeneration(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true")
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := configsqlite.Open(context.Background(), configsqlite.Options{
		Path: filepath.Join(directory, "config.sqlite"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Initialize(context.Background(), managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	document, revision, err := store.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	telemetryRuntime, err := otlpexport.FromEnvironment(context.Background(), "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = telemetryRuntime.Shutdown(context.Background()) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options := runtimeBuildOptions{
		Context: ctx, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		CredentialOptions: credentialbuiltin.Options{}, Ledger: ledger.DiscardRecorder{},
		Telemetry: telemetryRuntime,
	}
	initial, err := buildRuntimeGeneration(document, revision, options)
	if err != nil {
		t.Fatal(err)
	}
	slot := newRuntimeSlot(initial)
	defer slot.Close()
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcileManagedConfiguration(
			ctx, notifyingManagedStore{Store: store, ready: ready}, slot, options, options.Logger,
		)
	}()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("configuration watcher did not start")
	}
	next := config.Document{
		Providers:   []config.Provider{{Name: "provider", Type: "openai_compatible", BaseURL: "http://127.0.0.1/v1"}},
		Deployments: []config.Deployment{{Name: "deployment", Provider: "provider", Model: "upstream"}},
		VirtualModels: []config.VirtualModel{{
			Name:  "managed-model",
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "deployment", Weight: 1}}}},
		}},
	}
	result, err := store.ReplaceSet(context.Background(), managed.OwnerOperator, next, managed.ReplaceOptions{
		ExpectedActive: revision, Actor: "operator-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current := slot.current.Load()
		if current != nil && current.revision == result.Current.Version {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if current := slot.current.Load(); current == nil || current.revision != result.Current.Version {
		t.Fatalf("runtime revision = %v, want %s", current, result.Current.Version)
	}
	response := httptest.NewRecorder()
	slot.dataHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "managed-model") {
		t.Fatalf("reloaded model list = %d %s", response.Code, response.Body)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("configuration reconciler did not stop")
	}
}

func TestSparkrunIntegrationIsOptInBeforeAnyDeploymentExists(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		generation, err := buildRuntimeGeneration(managed.EmptyDocument(), "catalog-test", runtimeBuildOptions{
			Context: context.Background(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Telemetry: &otlpexport.Runtime{},
			AdminEnabled: true, AllowInsecureAdmin: true, SparkrunEnabled: enabled, SparkrunCommand: "/does/not/exist",
		})
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		generation.admin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil))
		var bootstrap struct {
			Features map[string]bool `json:"features"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
			t.Fatal(err)
		}
		if bootstrap.Features["sparkrun_catalog"] != enabled {
			t.Fatal("catalog advertisement ignored opt-in", response.Body.String())
		}
		request := httptest.NewRequest(http.MethodPost, "/v1/sparkrun/catalog", strings.NewReader(`{"operation":"catalog_clusters","arguments":{}}`))
		request.Header.Set("Content-Type", "application/json")
		response = httptest.NewRecorder()
		generation.admin.ServeHTTP(response, request)
		expected := http.StatusNotFound
		if enabled {
			expected = http.StatusUnprocessableEntity
		} // registered catalog, missing executable
		if response.Code != expected {
			t.Fatalf("enabled=%t: status=%d body=%s", enabled, response.Code, response.Body.String())
		}
		generation.close()
	}
	document := config.Document{Deployments: []config.Deployment{{Name: "cold", EndpointSource: config.EndpointSource{Controller: "sparkrun", Type: config.EndpointSourceActivatable}}}}
	if err := validateDocumentForIntegration(document, credentialbuiltin.Options{}, false); err == nil || !strings.Contains(err.Error(), "-sparkrun") {
		t.Fatal("disabled integration accepted a workload binding", err)
	}
}
