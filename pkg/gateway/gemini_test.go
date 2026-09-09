// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

type capturedGeminiRequest struct {
	Path     string
	RawQuery string
	Header   http.Header
	Body     map[string]json.RawMessage
}

func TestGeminiGenerateContentPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedGeminiRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedGeminiRequest{
			Path:     request.URL.Path,
			RawQuery: request.URL.RawQuery,
			Header:   request.Header.Clone(),
			Body:     readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"candidates":[{
				"content":{"role":"model","parts":[{"text":"hello"}]},
				"finishReason":"STOP","index":0
			}],
			"usageMetadata":{
				"promptTokenCount":12,
				"cachedContentTokenCount":3,
				"candidatesTokenCount":7,
				"toolUsePromptTokenCount":1,
				"thoughtsTokenCount":2,
				"totalTokenCount":21,
				"promptTokensDetails":[{"modality":"TEXT","tokenCount":12}],
				"candidatesTokensDetails":[{"modality":"TEXT","tokenCount":7}]
			},
			"modelVersion":"gemini-upstream",
			"responseId":"response-1",
			"futureResponseField":{"kept":true}
		}`)
	}))
	defer upstream.Close()

	document := geminiDocument(
		upstream.URL+"/v1beta",
		config.CapabilityTools,
		config.CapabilityVision,
		config.CapabilityJSONMode,
		config.CapabilityStructuredOutputs,
		config.CapabilityReasoning,
		config.CapabilitySeed,
		config.CapabilityMultipleChoices,
	)
	document.Providers[0].Auth = config.ProviderAuth{
		Type:       config.AuthHeader,
		Header:     "X-Goog-Api-Key",
		Credential: "env://GEMINI_API_KEY",
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: fakeCredentialSource{
			"env://GEMINI_API_KEY": "provider-secret",
		},
		Ledger: recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/default:generateContent?key=caller-secret",
		`{
			"contents":[{
				"role":"user",
				"parts":[
					{"text":"hello"},
					{"inlineData":{"mimeType":"image/png","data":"AA=="}}
				]
			}],
			"tools":[{"functionDeclarations":[{
				"name":"lookup","description":"lookup",
				"parameters":{"type":"object"}
			}]}],
			"generationConfig":{
				"responseMimeType":"application/json",
				"responseSchema":{"type":"object"},
				"thinkingConfig":{"thinkingBudget":128},
				"seed":7,
				"candidateCount":2
			},
			"futureRequestField":{"kept":true}
		}`,
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	var downstream map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &downstream); err != nil {
		t.Fatalf("decode downstream response: %v", err)
	}
	if string(downstream["modelVersion"]) != `"public"` ||
		!strings.Contains(
			string(downstream["futureResponseField"]),
			`"kept":true`,
		) {
		t.Fatalf("downstream response = %s", response.Body)
	}

	got := <-captured
	if got.Path !=
		"/v1beta/models/gemini-upstream:generateContent" {
		t.Fatalf("upstream path = %q", got.Path)
	}
	if got.RawQuery != "" {
		t.Fatalf("upstream query = %q", got.RawQuery)
	}
	if got.Header.Get("X-Goog-Api-Key") != "provider-secret" {
		t.Fatalf(
			"X-Goog-Api-Key = %q",
			got.Header.Get("X-Goog-Api-Key"),
		)
	}
	if _, exists := got.Body["model"]; exists ||
		!strings.Contains(
			string(got.Body["futureRequestField"]),
			`"kept":true`,
		) {
		t.Fatalf("upstream body = %#v", got.Body)
	}

	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	for _, usage := range []ledger.TokenUsage{
		records[0].Attempt.Usage,
		records[1].Request.Usage,
	} {
		assertGeminiUsage(t, usage, 12, 7, 21, 3, 2, 1)
		if usage.ProviderComponents["prompt.text"] != 12 ||
			usage.ProviderComponents["candidates.text"] != 7 {
			t.Errorf(
				"provider components = %#v",
				usage.ProviderComponents,
			)
		}
	}
	if records[1].Request.Protocol != "gemini" ||
		records[1].Request.Operation != "generate_content" ||
		records[1].Request.RequestedModel != "default" {
		t.Fatalf("request record = %#v", records[1].Request)
	}
}

func TestGeminiCountTokensNestedGenerateRequest(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedGeminiRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedGeminiRequest{
			Path: request.URL.Path,
			Body: readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"totalTokens":42,
			"cachedContentTokenCount":5,
			"promptTokensDetails":[
				{"modality":"TEXT","tokenCount":37},
				{"modality":"IMAGE","tokenCount":5}
			]
		}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilityTokenCounting,
			config.CapabilityTools,
		),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/default:countTokens",
		`{
			"generateContentRequest":{
				"model":"models/default",
				"contents":[{
					"role":"user",
					"parts":[
						{"text":"hello"},
						{"functionResponse":{
							"name":"lookup","response":{"value":"world"}
						}}
					]
				}],
				"futureRequestField":{"kept":true}
			}
		}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	got := <-captured
	if got.Path != "/v1beta/models/gemini-upstream:countTokens" {
		t.Fatalf("upstream path = %q", got.Path)
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(
		got.Body["generateContentRequest"],
		&nested,
	); err != nil {
		t.Fatalf("decode nested request: %v", err)
	}
	if string(nested["model"]) != `"models/gemini-upstream"` ||
		!strings.Contains(
			string(nested["futureRequestField"]),
			`"kept":true`,
		) {
		t.Fatalf("nested upstream request = %#v", nested)
	}
	records := recorder.snapshot()
	if len(records) != 2 || records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	usage := records[1].Request.Usage
	assertUsageValue(t, "input", usage.InputTokens, 42)
	assertUsageValue(t, "output", usage.OutputTokens, 0)
	assertUsageValue(t, "total", usage.TotalTokens, 42)
	assertUsageValue(t, "cached", usage.CachedInputTokens, 5)
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion != geminiCountTokensUsageVersion ||
		usage.ProviderComponents["prompt.text"] != 37 ||
		usage.ProviderComponents["prompt.image"] != 5 ||
		records[1].Request.Protocol != "gemini" ||
		records[1].Request.Operation != "count_tokens" {
		t.Fatalf("request record = %#v", records[1].Request)
	}
}

