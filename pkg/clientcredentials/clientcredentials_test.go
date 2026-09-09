// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package clientcredentials_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	clientcredentialsqlite "github.com/sparksq/sparkroute/pkg/clientcredentials/sqlite"
	"github.com/sparksq/sparkroute/pkg/identity"
)

func TestCredentialLifecycleAndAudit(t *testing.T) {
	t.Parallel()
	manager, store := newManager(t)
	issued, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name:             "training worker",
		PrincipalID:      "worker-a",
		PrincipalType:    "machine",
		PrincipalSubject: "service-account:worker-a",
		Roles:            []string{clientcredentials.RoleInference, "trace_ingest"},
		FixedAttribution: map[string]string{"source": "training"},
	}, "operator-a")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if !strings.HasPrefix(issued.APIKey, "llmgw_v1.cc_") || issued.Credential.TenantID != "" {
		t.Fatalf("issued credential = %#v", issued)
	}
	principal := authenticate(t, manager, issued.APIKey, clientcredentials.RoleInference)
	if principal.ID != "worker-a" || principal.Tenant != "" || principal.FixedAttribution["source"] != "training" {
		t.Fatalf("principal = %#v", principal)
	}

	page, err := manager.List(context.Background(), clientcredentials.ListQuery{})
	if err != nil || len(page.Credentials) != 1 {
		t.Fatalf("List() = %#v, %v", page, err)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), issued.APIKey) || strings.Contains(string(raw), "secret_sha256") {
		t.Fatalf("list leaked secret material: %s", raw)
	}

	rotated, err := manager.Rotate(context.Background(), issued.Credential.ID, "operator-a")
	if err != nil || rotated.APIKey == issued.APIKey || rotated.Credential.RotatedAt == nil {
		t.Fatalf("Rotate() = %#v, %v", rotated, err)
	}
	if _, err := manager.AuthenticateBearer(context.Background(), issued.APIKey); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("old key authentication error = %v", err)
	}
	authenticate(t, manager, rotated.APIKey, clientcredentials.RoleInference)

	if _, err := manager.SetState(context.Background(), issued.Credential.ID, clientcredentials.StateDisabled, "operator-a"); err != nil {
		t.Fatalf("disable error = %v", err)
	}
	if _, err := manager.AuthenticateBearer(context.Background(), rotated.APIKey); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("disabled authentication error = %v", err)
	}
	if _, err := manager.SetState(context.Background(), issued.Credential.ID, clientcredentials.StateActive, "operator-a"); err != nil {
		t.Fatalf("enable error = %v", err)
	}
	authenticate(t, manager, rotated.APIKey, clientcredentials.RoleInference)
	if _, err := manager.SetState(context.Background(), issued.Credential.ID, clientcredentials.StateRevoked, "operator-b"); err != nil {
		t.Fatalf("revoke error = %v", err)
	}
	if _, err := manager.SetState(context.Background(), issued.Credential.ID, clientcredentials.StateActive, "operator-b"); !errors.Is(err, clientcredentials.ErrRevoked) {
		t.Fatalf("revoked enable error = %v", err)
	}

	audit, err := manager.ListAudit(context.Background(), clientcredentials.AuditQuery{})
	if err != nil || len(audit.Events) != 5 {
		t.Fatalf("ListAudit() = %#v, %v", audit, err)
	}
	if audit.Events[0].Action != "revoked" || audit.Events[len(audit.Events)-1].Action != "created" {
		t.Fatalf("audit order = %#v", audit.Events)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestExpirationAndRequiredRoleFailClosed(t *testing.T) {
	t.Parallel()
	manager, _ := newManager(t)
	expires := time.Now().Add(50 * time.Millisecond).UTC()
	issued, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "short lived", PrincipalID: "reader-a", Roles: []string{clientcredentials.RoleRead},
		ExpiresAt: &expires,
	}, "operator-a")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+issued.APIKey)
	_, err = (clientcredentials.Authenticator{
		Manager: manager, RequiredRole: clientcredentials.RoleInference,
	}).Authenticate(context.Background(), request)
	if !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("role enforcement error = %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	if _, err := manager.AuthenticateBearer(context.Background(), issued.APIKey); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("expired authentication error = %v", err)
	}
}

