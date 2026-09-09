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

func TestChatCompletionsTranslatesToAnthropicMessages(t *testing.T) {
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
		_, _ = io.WriteString(w, `{
			"id":"msg_2",
			"type":"message",
			"role":"assistant",
			"model":"claude-upstream",
			"content":[
				{"type":"text","text":"checking"},
				{"type":"tool_use","id":"call-2","name":"lookup","input":{"city":"Austin"}}
			],
			"stop_reason":"tool_use",
			"stop_sequence":null,
			"usage":{
				"input_tokens":10,
				"cache_creation_input_tokens":3,
				"cache_read_input_tokens":2,
				"output_tokens":5
			}
		}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		anthropicDocument(
			upstream.URL+"/v1",
			config.CapabilityTools,
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
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":false
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
			Details          struct {
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
		translated.Usage.PromptTokens != 15 ||
		translated.Usage.CompletionTokens != 5 ||
		translated.Usage.TotalTokens != 20 ||
		translated.Usage.Details.CachedTokens != 2 {
		t.Fatalf("translated response = %#v", translated)
	}

	got := <-captured
	if got.Path != "/v1/messages" ||
		got.Header.Get("Anthropic-Version") != anthropicAPIVersion ||
		got.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream request = %#v", got)
	}
	for _, forbidden := range []string{
		"max_completion_tokens", "stream_options",
		"parallel_tool_calls", "frequency_penalty",
	} {
		if _, exists := got.Body[forbidden]; exists {
			t.Fatalf(
				"translated request retained Chat field %q: %#v",
				forbidden,
				got.Body,
			)
		}
	}
	if string(got.Body["model"]) != `"claude-upstream"` ||
		string(got.Body["max_tokens"]) != "128" ||
		!bytes.Contains(
			got.Body["system"],
			[]byte(`"type":"text"`),
		) ||
		!bytes.Contains(
			got.Body["messages"],
			[]byte(`"type":"image"`),
		) ||
		!bytes.Contains(
			got.Body["messages"],
			[]byte(`"type":"tool_result"`),
		) ||
		!bytes.Contains(got.Body["tools"], []byte(`"input_schema"`)) ||
		!bytes.Contains(
			got.Body["tool_choice"],
			[]byte(`"disable_parallel_tool_use":true`),
		) {
		t.Fatalf("translated upstream body = %#v", got.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Usage.TotalTokens == nil ||
		*records[0].Attempt.Usage.TotalTokens != 20 {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsTranslatesAnthropicMessagesStream(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_stream","type":"message","role":"assistant","content":[],"model":"claude-upstream","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":9,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"","citations":null}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"checking"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"lookup","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Austin\"}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":3}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		anthropicDocument(
			upstream.URL+"/v1",
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
		"max_tokens":64,
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
		`"content":"checking"`,
		`"tool_calls"`,
		`"arguments":"{\"city\":"`,
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
	_, retainedStreamOptions := got.Body["stream_options"]
	if got.Path != "/v1/messages" ||
		got.Header.Get("Accept") != "text/event-stream" ||
		string(got.Body["stream"]) != "true" ||
		retainedStreamOptions {
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

func TestChatCompletionsAnthropicStreamTerminalFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		terminal        string
		wantBody        string
		wantFailure     string
		wantStreamError bool
	}{
		{
			name: "post-200 error",
			terminal: "event: error\n" +
				`data: {"type":"error","error":{` +
				`"type":"overloaded_error","message":"Overloaded"}}` +
				"\n\n",
			wantBody:    `"code":"upstream_stream_error"`,
			wantFailure: "anthropic_stream_error_overloaded_error",
		},
		{
			name:            "EOF before message stop",
			wantFailure:     chatAnthropicStreamFailureClass,
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
				_, _ = io.WriteString(
					w,
					"event: message_start\n"+
						`data: {"type":"message_start","message":{`+
						`"id":"msg_1","type":"message","role":"assistant",`+
						`"content":[],"model":"claude-upstream",`+
						`"stop_reason":null,"stop_sequence":null,`+
						`"usage":{"input_tokens":1,"output_tokens":0}}}`+
						"\n\n",
				)
				_, _ = io.WriteString(w, test.terminal)
			}))
			defer upstream.Close()

			recorder := &collectingRecorder{}
			handler, err := NewDataHandler(
				anthropicDocument(upstream.URL+"/v1"),
				DataOptions{Ledger: recorder},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveChat(
				t,
				handler,
				`{"model":"default","max_tokens":8,"stream":true,`+
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

func TestChatCompletionsRejectsUnrepresentableAnthropicRequestsBeforeTraffic(
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
		anthropicDocument(
			upstream.URL+"/v1",
			config.CapabilityDeveloperMessages,
			config.CapabilityVision,
			config.CapabilityFileInput,
			config.CapabilityTools,
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
			name: "missing maximum",
			body: `{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "developer role",
			body: `{"model":"default","max_tokens":8,"messages":[{"role":"developer","content":"rules"},{"role":"user","content":"hi"}]}`,
		},
		{
			name: "remote image",
			body: `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://private.example/image.png"}}]}]}`,
		},
		{
			name: "file input",
			body: `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":[{"type":"file","file":{"filename":"a.txt","file_data":"YQ=="}}]}]}`,
		},
		{
			name: "temperature outside Anthropic range",
			body: `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"temperature":1.5}`,
		},
		{
			name: "strict tool schema",
			body: `{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","strict":true}}]}`,
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
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsMixedPoolSkipsIncompatibleAnthropicTarget(
	t *testing.T,
) {
	t.Parallel()

	var anthropicCalls atomic.Int64
	anthropic := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		anthropicCalls.Add(1)
	}))
	defer anthropic.Close()
	var openAICalls atomic.Int64
	openAI := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		openAICalls.Add(1)
		body, _ := io.ReadAll(request.Body)
		if bytes.Contains(body, []byte(`"max_tokens"`)) {
			t.Errorf("OpenAI request unexpectedly gained max_tokens: %s", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-openai",
			"model":   "upstream-openai",
			"choices": []any{},
		})
	}))
	defer openAI.Close()

	document := anthropicDocument(anthropic.URL + "/v1")
	document.Providers = append(document.Providers, config.Provider{
		Name: "openai", Type: "openai_compatible",
		BaseURL: openAI.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name: "openai", Provider: "openai",
			Model: "upstream-openai",
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
		`{"model":"default","messages":[{"role":"user","content":"hi"}]}`,
	)
	if response.Code != http.StatusOK ||
		anthropicCalls.Load() != 0 ||
		openAICalls.Load() != 1 {
		t.Fatalf(
			"response=%d anthropic=%d openai=%d body=%s",
			response.Code,
			anthropicCalls.Load(),
			openAICalls.Load(),
			response.Body,
		)
	}
}

func TestChatCompletionsTranslatesAnthropicHTTPErrorEnvelope(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{
			"type":"error",
			"error":{"type":"invalid_request_error","message":"invalid Anthropic input"},
			"request_id":"req_upstream"
		}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		anthropicDocument(upstream.URL+"/v1"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusBadRequest ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "upstream_error" ||
		failure.Error.Message != "invalid Anthropic input" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
}

func TestChatCompletionsRejectsUnrepresentableAnthropicSuccess(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"msg_1",
			"type":"message",
			"role":"assistant",
			"model":"claude-upstream",
			"content":[{"type":"thinking","thinking":"secret","signature":"sig"}],
			"stop_reason":"end_turn",
			"usage":{"input_tokens":1,"output_tokens":1}
		}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		anthropicDocument(upstream.URL+"/v1"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"default","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusBadGateway ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "upstream_response_error" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
}