func TestGeminiGatewayErrorsAndStateFailClosed(t *testing.T) {
	t.Parallel()

	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		upstreamCalls.Add(1)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
		code   string
	}{
		{
			name:   "unknown model",
			method: http.MethodPost,
			path:   "/v1beta/models/missing:generateContent",
			body:   `{"contents":[{"parts":[{"text":"hello"}]}]}`,
			status: http.StatusNotFound,
			code:   "NOT_FOUND",
		},
		{
			name:   "method",
			method: http.MethodGet,
			path:   "/v1beta/models/public:generateContent",
			body:   `{}`,
			status: http.StatusMethodNotAllowed,
			code:   "INVALID_ARGUMENT",
		},
		{
			name:   "cached content",
			method: http.MethodPost,
			path:   "/v1beta/models/public:generateContent",
			body: `{
				"contents":[{"parts":[{"text":"hello"}]}],
				"cachedContent":"cachedContents/private"
			}`,
			status: http.StatusBadRequest,
			code:   "INVALID_ARGUMENT",
		},
		{
			name:   "provider file",
			method: http.MethodPost,
			path:   "/v1beta/models/public:generateContent",
			body: `{"contents":[{"parts":[{"file_data":{
				"mime_type":"application/pdf",
				"file_uri":"https://generativelanguage.googleapis.com/v1beta/files/1"
			}}]}]}`,
			status: http.StatusBadRequest,
			code:   "INVALID_ARGUMENT",
		},
		{
			name:   "count request alternatives",
			method: http.MethodPost,
			path:   "/v1beta/models/public:countTokens",
			body: `{
				"contents":[{"parts":[{"text":"hello"}]}],
				"generateContentRequest":{
					"contents":[{"parts":[{"text":"hello"}]}]
				}
			}`,
			status: http.StatusBadRequest,
			code:   "INVALID_ARGUMENT",
		},
		{
			name:   "count capability",
			method: http.MethodPost,
			path:   "/v1beta/models/public:countTokens",
			body: `{
				"contents":[{"parts":[{"text":"hello"}]}]
			}`,
			status: http.StatusBadRequest,
			code:   "INVALID_ARGUMENT",
		},
		{
			name:   "ambiguous field aliases",
			method: http.MethodPost,
			path:   "/v1beta/models/public:generateContent",
			body: `{
				"contents":[{"parts":[{"text":"hello"}]}],
				"generationConfig":null,
				"generation_config":{"thinking_config":{}}
			}`,
			status: http.StatusBadRequest,
			code:   "INVALID_ARGUMENT",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := serveGeminiRequest(
				handler,
				test.method,
				test.path,
				test.body,
			)
			assertGeminiError(t, response, test.status, test.code)
		})
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
	}
}

