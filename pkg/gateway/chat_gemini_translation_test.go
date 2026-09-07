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
	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestChatCompletionsTranslatesToGeminiGenerateContent(
	t *testing.T,
) {
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
				"content":{"role":"model","parts":[
					{"text":"checking"},
					{"functionCall":{
						"id":"call-2",
						"name":"lookup",
						"args":{"city":"Austin"}
					}}
				]},
				"finishReason":"STOP",
				"safetyRatings":[{
					"category":"HARM_CATEGORY_HARASSMENT",
					"probability":"NEGLIGIBLE"
				}],
				"index":0
			}],
			"usageMetadata":{
				"promptTokenCount":10,
				"cachedContentTokenCount":2,
				"candidatesTokenCount":5,
				"thoughtsTokenCount":1,
				"totalTokenCount":16
			},
			"modelVersion":"gemini-upstream",
			"responseId":"response-1"
		}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilityTools,
			config.CapabilityParallelTools,
			config.CapabilityVision,
		),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{
		"model":"default",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":[
				{"type":"text","text":"Use this image."},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}
			]},
			{"role":"assistant","content":null,"tool_calls":[{
				"id":"call-1","type":"function",
				"function":{"name":"lookup","arguments":"{\"city\":\"Dallas\"}"}
			}]},
			{"role":"tool","tool_call_id":"call-1","content":"{\"weather\":\"sunny\"}"}
		],
		"max_completion_tokens":128,
		"temperature":1.5,
		"top_p":0.8,
		"frequency_penalty":0,
		"presence_penalty":0,
		"logprobs":false,
		"store":false,
		"stop":["END"],
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"description":"Look up weather",
				"parameters":{
					"type":"object",
					"properties":{"city":{"type":"string"}},
					"required":["city"]
				},
				"strict":false
			}
		}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":true
	}`)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"status = %d, want 200; body=%s",
			response.Code,
			response.Body,
		)
	}
	var translated struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &translated); err != nil {
		t.Fatalf("decode translated response: %v", err)
	}
	if translated.Object != "chat.completion" ||
		translated.Model != "public" ||
		len(translated.Choices) != 1 ||
		translated.Choices[0].FinishReason != "tool_calls" ||
		translated.Choices[0].Message.Content != "checking" ||
		len(translated.Choices[0].Message.ToolCalls) != 1 ||
		translated.Choices[0].Message.ToolCalls[0].ID != "call-2" ||
		translated.Choices[0].Message.ToolCalls[0].Function.Name !=
			"lookup" ||
		translated.Choices[0].Message.ToolCalls[0].Function.Arguments !=
			`{"city":"Austin"}` ||
		translated.Usage.PromptTokens != 10 ||
		translated.Usage.CompletionTokens != 5 ||
		translated.Usage.TotalTokens != 16 ||
		translated.Usage.PromptDetails.CachedTokens != 2 ||
		translated.Usage.CompletionDetails.ReasoningTokens != 1 {
		t.Fatalf("translated response = %#v", translated)
	}

	got := <-captured
	if got.Path !=
		"/v1beta/models/gemini-upstream:generateContent" ||
		got.RawQuery != "" ||
		got.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream request = %#v", got)
	}
	for _, forbidden := range []string{
		"model", "stream", "stream_options",
		"max_completion_tokens", "parallel_tool_calls",
	} {
		if _, exists := got.Body[forbidden]; exists {
			t.Fatalf(
				"translated request retained Chat field %q: %#v",
				forbidden,
				got.Body,
			)
		}
	}
	for _, fragment := range []struct {
		field string
		value string
	}{
		{"systemInstruction", `"role":"system"`},
		{"contents", `"inlineData"`},
		{"contents", `"functionCall"`},
		{"contents", `"functionResponse"`},
		{"generationConfig", `"maxOutputTokens":128`},
		{"tools", `"parametersJsonSchema"`},
		{"toolConfig", `"allowedFunctionNames":["lookup"]`},
	} {
		if !bytes.Contains(
			got.Body[fragment.field],
			[]byte(fragment.value),
		) {
			t.Fatalf(
				"translated %s missing %s: %#v",
				fragment.field,
				fragment.value,
				got.Body,
			)
		}
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Usage.TotalTokens == nil ||
		*records[0].Attempt.Usage.TotalTokens != 16 {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsTranslatesGeminiGenerateContentStream(
	t *testing.T,
) {
	t.Parallel()

	stream := strings.Join([]string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"checking"}]},"index":0}],"usageMetadata":{"promptTokenCount":9}}`,
		"",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call-1","name":"lookup","args":{"city":"Austin"}}}]},"finishReason":"STOP","index":0}],"usageMetadata":{"candidatesTokenCount":3,"totalTokenCount":12}}`,
		"",
		"",
	}, "\n")
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilityTools,
			config.CapabilityStreamUsage,
		),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{
		"model":"default",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{
			"type":"function",
			"function":{"name":"lookup","description":"Look up weather"}
		}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`)
	if response.Code != http.StatusOK ||
		!isSSEStream(response.Header().Get("Content-Type")) {
		t.Fatalf(
			"response = %d content-type=%q body=%s",
			response.Code,
			response.Header().Get("Content-Type"),
			response.Body,
		)
	}
	body := response.Body.String()
	for _, fragment := range []string{
		`"role":"assistant"`,
		`"content":"checking"`,
		`"tool_calls"`,
		`"arguments":"{\"city\":\"Austin\"}"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":9`,
		`"completion_tokens":3`,
		"data: [DONE]\n\n",
	} {
		if !strings.Contains(body, fragment) {
			t.Fatalf(
				"translated stream missing %q: %s",
				fragment,
				body,
			)
		}
	}
	got := <-captured
	if got.Path !=
		"/v1beta/models/gemini-upstream:streamGenerateContent" ||
		got.RawQuery != "alt=sse" ||
		got.Header.Get("Accept") != "text/event-stream" {
		t.Fatalf("upstream request = %#v", got)
	}
	if _, exists := got.Body["stream"]; exists {
		t.Fatalf("translated Gemini request retained stream: %#v", got.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Outcome != ledger.OutcomeSuccess ||
		records[0].Attempt.Usage.TotalTokens == nil ||
		*records[0].Attempt.Usage.TotalTokens != 12 {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsGeminiStreamTerminalFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		stream          string
		wantBody        string
		wantFailure     string
		wantStreamError bool
	}{
		{
			name: "post-200 error",
			stream: `data: {"error":{"code":503,` +
				`"message":"unavailable","status":"UNAVAILABLE"}}` +
				"\n\n",
			wantBody:    `"code":"upstream_stream_error"`,
			wantFailure: "gemini_stream_error_unavailable",
		},
		{
			name: "EOF before finish",
			stream: `data: {"candidates":[{"content":{` +
				`"role":"model","parts":[{"text":"partial"}]},` +
				`"index":0}],"usageMetadata":{"promptTokenCount":1}}` +
				"\n\n",
			wantFailure:     chatGeminiStreamFailureClass,
			wantStreamError: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.stream)
			}))
			defer upstream.Close()
			recorder := &collectingRecorder{}
			handler, err := NewDataHandler(
				geminiDocument(upstream.URL+"/v1beta"),
				DataOptions{Ledger: recorder},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveChat(
				t,
				handler,
				`{"model":"default","stream":true,`+
					`"messages":[{"role":"user","content":"hi"}]}`,
			)
			if response.Code != http.StatusOK ||
				strings.Contains(response.Body.String(), "data: [DONE]") ||
				test.wantBody != "" &&
					!strings.Contains(
						response.Body.String(),
						test.wantBody,
					) {
				t.Fatalf(
					"response=%d body=%s",
					response.Code,
					response.Body,
				)
			}
			records := recorder.snapshot()
			if len(records) != 2 ||
				records[0].Attempt == nil ||
				records[0].Attempt.FailureClass != test.wantFailure {
				t.Fatalf("records = %#v", records)
			}
			wantOutcome := ledger.OutcomeUpstreamError
			if test.wantStreamError {
				wantOutcome = ledger.OutcomeStreamError
			}
			if records[0].Attempt.Outcome != wantOutcome {
				t.Fatalf(
					"attempt outcome = %q, want %q",
					records[0].Attempt.Outcome,
					wantOutcome,
				)
			}
		})
	}
}

