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

func TestAnthropicMessagesPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedUpstreamRequest{
			Path:   request.URL.Path,
			Header: request.Header.Clone(),
			Body:   readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_upstream_anthropic")
		_, _ = io.WriteString(w, `{
			"id":"msg_1",
			"type":"message",
			"role":"assistant",
			"model":"claude-upstream",
			"content":[{"type":"text","text":"hello"}],
			"stop_reason":"end_turn",
			"stop_sequence":null,
			"usage":{
				"input_tokens":10,
				"cache_creation_input_tokens":3,
				"cache_read_input_tokens":2,
				"output_tokens":5,
				"output_tokens_details":{"thinking_tokens":1},
				"cache_creation":{"ephemeral_5m_input_tokens":3},
				"server_tool_use":{"web_search_requests":2}
			},
			"future_response_field":{"kept":true}
		}`)
	}))
	defer upstream.Close()

	document := anthropicDocument(
		upstream.URL+"/v1",
		config.CapabilityTools,
		config.CapabilityVision,
		config.CapabilityPromptCaching,
	)
	document.Providers[0].Auth = config.ProviderAuth{
		Type:       config.AuthHeader,
		Header:     "X-Api-Key",
		Credential: "env://ANTHROPIC_API_KEY",
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: fakeCredentialSource{
			"env://ANTHROPIC_API_KEY": "provider-secret",
		},
		Ledger: recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	request := anthropicRequest(
		http.MethodPost,
		"/v1/messages",
		`{
			"model":"default",
			"max_tokens":128,
			"messages":[{
				"role":"user",
				"content":[
					{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}}
				]
			}],
			"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object"}}],
			"metadata":{"user_id":"opaque"},
			"future_request_field":{"kept":true}
		}`,
	)
	request.Header.Set("X-Api-Key", "caller-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if response.Header().Get("Request-Id") != "req_upstream_anthropic" {
		t.Fatalf(
			"Request-Id = %q",
			response.Header().Get("Request-Id"),
		)
	}
	if response.Header().Get("X-Request-Id") == "" {
		t.Fatal("missing gateway X-Request-Id")
	}
	var downstream map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &downstream); err != nil {
		t.Fatalf("decode downstream response: %v", err)
	}
	if string(downstream["model"]) != `"public"` ||
		!strings.Contains(
			string(downstream["future_response_field"]),
			`"kept":true`,
		) {
		t.Fatalf("downstream response = %s", response.Body)
	}

	got := <-captured
	if got.Path != "/v1/messages" {
		t.Fatalf("upstream path = %q", got.Path)
	}
	if got.Header.Get("Anthropic-Version") != anthropicAPIVersion {
		t.Fatalf(
			"Anthropic-Version = %q",
			got.Header.Get("Anthropic-Version"),
		)
	}
	if got.Header.Get("X-Api-Key") != "provider-secret" {
		t.Fatalf("X-Api-Key = %q", got.Header.Get("X-Api-Key"))
	}
	if string(got.Body["model"]) != `"claude-upstream"` ||
		!strings.Contains(
			string(got.Body["future_request_field"]),
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
	for _, record := range []struct {
		protocol  string
		operation string
		usage     ledger.TokenUsage
	}{
		{
			records[1].Request.Protocol,
			records[1].Request.Operation,
			records[1].Request.Usage,
		},
		{
			"anthropic",
			"messages",
			records[0].Attempt.Usage,
		},
	} {
		if record.protocol != "anthropic" ||
			record.operation != "messages" {
			t.Errorf(
				"protocol/operation = %q/%q",
				record.protocol,
				record.operation,
			)
		}
		assertAnthropicUsage(t, record.usage, 10, 5, 20, 2, 3, 1)
		if record.usage.ProviderComponents["server_tool_use.web_search_requests"] != 2 ||
			record.usage.ProviderComponents["cache_creation.ephemeral_5m_input_tokens"] != 3 {
			t.Errorf(
				"provider components = %#v",
				record.usage.ProviderComponents,
			)
		}
	}
}

