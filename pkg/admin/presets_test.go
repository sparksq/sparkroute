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

	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
)

func TestPresetAPIManagementAndReadOnlyPermissions(t *testing.T) {
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
	if _, err := store.Initialize(ctx, managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	doc, revision, _ := store.Load(ctx)
	handler := NewHandler(doc, revision, Options{ManagedConfig: store, AllowInsecureAdmin: true})
	call := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer metadata-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	body := func(id, name string) map[string]any {
		catalog, err := store.ListPresets(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"id": id, "name": name, "expected_active_revision": catalog.ActiveRevision, "expected_presets_revision": catalog.PresetsRevision}
	}
	stale := body("", "Work")
	response := call("POST", "/v1/config/presets/save", stale)
	var work managed.PresetMetadata
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &work) != nil {
		t.Fatal(response.Code, response.Body.String())
	}
	if response := call("POST", "/v1/config/presets/save", stale); response.Code != 409 {
		t.Fatal("stale copy accepted", response.Code, response.Body.String())
	}
	// A selected preset change with an identical document also fences normal PUTs.
	stale["document"] = managed.EmptyDocument()
	delete(stale, "id")
	delete(stale, "name")
	if response := call("PUT", "/v1/config/managed-sets/operator", stale); response.Code != 409 {
		t.Fatal("stale operator edit accepted", response.Code, response.Body.String())
	}
	for _, item := range []struct {
		operation, id, name string
		code                int
	}{
		{"rename", work.ID, "Office", 200}, {"delete", work.ID, "", 400},
		{"activate", "default", "", 200}, {"delete", work.ID, "", 200},
		{"activate", work.ID, "", 404}, {"rename", "default", "Changed", 400},
	} {
		if response := call("POST", "/v1/config/presets/"+item.operation, body(item.id, item.name)); response.Code != item.code {
			t.Fatal(item.operation, response.Code, response.Body.String())
		}
	}
	if response := call("POST", "/v1/config/presets/save", map[string]any{"name": "Missing revisions"}); response.Code != 400 {
		t.Fatal(response.Code, response.Body.String())
	}
	handler = NewHandler(doc, revision, Options{ManagedConfig: store, Authenticator: metadataTestAuthenticator{}})
	if response := call("GET", "/v1/config/presets", nil); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	for _, operation := range []string{"save", "activate", "rename", "delete"} {
		if response := call(http.MethodPost, "/v1/config/presets/"+operation, body("default", "Work")); response.Code != 403 {
			t.Fatal("read-only mutation", operation, response.Code, response.Body.String())
		}
	}
}
