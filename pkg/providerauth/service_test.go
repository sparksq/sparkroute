// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package providerauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	llmauth "github.com/scitrera/go-llm/auth"
	openaiauth "github.com/scitrera/go-llm/auth/openai"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenStore(t.Context(), filepath.Join(t.TempDir(), "private", "provider-auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStorePersistenceAndProcessExclusion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "auth.db")
	s, err := OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	want := llmauth.Credential{Provider: openaiauth.Provider, AccessToken: "secret-access", RefreshToken: "secret-refresh"}
	if err := s.Save(t.Context(), "codex", want); err != nil {
		t.Fatal(err)
	}
	if second, err := OpenStore(t.Context(), path); err == nil {
		_ = second.Close()
		t.Fatal("allowed concurrent credential owners")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = OpenStore(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(t.Context(), "codex")
	if err != nil || got != want {
		t.Fatal("credentials did not survive restart")
	}
	if err := s.Delete(t.Context(), "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("deleted profile remains available")
	}
	if err := s.Delete(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("missing profile contract")
	}
}

func idToken() string {
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"email":"operator@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"account-one","chatgpt_user_id":"user-one","chatgpt_plan_type":"pro"}}`)) + ".signature"
}

func waitStatus(t *testing.T, s *Service, want string) Status {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := s.Status(t.Context(), "codex", "operator")
		if err != nil {
			t.Fatal(err)
		}
		if status.State == want {
			return status
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sign-in did not reach %s", want)
	return Status{}
}

func TestDeviceLoginStatusLogoutAndCancellation(t *testing.T) {
	s := testStore(t)
	var approve atomic.Bool
	var revoked atomic.Bool
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = w.Write([]byte(`{"device_auth_id":"never-expose-device-id","user_code":"ABCD-EFGH","interval":"0.001"}`))
		case "/api/accounts/deviceauth/token":
			if !approve.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"authorization_code":"private-code","code_verifier":"private-verifier"}`))
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "secret-access", "refresh_token": "secret-refresh", "id_token": idToken(), "expires_in": 3600})
		case "/oauth/revoke":
			revoked.Store(true)
		default:
			t.Errorf("unexpected issuer request: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer issuer.Close()
	service := New(t.Context(), s, openaiauth.Config{IssuerURL: issuer.URL})
	defer service.Close()
	if _, err := service.Begin(t.Context(), "codex", "operator"); err != nil {
		t.Fatal(err)
	}
	status := waitStatus(t, service, "pending")
	if status.UserCode != "ABCD-EFGH" || status.LoginExpiresAt == nil {
		t.Fatal("missing login guidance")
	}
	other, err := service.Status(t.Context(), "codex", "other-operator")
	if err != nil || other.UserCode != "" || other.VerificationURL != "" {
		t.Fatal("login code exposed to another administrator")
	}
	if _, err := service.Begin(t.Context(), "codex", "other-operator"); err == nil {
		t.Fatal("parallel login allowed")
	}
	if err := service.Cancel(t.Context(), "codex", "other-operator", false); err == nil {
		t.Fatal("another administrator cancelled the session")
	}
	if err := service.Cancel(t.Context(), "codex", "operator", false); err != nil {
		t.Fatal(err)
	}
	approve.Store(true)
	if _, err := s.Load(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("cancelled login saved credentials")
	}
	if _, err := service.Begin(t.Context(), "codex", "operator"); err != nil {
		t.Fatal(err)
	}
	status = waitStatus(t, service, "connected")
	if status.Email != "operator@example.test" || status.Plan != "pro" || status.AccountID != "account-one" {
		t.Fatal("account status missing")
	}
	raw, _ := json.Marshal(status)
	for _, secret := range []string{"secret-access", "secret-refresh", "never-expose-device-id", idToken(), "access_token", "refresh_token", "id_token", "ABCD-EFGH"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("status contains secret material")
		}
	}
	if _, err := service.Begin(t.Context(), "codex", "operator"); err == nil {
		t.Fatal("login replaced an existing account")
	}
	if err := service.Cancel(t.Context(), "codex", "operator", true); err != nil {
		t.Fatal(err)
	}
	if !revoked.Load() {
		t.Fatal("logout did not attempt revocation")
	}
	if _, err := s.Load(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("logout retained credentials")
	}
}

func TestConcurrentRefreshAndRevocationFailure(t *testing.T) {
	s := testStore(t)
	var refreshes atomic.Int64
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/revoke" {
			http.Error(w, "secret-upstream-message", http.StatusServiceUnavailable)
			return
		}
		refreshes.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"rotated-refresh","expires_in":3600}`))
	}))
	defer issuer.Close()
	service := New(t.Context(), s, openaiauth.Config{IssuerURL: issuer.URL})
	defer service.Close()
	if err := s.Save(t.Context(), "codex", llmauth.Credential{Provider: openaiauth.Provider, AccessToken: "expired", RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
			if err := service.Apply(t.Context(), "codex", r); err != nil {
				t.Error(err)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("originator") != "sparkroute" {
				t.Error("incorrect subscription headers")
			}
		})
	}
	workers.Wait()
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d", refreshes.Load())
	}
	err := service.Cancel(t.Context(), "codex", "operator", true)
	if err == nil || strings.Contains(err.Error(), "secret-upstream-message") {
		t.Fatal("revocation failure was not sanitized")
	}
	if _, err := s.Load(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("failed remote revocation retained local credentials")
	}
}