func TestMultiProtocolOpenAICompatibleDeploymentUsesNativeIngressDialects(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedUpstreamRequest{
			Path: request.URL.Path,
			Body: readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/v1/chat/completions" {
			_, _ = io.WriteString(w, `{
				"id":"chat_sparkrun",
				"object":"chat.completion",
				"created":1,
				"model":"runtime-model",
				"choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"id":"msg_sparkrun",
			"type":"message",
			"role":"assistant",
			"model":"runtime-model",
			"content":[{"type":"text","text":"hello"}],
			"stop_reason":"end_turn",
			"stop_sequence":null,
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer upstream.Close()

	document := anthropicDocument(upstream.URL + "/v1")
	document.Providers[0].Name = "sparkrun"
	document.Providers[0].Type = "openai_compatible"
	document.Deployments[0].Provider = "sparkrun"
	document.Deployments[0].NativeProtocols = []config.Protocol{
		config.ProtocolOpenAI,
		config.ProtocolAnthropic,
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		anthropicRequest(
			http.MethodPost,
			"/v1/messages",
			`{
				"model":"default",
				"max_tokens":32,
				"messages":[{"role":"user","content":"hello"}]
			}`,
		),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	got := <-captured
	if got.Path != "/v1/messages" ||
		string(got.Body["model"]) != `"claude-upstream"` {
		t.Fatalf("upstream request = %#v", got)
	}

	response = httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"default",
			"messages":[{"role":"user","content":"hello"}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("OpenAI status = %d; body=%s", response.Code, response.Body)
	}
	got = <-captured
	if got.Path != "/v1/chat/completions" ||
		string(got.Body["model"]) != `"claude-upstream"` {
		t.Fatalf("OpenAI upstream request = %#v", got)
	}
}

func TestAnthropicCountTokens(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedUpstreamRequest{
			Path: request.URL.Path,
			Body: readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"input_tokens":2095}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		anthropicDocument(
			upstream.URL+"/v1",
			config.CapabilityTokenCounting,
		),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		anthropicRequest(
			http.MethodPost,
			"/v1/messages/count_tokens",
			`{
				"model":"public",
				"messages":[{"role":"user","content":"hello"}],
				"system":"count this exactly"
			}`,
		),
	)

	if response.Code != http.StatusOK ||
		strings.TrimSpace(response.Body.String()) != `{"input_tokens":2095}` {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	got := <-captured
	if got.Path != "/v1/messages/count_tokens" ||
		string(got.Body["model"]) != `"claude-upstream"` {
		t.Fatalf("upstream request = %#v", got)
	}
	records := recorder.snapshot()
	if len(records) != 2 || records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	requestRecord := records[1].Request
	if requestRecord.Protocol != "anthropic" ||
		requestRecord.Operation != "messages_count_tokens" {
		t.Fatalf("request record = %#v", requestRecord)
	}
	assertUsageValue(t, "input", requestRecord.Usage.InputTokens, 2095)
	assertUsageValue(t, "output", requestRecord.Usage.OutputTokens, 0)
	assertUsageValue(t, "total", requestRecord.Usage.TotalTokens, 2095)
	if requestRecord.Usage.Completeness != ledger.UsageComplete ||
		requestRecord.Usage.NormalizationVersion !=
			anthropicCountTokensUsageVersion {
		t.Fatalf("usage = %#v", requestRecord.Usage)
	}
}

func TestAnthropicBetaRoutesOnlyToApprovedTarget(t *testing.T) {
	t.Parallel()

	var unapprovedCalls atomic.Int64
	unapproved := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		unapprovedCalls.Add(1)
	}))
	defer unapproved.Close()
	captured := make(chan http.Header, 1)
	approved := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- request.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			w,
			`{"id":"msg_beta","type":"message","role":"assistant",`+
				`"model":"second-upstream","content":[],"stop_reason":"end_turn",`+
				`"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
		)
	}))
	defer approved.Close()

	document := anthropicDocument(unapproved.URL + "/v1")
	document.Providers = append(document.Providers, config.Provider{
		Name:    "anthropic-second",
		Type:    "anthropic",
		BaseURL: approved.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:     "anthropic-second",
			Provider: "anthropic-second",
			Model:    "second-upstream",
			Capabilities: []config.Capability{
				"x-anthropic-beta.files-api-2025-04-14",
			},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "anthropic-second",
			Weight:     100,
		},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := anthropicRequest(
		http.MethodPost,
		"/v1/messages",
		`{"model":"public","max_tokens":1,`+
			`"messages":[{"role":"user","content":"hello"}]}`,
	)
	request.Header.Set(
		"Anthropic-Beta",
		"files-api-2025-04-14, files-api-2025-04-14",
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if unapprovedCalls.Load() != 0 {
		t.Fatalf("unapproved calls = %d", unapprovedCalls.Load())
	}
	if got := (<-captured).Get("Anthropic-Beta"); got !=
		"files-api-2025-04-14" {
		t.Fatalf("Anthropic-Beta = %q", got)
	}
}

func TestAnthropicProtocolFailuresUseNativeErrorsBeforeUpstream(
	t *testing.T,
) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls.Add(1)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		anthropicDocument(upstream.URL+"/v1"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	tests := []struct {
		name   string
		header func(http.Header)
		body   string
	}{
		{
			name: "missing version",
			header: func(header http.Header) {
				header.Del("Anthropic-Version")
			},
		},
		{
			name: "unsupported version",
			header: func(header http.Header) {
				header.Set("Anthropic-Version", "2024-01-01")
			},
		},
		{
			name: "invalid beta",
			header: func(header http.Header) {
				header.Set("Anthropic-Beta", "INVALID BETA")
			},
		},
		{
			name: "unapproved beta",
			header: func(header http.Header) {
				header.Set("Anthropic-Beta", "files-api-2025-04-14")
			},
		},
		{
			name: "profile forwarding",
			header: func(header http.Header) {
				header.Set("Anthropic-User-Profile-Id", "profile_1")
			},
		},
		{
			name: "provider container",
			body: `{"model":"public","max_tokens":1,` +
				`"container":"container_1",` +
				`"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "file affinity",
			body: `{"model":"public","max_tokens":1,"messages":[{` +
				`"role":"user","content":[{"type":"document","source":{` +
				`"type":"file","file_id":"file_1"}}]}]}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			if body == "" {
				body = `{"model":"public","max_tokens":1,` +
					`"messages":[{"role":"user","content":"hello"}]}`
			}
			request := anthropicRequest(
				http.MethodPost,
				"/v1/messages",
				body,
			)
			if test.header != nil {
				test.header(request.Header)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertAnthropicError(
				t,
				response,
				http.StatusBadRequest,
				"invalid_request_error",
			)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestAnthropicGatewayFailuresUseNativeErrorEnvelope(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `not-json`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		anthropicDocument(upstream.URL+"/v1"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	methodResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		methodResponse,
		anthropicRequest(
			http.MethodGet,
			"/v1/messages",
			"",
		),
	)
	assertAnthropicError(
		t,
		methodResponse,
		http.StatusMethodNotAllowed,
		"invalid_request_error",
	)
	if methodResponse.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("Allow = %q", methodResponse.Header().Get("Allow"))
	}

	upstreamResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		upstreamResponse,
		anthropicRequest(
			http.MethodPost,
			"/v1/messages",
			`{"model":"public","max_tokens":1,`+
				`"messages":[{"role":"user","content":"hello"}]}`,
		),
	)
	assertAnthropicError(
		t,
		upstreamResponse,
		http.StatusBadGateway,
		"api_error",
	)
	if strings.Contains(upstreamResponse.Body.String(), "not-json") {
		t.Fatalf(
			"invalid upstream body leaked: %s",
			upstreamResponse.Body,
		)
	}
}

func TestDetectAnthropicCapabilities(t *testing.T) {
	t.Parallel()

	envelope, _, _, err := decodeAnthropicMessagesRequest([]byte(`{
		"model":"public",
		"max_tokens":2048,
		"messages":[{
			"role":"user",
			"content":[
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AA=="}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AA=="},"citations":{"enabled":true}},
				{"type":"tool_result","tool_use_id":"tool_1","content":"done"},
				{"type":"thinking","thinking":"...","signature":"sig"}
			]
		}],
		"system":[{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}],
		"tools":[
			{"name":"local","input_schema":{"type":"object"}},
			{"type":"web_search_20260209","name":"web_search"}
		],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":false},
		"thinking":{"type":"enabled","budget_tokens":1024},
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},
		"service_tier":"auto"
	}`))
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	got, err := detectAnthropicCapabilities(
		envelope,
		anthropicOperationMessages,
	)
	if err != nil {
		t.Fatalf("detect capabilities: %v", err)
	}
	want := []config.Capability{
		config.CapabilityCitations,
		config.CapabilityFileInput,
		config.CapabilityParallelTools,
		config.CapabilityPromptCaching,
		config.CapabilityProviderTools,
		config.CapabilityReasoning,
		config.CapabilityServiceTier,
		config.CapabilityStructuredOutputs,
		config.CapabilityTools,
		config.CapabilityVision,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestAnthropicHostedToolsAndCacheWritesUseOneAttempt(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"type":"error","error":{`+
			`"type":"api_error","message":"failed"}}`)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(
			w,
			`{"id":"msg_2","type":"message","role":"assistant",`+
				`"model":"second","content":[],"stop_reason":"end_turn",`+
				`"stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
		)
	}))
	defer second.Close()

	document := anthropicDocument(
		first.URL+"/v1",
		config.CapabilityTools,
		config.CapabilityProviderTools,
		config.CapabilityPromptCaching,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "anthropic-second",
		Type:    "anthropic",
		BaseURL: second.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:     "anthropic-second",
			Provider: "anthropic-second",
			Model:    "second",
			Capabilities: []config.Capability{
				config.CapabilityTools,
				config.CapabilityProviderTools,
				config.CapabilityPromptCaching,
			},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "anthropic-second",
			Weight:     100,
		},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		anthropicRequest(
			http.MethodPost,
			"/v1/messages",
			`{"model":"public","max_tokens":1,`+
				`"messages":[{"role":"user","content":[{`+
				`"type":"text","text":"cached",`+
				`"cache_control":{"type":"ephemeral"}}]}],`+
				`"tools":[{"type":"web_search_20260209","name":"web_search"}]}`,
		),
	)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf(
			"calls = first:%d second:%d",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
	if response.Header().Get("X-SparkRoute-Attempt-Count") != "1" {
		t.Fatalf(
			"attempt count = %q",
			response.Header().Get("X-SparkRoute-Attempt-Count"),
		)
	}
}

func anthropicDocument(
	baseURL string,
	capabilities ...config.Capability,
) config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "anthropic",
			Type:    "anthropic",
			BaseURL: baseURL,
		}},
		Deployments: []config.Deployment{{
			Name:         "anthropic",
			Provider:     "anthropic",
			Model:        "claude-upstream",
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
					Deployment: "anthropic",
					Weight:     100,
				}},
			}},
		}},
	}
}

