// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	openaiauth "github.com/scitrera/go-llm/auth/openai"
	"github.com/sparksq/sparkroute/pkg/adminapi"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/providerauth"
)

type providerTestAuthenticator struct{}

func (providerTestAuthenticator) Authenticate(_ context.Context, r *http.Request) (identity.Principal, error) {
	switch r.Header.Get("Authorization") {
	case "Bearer writer":
		return identity.Principal{ID: "operator", Roles: []string{RoleConfigWrite}}, nil
	case "Bearer reader":
		return identity.Principal{ID: "reader", Roles: []string{RoleConfigRead}}, nil
	default:
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
}

func TestProviderAuthAuthorizationAndRequestBoundaries(t *testing.T) {
	store, err := providerauth.OpenStore(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	service := providerauth.New(t.Context(), store, openaiauth.Config{})
	defer service.Close()
	h := NewHandler(config.Document{}, "initial", Options{ProviderAuth: service, Authenticator: providerTestAuthenticator{}})
	for _, tt := range []struct {
		name, method, path, token, origin, content, body string
		want                                             int
	}{
		{name: "anonymous", method: "GET", path: "/v1/provider-auth/openai/codex", want: 401},
		{name: "read-only", method: "GET", path: "/v1/provider-auth/openai/codex", token: "reader", want: 403},
		{name: "writer status", method: "GET", path: "/v1/provider-auth/openai/codex", token: "writer", want: 200},
		{name: "query secrets", method: "GET", path: "/v1/provider-auth/openai/codex?token=value", token: "writer", want: 400},
		{name: "cross origin", method: "POST", path: "/v1/provider-auth/openai/codex/login", token: "writer", origin: "https://other.example", content: "application/json", body: "{}", want: 403},
		{name: "simple form", method: "POST", path: "/v1/provider-auth/openai/codex/login", token: "writer", content: "text/plain", body: "{}", want: 415},
		{name: "unknown field", method: "POST", path: "/v1/provider-auth/openai/codex/login", token: "writer", content: "application/json", body: `{"issuer":"https://other.example"}`, want: 400},
		{name: "profile path", method: "GET", path: "/v1/provider-auth/openai/invalid.profile", token: "writer", want: 404},
		{name: "sign out absent", method: "POST", path: "/v1/provider-auth/openai/codex/logout", token: "writer", content: "application/json", body: "{}", want: 200},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(tt.method, "http://localhost"+tt.path, strings.NewReader(tt.body))
			if tt.token != "" {
				r.Header.Set("Authorization", "Bearer "+tt.token)
			}
			r.Header.Set("Origin", tt.origin)
			r.Header.Set("Content-Type", tt.content)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want {
				t.Fatalf("status = %d: %s", w.Code, w.Body)
			}
			if tt.want == 200 && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("credential status may be cached")
			}
		})
	}
	if !adminapi.IsPath("/v1/provider-auth/openai/codex") || adminapi.IsPath("/v1/provider-auth-other") {
		t.Fatal("incorrect admin routing boundary")
	}
	for _, token := range []string{"reader", "writer"} {
		r := httptest.NewRequest("GET", "/v1/ui/bootstrap", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		want := `"provider_auth":` + map[string]string{"reader": "false", "writer": "true"}[token]
		if !strings.Contains(w.Body.String(), want) {
			t.Fatalf("bootstrap omitted role-aware feature: %s", w.Body)
		}
	}
}