func TestDetectGeminiCapabilities(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"contents":[{
			"role":"user",
			"parts":[
				{"inline_data":{"mime_type":"audio/wav","data":"AA=="}},
				{"inlineData":{"mimeType":"application/pdf","data":"AA=="}},
				{"functionCall":{"name":"lookup","args":{}}},
				{"thought":true,"text":"private reasoning"}
			]
		}],
		"tools":[
			{"functionDeclarations":[{"name":"lookup"}]},
			{"code_execution":{}}
		],
		"tool_config":{"functionCallingConfig":{"mode":"AUTO"}},
		"service_tier":"PRIORITY",
		"store":true,
		"generation_config":{
			"response_mime_type":"application/json",
			"responseJsonSchema":{"type":"object"},
			"response_modalities":["TEXT","AUDIO","IMAGE"],
			"candidate_count":2,
			"seed":1,
			"response_logprobs":true,
			"thinking_config":{}
		}
	}`), &envelope); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	got, err := detectGeminiCapabilities(
		envelope,
		geminiOperationGenerateContent,
	)
	if err != nil {
		t.Fatalf("detectGeminiCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityAudioInput,
		config.CapabilityAudioOutput,
		config.CapabilityFileInput,
		config.CapabilityJSONMode,
		config.CapabilityLogprobs,
		config.CapabilityMultipleChoices,
		config.CapabilityProviderTools,
		config.CapabilityReasoning,
		config.CapabilitySeed,
		config.CapabilityServiceTier,
		config.CapabilityStoredCompletion,
		config.CapabilityStructuredOutputs,
		config.CapabilityTools,
		config.CapabilityVision,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %#v, want %#v", got, want)
	}

	countEnvelope := map[string]json.RawMessage{
		"contents": json.RawMessage(
			`[{"parts":[{"text":"hello"}]}]`,
		),
	}
	got, err = detectGeminiCapabilities(
		countEnvelope,
		geminiOperationCountTokens,
	)
	if err != nil {
		t.Fatalf("count capabilities error = %v", err)
	}
	if !reflect.DeepEqual(
		got,
		[]config.Capability{config.CapabilityTokenCounting},
	) {
		t.Fatalf("count capabilities = %#v", got)
	}
}

func TestGeminiProviderHostedToolsUseSingleAttempt(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{
				"code":    http.StatusServiceUnavailable,
				"message": "unavailable",
				"status":  "UNAVAILABLE",
			},
		})
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		secondCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"candidates":   []any{},
			"modelVersion": "gemini-second",
		})
	}))
	defer second.Close()

	document := geminiDocument(
		first.URL+"/v1beta",
		config.CapabilityProviderTools,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "gemini-second",
		Type:    "gemini",
		BaseURL: second.URL + "/v1beta",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:         "gemini-second",
			Provider:     "gemini-second",
			Model:        "gemini-second",
			Capabilities: []config.Capability{config.CapabilityProviderTools},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "gemini-second",
			Weight:     100,
		},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/public:generateContent",
		`{
			"contents":[{"parts":[{"text":"run code"}]}],
			"tools":[{"codeExecution":{}}]
		}`,
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf(
			"calls first/second = %d/%d, want 1/0",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func geminiDocument(
	baseURL string,
	capabilities ...config.Capability,
) config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "gemini",
			Type:    "gemini",
			BaseURL: baseURL,
		}},
		Deployments: []config.Deployment{{
			Name:         "gemini",
			Provider:     "gemini",
			Model:        "models/gemini-upstream",
			Capabilities: capabilities,
		}},
		VirtualModels: []config.VirtualModel{{
			Name:    "public",
			Aliases: []string{"default"},
			Limits: config.ModelLimits{
				MaxAttempts: 3,
			},
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "gemini",
					Weight:     100,
				}},
			}},
		}},
	}
}

func serveGeminiRequest(
	handler http.Handler,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertGeminiError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	errorStatus string,
) {
	t.Helper()
	if response.Code != status {
		t.Fatalf(
			"status = %d, want %d; body=%s",
			response.Code,
			status,
			response.Body,
		)
	}
	var envelope geminiErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Gemini error: %v; body=%s", err, response.Body)
	}
	if envelope.Error.Code != status ||
		envelope.Error.Status != errorStatus ||
		envelope.Error.Message == "" {
		t.Fatalf("Gemini error = %#v", envelope)
	}
}

func assertGeminiUsage(
	t *testing.T,
	usage ledger.TokenUsage,
	input int64,
	output int64,
	total int64,
	cached int64,
	reasoning int64,
	toolUse int64,
) {
	t.Helper()
	assertUsageValue(t, "input", usage.InputTokens, input)
	assertUsageValue(t, "output", usage.OutputTokens, output)
	assertUsageValue(t, "total", usage.TotalTokens, total)
	assertUsageValue(t, "cached", usage.CachedInputTokens, cached)
	assertUsageValue(t, "reasoning", usage.ReasoningTokens, reasoning)
	assertUsageValue(t, "tool use", usage.ToolUsePromptTokens, toolUse)
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion != geminiGenerateContentUsageVersion {
		t.Errorf("usage = %#v", usage)
	}
}