func TestStandaloneDataMiddlewareInstallsPrincipalAndStripsIdentityHeaders(t *testing.T) {
	t.Parallel()
	manager, _ := newManager(t)
	issued, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "caller", PrincipalID: "caller-a", Roles: []string{clientcredentials.RoleInference},
	}, "operator-a")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	var captured identity.Identity
	var capturedHeader http.Header
	handler := clientcredentials.WrapDataPlane(clientcredentials.Authenticator{
		Manager: manager, RequiredRole: clientcredentials.RoleInference,
	}, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		captured, _ = identity.FromContext(request.Context())
		capturedHeader = request.Header.Clone()
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+issued.APIKey)
	request.Header.Set("X-SparkRoute-Tenant", "untrusted")
	request.Header.Set("X-SparkRoute-Thread-Id", "thread-a")
	request.Header.Set("X-Auth-User", "untrusted")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || captured.Principal.ID != "caller-a" ||
		captured.Attribution[identity.AttributeThreadID] != "thread-a" {
		t.Fatalf("middleware result = %d %#v", response.Code, captured)
	}
	if capturedHeader.Get("Authorization") != "" ||
		capturedHeader.Get("X-SparkRoute-Tenant") != "" ||
		capturedHeader.Get("X-SparkRoute-Thread-Id") != "" ||
		capturedHeader.Get("X-Auth-User") != "" {
		t.Fatalf("sensitive headers survived: %#v", capturedHeader)
	}
}

func TestAdminHandlerSingleAndMultiTenantScopes(t *testing.T) {
	t.Parallel()
	manager, _ := newManager(t)
	oss, err := clientcredentials.NewAdminHandler(clientcredentials.AdminOptions{Manager: manager})
	if err != nil {
		t.Fatalf("NewAdminHandler() error = %v", err)
	}
	response := serveAdmin(oss, identity.Principal{
		ID: "operator-a", Roles: []string{clientcredentials.RoleWrite},
	}, http.MethodPost, "/v1/client-credentials", `{
		"name":"bad tenant","tenant_id":"tenant-a","principal_id":"worker-a","roles":["inference"]
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("standalone tenant response = %d %s", response.Code, response.Body)
	}

	multi, err := clientcredentials.NewAdminHandler(clientcredentials.AdminOptions{
		Manager: manager, MultiTenant: true,
	})
	if err != nil {
		t.Fatalf("NewAdminHandler() error = %v", err)
	}
	response = serveAdmin(multi, identity.Principal{
		ID: "tenant-operator", Tenant: "tenant-a", Roles: []string{clientcredentials.RoleWrite},
	}, http.MethodPost, "/v1/client-credentials", `{
		"name":"worker","principal_id":"worker-a","roles":["inference"]
	}`)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"tenant_id":"tenant-a"`) {
		t.Fatalf("tenant create response = %d %s", response.Code, response.Body)
	}
	response = serveAdmin(multi, identity.Principal{
		ID: "global-operator", Roles: []string{clientcredentials.RoleWrite},
	}, http.MethodPost, "/v1/client-credentials", `{
		"name":"missing tenant","principal_id":"worker-b","roles":["inference"]
	}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("global missing tenant response = %d %s", response.Code, response.Body)
	}
}

func newManager(t *testing.T) (*clientcredentials.Manager, *clientcredentialsqlite.Store) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	store, err := clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{
		Path: filepath.Join(directory, "credentials.sqlite"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := clientcredentials.NewManager(store)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	return manager, store
}

func authenticate(
	t *testing.T,
	manager *clientcredentials.Manager,
	apiKey string,
	role string,
) identity.Principal {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	principal, err := (clientcredentials.Authenticator{
		Manager: manager, RequiredRole: role,
	}).Authenticate(context.Background(), request)
	if err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	return principal
}

func serveAdmin(
	handler http.Handler,
	principal identity.Principal,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	request = request.Clone(identity.WithContext(
		request.Context(), identity.Identity{Principal: principal},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
