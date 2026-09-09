// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

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

func TestChatCompletionsTranslatesToBedrockConverse(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedBedrockRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedBedrockRequest{
			Path:   request.URL.Path,
			Header: request.Header.Clone(),
			Body:   body,
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"output":{"message":{"role":"assistant","content":[
				{"text":"checking"},
				{"toolUse":{"toolUseId":"call-2","name":"lookup","input":{"city":"Austin"}}}
			]}},
			"stopReason":"tool_use",
			"usage":{"inputTokens":21,"outputTokens":6,"totalTokens":27,"cacheReadInputTokens":4}
		}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(
			upstream.URL,
			config.CapabilityTools,
			config.CapabilityVision,
			config.CapabilityFileInput,
		),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{
		"model":"default",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":[
				{"type":"text","text":"Use these inputs."},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}},
				{"type":"file","file":{"filename":"notes.txt","file_data":"aGVsbG8="}}
			]},
			{"role":"assistant","content":null,"tool_calls":[{
				"id":"call-1","type":"function",
				"function":{"name":"lookup","arguments":"{\"city\":\"Dallas\"}"}
			}]},
			{"role":"tool","tool_call_id":"call-1","content":"sunny"}
		],
		"max_completion_tokens":128,
		"temperature":0.4,
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
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
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
			PromptTokens int64 `json:"prompt_tokens"`
			Details      struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
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
		translated.Usage.PromptTokens != 21 ||
		translated.Usage.Details.CachedTokens != 4 {
		t.Fatalf("translated response = %#v", translated)
	}
	got := <-captured
	if got.Path !=
		"/model/us.anthropic.claude-sonnet-4-20250514-v1:0/converse" ||
		got.Header.Get("Authorization") == "" ||
		got.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream request = %#v", got)
	}
	var requestBody map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &requestBody); err != nil {
		t.Fatalf("decode Bedrock request: %v", err)
	}
	for _, forbidden := range []string{
		"model", "stream", "max_completion_tokens", "temperature",
		"tools", "tool_choice",
	} {
		if _, exists := requestBody[forbidden]; exists {
			t.Fatalf(
				"translated request retained Chat field %q: %s",
				forbidden,
				got.Body,
			)
		}
	}
	for _, required := range []string{
		"messages", "system", "inferenceConfig", "toolConfig",
	} {
		if !rawNonNull(requestBody[required]) {
			t.Fatalf(
				"translated request is missing %q: %s",
				required,
				got.Body,
			)
		}
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Usage.InputTokens == nil ||
		*records[0].Attempt.Usage.InputTokens != 21 {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsTranslatesBedrockConverseStream(t *testing.T) {
	t.Parallel()

	stream := bytes.Join([][]byte{
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStart"),
			[]byte(`{"role":"assistant"}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("contentBlockStart"),
			[]byte(`{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"call-1","name":"lookup"}}}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("contentBlockDelta"),
			[]byte(`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":"}}}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("contentBlockDelta"),
			[]byte(`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\"Austin\"}"}}}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("contentBlockStop"),
			[]byte(`{"contentBlockIndex":1}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"tool_use"}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("metadata"),
			[]byte(`{"usage":{"inputTokens":9,"outputTokens":3,"totalTokens":12}}`),
		),
	}, nil)
	captured := make(chan capturedBedrockRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedBedrockRequest{
			Path: request.URL.Path, Header: request.Header.Clone(),
			Body: body,
		}
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = w.Write(stream)
	}))
	defer upstream.Close()
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(
			upstream.URL,
			config.CapabilityTools,
			config.CapabilityStreamUsage,
		),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{
		"model":"default",
		"messages":[{"role":"user","content":"weather"}],
		"tools":[{"type":"function","function":{"name":"lookup"}}],
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
		`"tool_calls"`,
		`"arguments":"{\"city\":"`,
		`"finish_reason":"tool_calls"`,
		`"prompt_tokens":9`,
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
	if !strings.HasSuffix(got.Path, "/converse-stream") ||
		got.Header.Get("Accept") !=
			"application/vnd.amazon.eventstream" ||
		bytes.Contains(got.Body, []byte(`"stream"`)) {
		t.Fatalf("upstream request = %#v", got)
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

func TestChatCompletionsBedrockStreamRequiresRequestedUsageMetadata(
	t *testing.T,
) {
	t.Parallel()

	stream := bytes.Join([][]byte{
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStart"),
			[]byte(`{"role":"assistant"}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"end_turn"}`),
		),
	}, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = w.Write(stream)
	}))
	defer upstream.Close()
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(
			upstream.URL,
			config.CapabilityStreamUsage,
		),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{
		"model":"default",
		"messages":[{"role":"user","content":"hello"}],
		"stream":true,
		"stream_options":{"include_usage":true}
	}`)
	records := recorder.snapshot()
	if response.Code != http.StatusOK ||
		strings.Contains(response.Body.String(), "data: [DONE]") ||
		len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Outcome != ledger.OutcomeStreamError ||
		records[0].Attempt.FailureClass !=
			chatBedrockStreamFailureClass {
		t.Fatalf(
			"response=%d body=%s records=%#v",
			response.Code,
			response.Body,
			records,
		)
	}
}

