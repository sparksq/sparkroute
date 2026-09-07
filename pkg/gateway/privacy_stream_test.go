package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/privacy"
)

func TestStreamPIIChatRestoresSplitKnownTokenAndRedactsSplitNovelPII(t *testing.T) {
	t.Parallel()

	session, token := deterministicStreamPIISession(t)
	recorder := httptest.NewRecorder()
	trace := newSavedTraceResponseWriter(
		recorder,
		&savedTraceCollector{},
		1<<20,
	)
	observation := &requestObservation{}
	policy := streamPIITestPolicy()
	output := newStreamPIIOutputWriter(
		context.Background(), trace, trace, observation, session, policy, piiStreamSSE,
	)
	masker := newStreamPIIMaskWriter(
		context.Background(), output, observation,
		openAIOperationChatCompletions, session, policy, piiStreamSSE,
	)
	fragments := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"known ` + token[:14] + `"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"` + token[14:] + ` novel other@"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"example.com done"},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, fragment := range fragments {
		if _, err := masker.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := masker.Finish(); err != nil {
		t.Fatal(err)
	}
	callerText := chatStreamText(t, recorder.Body.String())
	if callerText != "known known@example.com novel [[SPARKROUTE_PII_EMAIL_REDACTED]] done" {
		t.Fatalf("caller stream text = %q; wire = %s", callerText, recorder.Body)
	}
	maskedWire := trace.payload().Body
	if strings.Contains(maskedWire, "known@example.com") ||
		strings.Contains(maskedWire, "other@example.com") ||
		!strings.Contains(maskedWire, token) ||
		!strings.Contains(maskedWire, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
		t.Fatalf("masked trace = %s", maskedWire)
	}
}

func TestStreamPIIBedrockRestoresAndRechecksFrameCRCs(t *testing.T) {
	t.Parallel()

	session, token := deterministicStreamPIISession(t)
	var caller bytes.Buffer
	observation := &requestObservation{}
	policy := streamPIITestPolicy()
	output := newStreamPIIOutputWriter(
		context.Background(), &caller, nil, observation, session, policy, piiStreamAWS,
	)
	masker := newStreamPIIMaskWriter(
		context.Background(), output, observation,
		bedrockOperationConverseStream, session, policy, piiStreamAWS,
	)
	frames := [][]byte{
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"), []byte(
			`{"contentBlockIndex":0,"delta":{"text":"known `+token[:13]+`"}}`,
		)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"), []byte(
			`{"contentBlockIndex":0,"delta":{"text":"`+token[13:]+` novel other@"}}`,
		)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"), []byte(
			`{"contentBlockIndex":0,"delta":{"text":"example.com"}}`,
		)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockStop"), []byte(
			`{"contentBlockIndex":0}`,
		)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"), []byte(
			`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"email\":\"other@"}}}`,
		)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"), []byte(
			`{"contentBlockIndex":1,"delta":{"toolUse":{"input":"example.com\"}"}}}`,
		)),
		encodeAWSEventMessage(
			t, bedrockEventHeaders("messageStop"), []byte(`{"stopReason":"end_turn"}`),
		),
	}
	for _, frame := range frames {
		if _, err := masker.Write(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := masker.Finish(); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	reader := bytes.NewReader(caller.Bytes())
	for {
		message, err := readAWSEventMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		var payload struct {
			Delta struct {
				Text    string `json:"text"`
				ToolUse struct {
					Input string `json:"input"`
				} `json:"toolUse"`
			} `json:"delta"`
		}
		if json.Unmarshal(message.payload, &payload) == nil {
			text.WriteString(payload.Delta.Text)
			text.WriteString(payload.Delta.ToolUse.Input)
		}
	}
	if got := text.String(); got !=
		"known known@example.com novel [[SPARKROUTE_PII_EMAIL_REDACTED]]"+
			`{"email":"[[SPARKROUTE_PII_EMAIL_REDACTED]]"}` {
		t.Fatalf("Bedrock caller text = %q", got)
	}
}

func TestStreamPIIRedactsSplitNativeProtocolAndToolDeltas(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		operation openAIOperation
		events    []string
	}{
		"chat tool arguments": {
			operation: openAIOperationChatCompletions,
			events: []string{
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"email\":\"drew@"}}]}}]}` + "\n\n",
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"example.com\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
				"data: [DONE]\n\n",
			},
		},
		"responses function arguments": {
			operation: openAIOperationResponses,
			events: []string{
				`event: response.function_call_arguments.delta` + "\n" +
					`data: {"type":"response.function_call_arguments.delta","item_id":"call_1","output_index":0,"delta":"{\"email\":\"drew@"}` + "\n\n",
				`event: response.function_call_arguments.delta` + "\n" +
					`data: {"type":"response.function_call_arguments.delta","item_id":"call_1","output_index":0,"delta":"example.com\"}"}` + "\n\n",
				`event: response.completed` + "\n" +
					`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[]}}` + "\n\n",
			},
		},
		"anthropic input json": {
			operation: anthropicOperationMessages,
			events: []string{
				`event: content_block_delta` + "\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"email\":\"drew@"}}` + "\n\n",
				`event: content_block_delta` + "\n" +
					`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"example.com\"}"}}` + "\n\n",
				`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n",
			},
		},
		"gemini text": {
			operation: geminiOperationStreamContent,
			events: []string{
				`data: {"candidates":[{"index":0,"content":{"parts":[{"text":"drew@"}]}}]}` + "\n\n",
				`data: {"candidates":[{"index":0,"content":{"parts":[{"text":"example.com"}]},"finishReason":"STOP"}]}` + "\n\n",
			},
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			session, _ := deterministicStreamPIISession(t)
			var output bytes.Buffer
			observation := &requestObservation{}
			policy := streamPIITestPolicy()
			policy.Response = config.PIIResponseMasked
			lower := newStreamPIIOutputWriter(
				context.Background(), &output, nil, observation,
				session, policy, piiStreamSSE,
			)
			masker := newStreamPIIMaskWriter(
				context.Background(), lower, observation,
				test.operation, session, policy, piiStreamSSE,
			)
			for _, event := range test.events {
				if _, err := masker.Write([]byte(event)); err != nil {
					t.Fatal(err)
				}
			}
			if err := masker.Finish(); err != nil {
				t.Fatal(err)
			}
			wire := output.String()
			if strings.Contains(wire, "drew@example.com") ||
				!strings.Contains(wire, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
				t.Fatalf("privacy-screened stream = %s", wire)
			}
		})
	}
}

func deterministicStreamPIISession(t *testing.T) (privacy.Session, string) {
	t.Helper()
	session := newTestPrivacySession()
	token, err := session.Substitute(context.Background(), "known@example.com")
	if err != nil {
		t.Fatal(err)
	}
	return session, token
}

func streamPIITestPolicy() config.EffectivePIIPolicy {
	return config.EffectivePIIPolicy{
		Mode: config.PIIModeSubstitute, Response: config.PIIResponseRestore,
		TraceContent: config.PIITraceMasked, FailureMode: config.PIIFailClosed,
	}
}

func chatStreamText(t *testing.T, wire string) string {
	t.Helper()
	var result strings.Builder
	if err := proxySSEEvents(
		io.Discard, strings.NewReader(wire), nil,
		func(payload []byte) error {
			if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
				return nil
			}
			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				return err
			}
			for _, choice := range event.Choices {
				result.WriteString(choice.Delta.Content)
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	return result.String()
}
