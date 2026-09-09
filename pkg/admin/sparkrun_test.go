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