func TestChatCompletionsRejectsInvalidTranslatedBedrockSuccess(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		contentType string
		body        string
	}{
		{
			name:        "unrepresentable output block",
			contentType: "application/json",
			body: `{
				"output":{"message":{"role":"assistant","content":[
					{"reasoningContent":{"reasoningText":{"text":"secret"}}}
				]}},
				"stopReason":"end_turn",
				"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}
			}`,
		},
		{
			name:        "nonstream event content type",
			contentType: "application/vnd.amazon.eventstream",
			body:        "not-an-event-stream",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			handler, err := NewDataHandler(
				bedrockDocument(upstream.URL),
				DataOptions{
					Credentials: bedrockCredentialSource(),
				},
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
			if response.Code != http.StatusBadGateway ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code != "upstream_response_error" {
				t.Fatalf(
					"response=%d body=%s failure=%#v",
					response.Code,
					response.Body,
					failure,
				)
			}
		})
	}
}

func TestChatCompletionsRejectsUnrepresentableBedrockRequestsBeforeTraffic(
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
		bedrockDocument(
			upstream.URL,
			config.CapabilityTools,
			config.CapabilityVision,
			config.CapabilityMultipleChoices,
			config.CapabilityDeveloperMessages,
		),
		DataOptions{Credentials: bedrockCredentialSource()},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "multiple choices",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"n":2}`,
		},
		{
			name: "developer role",
			body: `{"model":"default","messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "remote image",
			body: `{"model":"default","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private.example/image.png"}}]}]}`,
		},
		{
			name: "provider file reference",
			body: `{"model":"default","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file-provider-owned"}}]}]}`,
		},
		{
			name: "disabled parallel calls",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"parallel_tool_calls":false}`,
		},
		{
			name: "unknown semantic field",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"frequency_penalty":0.5}`,
		},
		{
			name: "temperature outside Bedrock range",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}],"temperature":1.5}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := serveChat(t, handler, test.body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf(
					"status = %d, want 400; body=%s",
					response.Code,
					response.Body,
				)
			}
			var failure openAIErrorEnvelope
			if err := json.Unmarshal(
				response.Body.Bytes(),
				&failure,
			); err != nil ||
				failure.Error.Code != "unsupported_feature" {
				t.Fatalf("failure = %#v, %v", failure, err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsMixedPoolKeepsRicherRequestOnOpenAITarget(
	t *testing.T,
) {
	t.Parallel()

	var bedrockCalls atomic.Int64
	bedrock := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		bedrockCalls.Add(1)
	}))
	defer bedrock.Close()
	var openAICalls atomic.Int64
	openAI := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		openAICalls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if !bytes.Contains(body, []byte(`"seed":42`)) {
			t.Errorf("OpenAI request lost seed: %s", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-openai",
			"model":   "upstream-openai",
			"choices": []any{},
		})
	}))
	defer openAI.Close()

	document := bedrockDocument(
		bedrock.URL,
		config.CapabilitySeed,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name: "openai", Type: "openai_compatible",
		BaseURL: openAI.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name: "openai", Provider: "openai",
			Model:        "upstream-openai",
			Capabilities: []config.Capability{config.CapabilitySeed},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{Deployment: "openai", Weight: 1},
	)
	handler, err := NewDataHandler(document, DataOptions{
		Credentials:   bedrockCredentialSource(),
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","messages":[{"role":"user","content":"hi"}],"seed":42}`,
	)
	if response.Code != http.StatusOK ||
		bedrockCalls.Load() != 0 ||
		openAICalls.Load() != 1 {
		t.Fatalf(
			"response=%d bedrock=%d openai=%d body=%s",
			response.Code,
			bedrockCalls.Load(),
			openAICalls.Load(),
			response.Body,
		)
	}
}

