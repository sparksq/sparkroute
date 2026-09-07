// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func TestMMProjectionMakesTextOnlySelectorCandidateEligible(t *testing.T) {
	t.Parallel()

	var bridgeCalls atomic.Int64
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		bridgeCalls.Add(1)
		if request.URL.Path != "/v1/mmprojection/chat/completions" ||
			request.Header.Get("Authorization") != "Bearer bridge-secret" ||
			request.Header.Get(mmprojection.HopHeader) != "1" ||
			request.Header.Get(mmprojection.AnalyzerModelHeader) != "analyzer" {
			t.Errorf("bridge request = %s %#v", request.URL.Path, request.Header)
		}
		var body map[string]json.RawMessage
		if json.NewDecoder(request.Body).Decode(&body) != nil ||
			string(body["model"]) != `"public"` {
			t.Errorf("bridge body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "applied": true, "media_count": 1,
			"analyzer_model": "analyzer",
			"body": map[string]any{
				"model": "public",
				"messages": []any{map[string]any{
					"role": "user", "content": "projected evidence",
				}},
			},
		})
	}))
	defer bridge.Close()

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "image_url") ||
			!strings.Contains(string(body), "projected evidence") {
			t.Errorf("upstream body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-projected","model":"upstream-model","choices":[]}`)
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.Deployments[0].CapabilityPolicy.Unsupported = []config.Capability{
		config.CapabilityVision,
		config.CapabilityAudioInput,
	}
	router := projectionTestRouter(t, "fallback")
	traceRecorder := &savedTraceCollector{}
	projector, err := mmprojection.New(mmprojection.Options{
		URL: bridge.URL + "/v1", TokenReference: "env://BRIDGE_TOKEN",
		Credentials: fakeCredentialSource{"env://BRIDGE_TOKEN": "bridge-secret"},
	})
	if err != nil {
		t.Fatalf("mmprojection.New() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ModelRouter: router, MMProjection: projector, SavedTraces: traceRecorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"auto",
			"messages":[{"role":"user","content":[
				{"type":"text","text":"inspect"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}
			]}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK ||
		response.Header().Get("X-MM-Projection") != "applied" ||
		response.Header().Get("X-MM-Projection-Provider") != mmprojection.ProviderID ||
		bridgeCalls.Load() != 1 || upstreamCalls.Load() != 1 {
		t.Fatalf(
			"status=%d headers=%#v bridge=%d upstream=%d body=%s",
			response.Code, response.Header(), bridgeCalls.Load(),
			upstreamCalls.Load(), response.Body,
		)
	}
	if !strings.Contains(response.Body.String(), `"model":"auto"`) {
		t.Fatalf("response did not retain selector model: %s", response.Body)
	}
	traces := traceRecorder.snapshot()
	if len(traces) != 1 ||
		traces[0].Metadata["sparkroute.mm_projection.state"] != "applied" ||
		traces[0].Metadata["sparkroute.mm_projection.provider"] != mmprojection.ProviderID ||
		traces[0].Metadata["sparkroute.mm_projection.analyzer_model"] != "analyzer" ||
		!strings.Contains(traces[0].Request.Body, "image_url") {
		t.Fatalf("saved projection trace = %#v", traces)
	}
}

func TestMMProjectionPreservesResponsesProtocolAndStateControls(t *testing.T) {
	t.Parallel()

	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/mmprojection/responses" {
			t.Errorf("bridge path = %q", request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "applied": true, "media_count": 1,
			"analyzer_model": "analyzer",
			"body": map[string]any{
				"model": "public", "input": "projected response evidence",
				"store": false,
			},
		})
	}))
	defer bridge.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body map[string]json.RawMessage
		if json.NewDecoder(request.Body).Decode(&body) != nil ||
			string(body["input"]) != `"projected response evidence"` ||
			string(body["store"]) != "false" {
			t.Errorf("upstream Responses body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp_projected", "object": "response",
			"model": "upstream-model", "status": "completed",
			"output": []any{},
		})
	}))
	defer upstream.Close()

	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].CapabilityPolicy.Unsupported = []config.Capability{
		config.CapabilityVision,
	}
	projector, err := mmprojection.New(mmprojection.Options{
		URL: bridge.URL + "/v1", TokenReference: "env://TOKEN",
		Credentials: fakeCredentialSource{"env://TOKEN": "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ModelRouter:  projectionTestRouter(t, "fail_closed"),
		MMProjection: projector,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "/v1/responses",
		strings.NewReader(`{
			"model":"auto","store":false,
			"input":[{"role":"user","content":[
				{"type":"input_image","image_url":"data:image/png;base64,eA=="}
			]}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK ||
		response.Header().Get("X-MM-Projection") != "applied" ||
		!strings.Contains(response.Body.String(), `"model":"auto"`) {
		t.Fatalf("status=%d headers=%#v body=%s", response.Code, response.Header(), response.Body)
	}
}

func TestRequiredMMTextInspectionAttestationGatesProjectedPII(t *testing.T) {
	for _, test := range []struct {
		name             string
		attest           bool
		wantStatus       int
		wantUpstreamCall int64
	}{
		{name: "complete attestation", attest: true, wantStatus: http.StatusOK, wantUpstreamCall: 1},
		{name: "missing attestation", wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				response := map[string]any{
					"version": 1, "applied": true, "media_count": 1,
					"analyzer_model": "analyzer",
					"body": map[string]any{
						"model": "public",
						"messages": []any{map[string]any{
							"role": "user", "content": "OCR alice@example.com",
						}},
					},
				}
				if test.attest {
					response["text_inspection"] = map[string]any{
						"version": 1, "state": "complete", "media_count": 1,
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer bridge.Close()
			var upstreamCalls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				upstreamCalls.Add(1)
				body, _ := io.ReadAll(request.Body)
				if strings.Contains(string(body), "alice@example.com") ||
					!strings.Contains(string(body), "[[SPARKROUTE_PII_EMAIL_") {
					t.Errorf("upstream PII body = %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-pii","model":"upstream-model","choices":[]}`)
			}))
			defer upstream.Close()

			document := proxyDocument(upstream.URL + "/v1")
			document.Deployments[0].CapabilityPolicy.Unsupported = []config.Capability{config.CapabilityVision}
			document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
				Entities:  []config.PIIEntity{config.PIIEntityEmail},
				MediaText: config.PIIMediaTextRequired,
			}}
			projector, err := mmprojection.New(mmprojection.Options{
				URL: bridge.URL + "/v1", TokenReference: "env://TOKEN",
				Credentials: fakeCredentialSource{"env://TOKEN": "secret"},
			})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewDataHandler(document, DataOptions{
				ModelRouter: projectionTestRouter(t, "fallback"), MMProjection: projector,
				Privacy: testPrivacyProvider{},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus || upstreamCalls.Load() != test.wantUpstreamCall {
				t.Fatalf("status=%d upstream=%d body=%s", response.Code, upstreamCalls.Load(), response.Body)
			}
		})
	}
}

func TestMMProjectionFailureModesAndInvalidMedia(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		failureMode   string
		bridgeStatus  int
		wantStatus    int
		wantState     string
		wantUpstreams int64
		withoutBridge bool
	}{
		{name: "operational fallback", failureMode: "fallback", bridgeStatus: http.StatusBadGateway, wantStatus: http.StatusOK, wantState: "fallback", wantUpstreams: 1},
		{name: "operational fail closed", failureMode: "fail_closed", bridgeStatus: http.StatusBadGateway, wantStatus: http.StatusServiceUnavailable, wantState: "failed"},
		{name: "invalid media always closed", failureMode: "fallback", bridgeStatus: http.StatusUnsupportedMediaType, wantStatus: http.StatusUnsupportedMediaType, wantState: "failed"},
		{name: "missing bridge obeys fail closed", failureMode: "fail_closed", bridgeStatus: http.StatusOK, wantStatus: http.StatusServiceUnavailable, wantState: "failed", withoutBridge: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var upstreamCalls atomic.Int64
			bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.bridgeStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"projection rejected test input"}}`)
			}))
			defer bridge.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"chatcmpl-fallback","model":"upstream-model","choices":[]}`)
			}))
			defer upstream.Close()

			document := proxyDocument(upstream.URL + "/v1")
			document.Deployments[0].Capabilities = append(
				document.Deployments[0].Capabilities,
				config.CapabilityVision,
			)
			var projector *mmprojection.Client
			var err error
			if !test.withoutBridge {
				projector, err = mmprojection.New(mmprojection.Options{
					URL: bridge.URL, TokenReference: "env://TOKEN",
					Credentials: fakeCredentialSource{"env://TOKEN": "secret"},
				})
				if err != nil {
					t.Fatalf("mmprojection.New() error = %v", err)
				}
			}
			handler, err := NewDataHandler(document, DataOptions{
				ModelRouter:  projectionTestRouter(t, test.failureMode),
				MMProjection: projector,
			})
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			request := httptest.NewRequest(
				http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,eA=="}}]}]}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus ||
				response.Header().Get("X-MM-Projection") != test.wantState ||
				upstreamCalls.Load() != test.wantUpstreams {
				t.Fatalf(
					"status=%d state=%q upstream=%d body=%s",
					response.Code, response.Header().Get("X-MM-Projection"),
					upstreamCalls.Load(), response.Body,
				)
			}
		})
	}
}

func TestMMProjectionAnalyzerBypassesPublicAuthAndSelector(t *testing.T) {
	t.Parallel()

	client, err := mmprojection.New(mmprojection.Options{
		URL: "https://mmbridge.example/v1", TokenReference: "env://TOKEN",
		Credentials:          fakeCredentialSource{"env://TOKEN": "shared-token"},
		DefaultAnalyzerModel: "analyzer",
	})
	if err != nil {
		t.Fatalf("mmprojection.New() error = %v", err)
	}
	public := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	internal := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" ||
			request.Header.Get("Authorization") != "" ||
			!isMMProjectionAnalyzerInvocation(request.Context()) {
			t.Errorf("internal request = %s %#v", request.URL.Path, request.Header)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler := WrapMMProjectionAnalyzer(public, internal, client, nil)
	request := httptest.NewRequest(
		http.MethodPost,
		mmprojection.AnalyzerChatPath,
		strings.NewReader(`{"model":"analyzer","messages":[]}`),
	)
	request.Header.Set("Authorization", "Bearer shared-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("authorized status=%d body=%s", response.Code, response.Body)
	}

	wrongModel := httptest.NewRequest(
		http.MethodPost,
		mmprojection.AnalyzerChatPath,
		strings.NewReader(`{"model":"auto","messages":[]}`),
	)
	wrongModel.Header.Set("Authorization", "Bearer shared-token")
	wrong := httptest.NewRecorder()
	handler.ServeHTTP(wrong, wrongModel)
	if wrong.Code != http.StatusNotFound {
		t.Fatalf("wrong model status=%d body=%s", wrong.Code, wrong.Body)
	}
}

func projectionTestRouter(t *testing.T, failureMode string) *modelrouter.RouterManager {
	t.Helper()
	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version:             modelrouter.RoutingPolicyVersion,
		Revision:            1,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {
				Strategy: "balanced", Models: []string{"public"},
				MMProjection: &modelrouter.MMProjectionPolicy{
					AnalyzerModel: "analyzer", FailureMode: failureMode,
				},
			},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"public": {Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("NewRouterManager() error = %v", err)
	}
	return router
}
