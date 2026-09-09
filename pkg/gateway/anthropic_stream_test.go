// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestAnthropicMessagesStreamingLifecycleAndUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: message_start\n")
		_, _ = io.WriteString(
			w,
			`data: {"type":"message_start","message":{`+
				`"id":"msg_stream","type":"message","role":"assistant",`+
				`"content":[],"model":"claude-upstream","stop_reason":null,`+
				`"stop_sequence":null,"usage":{"input_tokens":10,`+
				`"cache_creation_input_tokens":3,"cache_read_input_tokens":2,`+
				`"output_tokens":1}}}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			"event: content_block_delta\n"+
				`data: {"type":"content_block_delta","index":0,`+
				`"delta":{"type":"text_delta","text":"hello"}}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			"event: message_delta\n"+
				`data: {"type":"message_delta","delta":{`+
				`"stop_reason":"end_turn","stop_sequence":null},`+
				`"usage":{"output_tokens":5,`+
				`"output_tokens_details":{"thinking_tokens":1}}}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			"event: message_stop\n"+
				`data: {"type":"message_stop"}`+"\n\n",
		)
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
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		anthropicRequest(
			http.MethodPost,
			"/v1/messages",
			`{"model":"public","max_tokens":32,"stream":true,`+
				`"messages":[{"role":"user","content":"hello"}]}`,
		),
	)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	for _, expected := range []string{
		"event: message_start",
		`"model":"public"`,
		"event: content_block_delta",
		"event: message_delta",
		"event: message_stop",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("stream missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Anthropic stream contains OpenAI terminal marker: %s", body)
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
		assertAnthropicUsage(t, usage, 10, 5, 20, 2, 3, 1)
	}
	if records[0].Attempt.Outcome != ledger.OutcomeSuccess ||
		records[1].Request.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("records = %#v", records)
	}
}

func TestAnthropicMessagesStreamingTerminalOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		terminal    string
		wantOutcome ledger.Outcome
		wantFailure string
	}{
		{
			name: "post-200 error event",
			terminal: "event: error\n" +
				`data: {"type":"error","error":{` +
				`"type":"overloaded_error","message":"Overloaded"}}` + "\n\n",
			wantOutcome: ledger.OutcomeUpstreamError,
			wantFailure: "anthropic_stream_error_overloaded_error",
		},
		{
			name:        "EOF before terminal event",
			wantOutcome: ledger.OutcomeStreamError,
			wantFailure: "anthropic_stream_incomplete",
		},
	}
	for _, test := range tests {
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
						`"usage":{"input_tokens":1,"output_tokens":1}}}`+
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
			response := httptest.NewRecorder()
			handler.ServeHTTP(
				response,
				anthropicRequest(
					http.MethodPost,
					"/v1/messages",
					`{"model":"public","max_tokens":1,"stream":true,`+
						`"messages":[{"role":"user","content":"hello"}]}`,
				),
			)
			if response.Code != http.StatusOK {
				t.Fatalf(
					"status = %d; body=%s",
					response.Code,
					response.Body,
				)
			}
			if test.terminal != "" &&
				!strings.Contains(response.Body.String(), "event: error") {
				t.Fatalf("error event was not preserved: %s", response.Body)
			}
			records := recorder.snapshot()
			if len(records) != 2 ||
				records[0].Attempt == nil ||
				records[1].Request == nil {
				t.Fatalf("records = %#v", records)
			}
			for _, result := range []struct {
				outcome ledger.Outcome
				failure string
			}{
				{
					records[0].Attempt.Outcome,
					records[0].Attempt.FailureClass,
				},
				{
					records[1].Request.Outcome,
					records[1].Request.FailureClass,
				},
			} {
				if result.outcome != test.wantOutcome ||
					result.failure != test.wantFailure {
					t.Errorf(
						"outcome/failure = %q/%q, want %q/%q",
						result.outcome,
						result.failure,
						test.wantOutcome,
						test.wantFailure,
					)
				}
			}
		})
	}
}