func TestChatCompletionsTranslatesBedrockHTTPErrorEnvelope(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"invalid Bedrock input"}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		bedrockDocument(upstream.URL),
		DataOptions{Credentials: bedrockCredentialSource()},
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
		failure.Error.Message != "invalid Bedrock input" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
}

func TestChatCompletionsBedrockTranslationRunsPostGuardrailOnOpenAIShape(
	t *testing.T,
) {
	t.Parallel()

	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"bedrock-payload"`)
		_, _ = io.WriteString(w, `{
			"output":{"message":{"role":"assistant","content":[{"text":"SSN 123"}]}},
			"stopReason":"end_turn",
			"usage":{"inputTokens":5,"outputTokens":2,"totalTokens":7}
		}`)
	}))
	defer primary.Close()
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		input := readGuardrailUpstreamRequest(t, request)
		if !strings.Contains(input.LastContent, `"phase":"post"`) ||
			!strings.Contains(input.LastContent, `"chat.completion"`) ||
			!strings.Contains(input.LastContent, "SSN 123") ||
			strings.Contains(input.LastContent, `"stopReason"`) {
			t.Errorf("post guard input = %#v", input)
		}
		writeGuardrailVerdict(
			w,
			"upstream-guard",
			`{"action":"replace","replacement":{"choices":[{"index":0,"message":{"role":"assistant","content":"[REDACTED]"},"finish_reason":"stop"}]}}`,
		)
	}))
	defer guard.Close()
	document := guardrailDocument(primary.URL, guard.URL+"/v1")
	document.Providers[0] = config.Provider{
		Name:    "provider",
		Type:    "bedrock",
		BaseURL: primary.URL,
		Region:  "us-east-1",
		Auth: config.ProviderAuth{
			Type:       config.AuthAWSSigV4,
			Credential: "workload://aws",
		},
	}
	document.Deployments[0].Model =
		"us.anthropic.claude-sonnet-4-20250514-v1:0"
	document.VirtualModels[0].Guardrails.Post = []config.Guardrail{{
		Name:             "response-pii",
		Model:            "safety",
		AllowReplacement: true,
	}}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: bedrockCredentialSource(),
		Ledger:      recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","messages":[{"role":"user","content":"hi"}]}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "[REDACTED]") ||
		strings.Contains(response.Body.String(), "SSN 123") ||
		response.Header().Get("ETag") != "" {
		t.Fatalf(
			"response=%d etag=%q body=%s",
			response.Code,
			response.Header().Get("ETag"),
			response.Body,
		)
	}
	records := recorder.snapshot()
	var outer *ledger.RequestRecord
	for _, record := range records {
		if record.Request != nil &&
			record.Request.VirtualModel == "public" {
			outer = record.Request
		}
	}
	if outer == nil || outer.Usage.TotalTokens == nil ||
		*outer.Usage.TotalTokens != 7 {
		t.Fatalf("outer usage = %#v", outer)
	}
}
