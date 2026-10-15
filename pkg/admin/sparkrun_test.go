// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	"github.com/sparksq/sparkroute/pkg/sparkrun"
)

type adminCatalog struct {
	revision string
	calls    []string
}

func (f *adminCatalog) Catalog(_ context.Context, operation string, _ map[string]any, result any) error {
	f.calls = append(f.calls, operation)
	var value any
	switch operation {
	case "catalog_resolve":
		value = sparkrun.RecipeDetails{Reference: "catalog:selection", Revision: f.revision, Model: "test/model", NativeProtocols: []config.Protocol{config.ProtocolOpenAI}}
	case "catalog_clusters":
		value = map[string]any{"clusters": []sparkrun.Cluster{{Name: "lab", HostCount: 2, Default: true}}}
	case "catalog_retain":
		value = map[string]bool{"retained": true}
	default:
		value = map[string]any{"recipes": []any{}, "total": 0}
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, result)
}

func TestRecipeUIFlowValidatesAndSavesWithoutLaunching(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := configsqlite.Open(context.Background(), configsqlite.Options{Path: filepath.Join(directory, "config.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	empty := managed.EmptyDocument()
	_, _ = store.Initialize(context.Background(), empty, "test", "")
	_, revision, _ := store.Load(context.Background())
	catalog := &adminCatalog{revision: "recipe-revision"}
	handler := NewHandler(empty, revision, Options{ManagedConfig: store, SparkrunCatalog: catalog, AllowInsecureAdmin: true})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	draft := call("POST", "/v1/sparkrun/recipe-draft", map[string]any{"document": empty, "expected_active_revision": revision, "recipe": map[string]any{
		"name": "coding", "aliases": []string{"code"}, "reference": "catalog:selection", "recipe_revision": catalog.revision, "cluster": "lab",
	}})
	if draft.Code != 200 {
		t.Fatal(draft.Code, draft.Body.String())
	}
	var prepared struct {
		Document config.Document `json:"document"`
	}
	_ = json.Unmarshal(draft.Body.Bytes(), &prepared)
	// Keep Default empty while normal configuration writes update Work.
	presets, _ := store.ListPresets(context.Background())
	copyResponse := call("POST", "/v1/config/presets/save", map[string]any{"name": "Work", "expected_active_revision": revision, "expected_presets_revision": presets.PresetsRevision})
	var work managed.PresetMetadata
	if copyResponse.Code != 200 || json.Unmarshal(copyResponse.Body.Bytes(), &work) != nil {
		t.Fatal(copyResponse.Code, copyResponse.Body.String())
	}
	mutation := map[string]any{"document": prepared.Document, "expected_active_revision": revision}
	if response := call("POST", "/v1/config/managed-sets/operator/validate", mutation); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	// A changed file between Validate and Save must preserve the stored set.
	catalog.revision = "changed"
	if response := call("PUT", "/v1/config/managed-sets/operator", mutation); response.Code != 422 {
		t.Fatal(response.Code, response.Body.String())
	} else {
		var body struct {
			Error struct{ Code, Deployment string }
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code != "recipe_revision_changed" || body.Error.Deployment != prepared.Document.Deployments[0].Name {
			t.Fatal(response.Body.String(), err)
		}
	}
	_, still, _ := store.Load(context.Background())
	if still != revision {
		t.Fatal("failed save changed configuration")
	}
	catalog.revision = "recipe-revision"
	if response := call("PUT", "/v1/config/managed-sets/operator", mutation); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	saved, current, _ := store.Load(context.Background())
	if _, ok := saved.CanonicalModel("code"); !ok {
		t.Fatal("saved alias missing")
	}
	if current == revision {
		t.Fatal("configuration did not change")
	}
	if response := call("PUT", "/v1/config/managed-sets/operator", mutation); response.Code != 409 {
		t.Fatal("stale save accepted", response.Body.String())
	}
	activatePreset := func(id string) *httptest.ResponseRecorder {
		presets, _ := store.ListPresets(context.Background())
		return call("POST", "/v1/config/presets/activate", map[string]any{"id": id, "expected_active_revision": presets.ActiveRevision, "expected_presets_revision": presets.PresetsRevision})
	}
	if response := activatePreset("default"); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	catalog.revision = "changed"
	if response := activatePreset(work.ID); response.Code != 422 {
		t.Fatal("stale recipe activated", response.Code, response.Body.String())
	}
	presets, _ = store.ListPresets(context.Background())
	if presets.ActivePreset != "default" {
		t.Fatal("failed recipe activation changed selection")
	}
	catalog.revision = "recipe-revision"
	if response := activatePreset(work.ID); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, operation := range catalog.calls {
		if operation == "ensure_ready" || operation == "stop" {
			t.Fatal("configuration changed a workload")
		}
	}
}

func TestCatalogReadRoleCannotImportRefreshOrInvokeLifecycle(t *testing.T) {
	catalog := &adminCatalog{}
	handler := NewHandler(config.Document{}, "revision", Options{SparkrunCatalog: catalog, Authenticator: metadataTestAuthenticator{}})
	for _, operation := range []string{"catalog_import", "catalog_refresh", "ensure_ready"} {
		raw, _ := json.Marshal(map[string]any{"operation": operation, "arguments": map[string]any{}})
		request := httptest.NewRequest(http.MethodPost, "/v1/sparkrun/catalog", bytes.NewReader(raw))
		request.Header.Set("Authorization", "Bearer metadata-token")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != 403 && response.Code != 400 {
			t.Fatal(operation, response.Code)
		}
	}
	if len(catalog.calls) != 0 {
		t.Fatal("forbidden operation reached bridge")
	}
}

type adminWorkloadControl struct {
	deployment, job, action string
	calls                   int
}

func (c *adminWorkloadControl) WorkloadAction(_ context.Context, deployment, job, action string) (sparkrun.WorkloadInfo, error) {
	c.deployment, c.job, c.action = deployment, job, action
	c.calls++
	return sparkrun.WorkloadInfo{State: "ready"}, nil
}

func TestWorkloadStartStopAPIRequiresConfigWrite(t *testing.T) {
	for _, action := range []string{"start", "stop"} {
		for _, readOnly := range []bool{false, true} {
			control := &adminWorkloadControl{}
			options := Options{SparkrunControl: control, AllowInsecureAdmin: !readOnly}
			if readOnly {
				options.Authenticator = metadataTestAuthenticator{}
			}
			handler := NewHandler(config.Document{}, "revision", options)
			job := "job"
			if action == "start" {
				job = ""
			}
			raw, _ := json.Marshal(map[string]string{"deployment": "recipe", "job_id": job, "action": action})
			request := httptest.NewRequest(http.MethodPost, "/v1/sparkrun/workload", bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer metadata-token")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if readOnly {
				if response.Code != http.StatusForbidden || control.calls != 0 {
					t.Fatal("read-only caller changed a workload", action, response.Code)
				}
			} else if response.Code != http.StatusOK || control.calls != 1 || control.deployment != "recipe" || control.job != job || control.action != action {
				t.Fatal(action, response.Code, response.Body.String(), control)
			}
		}
	}
}

func TestRoutingEditsAndPresetsDoNotResolveUnchangedRecipes(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := configsqlite.Open(ctx, configsqlite.Options{Path: filepath.Join(directory, "config.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	catalog := &adminCatalog{revision: "saved"}
	document, _, _, err := sparkrun.PrepareRecipeDraft(ctx, catalog, managed.EmptyDocument(), managed.EmptyDocument(), sparkrun.RecipeDraft{Reference: "catalog:selection", RecipeRevision: "saved", Name: "coding", Cluster: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Initialize(ctx, document, "test", ""); err != nil {
		t.Fatal(err)
	}
	_, revision, _ := store.Load(ctx)
	// Retain a preset with identical bindings but different routing.
	presets, _ := store.ListPresets(ctx)
	work, err := store.SavePreset(ctx, "Work", managed.ReplaceOptions{ExpectedActive: revision, ExpectedPresetsRevision: &presets.PresetsRevision, Actor: "test"})
	if err != nil {
		t.Fatal(err)
	}
	// The bridge could be unavailable or its recipe removed. Neither should be
	// consulted by Validate, Save (including retention), or a routing-only preset.
	handler := NewHandler(document, revision, Options{ManagedConfig: store, AllowInsecureAdmin: true})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	mutation := func() map[string]any {
		return map[string]any{"document": document, "expected_active_revision": revision}
	}
	if response := call("POST", "/v1/config/managed-sets/operator/validate", mutation()); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	// Add an independently discovered target without changing the saved pin.
	document.Deployments = append(document.Deployments, config.Deployment{Name: "discovered", Provider: "sparkrun", Model: "test/model", EndpointSource: config.EndpointSource{Type: config.EndpointSourceDiscovered, Controller: "sparkrun"}})
	document.VirtualModels[0].Aliases = []string{"code"}
	document.VirtualModels[0].Pools = append(document.VirtualModels[0].Pools, config.RoutingPool{Priority: 1, Targets: []config.WeightedTarget{{Deployment: "discovered", Weight: 1}}})
	preferRunning := true
	document.VirtualModels[0].Selection.PreferRunning = &preferRunning
	for _, method := range []string{"POST", "PUT"} {
		path := "/v1/config/managed-sets/operator"
		if method == "POST" {
			path += "/validate"
		}
		if response := call(method, path, mutation()); response.Code != 200 {
			t.Fatal(method, response.Code, response.Body.String())
		}
	}
	saved, current, _ := store.Load(ctx)
	if saved.Deployments[0].EndpointSource.RecipeRevision != "saved" {
		t.Fatal("routing save advanced pin")
	}
	if response := call("PUT", "/v1/config/managed-sets/operator", mutation()); response.Code != 409 {
		t.Fatal("stale revision accepted", response.Code)
	}
	revision = current
	for _, id := range []string{"default", work.ID} {
		presets, _ = store.ListPresets(ctx)
		response := call("POST", "/v1/config/presets/activate", map[string]any{"id": id, "expected_active_revision": presets.ActiveRevision, "expected_presets_revision": presets.PresetsRevision})
		if response.Code != 200 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
}

// Simulate a generated inventory update between the baseline read and candidate
// construction. The request must not use an unversioned baseline to skip checks.
type changingBaselineStore struct {
	managed.Store
	change func()
}

func (s *changingBaselineStore) Load(ctx context.Context) (config.Document, config.Version, error) {
	document, revision, err := s.Store.Load(ctx)
	if s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return document, revision, err
}
func TestRecipeBaselineUsesExpectedRevisionAcrossConcurrentUpdate(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := configsqlite.Open(ctx, configsqlite.Options{Path: filepath.Join(directory, "config.sqlite")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	empty := managed.EmptyDocument()
	_, _ = store.Initialize(ctx, empty, "test", "")
	_, revision, _ := store.Load(ctx)
	changing := &changingBaselineStore{Store: store, change: func() {
		replacement := managed.EmptyDocument()
		replacement.Providers = []config.Provider{{Name: "other", Type: "openai", BaseURL: "https://api.example.test/v1"}}
		if _, err := store.ReplaceSet(ctx, managed.OwnerOperator, replacement, managed.ReplaceOptions{ExpectedActive: revision, Actor: "concurrent"}); err != nil {
			t.Fatal(err)
		}
	}}
	h := NewHandler(empty, revision, Options{ManagedConfig: changing, AllowInsecureAdmin: true})
	raw, _ := json.Marshal(map[string]any{"document": empty, "expected_active_revision": revision})
	request := httptest.NewRequest("POST", "/v1/config/managed-sets/operator/validate", bytes.NewReader(raw))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != 409 {
		t.Fatal(response.Code, response.Body.String())
	}
	saved, _, _ := store.Load(ctx)
	if len(saved.Providers) != 1 || saved.Providers[0].Name != "other" {
		t.Fatal("concurrent update lost")
	}
}
