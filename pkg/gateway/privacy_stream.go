// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/privacy"
)

type piiStreamFormat uint8

const (
	piiStreamSSE piiStreamFormat = iota
	piiStreamAWS
	maximumPIIStreamChannels     = 4096
	maximumPIIStreamPendingBytes = 4 << 20
)

type streamPIIContext struct {
	session     privacy.Session
	policy      config.EffectivePIIPolicy
	trace       *savedTraceResponseWriter
	observation *requestObservation
}

type streamResponsePipeline struct {
	destination io.Writer
	privacy     *streamPIIMaskWriter
	guardrail   *streamPostGuardrailWriter
}

func newStreamResponsePipeline(
	destination io.Writer,
	handler *chatCompletionsHandler,
	request *http.Request,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	enableGuardrail bool,
	operation openAIOperation,
	format piiStreamFormat,
	privacyContext streamPIIContext,
) *streamResponsePipeline {
	pipeline := &streamResponsePipeline{destination: destination}
	lower := destination
	if privacyContext.session != nil {
		lower = newStreamPIIOutputWriter(
			request.Context(),
			lower,
			privacyContext.trace,
			privacyContext.observation,
			privacyContext.session,
			privacyContext.policy,
			format,
		)
	}
	if enableGuardrail && len(guardrails.Post) != 0 && guardrails.Stream != nil {
		if format == piiStreamAWS {
			pipeline.guardrail = newBedrockStreamPostGuardrailWriter(
				lower, handler, request, guardrails.Post,
				requestBody, guardrails.Stream.Effective(),
			)
		} else {
			pipeline.guardrail = newStreamPostGuardrailWriter(
				lower, handler, request, guardrails.Post,
				requestBody, guardrails.Stream.Effective(),
			)
		}
		lower = pipeline.guardrail
	}
	if privacyContext.session != nil {
		pipeline.privacy = newStreamPIIMaskWriter(
			request.Context(),
			lower,
			privacyContext.observation,
			operation,
			privacyContext.session,
			privacyContext.policy,
			format,
		)
		lower = pipeline.privacy
	}
	pipeline.destination = lower
	return pipeline
}

func (p *streamResponsePipeline) Finish(err error) error {
	if err == nil && p != nil && p.privacy != nil {
		err = p.privacy.Finish()
	}
	if err == nil && p != nil && p.guardrail != nil {
		err = p.guardrail.Finish()
	}
	return err
}

func writePIIBufferedResponse(
	destination http.ResponseWriter,
	source *bufferedResponseWriter,
	operation openAIOperation,
	privacyContext streamPIIContext,
	ctx context.Context,
) error {
	callerBody := source.body.Bytes()
	traceBody, err := redactAllPIIJSONStrings(
		ctx, callerBody, privacyContext.session,
	)
	if err != nil {
		privacyContext.observation.disableSavedTrace()
		if privacyContext.policy.FailureMode == config.PIIFailClosed {
			return &streamPIIError{cause: err}
		}
		return writeBufferedResponse(destination, source, callerBody, operation)
	}
	callerBody = traceBody
	if privacyContext.policy.Response == config.PIIResponseRestore {
		callerBody, err = restoreAllPIIJSONStrings(
			ctx, traceBody, privacyContext.session,
		)
		if err != nil {
			privacyContext.observation.disableSavedTrace()
			if privacyContext.policy.FailureMode == config.PIIFailClosed {
				return &streamPIIError{cause: err}
			}
			callerBody = traceBody
		}
	}
	if privacyContext.policy.TraceContent == config.PIITraceMasked {
		privacyContext.trace.overrideCapture(traceBody)
	}
	return writeBufferedResponse(destination, source, callerBody, operation)
}

type streamPIIError struct{ cause error }

func (e *streamPIIError) Error() string {
	return "PII stream transformation failed: " + e.cause.Error()
}

