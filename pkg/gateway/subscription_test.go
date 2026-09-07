// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	llmauth "github.com/scitrera/go-llm/auth"
	openaiauth "github.com/scitrera/go-llm/auth/openai"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/providerauth"
	"github.com/sparksq/sparkroute/pkg/routing"
)

func TestSubscriptionRequestPreparation(t *testing.T) {
	body, err := prepareSubscriptionBody([]byte(`{"model":"codex-model","input":"hello","instructions":"Follow the user's instructions.","reasoning":{"effort":"high"},"future_field":{"preserve":true},"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["store"]) != "false" || string(fields["stream"]) != "true" || string(fields["future_field"]) != `{"preserve":true}` || !strings.Contains(string(fields["tools"]), `"type":"custom"`) || !strings.Contains(string(fields["include"]), "reasoning.encrypted_content") {
		t.Fatalf("incorrect subscription profile: %s", body)
	}
	for _, field := range []string{`"store":true`, `"background":true`, `"previous_response_id":"resp-old"`, `"conversation":"conv-old"`} {
		if _, err := prepareSubscriptionBody([]byte(`{"model":"m","input":"x",` + field + `}`)); err == nil {
			t.Fatalf("accepted unsupported state: %s", field)
		}
	}
}

func TestSubscriptionBufferedAndStreamingWithAuthRecovery(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint(streaming), func(t *testing.T) {
			store, err := providerauth.OpenStore(t.Context(), ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			var refreshes atomic.Int64
			issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				refreshes.Add(1)
				_, _ = w.Write([]byte(`{"access_token":"renewed","refresh_token":"rotated","expires_in":3600}`))
			}))
			defer issuer.Close()
			auth := providerauth.New(t.Context(), store, openaiauth.Config{IssuerURL: issuer.URL})
			defer auth.Close()
			if err := store.Save(t.Context(), "codex", llmauth.Credential{Provider: openaiauth.Provider, AccessToken: "old", RefreshToken: "refresh", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			var requestBodies []string
			transport := bedrockRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || request.Header.Get("ChatGPT-Account-ID") != "account" || request.Header.Get("originator") != "sparkroute" {
					t.Error("incorrect subscription endpoint or headers")
				}
				raw, _ := io.ReadAll(request.Body)
				_ = request.Body.Close()
				requestBodies = append(requestBodies, string(raw))
				if calls == 1 {
					return &http.Response{StatusCode: 401, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("expired"))}, nil
				}
				if request.Header.Get("Authorization") != "Bearer renewed" {
					t.Error("recovery did not replace bearer")
				}
				wire := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"upstream-model\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
			})
			doc := responsesDocument("")
			doc.Providers[0].Type, doc.Providers[0].SubscriptionProfile = "openai_subscription", "codex"
			handler, err := NewDataHandler(doc, DataOptions{ProviderAuth: auth, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"default","input":"hello","stream":%t,"store":false}`, streaming)))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"total_tokens":5`) {
				t.Fatalf("response = %d %s", w.Code, w.Body)
			}
			if streaming != strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
				t.Fatalf("wrong response framing: %s", w.Header())
			}
			if calls != 2 || refreshes.Load() != 1 || requestBodies[0] != requestBodies[1] {
				t.Fatal("auth replay was not bounded or changed request")
			}
			for _, endpoint := range []string{"/v1/chat/completions", "/v1/embeddings", "/v1/responses/compact"} {
				r := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(`{"model":"default","input":"hello","messages":[{"role":"user","content":"hello"}]}`))
				r.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(httptest.NewRecorder(), r)
			}
			if calls != 2 {
				t.Fatal("unsupported operation reached subscription backend")
			}
		})
	}
}

func TestSubscriptionBufferedRequiresBoundedTerminalResponse(t *testing.T) {
	for _, wire := range []string{
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n",
		"data: {\"type\":\"response.failed\"}\n\n",
		"data: {\"type\":\"response.completed\",\"response\":null}\n\n",
	} {
		if _, err := collectSubscriptionResponse(strings.NewReader(wire), 4096); err == nil {
			t.Fatal("accepted incomplete or failed response")
		}
	}
	if _, err := collectSubscriptionResponse(strings.NewReader(strings.Repeat("x", 100)), 50); err == nil {
		t.Fatal("response limit bypassed")
	}
}

func TestSubscriptionRecoveryStopsAfterSecondUnauthorized(t *testing.T) {
	store, err := providerauth.OpenStore(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"renewed","expires_in":3600}`))
	}))
	defer issuer.Close()
	auth := providerauth.New(t.Context(), store, openaiauth.Config{IssuerURL: issuer.URL})
	defer auth.Close()
	if err := store.Save(t.Context(), "codex", llmauth.Credential{Provider: openaiauth.Provider, AccessToken: "old", RefreshToken: "refresh"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := chatCompletionsHandler{providerAuth: auth, client: &http.Client{Transport: bedrockRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("unauthorized"))}, nil
	})}}
	r, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(`{}`))
	response, err := h.sendUpstream(r, routing.Selection{Provider: config.Provider{Type: "openai_subscription", SubscriptionProfile: "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized || calls != 2 {
		t.Fatal("authentication recovery was not bounded")
	}
}