func anthropicRequest(
	method string,
	path string,
	body string,
) *http.Request {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Anthropic-Version", anthropicAPIVersion)
	return request
}

func assertAnthropicError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	errorType string,
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
	var envelope anthropicErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode Anthropic error: %v; body=%s", err, response.Body)
	}
	if envelope.Type != "error" ||
		envelope.Error.Type != errorType ||
		envelope.Error.Message == "" ||
		envelope.RequestID == "" ||
		envelope.RequestID != response.Header().Get("Request-Id") {
		t.Fatalf("Anthropic error = %#v; headers=%v", envelope, response.Header())
	}
}

func assertAnthropicUsage(
	t *testing.T,
	usage ledger.TokenUsage,
	input int64,
	output int64,
	total int64,
	cached int64,
	cacheCreation int64,
	reasoning int64,
) {
	t.Helper()
	assertUsageValue(t, "input", usage.InputTokens, input)
	assertUsageValue(t, "output", usage.OutputTokens, output)
	assertUsageValue(t, "total", usage.TotalTokens, total)
	assertUsageValue(t, "cached", usage.CachedInputTokens, cached)
	assertUsageValue(
		t,
		"cache creation",
		usage.CacheCreationTokens,
		cacheCreation,
	)
	assertUsageValue(t, "reasoning", usage.ReasoningTokens, reasoning)
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion != anthropicMessagesUsageVersion {
		t.Errorf("usage = %#v", usage)
	}
}

func assertUsageValue(
	t *testing.T,
	name string,
	got *int64,
	want int64,
) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s tokens = %v, want %d", name, got, want)
	}
}