func (e *streamPIIError) Unwrap() error { return e.cause }

// streamPIIOutputWriter receives privacy-screened frames after optional
// guardrails. It captures that representation for masked traces and restores
// only request-minted tokens in the caller-facing representation.
type streamPIIOutputWriter struct {
	ctx         context.Context
	destination io.Writer
	trace       *savedTraceResponseWriter
	observation *requestObservation
	session     privacy.Session
	policy      config.EffectivePIIPolicy
	format      piiStreamFormat
}

func newStreamPIIOutputWriter(
	ctx context.Context,
	destination io.Writer,
	trace *savedTraceResponseWriter,
	observation *requestObservation,
	session privacy.Session,
	policy config.EffectivePIIPolicy,
	format piiStreamFormat,
) *streamPIIOutputWriter {
	if policy.TraceContent == config.PIITraceMasked {
		trace.overrideCapture(nil)
	}
	return &streamPIIOutputWriter{
		ctx: ctx, destination: destination, trace: trace,
		observation: observation, session: session, policy: policy,
		format: format,
	}
}

func (w *streamPIIOutputWriter) Write(masked []byte) (int, error) {
	if w.policy.TraceContent == config.PIITraceMasked {
		w.trace.appendOverrideCapture(masked)
	}
	caller := masked
	if w.policy.Response == config.PIIResponseRestore {
		var err error
		caller, err = transformPIIStreamFrame(
			w.ctx,
			masked,
			w.format,
			func(_ context.Context, value string) (string, error) {
				return w.session.Restore(value), nil
			},
		)
		if err != nil {
			w.observation.disableSavedTrace()
			if w.policy.FailureMode == config.PIIFailClosed {
				return 0, &streamPIIError{cause: err}
			}
			caller = masked
		}
	}
	if _, err := w.destination.Write(caller); err != nil {
		return 0, err
	}
	return len(masked), nil
}

