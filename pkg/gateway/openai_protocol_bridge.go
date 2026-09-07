// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	llmprotocol "github.com/scitrera/go-llm/protocol"
	protocolcodec "github.com/scitrera/go-llm/protocol/codec"
)

var openAIProtocolRegistry = protocolcodec.NewDefaultRegistry()

func translateResponsesRequestToChat(raw []byte) ([]byte, error) {
	translated, err := openAIProtocolRegistry.TranslateRequest(
		llmprotocol.FormatOpenAIResponses,
		llmprotocol.FormatOpenAIChat,
		raw,
		llmprotocol.StrictPolicy(),
	)
	if err != nil {
		return nil, unsupportedFeature(
			"Responses request cannot be represented by Chat Completions: %v",
			err,
		)
	}
	body := append([]byte(nil), translated.Body...)
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) == nil && rawJSONBool(envelope["stream"]) {
		streamOptions := map[string]json.RawMessage{}
		_ = json.Unmarshal(envelope["stream_options"], &streamOptions)
		streamOptions["include_usage"] = json.RawMessage("true")
		encoded, marshalErr := json.Marshal(streamOptions)
		if marshalErr != nil {
			return nil, marshalErr
		}
		envelope["stream_options"] = encoded
		body, marshalErr = json.Marshal(envelope)
		if marshalErr != nil {
			return nil, marshalErr
		}
	}
	return body, nil
}

func rewriteTranslatedOpenAIModel(body []byte, model string) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, fmt.Errorf("translated OpenAI request must be a JSON object")
	}
	return rewriteModel(envelope, model)
}

func translateChatResponseToResponses(raw []byte) ([]byte, error) {
	translated, err := openAIProtocolRegistry.TranslateResponse(
		llmprotocol.FormatOpenAIChat,
		llmprotocol.FormatOpenAIResponses,
		raw,
		llmprotocol.StrictPolicy(),
	)
	if err != nil {
		return nil, fmt.Errorf("translate Chat Completions response to Responses: %w", err)
	}
	return append([]byte(nil), translated.Body...), nil
}

// chatToResponsesStreamWriter maps one complete upstream Chat SSE event into
// zero or more Responses SSE events. The surrounding SSE parser provides one
// complete event per Write call, so memory remains bounded by the gateway's
// existing per-event limit.
type chatToResponsesStreamWriter struct {
	destination io.Writer
	state       protocolcodec.StreamState
	model       string
	onPayload   func([]byte) error
}

func (w *chatToResponsesStreamWriter) Write(event []byte) (int, error) {
	payload, exists := sseDataPayload(event)
	if !exists {
		if _, err := w.destination.Write(event); err != nil {
			return 0, err
		}
		return len(event), nil
	}
	wire := llmprotocol.WireEvent{Event: sseEventType(event), Data: payload}
	translated, _, err := openAIProtocolRegistry.TranslateStreamEvent(
		&w.state,
		llmprotocol.FormatOpenAIChat,
		llmprotocol.FormatOpenAIResponses,
		wire,
		llmprotocol.StrictPolicy(),
	)
	if err != nil {
		return 0, fmt.Errorf("translate Chat Completions stream to Responses: %w", err)
	}
	for _, output := range translated {
		encoded := encodeSSEWireEvent(output)
		if w.model != "" {
			encoded = rewriteResponsesSSEEventModel(encoded, w.model)
		}
		if w.onPayload != nil {
			if err := w.onPayload(output.Data); err != nil {
				return 0, err
			}
		}
		if _, err := w.destination.Write(encoded); err != nil {
			return 0, err
		}
	}
	return len(event), nil
}

func (w *chatToResponsesStreamWriter) Flush() {
	if flusher, ok := w.destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func proxyChatStreamToResponses(
	destination io.Writer,
	source io.Reader,
	model string,
	onPayload func([]byte) error,
) error {
	writer := &chatToResponsesStreamWriter{
		destination: destination,
		model:       model,
		onPayload:   onPayload,
	}
	return proxySSEEvents(writer, source, nil, nil)
}

func sseEventType(event []byte) string {
	for _, line := range splitSSELines(event) {
		content := line.content
		if bytes.HasPrefix(content, []byte("event:")) {
			return string(bytes.TrimSpace(content[len("event:"):]))
		}
	}
	return ""
}

func encodeSSEWireEvent(event llmprotocol.WireEvent) []byte {
	var output bytes.Buffer
	if event.Event != "" {
		output.WriteString("event: ")
		output.WriteString(event.Event)
		output.WriteByte('\n')
	}
	output.WriteString("data: ")
	output.Write(event.Data)
	output.WriteString("\n\n")
	return output.Bytes()
}
