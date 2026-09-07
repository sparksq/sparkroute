// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package gateway

import (
	"bytes"
	"strings"
	"testing"
)

func TestSharedProtocolBridgeTranslatesStatelessResponsesRequest(t *testing.T) {
	t.Parallel()
	body, err := translateResponsesRequestToChat([]byte(`{
		"model":"logical", "input":"hello", "store":false,
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"messages"`)) ||
		!bytes.Contains(body, []byte(`"lookup"`)) {
		t.Fatalf("translated body = %s", body)
	}
}

func TestSharedProtocolBridgeRejectsResponsesState(t *testing.T) {
	t.Parallel()
	_, err := translateResponsesRequestToChat([]byte(`{
		"model":"logical", "input":"continue", "previous_response_id":"resp_1"
	}`))
	if err == nil || !strings.Contains(err.Error(), "Responses request cannot be represented") {
		t.Fatalf("translation error = %v", err)
	}
}

func TestSharedProtocolBridgeTranslatesChatStream(t *testing.T) {
	t.Parallel()
	input := strings.Join([]string{
		`data: {"id":"chat_1","model":"upstream","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"chat_1","model":"upstream","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		`data: {"id":"chat_1","model":"upstream","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: {"id":"chat_1","model":"upstream","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")
	var output bytes.Buffer
	if err := proxyChatStreamToResponses(&output, strings.NewReader(input), "logical", nil); err != nil {
		t.Fatal(err)
	}
	translated := output.String()
	for _, event := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.output_text.delta",
		"event: response.completed",
		`"total_tokens":3`,
		`"model":"logical"`,
	} {
		if !strings.Contains(translated, event) {
			t.Fatalf("translated stream is missing %q:\n%s", event, translated)
		}
	}
}