func (w *streamPIIOutputWriter) Flush() {
	if flusher, ok := w.destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

type streamPIIChannel struct {
	sequence int
	redactor privacy.StreamRedactor
	flush    func(string) ([]byte, error)
}

// streamPIIMaskWriter receives one complete SSE event or AWS event-stream
// message per Write call. It redacts novel PII across arbitrary semantic delta
// boundaries while leaving known opaque tokens for the lower restoration
// writer.
type streamPIIMaskWriter struct {
	ctx          context.Context
	destination  io.Writer
	observation  *requestObservation
	operation    openAIOperation
	session      privacy.Session
	policy       config.EffectivePIIPolicy
	format       piiStreamFormat
	channels     map[string]*streamPIIChannel
	nextChannel  int
	pendingBytes int
	passthrough  bool
}

func newStreamPIIMaskWriter(
	ctx context.Context,
	destination io.Writer,
	observation *requestObservation,
	operation openAIOperation,
	session privacy.Session,
	policy config.EffectivePIIPolicy,
	format piiStreamFormat,
) *streamPIIMaskWriter {
	return &streamPIIMaskWriter{
		ctx: ctx, destination: destination, observation: observation,
		operation: operation, session: session, policy: policy, format: format,
		channels: make(map[string]*streamPIIChannel),
	}
}

func (w *streamPIIMaskWriter) Write(frame []byte) (int, error) {
	if w.passthrough {
		if _, err := w.destination.Write(frame); err != nil {
			return 0, err
		}
		return len(frame), nil
	}
	var err error
	switch w.format {
	case piiStreamSSE:
		err = w.writeSSE(frame)
	case piiStreamAWS:
		err = w.writeAWS(frame)
	default:
		err = fmt.Errorf("unknown PII stream format")
	}
	if err == nil {
		return len(frame), nil
	}
	if w.policy.FailureMode == config.PIIFailClosed {
		return 0, &streamPIIError{cause: err}
	}
	w.observation.disableSavedTrace()
	if flushErr := w.abortPending(); flushErr != nil {
		return 0, flushErr
	}
	w.passthrough = true
	if _, writeErr := w.destination.Write(frame); writeErr != nil {
		return 0, writeErr
	}
	return len(frame), nil
}

func (w *streamPIIMaskWriter) Flush() {
	if flusher, ok := w.destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (w *streamPIIMaskWriter) Finish() error {
	if w.passthrough {
		return nil
	}
	if err := w.flushPending(); err != nil {
		if w.policy.FailureMode == config.PIIFailClosed {
			return &streamPIIError{cause: err}
		}
		w.observation.disableSavedTrace()
		if abortErr := w.abortPending(); abortErr != nil {
			return abortErr
		}
		w.passthrough = true
	}
	return nil
}

func (w *streamPIIMaskWriter) writeSSE(event []byte) error {
	payload, exists := sseDataPayload(event)
	if !exists {
		_, err := w.destination.Write(event)
		return err
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		if err := w.flushPending(); err != nil {
			return err
		}
		_, err := w.destination.Write(event)
		return err
	}
	document, err := decodePIIJSON(payload)
	if err != nil {
		return err
	}
	root, ok := document.(map[string]any)
	if !ok {
		return fmt.Errorf("PII stream event payload must be a JSON object")
	}
	terminal := piiSSETerminal(w.operation, root, payload)
	fields := piiSSEFields(w.operation, event, root)
	if boundary := piiSSEBoundaryChannel(w.operation, root); boundary != "" {
		if err := w.flushMatching(func(key string) bool {
			return strings.HasPrefix(key, boundary)
		}); err != nil {
			return err
		}
	}
	if terminal {
		if err := w.flushPendingExcept(fieldChannels(fields)); err != nil {
			return err
		}
	}
	if err := w.transformFields(fields, terminal); err != nil {
		return err
	}
	if err := maskPIIJSONDocument(w.ctx, &document, w.session); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if _, err := w.destination.Write(replaceSSEDataPayload(event, encoded)); err != nil {
		return err
	}
	if terminal {
		w.channels = make(map[string]*streamPIIChannel)
		w.pendingBytes = 0
	}
	return nil
}

func (w *streamPIIMaskWriter) writeAWS(raw []byte) error {
	message, err := readAWSEventMessage(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	document, err := decodePIIJSON(message.payload)
	if err != nil {
		return err
	}
	root, ok := document.(map[string]any)
	if !ok {
		return fmt.Errorf("PII AWS stream payload must be a JSON object")
	}
	terminal, _, _ := inspectBedrockStreamMessage(message)
	fields := piiBedrockFields(message, root)
	if message.header(":event-type") == "contentBlockStop" {
		boundary := "bedrock:" + streamIndex(root["contentBlockIndex"], 0) + ":"
		if err := w.flushMatching(func(key string) bool {
			return strings.HasPrefix(key, boundary)
		}); err != nil {
			return err
		}
	}
	if terminal {
		if err := w.flushPendingExcept(fieldChannels(fields)); err != nil {
			return err
		}
	}
	if err := w.transformFields(fields, terminal); err != nil {
		return err
	}
	if err := maskPIIJSONDocument(w.ctx, &document, w.session); err != nil {
		return err
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	output, err := replaceAWSEventPayload(message, encoded)
	if err != nil {
		return err
	}
	if _, err := w.destination.Write(output); err != nil {
		return err
	}
	if terminal {
		w.channels = make(map[string]*streamPIIChannel)
		w.pendingBytes = 0
	}
	return nil
}

type streamPIIField struct {
	channel string
	value   string
	set     func(string)
	flush   func(string) ([]byte, error)
}

func (w *streamPIIMaskWriter) transformFields(fields []streamPIIField, final bool) error {
	for _, field := range fields {
		channel := w.channels[field.channel]
		if channel == nil {
			if len(w.channels) >= maximumPIIStreamChannels {
				return fmt.Errorf(
					"PII stream exceeds %d semantic channels",
					maximumPIIStreamChannels,
				)
			}
			redactor, err := w.session.NewStreamRedactor(privacy.StreamOptions{})
			if err != nil {
				return err
			}
			channel = &streamPIIChannel{sequence: w.nextChannel, redactor: redactor}
			w.nextChannel++
			w.channels[field.channel] = channel
		}
		channel.flush = field.flush
		previousPending := len(channel.redactor.Pending())
		masked, err := channel.redactor.Transform(w.ctx, field.value, final)
		if err != nil {
			return err
		}
		w.pendingBytes += len(channel.redactor.Pending()) - previousPending
		if w.pendingBytes > maximumPIIStreamPendingBytes {
			return fmt.Errorf(
				"PII stream pending suffixes exceed %d bytes",
				maximumPIIStreamPendingBytes,
			)
		}
		field.set(masked)
		if final {
			delete(w.channels, field.channel)
		}
	}
	return nil
}

func (w *streamPIIMaskWriter) orderedChannels() []string {
	keys := make([]string, 0, len(w.channels))
	for key := range w.channels {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return w.channels[keys[left]].sequence < w.channels[keys[right]].sequence
	})
	return keys
}

func (w *streamPIIMaskWriter) flushPending() error {
	return w.flushPendingExcept(nil)
}

func (w *streamPIIMaskWriter) flushPendingExcept(excluded map[string]struct{}) error {
	for _, key := range w.orderedChannels() {
		if _, skip := excluded[key]; skip {
			continue
		}
		if err := w.flushChannel(key); err != nil {
			return err
		}
	}
	return nil
}

func (w *streamPIIMaskWriter) flushMatching(match func(string) bool) error {
	for _, key := range w.orderedChannels() {
		if !match(key) {
			continue
		}
		if err := w.flushChannel(key); err != nil {
			return err
		}
	}
	return nil
}

func (w *streamPIIMaskWriter) flushChannel(key string) error {
	channel := w.channels[key]
	if channel == nil {
		return nil
	}
	previousPending := len(channel.redactor.Pending())
	masked, err := channel.redactor.Transform(w.ctx, "", true)
	if err != nil {
		return err
	}
	w.pendingBytes -= previousPending
	if masked != "" && channel.flush != nil {
		frame, err := channel.flush(masked)
		if err != nil {
			return err
		}
		frame, err = transformPIIStreamFrame(
			w.ctx, frame, w.format, w.session.Redact,
		)
		if err != nil {
			return err
		}
		if _, err := w.destination.Write(frame); err != nil {
			return err
		}
	}
	delete(w.channels, key)
	return nil
}

func fieldChannels(fields []streamPIIField) map[string]struct{} {
	if len(fields) == 0 {
		return nil
	}
	result := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		result[field.channel] = struct{}{}
	}
	return result
}

func (w *streamPIIMaskWriter) abortPending() error {
	for _, key := range w.orderedChannels() {
		channel := w.channels[key]
		previousPending := len(channel.redactor.Pending())
		pending := channel.redactor.Abort("")
		w.pendingBytes -= previousPending
		if pending != "" && channel.flush != nil {
			frame, err := channel.flush(pending)
			if err != nil {
				return err
			}
			if _, err := w.destination.Write(frame); err != nil {
				return err
			}
		}
		delete(w.channels, key)
	}
	return nil
}

func maskPIIJSONDocument(
	ctx context.Context,
	document *any,
	session privacy.Session,
) error {
	walker := &piiJSONTransformer{ctx: ctx, transform: session.Redact}
	return transformAllStrings(walker, document)
}

func transformPIIStreamFrame(
	ctx context.Context,
	frame []byte,
	format piiStreamFormat,
	transform func(context.Context, string) (string, error),
) ([]byte, error) {
	switch format {
	case piiStreamSSE:
		payload, exists := sseDataPayload(frame)
		if !exists || bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			return append([]byte(nil), frame...), nil
		}
		document, err := decodePIIJSON(payload)
		if err != nil {
			return nil, err
		}
		walker := &piiJSONTransformer{ctx: ctx, transform: transform}
		if err := transformAllStrings(walker, &document); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		return replaceSSEDataPayload(frame, encoded), nil
	case piiStreamAWS:
		message, err := readAWSEventMessage(bytes.NewReader(frame))
		if err != nil {
			return nil, err
		}
		document, err := decodePIIJSON(message.payload)
		if err != nil {
			return nil, err
		}
		walker := &piiJSONTransformer{ctx: ctx, transform: transform}
		if err := transformAllStrings(walker, &document); err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		return replaceAWSEventPayload(message, encoded)
	default:
		return nil, fmt.Errorf("unknown PII stream format")
	}
}

func replaceSSEDataPayload(event []byte, payload []byte) []byte {
	lines := splitSSELines(event)
	firstData := -1
	for index := range lines {
		if bytes.Equal(lines[index].content, []byte("data")) ||
			bytes.HasPrefix(lines[index].content, []byte("data:")) {
			lines[index].isData = true
			if firstData < 0 {
				firstData = index
			}
		}
	}
	if firstData < 0 {
		return append([]byte(nil), event...)
	}
	var result bytes.Buffer
	result.Grow(len(event) + len(payload))
	for index, line := range lines {
		if !line.isData {
			result.Write(line.raw)
			continue
		}
		if index == firstData {
			result.WriteString("data: ")
			result.Write(payload)
			result.Write(line.ending)
		}
	}
	return result.Bytes()
}

func piiSSETerminal(operation openAIOperation, root map[string]any, payload []byte) bool {
	terminal, _, _ := operation.inspectStreamEvent(payload)
	if terminal {
		return true
	}
	if operation == geminiOperationStreamContent {
		candidates, _ := root["candidates"].([]any)
		for _, rawCandidate := range candidates {
			candidate, _ := rawCandidate.(map[string]any)
			if value, _ := candidate["finishReason"].(string); value != "" {
				return true
			}
		}
	}
	if operation == openAIOperationChatCompletions {
		choices, _ := root["choices"].([]any)
		for _, rawChoice := range choices {
			choice, _ := rawChoice.(map[string]any)
			if value, exists := choice["finish_reason"]; exists && value != nil {
				return true
			}
		}
	}
	return false
}

func piiSSEBoundaryChannel(operation openAIOperation, root map[string]any) string {
	switch operation {
	case anthropicOperationMessages:
		if root["type"] == "content_block_stop" {
			return "anthropic:" + streamIndex(root["index"], 0) + ":"
		}
	case openAIOperationResponses:
		eventType, _ := root["type"].(string)
		if strings.HasSuffix(eventType, ".done") {
			deltaType := strings.TrimSuffix(eventType, ".done") + ".delta"
			channel := "responses:" + deltaType
			for _, name := range []string{"item_id", "output_index", "content_index"} {
				channel += ":" + fmt.Sprint(root[name])
			}
			return channel
		}
	}
	return ""
}

func piiSSEFields(
	operation openAIOperation,
	event []byte,
	root map[string]any,
) []streamPIIField {
	switch operation {
	case openAIOperationChatCompletions:
		return piiChatStreamFields(event, root)
	case openAIOperationResponses:
		return piiResponsesStreamFields(event, root)
	case anthropicOperationMessages:
		return piiAnthropicStreamFields(event, root)
	case geminiOperationStreamContent:
		return piiGeminiStreamFields(event, root)
	default:
		return nil
	}
}

func piiChatStreamFields(event []byte, root map[string]any) []streamPIIField {
	var fields []streamPIIField
	choices, _ := root["choices"].([]any)
	for choicePosition, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		choiceIndex := streamIndex(choice["index"], choicePosition)
		for _, name := range []string{"content", "refusal"} {
			value, ok := delta[name].(string)
			if !ok {
				continue
			}
			fieldName := name
			fields = append(fields, streamPIIField{
				channel: "chat:" + choiceIndex + ":" + fieldName,
				value:   value,
				set:     func(value string) { delta[fieldName] = value },
				flush:   chatSSEFlush(event, root, choice["index"], fieldName, nil),
			})
		}
		if functionCall, _ := delta["function_call"].(map[string]any); functionCall != nil {
			if value, ok := functionCall["arguments"].(string); ok {
				fields = append(fields, streamPIIField{
					channel: "chat:" + choiceIndex + ":function_call",
					value:   value,
					set: func(value string) {
						functionCall["arguments"] = value
					},
					flush: chatSSEFlush(event, root, choice["index"], "arguments", "function_call"),
				})
			}
		}
		toolCalls, _ := delta["tool_calls"].([]any)
		for toolPosition, rawTool := range toolCalls {
			tool, _ := rawTool.(map[string]any)
			function, _ := tool["function"].(map[string]any)
			value, ok := function["arguments"].(string)
			if !ok {
				continue
			}
			toolIndex := streamIndex(tool["index"], toolPosition)
			fields = append(fields, streamPIIField{
				channel: "chat:" + choiceIndex + ":tool:" + toolIndex,
				value:   value,
				set:     func(value string) { function["arguments"] = value },
				flush:   chatSSEFlush(event, root, choice["index"], "arguments", tool["index"]),
			})
		}
	}
	return fields
}

