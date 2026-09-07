package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestResponsesGuardrailsReplaceProtocolPayloadsOnly(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("ETag", `"primary-response"`)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       "resp_guarded",
			"object":   "response",
			"model":    "upstream-model",
			"status":   "completed",
			"metadata": map[string]string{"source": "primary"},
			"output": []any{map[string]any{
				"type": "message", "role": "assistant",
				"content": []any{map[string]any{
					"type": "output_text", "text": "unsafe response",
				}},
			}},
			"usage": map[string]any{
				"input_tokens": 4, "output_tokens": 2, "total_tokens": 6,
			},
		})
	}))
	defer primary.Close()

	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Version != guardrailProtocolVersion ||
			input.Protocol != "openai" ||
			input.Operation != openAIOperationResponses {
			t.Errorf("guardrail envelope = %#v", input)
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"public","input":"[REDACTED]","store":false}}`,
			)
		case guardrailPhasePost:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"id":"spoofed","model":"spoofed",`+
					`"usage":{"total_tokens":999},`+
					`"output":[{"type":"message","role":"assistant",`+
					`"content":[{"type":"output_text","text":"safe response"}]}]}}`,
			)
		default:
			t.Errorf("unexpected guardrail phase %q", input.Phase)
			writeGuardrailVerdict(w, "upstream-guard", `{"action":"block"}`)
		}
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
	)
	preGuardrail := config.Guardrail{
		Name:             "protocol-safety",
		Model:            "safety",
		AllowReplacement: true,
	}
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{
		preGuardrail,
	}
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{
		{
			Name:             "protocol-output-safety",
			Model:            "safety",
			AllowReplacement: true,
		},
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1/responses",
		`{"model":"public","input":"SSN 123","store":false}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	upstream := <-captured
	if string(upstream["model"]) != `"upstream-model"` ||
		string(upstream["input"]) != `"[REDACTED]"` ||
		string(upstream["store"]) != "false" {
		t.Fatalf("primary request = %#v", upstream)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["id"]) != `"resp_guarded"` ||
		string(body["model"]) != `"public"` ||
		!strings.Contains(string(body["metadata"]), `"source":"primary"`) ||
		!strings.Contains(string(body["usage"]), `"total_tokens":6`) ||
		!strings.Contains(string(body["output"]), "safe response") ||
		strings.Contains(string(body["output"]), "unsafe response") {
		t.Fatalf("guarded response = %s", response.Body)
	}
	if response.Header().Get("ETag") != "" {
		t.Fatalf("stale ETag = %q", response.Header().Get("ETag"))
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guardrail calls = %d, want 2", guardCalls.Load())
	}
}

func TestAnthropicMessagesGuardrailsUseNativeProtocolPayloads(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          "msg_guarded",
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-primary",
			"content":     []any{map[string]any{"type": "text", "text": "unsafe"}},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens": 2, "output_tokens": 1,
			},
		})
	}))
	defer primary.Close()

	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Protocol != "anthropic" ||
			input.Operation != anthropicOperationMessages {
			t.Errorf("guardrail envelope = %#v", input)
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"public","max_tokens":8,`+
					`"messages":[{"role":"user","content":"[REDACTED]"}]}}`,
			)
		case guardrailPhasePost:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"spoofed","usage":{"output_tokens":999},`+
					`"content":[{"type":"text","text":"safe"}]}}`,
			)
		default:
			t.Errorf("unexpected guardrail phase %q", input.Phase)
		}
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.Providers[0].Type = "anthropic"
	document.Deployments[0].Model = "claude-primary"
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Pre: []config.Guardrail{{
			Name:             "anthropic-input",
			Model:            "safety",
			AllowReplacement: true,
		}},
		Post: []config.Guardrail{{
			Name:             "anthropic-output",
			Model:            "safety",
			AllowReplacement: true,
		}},
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
			`{"model":"public","max_tokens":8,`+
				`"messages":[{"role":"user","content":"secret"}]}`,
		),
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if !strings.Contains(string((<-captured)["messages"]), "[REDACTED]") {
		t.Fatal("pre-guardrail replacement did not reach primary")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["model"]) != `"public"` ||
		!strings.Contains(string(body["content"]), `"safe"`) ||
		strings.Contains(string(body["content"]), `"unsafe"`) ||
		!strings.Contains(string(body["usage"]), `"output_tokens":1`) {
		t.Fatalf("guarded response = %s", response.Body)
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guard calls = %d", guardCalls.Load())
	}
}

func TestGeminiGenerateContentGuardrailsUsePathModelEnvelope(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, map[string]any{
			"candidates": []any{map[string]any{
				"content": map[string]any{
					"role": "model",
					"parts": []any{map[string]any{
						"text": "unsafe",
					}},
				},
				"finishReason": "STOP",
				"index":        0,
			}},
			"usageMetadata": map[string]any{
				"promptTokenCount":     2,
				"candidatesTokenCount": 1,
				"totalTokenCount":      3,
			},
			"modelVersion": "gemini-primary",
			"responseId":   "response-guarded",
		})
	}))
	defer primary.Close()

	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Protocol != "gemini" ||
			input.Operation != geminiOperationGenerateContent {
			t.Errorf("guardrail envelope = %#v", input)
		}
		var guardedRequest map[string]json.RawMessage
		if err := json.Unmarshal(
			input.Request,
			&guardedRequest,
		); err != nil {
			t.Errorf("decode guarded request: %v", err)
		}
		if string(guardedRequest["model"]) != `"default"` {
			t.Errorf(
				"guarded request model = %s",
				guardedRequest["model"],
			)
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"default","contents":[{"role":"user",`+
					`"parts":[{"text":"[REDACTED]"}]}]}}`,
			)
		case guardrailPhasePost:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"modelVersion":"spoofed",`+
					`"usageMetadata":{"totalTokenCount":999},`+
					`"candidates":[{"content":{"role":"model",`+
					`"parts":[{"text":"safe"}]},"finishReason":"STOP",`+
					`"index":0}]}}`,
			)
		default:
			t.Errorf("unexpected guardrail phase %q", input.Phase)
		}
	}))
	defer guard.Close()

	document := guardrailDocument(
		primary.URL+"/v1beta",
		guard.URL+"/v1",
	)
	document.Providers[0].Type = "gemini"
	document.Deployments[0].Model = "gemini-primary"
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Pre: []config.Guardrail{{
			Name:             "gemini-input",
			Model:            "safety",
			AllowReplacement: true,
		}},
		Post: []config.Guardrail{{
			Name:             "gemini-output",
			Model:            "safety",
			AllowReplacement: true,
		}},
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1beta/models/default:generateContent",
		`{"contents":[{"role":"user","parts":[{"text":"secret"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	upstream := <-captured
	if _, exists := upstream["model"]; exists ||
		!strings.Contains(string(upstream["contents"]), "[REDACTED]") {
		t.Fatalf("primary request = %#v", upstream)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["modelVersion"]) != `"public"` ||
		!strings.Contains(string(body["candidates"]), `"safe"`) ||
		strings.Contains(string(body["candidates"]), `"unsafe"`) ||
		!strings.Contains(
			string(body["usageMetadata"]),
			`"totalTokenCount":3`,
		) {
		t.Fatalf("guarded response = %s", response.Body)
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guard calls = %d", guardCalls.Load())
	}
}

func TestBedrockConverseGuardrailsUseNativeProtocolPayloads(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("Content-Type", "application/json")
		writeJSON(w, http.StatusOK, map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role": "assistant",
					"content": []any{
						map[string]any{"text": "unsafe"},
					},
				},
			},
			"stopReason": "end_turn",
			"usage": map[string]any{
				"inputTokens": 2, "outputTokens": 1,
				"totalTokens": 3,
			},
			"metrics": map[string]any{"latencyMs": 5},
		})
	}))
	defer primary.Close()

	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Protocol != "bedrock" ||
			input.Operation != bedrockOperationConverse {
			t.Errorf("guardrail envelope = %#v", input)
		}
		var guardedRequest map[string]json.RawMessage
		if err := json.Unmarshal(
			input.Request,
			&guardedRequest,
		); err != nil {
			t.Errorf("decode guarded request: %v", err)
		}
		if _, exists := guardedRequest["model"]; exists {
			t.Error("Bedrock guardrail request unexpectedly contains model")
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"messages":[{"role":"user","content":[`+
					`{"text":"[REDACTED]"}]}]}}`,
			)
		case guardrailPhasePost:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"usage":{"totalTokens":999},`+
					`"output":{"message":{"role":"assistant",`+
					`"content":[{"text":"safe"}]}}}}`,
			)
		default:
			t.Errorf("unexpected guardrail phase %q", input.Phase)
		}
	}))
	defer guard.Close()

	document := bedrockGuardrailDocument(
		primary.URL,
		guard.URL+"/v1",
	)
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Pre: []config.Guardrail{{
			Name:             "bedrock-input",
			Model:            "safety",
			AllowReplacement: true,
		}},
		Post: []config.Guardrail{{
			Name:             "bedrock-output",
			Model:            "safety",
			AllowReplacement: true,
		}},
	}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: bedrockCredentialSource(),
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse",
		`{"messages":[{"role":"user","content":[{"text":"secret"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if !strings.Contains(
		string((<-captured)["messages"]),
		"[REDACTED]",
	) {
		t.Fatal("pre-guardrail replacement did not reach primary")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(string(body["output"]), `"safe"`) ||
		strings.Contains(string(body["output"]), `"unsafe"`) ||
		!strings.Contains(string(body["usage"]), `"totalTokens":3`) {
		t.Fatalf("guarded response = %s", response.Body)
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guard calls = %d", guardCalls.Load())
	}
}

func TestBedrockStreamingPostGuardrailScreensEventPayloads(t *testing.T) {
	t.Parallel()

	stream := bytes.Join([][]byte{
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("contentBlockDelta"),
			[]byte(`{"contentBlockIndex":0,`+
				`"delta":{"text":"safe"}}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"end_turn"}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("metadata"),
			[]byte(`{"usage":{"inputTokens":1,`+
				`"outputTokens":1,"totalTokens":2}}`),
		),
	}, nil)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = w.Write(stream)
	}))
	defer primary.Close()
	inputs := make(chan guardrailInput, 1)
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		input := readGuardrailInput(t, request)
		inputs <- input
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"allow"}`,
		)
	}))
	defer guard.Close()
	document := bedrockGuardrailDocument(
		primary.URL,
		guard.URL+"/v1",
	)
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:  "bedrock-stream-safety",
		Model: "safety",
	}}
	document.VirtualModels[0].Guardrails.Stream =
		&config.GuardrailStreamPolicy{WindowBytes: 1024}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: bedrockCredentialSource(),
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse-stream",
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK ||
		!bytes.Equal(response.Body.Bytes(), stream) {
		t.Fatalf(
			"response = %d equal:%v",
			response.Code,
			bytes.Equal(response.Body.Bytes(), stream),
		)
	}
	input := <-inputs
	if input.Protocol != "bedrock" ||
		input.Operation != bedrockOperationConverseStream ||
		input.Phase != guardrailPhasePostStream ||
		input.Stream == nil ||
		len(input.Stream.Pending) != 3 ||
		!input.Stream.Final {
		t.Fatalf("stream guardrail input = %#v", input)
	}
}

func bedrockGuardrailDocument(
	primaryURL string,
	guardURL string,
) config.Document {
	document := bedrockDocument(primaryURL)
	document.Providers = append(
		document.Providers,
		config.Provider{
			Name:    "guard-provider",
			Type:    "openai_compatible",
			BaseURL: guardURL,
		},
	)
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:     "guard-deployment",
			Provider: "guard-provider",
			Model:    "upstream-guard",
		},
	)
	document.VirtualModels = append(
		document.VirtualModels,
		config.VirtualModel{
			Name:       "safety",
			Visibility: config.ModelVisibilityInternal,
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "guard-deployment",
					Weight:     1,
				}},
			}},
		},
	)
	return document
}

func TestResponsesPreGuardrailCannotChangeProviderStateControls(t *testing.T) {
	t.Parallel()

	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		primaryCalls.Add(1)
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"replace","replacement":{`+
				`"model":"public","input":"changed","store":true}}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
	)
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:             "request-safety",
		Model:            "safety",
		AllowReplacement: true,
	}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1/responses",
		`{"model":"public","input":"hello","store":false}`,
	)
	assertOpenAIErrorCode(t, response, http.StatusBadGateway, "guardrail_failed")
	if primaryCalls.Load() != 0 {
		t.Fatalf("primary calls = %d, want 0", primaryCalls.Load())
	}
}

func TestResponsesPreGuardrailReplacementRecomputesCapabilities(t *testing.T) {
	t.Parallel()

	var primaryCalls atomic.Int64
	primary := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		primaryCalls.Add(1)
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"replace","replacement":{`+
				`"model":"public","store":false,"input":[{`+
				`"type":"message","role":"user","content":[{`+
				`"type":"input_image",`+
				`"image_url":"https://example.com/image.png"}]}]}}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
	)
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{{
		Name:             "request-modality",
		Model:            "safety",
		AllowReplacement: true,
	}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1/responses",
		`{"model":"public","input":"hello","store":false}`,
	)
	assertOpenAIErrorCode(t, response, http.StatusBadRequest, "unsupported_feature")
	if primaryCalls.Load() != 0 {
		t.Fatalf("primary calls = %d, want 0", primaryCalls.Load())
	}
}

func TestResponsesStreamingPostGuardrailUsesProtocolEnvelope(t *testing.T) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			"event: response.output_text.delta\n"+
				`data: {"type":"response.output_text.delta",`+
				`"sequence_number":0,"delta":"safe"}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			"event: response.completed\n"+
				`data: {"type":"response.completed","sequence_number":1,`+
				`"response":{"id":"resp_stream_guarded","model":"upstream-model",`+
				`"status":"completed","usage":{"input_tokens":1,`+
				`"output_tokens":1,"total_tokens":2}}}`+"\n\n",
		)
	}))
	defer primary.Close()
	inputs := make(chan guardrailInput, 1)
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		input := readGuardrailInput(t, request)
		inputs <- input
		writeGuardrailVerdict(w, "upstream-guard", `{"action":"allow"}`)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
	)
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:  "stream-safety",
		Model: "safety",
	}}
	document.VirtualModels[0].Guardrails.Stream = &config.GuardrailStreamPolicy{
		WindowBytes: 1024,
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1/responses",
		`{"model":"public","input":"hello","store":false,"stream":true}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"delta":"safe"`) ||
		!strings.Contains(response.Body.String(), `"model":"public"`) {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	input := <-inputs
	if input.Protocol != "openai" ||
		input.Operation != openAIOperationResponses ||
		input.Phase != guardrailPhasePostStream ||
		input.Stream == nil ||
		len(input.Stream.Pending) != 2 ||
		!input.Stream.Final {
		t.Fatalf("stream guardrail input = %#v", input)
	}
}