func TestChatCompletionsRejectsUnrepresentableGeminiRequestsBeforeTraffic(
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
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilityDeveloperMessages,
			config.CapabilityVision,
			config.CapabilityTools,
			config.CapabilityJSONMode,
		),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "developer role",
			body: `{"model":"default","messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "remote image",
			body: `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private.example/image.png"}}]}]}`,
		},
		{
			name: "unsupported image type",
			body: `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/gif;base64,AA=="}}]}]}`,
		},
		{
			name: "parallel tools disabled",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"parallel_tool_calls":false}`,
		},
		{
			name: "strict tool schema",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","strict":true}}]}`,
		},
		{
			name: "non-object tool result",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-1","content":"sunny"}]}`,
		},
		{
			name: "JSON response format",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`,
		},
		{
			name: "nonzero penalty",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := serveChat(t, handler, test.body)
			var failure openAIErrorEnvelope
			if response.Code != http.StatusBadRequest ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code != "unsupported_feature" {
				t.Fatalf(
					"response=%d body=%s failure=%#v",
					response.Code,
					response.Body,
					failure,
				)
			}
		})
	}
	malformedCarrier := serveChat(
		t,
		handler,
		`{"model":"default","messages":[`+
			`{"role":"user","content":"hi"},`+
			`{"role":"assistant","content":null,"tool_calls":[{`+
			`"id":"call_gemini_sig_not-base64","type":"function",`+
			`"function":{"name":"lookup","arguments":"{}"}}]}]}`,
	)
	var malformedFailure openAIErrorEnvelope
	if malformedCarrier.Code != http.StatusBadRequest ||
		json.Unmarshal(
			malformedCarrier.Body.Bytes(),
			&malformedFailure,
		) != nil ||
		malformedFailure.Error.Code != "invalid_request_body" {
		t.Fatalf(
			"malformed carrier response=%d body=%s failure=%#v",
			malformedCarrier.Code,
			malformedCarrier.Body,
			malformedFailure,
		)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsMixedPoolSkipsIncompatibleGeminiTarget(
	t *testing.T,
) {
	t.Parallel()

	var geminiCalls atomic.Int64
	gemini := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		geminiCalls.Add(1)
	}))
	defer gemini.Close()
	var openAICalls atomic.Int64
	openAI := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		openAICalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-openai",
			"model":   "upstream-openai",
			"choices": []any{},
		})
	}))
	defer openAI.Close()

	document := geminiDocument(
		gemini.URL+"/v1beta",
		config.CapabilityDeveloperMessages,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name: "openai", Type: "openai_compatible",
		BaseURL: openAI.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name: "openai", Provider: "openai",
			Model: "upstream-openai",
			Capabilities: []config.Capability{
				config.CapabilityDeveloperMessages,
			},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{Deployment: "openai", Weight: 1},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","messages":[`+
			`{"role":"developer","content":"rules"},`+
			`{"role":"user","content":"hi"}]}`,
	)
	if response.Code != http.StatusOK ||
		geminiCalls.Load() != 0 ||
		openAICalls.Load() != 1 {
		t.Fatalf(
			"response=%d gemini=%d openai=%d body=%s",
			response.Code,
			geminiCalls.Load(),
			openAICalls.Load(),
			response.Body,
		)
	}
}