func chatSSEFlush(
	event []byte,
	root map[string]any,
	choiceIndex any,
	field string,
	kind any,
) func(string) ([]byte, error) {
	return func(value string) ([]byte, error) {
		delta := map[string]any{}
		switch kind {
		case "function_call":
			delta["function_call"] = map[string]any{"arguments": value}
		case nil:
			delta[field] = value
		default:
			delta["tool_calls"] = []any{map[string]any{
				"index":    kind,
				"function": map[string]any{"arguments": value},
			}}
		}
		choice := map[string]any{"delta": delta}
		if choiceIndex != nil {
			choice["index"] = choiceIndex
		}
		payload := copyStreamEnvelope(root, "id", "object", "created", "model")
		payload["choices"] = []any{choice}
		return marshalSSELike(event, payload)
	}
}

func piiResponsesStreamFields(event []byte, root map[string]any) []streamPIIField {
	eventType, _ := root["type"].(string)
	if !strings.HasSuffix(eventType, ".delta") {
		return nil
	}
	value, ok := root["delta"].(string)
	if !ok {
		return nil
	}
	channel := "responses:" + eventType
	for _, name := range []string{"item_id", "output_index", "content_index"} {
		channel += ":" + fmt.Sprint(root[name])
	}
	return []streamPIIField{{
		channel: channel,
		value:   value,
		set:     func(value string) { root["delta"] = value },
		flush: func(value string) ([]byte, error) {
			payload := copyStreamEnvelope(
				root, "type", "item_id", "output_index", "content_index", "sequence_number",
			)
			payload["delta"] = value
			return marshalSSELike(event, payload)
		},
	}}
}