func TestEmbeddingsGuardrailsReplaceProtocolPayloadsOnly(t *testing.T) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("ETag", `"primary-embedding"`)
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"model":  "upstream-model",
			"data": []any{map[string]any{
				"object": "embedding", "index": 0,
				"embedding": []float64{0.1, 0.2},
			}},
			"usage": map[string]any{
				"prompt_tokens": 7, "total_tokens": 7,
			},
		})
	}))
	defer primary.Close()
	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Protocol != "openai" ||
			input.Operation != openAIOperationEmbeddings {
			t.Errorf("guardrail envelope = %#v", input)
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"public","input":"[REDACTED]",`+
					`"encoding_format":"float"}}`,
			)
		case guardrailPhasePost:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"spoofed","usage":{"total_tokens":999},`+
					`"data":[{"object":"embedding","index":0,`+
					`"embedding":[0,0]}]}}`,
			)
		default:
			t.Errorf("unexpected guardrail phase %q", input.Phase)
			writeGuardrailVerdict(w, "upstream-guard", `{"action":"block"}`)
		}
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilitySingleVectorEmbedding,
	)
	preGuardrail := config.Guardrail{
		Name:             "embedding-safety",
		Model:            "safety",
		AllowReplacement: true,
	}
	document.VirtualModels[0].Guardrails.Pre = []config.Guardrail{
		preGuardrail,
	}
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{
		{
			Name:             "embedding-output-safety",
			Model:            "safety",
			AllowReplacement: true,
		},
	}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGuardedProtocol(
		t,
		handler,
		"/v1/embeddings",
		`{"model":"public","input":"SSN 123","encoding_format":"float"}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	upstream := <-captured
	if string(upstream["model"]) != `"upstream-model"` ||
		string(upstream["input"]) != `"[REDACTED]"` {
		t.Fatalf("primary request = %#v", upstream)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["object"]) != `"list"` ||
		string(body["model"]) != `"public"` ||
		!strings.Contains(string(body["usage"]), `"total_tokens":7`) ||
		!strings.Contains(string(body["data"]), `"embedding":[0,0]`) ||
		strings.Contains(string(body["data"]), `"embedding":[0.1,0.2]`) {
		t.Fatalf("guarded response = %s", response.Body)
	}
	if response.Header().Get("ETag") != "" {
		t.Fatalf("stale ETag = %q", response.Header().Get("ETag"))
	}
	if guardCalls.Load() != 2 {
		t.Fatalf("guardrail calls = %d, want 2", guardCalls.Load())
	}
}

func readGuardrailInput(
	t *testing.T,
	request *http.Request,
) guardrailInput {
	t.Helper()
	upstream := readGuardrailUpstreamRequest(t, request)
	var input guardrailInput
	if err := json.Unmarshal([]byte(upstream.LastContent), &input); err != nil {
		t.Errorf("decode guardrail input: %v", err)
	}
	return input
}

func serveGuardedProtocol(
	t *testing.T,
	handler http.Handler,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(
		http.MethodPost,
		path,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