func TestChatCompletionsRoundTripsGeminiToolThoughtSignature(
	t *testing.T,
) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"candidates":[{
				"content":{"role":"model","parts":[{
					"functionCall":{"id":"call-1","name":"lookup","args":{}},
					"thoughtSignature":"opaque-signature"
				}]},
				"finishReason":"STOP",
				"index":0
			}],
			"usageMetadata":{
				"promptTokenCount":1,
				"candidatesTokenCount":1,
				"totalTokenCount":2
			}
		}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilityTools,
		),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","messages":[{"role":"user","content":"hi"}],`+
			`"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}
	var translated struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(response.Body.Bytes(), &translated) != nil ||
		len(translated.Choices) != 1 ||
		len(translated.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("translated response = %s", response.Body)
	}
	opaqueID := translated.Choices[0].Message.ToolCalls[0].ID
	if !strings.HasPrefix(opaqueID, geminiToolCallIDPrefix) {
		t.Fatalf("tool call ID = %q", opaqueID)
	}
	followup, err := json.Marshal(map[string]any{
		"model": "default",
		"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
			map[string]any{
				"role": "assistant", "content": nil,
				"tool_calls": []any{map[string]any{
					"id": opaqueID, "type": "function",
					"function": map[string]any{
						"name": "lookup", "arguments": "{}",
					},
				}},
			},
			map[string]any{
				"role": "tool", "tool_call_id": opaqueID,
				"content": `{"result":"ok"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal follow-up: %v", err)
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(followup, &envelope) != nil {
		t.Fatalf("decode follow-up: %s", followup)
	}
	geminiBody, err := translateChatCompletionsRequestToGemini(envelope)
	if err != nil {
		t.Fatalf("translate follow-up: %v", err)
	}
	for _, fragment := range []string{
		`"id":"call-1"`,
		`"thoughtSignature":"opaque-signature"`,
		`"functionResponse":{"id":"call-1"`,
	} {
		if !bytes.Contains(geminiBody, []byte(fragment)) {
			t.Fatalf(
				"Gemini follow-up missing %q: %s",
				fragment,
				geminiBody,
			)
		}
	}
}