func TestLoginErrorsAndServiceClose(t *testing.T) {
	s := testStore(t)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "private-error-detail", http.StatusServiceUnavailable)
	}))
	defer issuer.Close()
	service := New(t.Context(), s, openaiauth.Config{IssuerURL: issuer.URL})
	if _, err := service.Begin(t.Context(), "codex", "operator"); err != nil {
		t.Fatal(err)
	}
	status := waitStatus(t, service, "failed")
	if strings.Contains(status.Message, "private-error-detail") || !strings.Contains(status.Message, "device-code") {
		t.Fatal("unsafe or unhelpful sign-in failure")
	}
	service.Close()
	if _, err := service.Begin(context.Background(), "codex", "operator"); err == nil {
		t.Fatal("closed service accepted login")
	}
}

type completingStore struct {
	*Store
	entered chan struct{}
	release chan struct{}
}

func (s *completingStore) Save(_ context.Context, name string, credential llmauth.Credential) error {
	close(s.entered)
	<-s.release
	// Model a storage commit that has already crossed its cancellation point.
	return s.Store.Save(context.Background(), name, credential)
}

func TestLogoutJoinsLoginAlreadyCommitting(t *testing.T) {
	store := &completingStore{Store: testStore(t), entered: make(chan struct{}), release: make(chan struct{})}
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			_, _ = w.Write([]byte(`{"device_auth_id":"device","user_code":"ABCD-EFGH"}`))
		case "/api/accounts/deviceauth/token":
			_, _ = w.Write([]byte(`{"authorization_code":"code","code_verifier":"verifier"}`))
		case "/oauth/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access", "refresh_token": "refresh", "id_token": idToken(), "expires_in": 3600})
		}
	}))
	defer issuer.Close()
	service := New(t.Context(), store, openaiauth.Config{IssuerURL: issuer.URL})
	defer service.Close()
	var release sync.Once
	defer release.Do(func() { close(store.release) })
	if _, err := service.Begin(t.Context(), "codex", "operator"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-store.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("login never reached persistence")
	}
	done := make(chan error, 1)
	go func() { done <- service.Cancel(t.Context(), "codex", "operator", true) }()
	select {
	case err := <-done:
		t.Fatalf("logout returned before login worker: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	release.Do(func() { close(store.release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("logout did not finish")
	}
	if _, err := store.Load(t.Context(), "codex"); !errors.Is(err, llmauth.ErrCredentialNotFound) {
		t.Fatal("login recreated credentials after logout")
	}
}