func piiAnthropicStreamFields(event []byte, root map[string]any) []streamPIIField {
	if root["type"] != "content_block_delta" {
		return nil
	}
	delta, _ := root["delta"].(map[string]any)
	if delta == nil {
		return nil
	}
	index := streamIndex(root["index"], 0)
	for _, field := range []string{"text", "partial_json", "thinking"} {
		value, ok := delta[field].(string)
		if !ok {
			continue
		}
		fieldName := field
		return []streamPIIField{{
			channel: "anthropic:" + index + ":" + fieldName,
			value:   value,
			set:     func(value string) { delta[fieldName] = value },
			flush: func(value string) ([]byte, error) {
				payload := map[string]any{
					"type": "content_block_delta", "index": root["index"],
					"delta": map[string]any{"type": delta["type"], fieldName: value},
				}
				return marshalSSELike(event, payload)
			},
		}}
	}
	return nil
}

func piiGeminiStreamFields(event []byte, root map[string]any) []streamPIIField {
	var fields []streamPIIField
	candidates, _ := root["candidates"].([]any)
	for candidatePosition, rawCandidate := range candidates {
		candidate, _ := rawCandidate.(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		candidateIndex := streamIndex(candidate["index"], candidatePosition)
		for partPosition, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			value, ok := part["text"].(string)
			if !ok {
				continue
			}
			partIndex := strconv.Itoa(partPosition)
			fields = append(fields, streamPIIField{
				channel: "gemini:" + candidateIndex + ":" + partIndex,
				value:   value,
				set:     func(value string) { part["text"] = value },
				flush: func(value string) ([]byte, error) {
					payload := map[string]any{"candidates": []any{map[string]any{
						"index": candidate["index"],
						"content": map[string]any{
							"role":  content["role"],
							"parts": []any{map[string]any{"text": value}},
						},
					}}}
					return marshalSSELike(event, payload)
				},
			})
		}
	}
	return fields
}