func TestChatCompletionsRejectsGeminiNonToolThoughtSignature(
	t *testing.T,
) {
	t.Parallel()

	_, _, err := translateGeminiResponseToChatCompletions(
		[]byte(`{
			"candidates":[{
				"content":{"role":"model","parts":[{
					"text":"answer",
					"thoughtSignature":"opaque-signature"
				}]},
				"finishReason":"STOP",
				"index":0
			}],
			"usageMetadata":{
				"promptTokenCount":1,
				"candidatesTokenCount":1,
				"totalTokenCount":2
			}
		}`),
		"request-1",
		"public",
		123,
	)
	if err == nil || !strings.Contains(err.Error(), "non-tool") {
		t.Fatalf("error = %v", err)
	}
}

func TestChatCompletionsTranslatesGeminiPromptBlock(t *testing.T) {
	t.Parallel()

	body, usage, err := translateGeminiResponseToChatCompletions(
		[]byte(`{
			"promptFeedback":{"blockReason":"SAFETY","safetyRatings":[]},
			"usageMetadata":{
				"promptTokenCount":4,
				"candidatesTokenCount":0,
				"totalTokenCount":4
			}
		}`),
		"request-1",
		"public",
		123,
	)
	if err != nil {
		t.Fatalf("translate response: %v", err)
	}
	if usage.TotalTokens == nil || *usage.TotalTokens != 4 ||
		!bytes.Contains(body, []byte(`"finish_reason":"content_filter"`)) ||
		!bytes.Contains(body, []byte(`"content":null`)) {
		t.Fatalf("translated body=%s usage=%#v", body, usage)
	}
}

func TestChatCompletionsTranslatesGeminiHTTPErrorEnvelope(
	t *testing.T,
) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{
			"error":{
				"code":400,
				"message":"invalid Gemini input",
				"status":"INVALID_ARGUMENT"
			}
		}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusBadRequest ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "upstream_error" ||
		failure.Error.Message != "invalid Gemini input" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
}