func piiBedrockFields(message awsEventMessage, root map[string]any) []streamPIIField {
	if message.header(":event-type") != "contentBlockDelta" {
		return nil
	}
	delta, _ := root["delta"].(map[string]any)
	if delta == nil {
		return nil
	}
	index := streamIndex(root["contentBlockIndex"], 0)
	if value, ok := delta["text"].(string); ok {
		return []streamPIIField{bedrockPIIField(
			message, root, delta, index, "text", nil, value,
		)}
	}
	for _, nestedName := range []string{"toolUse", "reasoningContent"} {
		nested, _ := delta[nestedName].(map[string]any)
		if nested == nil {
			continue
		}
		for _, fieldName := range []string{"input", "text"} {
			value, ok := nested[fieldName].(string)
			if !ok {
				continue
			}
			return []streamPIIField{bedrockPIIField(
				message, root, nested, index, fieldName, nestedName, value,
			)}
		}
	}
	return nil
}

func bedrockPIIField(
	message awsEventMessage,
	root map[string]any,
	target map[string]any,
	index string,
	field string,
	nested any,
	value string,
) streamPIIField {
	return streamPIIField{
		channel: "bedrock:" + index + ":" + fmt.Sprint(nested) + ":" + field,
		value:   value,
		set:     func(value string) { target[field] = value },
		flush: func(value string) ([]byte, error) {
			delta := map[string]any{field: value}
			if nested != nil {
				delta = map[string]any{fmt.Sprint(nested): delta}
			}
			payload, err := json.Marshal(map[string]any{
				"contentBlockIndex": root["contentBlockIndex"],
				"delta":             delta,
			})
			if err != nil {
				return nil, err
			}
			return replaceAWSEventPayload(message, payload)
		},
	}
}

func streamIndex(value any, fallback int) string {
	if value == nil {
		return strconv.Itoa(fallback)
	}
	return fmt.Sprint(value)
}

func copyStreamEnvelope(root map[string]any, names ...string) map[string]any {
	result := make(map[string]any, len(names))
	for _, name := range names {
		if value, exists := root[name]; exists {
			result[name] = value
		}
	}
	return result
}

func marshalSSELike(event []byte, payload map[string]any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return replaceSSEDataPayload(event, encoded), nil
}

var _ io.Writer = (*streamPIIMaskWriter)(nil)
var _ io.Writer = (*streamPIIOutputWriter)(nil)
var _ interface{ Flush() } = (*streamPIIMaskWriter)(nil)
var _ interface{ Flush() } = (*streamPIIOutputWriter)(nil)
