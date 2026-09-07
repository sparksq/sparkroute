package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
)

const (
	guardrailProtocolVersion = 2
	maxGuardrailReasonBytes  = 1024
)

type guardrailInvocationContextKey struct{}

func withGuardrailInvocation(ctx context.Context) context.Context {
	return context.WithValue(ctx, guardrailInvocationContextKey{}, true)
}

func isGuardrailInvocation(ctx context.Context) bool {
	value, _ := ctx.Value(guardrailInvocationContextKey{}).(bool)
	return value
}

type guardrailPhase string

const (
	guardrailPhasePre        guardrailPhase = "pre"
	guardrailPhasePost       guardrailPhase = "post"
	guardrailPhasePostStream guardrailPhase = "post_stream"
)

type guardrailAction string

const (
	guardrailActionAllow   guardrailAction = "allow"
	guardrailActionBlock   guardrailAction = "block"
	guardrailActionReplace guardrailAction = "replace"
)

type guardrailVerdict struct {
	Action      guardrailAction `json:"action"`
	Reason      string          `json:"reason,omitempty"`
	Replacement json.RawMessage `json:"replacement,omitempty"`
}

type guardrailResult struct {
	body    []byte
	blocked bool
	failed  bool
}

type guardrailInput struct {
	Version   int                   `json:"version"`
	Protocol  string                `json:"protocol"`
	Operation openAIOperation       `json:"operation"`
	Guardrail string                `json:"guardrail"`
	Phase     guardrailPhase        `json:"phase"`
	Request   json.RawMessage       `json:"request"`
	Response  json.RawMessage       `json:"response,omitempty"`
	Stream    *guardrailStreamInput `json:"stream,omitempty"`
}

type guardrailStreamInput struct {
	Sequence        int               `json:"sequence"`
	ApprovedContext []json.RawMessage `json:"approved_context,omitempty"`
	Pending         []json.RawMessage `json:"pending"`
	Final           bool              `json:"final"`
}

const guardrailProtocolPrompt = `You are an LLM gateway guardrail. Evaluate the versioned JSON envelope in the final user message. The protocol and operation fields identify the covered API. Respond with exactly one JSON object and no markdown. Valid verdicts are {"action":"allow"}, {"action":"block","reason":"brief reason"}, or, when supported, {"action":"replace","reason":"brief reason","replacement":{...}}. For a pre phase, replacement is a complete request for the declared operation and must preserve its model, streaming mode, and provider-state controls. For a post phase, replacement must contain only the replaceable result for the declared operation: choices for chat_completions, output for responses or Bedrock Converse, data for embeddings, content for Anthropic messages, input_tokens for Anthropic token counting, candidates for Gemini generation, or totalTokens for Gemini token counting. Responses compact output is canonical and supports only allow or block during the post phase. For a post_stream phase, approved_context contains bounded previously released stream JSON payloads and pending contains unreleased payloads; only allow or block is valid.`

func (h *chatCompletionsHandler) applyPreGuardrails(
	request *http.Request,
	guardrails []config.Guardrail,
	body []byte,
	requestedModel string,
	streaming bool,
) guardrailResult {
	current := append([]byte(nil), body...)
	for _, guardrail := range guardrails {
		verdict, err := h.invokeGuardrail(
			request,
			guardrail,
			guardrailPhasePre,
			current,
			nil,
		)
		if err != nil {
			if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
				return guardrailResult{body: current, failed: true}
			}
			continue
		}
		switch verdict.Action {
		case guardrailActionAllow:
			continue
		case guardrailActionBlock:
			return guardrailResult{body: current, blocked: true}
		case guardrailActionReplace:
			if !guardrail.AllowReplacement {
				if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
					return guardrailResult{body: current, failed: true}
				}
				continue
			}
			replacement, err := validatePreGuardrailReplacement(
				h.operation,
				current,
				verdict.Replacement,
				requestedModel,
				streaming,
			)
			if err != nil {
				if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
					return guardrailResult{body: current, failed: true}
				}
				continue
			}
			current = replacement
		}
	}
	return guardrailResult{body: current}
}

func (h *chatCompletionsHandler) applyPostGuardrails(
	request *http.Request,
	guardrails []config.Guardrail,
	requestBody []byte,
	responseBody []byte,
) guardrailResult {
	current := append([]byte(nil), responseBody...)
	for _, guardrail := range guardrails {
		verdict, err := h.invokeGuardrail(
			request,
			guardrail,
			guardrailPhasePost,
			requestBody,
			current,
		)
		if err != nil {
			if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
				return guardrailResult{body: current, failed: true}
			}
			continue
		}
		switch verdict.Action {
		case guardrailActionAllow:
			continue
		case guardrailActionBlock:
			return guardrailResult{body: current, blocked: true}
		case guardrailActionReplace:
			if !guardrail.AllowReplacement {
				if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
					return guardrailResult{body: current, failed: true}
				}
				continue
			}
			replacement, err := mergePostGuardrailReplacement(
				h.operation,
				current,
				verdict.Replacement,
			)
			if err != nil {
				if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
					return guardrailResult{body: current, failed: true}
				}
				continue
			}
			current = replacement
		}
	}
	return guardrailResult{body: current}
}

func (h *chatCompletionsHandler) applyStreamPostGuardrails(
	request *http.Request,
	guardrails []config.Guardrail,
	requestBody []byte,
	stream guardrailStreamInput,
) guardrailResult {
	for _, guardrail := range guardrails {
		input := guardrailInput{
			Version:   guardrailProtocolVersion,
			Protocol:  h.operation.protocol(),
			Operation: h.operation,
			Guardrail: guardrail.Name,
			Phase:     guardrailPhasePostStream,
			Request:   append(json.RawMessage(nil), requestBody...),
			Stream:    cloneGuardrailStreamInput(stream),
		}
		verdict, err := h.invokeGuardrailInput(request, guardrail, input)
		if err != nil {
			if request.Context().Err() != nil ||
				guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
				return guardrailResult{failed: true}
			}
			continue
		}
		switch verdict.Action {
		case guardrailActionAllow:
			continue
		case guardrailActionBlock:
			return guardrailResult{blocked: true}
		case guardrailActionReplace:
			// Already-released SSE events cannot be safely rewritten. Streaming
			// guardrails therefore intentionally expose an allow/block protocol.
			if guardrail.FailureMode.Effective() == config.GuardrailFailClosed {
				return guardrailResult{failed: true}
			}
		}
	}
	return guardrailResult{}
}

func cloneGuardrailStreamInput(input guardrailStreamInput) *guardrailStreamInput {
	result := &guardrailStreamInput{
		Sequence: input.Sequence,
		Final:    input.Final,
	}
	result.ApprovedContext = cloneRawMessages(input.ApprovedContext)
	result.Pending = cloneRawMessages(input.Pending)
	return result
}

func cloneRawMessages(values []json.RawMessage) []json.RawMessage {
	if len(values) == 0 {
		return nil
	}
	result := make([]json.RawMessage, len(values))
	for index := range values {
		result[index] = append(json.RawMessage(nil), values[index]...)
	}
	return result
}

func (h *chatCompletionsHandler) invokeGuardrail(
	request *http.Request,
	guardrail config.Guardrail,
	phase guardrailPhase,
	requestBody []byte,
	responseBody []byte,
) (guardrailVerdict, error) {
	input := guardrailInput{
		Version:   guardrailProtocolVersion,
		Protocol:  h.operation.protocol(),
		Operation: h.operation,
		Guardrail: guardrail.Name,
		Phase:     phase,
		Request:   append(json.RawMessage(nil), requestBody...),
	}
	if len(responseBody) != 0 {
		input.Response = append(json.RawMessage(nil), responseBody...)
	}
	return h.invokeGuardrailInput(request, guardrail, input)
}

func (h *chatCompletionsHandler) invokeGuardrailInput(
	request *http.Request,
	guardrail config.Guardrail,
	input guardrailInput,
) (guardrailVerdict, error) {
	inputJSON, err := json.Marshal(input)
	if err != nil {
		return guardrailVerdict{}, fmt.Errorf("encode guardrail input: %w", err)
	}
	messages := []map[string]string{{
		"role":    "system",
		"content": guardrailProtocolPrompt,
	}}
	if guardrail.Prompt != "" {
		messages = append(messages, map[string]string{
			"role":    "system",
			"content": guardrail.Prompt,
		})
	}
	messages = append(messages, map[string]string{
		"role":    "user",
		"content": string(inputJSON),
	})
	body, err := json.Marshal(map[string]any{
		"model":    guardrail.Model,
		"stream":   false,
		"messages": messages,
	})
	if err != nil {
		return guardrailVerdict{}, fmt.Errorf("encode guardrail request: %w", err)
	}
	internalRequest, err := http.NewRequestWithContext(
		withGuardrailInvocation(request.Context()),
		http.MethodPost,
		"http://sparkroute.internal/v1/chat/completions",
		bytes.NewReader(body),
	)
	if err != nil {
		return guardrailVerdict{}, fmt.Errorf("construct guardrail request: %w", err)
	}
	internalRequest.Header.Set("Content-Type", "application/json")
	response := newBufferedResponseWriter()
	guardrailHandler := *h
	guardrailHandler.operation = openAIOperationChatCompletions
	guardrailHandler.ServeHTTP(response, internalRequest)
	if response.statusCode() < 200 || response.statusCode() >= 300 {
		return guardrailVerdict{}, fmt.Errorf(
			"guardrail model returned gateway status %d",
			response.statusCode(),
		)
	}
	content, err := extractGuardrailContent(response.body.Bytes())
	if err != nil {
		return guardrailVerdict{}, err
	}
	return decodeGuardrailVerdict([]byte(content))
}

type streamPostGuardrailError struct {
	blocked bool
}

func (e *streamPostGuardrailError) Error() string {
	if e.blocked {
		return "streaming post-response guardrail blocked output"
	}
	return "streaming post-response guardrail failed"
}

// streamPostGuardrailWriter keeps a bounded window of complete SSE events or
// AWS event-stream messages private until every configured post guardrail
// allows their JSON payloads.
// Previously approved context is bounded independently and is never emitted
// twice.
type streamPostGuardrailWriter struct {
	destination    io.Writer
	handler        *chatCompletionsHandler
	request        *http.Request
	guardrails     []config.Guardrail
	requestBody    []byte
	windowBytes    int
	contextMax     int
	extractPayload func([]byte) ([]byte, bool, bool)

	pendingEvents   [][]byte
	pendingPayloads []json.RawMessage
	pendingBytes    int
	contextPayloads []json.RawMessage
	contextBytes    int
	sequence        int
}

func newStreamPostGuardrailWriter(
	destination io.Writer,
	handler *chatCompletionsHandler,
	request *http.Request,
	guardrails []config.Guardrail,
	requestBody []byte,
	policy config.EffectiveGuardrailStreamPolicy,
) *streamPostGuardrailWriter {
	return &streamPostGuardrailWriter{
		destination:    destination,
		handler:        handler,
		request:        request,
		guardrails:     append([]config.Guardrail(nil), guardrails...),
		requestBody:    append([]byte(nil), requestBody...),
		windowBytes:    policy.WindowBytes,
		contextMax:     policy.ContextBytes,
		extractPayload: sseGuardrailPayload,
	}
}

func newBedrockStreamPostGuardrailWriter(
	destination io.Writer,
	handler *chatCompletionsHandler,
	request *http.Request,
	guardrails []config.Guardrail,
	requestBody []byte,
	policy config.EffectiveGuardrailStreamPolicy,
) *streamPostGuardrailWriter {
	writer := newStreamPostGuardrailWriter(
		destination,
		handler,
		request,
		guardrails,
		requestBody,
		policy,
	)
	writer.extractPayload = bedrockGuardrailPayload
	return writer
}

func (w *streamPostGuardrailWriter) Write(event []byte) (int, error) {
	copiedEvent := append([]byte(nil), event...)
	w.pendingEvents = append(w.pendingEvents, copiedEvent)
	w.pendingBytes += len(copiedEvent)
	final := false
	if payload, exists, extractedFinal := w.extractPayload(
		copiedEvent,
	); exists {
		final = extractedFinal
		if !final {
			w.pendingPayloads = append(
				w.pendingPayloads,
				append(json.RawMessage(nil), payload...),
			)
		}
	}
	if w.pendingBytes < w.windowBytes && !final {
		return len(event), nil
	}
	if err := w.release(final); err != nil {
		return 0, err
	}
	return len(event), nil
}

func sseGuardrailPayload(event []byte) ([]byte, bool, bool) {
	payload, exists := sseDataPayload(event)
	if !exists {
		return nil, false, false
	}
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return payload, true, true
	}
	return payload, true, false
}

func bedrockGuardrailPayload(
	event []byte,
) ([]byte, bool, bool) {
	message, err := readAWSEventMessage(bytes.NewReader(event))
	if err != nil || !json.Valid(message.payload) {
		return nil, false, false
	}
	return message.payload, true, false
}

// Flush implements the response-writer flusher shape used by proxySSEEvents.
// A caller flush cannot release an unapproved partial guardrail window.
func (w *streamPostGuardrailWriter) Flush() {
	if len(w.pendingEvents) != 0 {
		return
	}
	if flusher, ok := w.destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
}

func (w *streamPostGuardrailWriter) Finish() error {
	return w.release(true)
}

func (w *streamPostGuardrailWriter) release(final bool) error {
	if len(w.pendingEvents) == 0 {
		return nil
	}
	if len(w.pendingPayloads) != 0 {
		result := w.handler.applyStreamPostGuardrails(
			w.request,
			w.guardrails,
			w.requestBody,
			guardrailStreamInput{
				Sequence:        w.sequence,
				ApprovedContext: w.contextPayloads,
				Pending:         w.pendingPayloads,
				Final:           final,
			},
		)
		if result.blocked {
			return &streamPostGuardrailError{blocked: true}
		}
		if result.failed {
			return &streamPostGuardrailError{}
		}
		w.sequence++
	}
	for _, event := range w.pendingEvents {
		if _, err := w.destination.Write(event); err != nil {
			return err
		}
	}
	w.rememberApproved(w.pendingPayloads)
	w.pendingEvents = nil
	w.pendingPayloads = nil
	w.pendingBytes = 0
	if flusher, ok := w.destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func (w *streamPostGuardrailWriter) rememberApproved(
	payloads []json.RawMessage,
) {
	for _, payload := range payloads {
		if len(payload) > w.contextMax {
			w.contextPayloads = nil
			w.contextBytes = 0
			continue
		}
		copied := append(json.RawMessage(nil), payload...)
		w.contextPayloads = append(w.contextPayloads, copied)
		w.contextBytes += len(copied)
		for w.contextBytes > w.contextMax && len(w.contextPayloads) != 0 {
			w.contextBytes -= len(w.contextPayloads[0])
			w.contextPayloads[0] = nil
			w.contextPayloads = w.contextPayloads[1:]
		}
		if len(w.contextPayloads) == 0 {
			w.contextPayloads = nil
		}
	}
}

func extractGuardrailContent(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode guardrail Chat Completions response: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", fmt.Errorf("guardrail response contains no choices")
	}
	var content string
	if err := json.Unmarshal(response.Choices[0].Message.Content, &content); err != nil {
		return "", fmt.Errorf("guardrail response content must be a string")
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("guardrail response content is empty")
	}
	return content, nil
}

func decodeGuardrailVerdict(raw []byte) (guardrailVerdict, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var verdict guardrailVerdict
	if err := decoder.Decode(&verdict); err != nil {
		return guardrailVerdict{}, fmt.Errorf("decode guardrail verdict: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return guardrailVerdict{}, fmt.Errorf("guardrail verdict has a trailing JSON value")
		}
		return guardrailVerdict{}, fmt.Errorf("decode guardrail verdict trailer: %w", err)
	}
	if len(verdict.Reason) > maxGuardrailReasonBytes {
		return guardrailVerdict{}, fmt.Errorf(
			"guardrail reason exceeds %d bytes",
			maxGuardrailReasonBytes,
		)
	}
	switch verdict.Action {
	case guardrailActionAllow, guardrailActionBlock:
		if len(verdict.Replacement) != 0 {
			return guardrailVerdict{}, fmt.Errorf(
				"guardrail %s verdict must not contain replacement",
				verdict.Action,
			)
		}
	case guardrailActionReplace:
		if !isJSONObject(verdict.Replacement) {
			return guardrailVerdict{}, fmt.Errorf(
				"guardrail replacement must be a JSON object",
			)
		}
	default:
		return guardrailVerdict{}, fmt.Errorf(
			"guardrail action must be allow, block, or replace",
		)
	}
	return verdict, nil
}

func validatePreGuardrailReplacement(
	operation openAIOperation,
	original json.RawMessage,
	replacement json.RawMessage,
	requestedModel string,
	streaming bool,
) ([]byte, error) {
	originalEnvelope, _, _, err := operation.decodeRequest(
		original,
		requestedModel,
	)
	if err != nil {
		return nil, fmt.Errorf("decode current request: %w", err)
	}
	envelope, replacementModel, replacementStreaming, err := operation.decodeRequest(
		replacement,
		requestedModel,
	)
	if err != nil {
		return nil, err
	}
	if replacementModel != requestedModel {
		return nil, fmt.Errorf("pre guardrail cannot change the requested model")
	}
	if replacementStreaming != streaming {
		return nil, fmt.Errorf("pre guardrail cannot change streaming mode")
	}
	if operation.isGemini() && !rawNonNull(envelope["model"]) {
		return nil, fmt.Errorf(
			"pre guardrail Gemini replacement must preserve model",
		)
	}
	if operation == openAIOperationResponses &&
		!sameResponsesGuardrailState(originalEnvelope, envelope) {
		return nil, fmt.Errorf(
			"pre guardrail cannot change Responses provider-state controls",
		)
	}
	if operation == openAIOperationResponsesCompact &&
		!sameResponsesCompactGuardrailState(
			originalEnvelope,
			envelope,
		) {
		return nil, fmt.Errorf(
			"pre guardrail cannot change Responses compact provider-state controls",
		)
	}
	return json.Marshal(envelope)
}

func sameResponsesGuardrailState(
	left map[string]json.RawMessage,
	right map[string]json.RawMessage,
) bool {
	leftState := classifyResponsesState(left)
	rightState := classifyResponsesState(right)
	return leftState.previousResponseID == rightState.previousResponseID &&
		leftState.conversationID == rightState.conversationID &&
		leftState.storesResponse == rightState.storesResponse &&
		leftState.background == rightState.background
}

func sameResponsesCompactGuardrailState(
	left map[string]json.RawMessage,
	right map[string]json.RawMessage,
) bool {
	leftState := classifyResponsesCompactState(left)
	rightState := classifyResponsesCompactState(right)
	if leftState.previousResponseID !=
		rightState.previousResponseID {
		return false
	}
	leftFiles, leftErr := collectProviderFileIDs(
		openAIOperationResponsesCompact,
		left,
	)
	rightFiles, rightErr := collectProviderFileIDs(
		openAIOperationResponsesCompact,
		right,
	)
	if leftErr != nil ||
		rightErr != nil ||
		!sameStringSet(leftFiles, rightFiles) {
		return false
	}
	leftItems, leftErr := collectResponsesItemAffinityIDs(
		left["input"],
	)
	rightItems, rightErr := collectResponsesItemAffinityIDs(
		right["input"],
	)
	if leftErr != nil ||
		rightErr != nil ||
		!sameStringSet(leftItems, rightItems) {
		return false
	}
	leftCompactions, leftErr := responsesCompactOpaqueState(
		left["input"],
	)
	rightCompactions, rightErr := responsesCompactOpaqueState(
		right["input"],
	)
	return leftErr == nil &&
		rightErr == nil &&
		sameStringMap(leftCompactions, rightCompactions)
}

func sameStringSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	values := make(map[string]struct{}, len(left))
	for _, value := range left {
		values[value] = struct{}{}
	}
	for _, value := range right {
		if _, exists := values[value]; !exists {
			return false
		}
	}
	return true
}

func sameStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		rightValue, exists := right[key]
		if !exists || rightValue != value {
			return false
		}
	}
	return true
}

func responsesCompactOpaqueState(
	raw json.RawMessage,
) (map[string]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 ||
		bytes.Equal(trimmed, []byte("null")) ||
		trimmed[0] == '"' {
		return nil, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf(
			"the Responses compact input must be a string or array",
		)
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for index, item := range items {
		var itemType string
		if json.Unmarshal(item["type"], &itemType) != nil ||
			itemType != "compaction" {
			continue
		}
		var itemID, encryptedContent string
		if json.Unmarshal(item["id"], &itemID) != nil ||
			json.Unmarshal(
				item["encrypted_content"],
				&encryptedContent,
			) != nil {
			return nil, fmt.Errorf(
				"the Responses compact input item %d has invalid opaque state",
				index,
			)
		}
		if existing, exists := result[itemID]; exists &&
			existing != encryptedContent {
			return nil, fmt.Errorf(
				"the Responses compact input contains conflicting state for %s",
				itemID,
			)
		}
		result[itemID] = encryptedContent
	}
	return result, nil
}

func mergePostGuardrailReplacement(
	operation openAIOperation,
	original []byte,
	replacement json.RawMessage,
) ([]byte, error) {
	if operation == openAIOperationResponsesCompact {
		return nil, fmt.Errorf(
			"the Responses compact post guardrails cannot replace canonical output",
		)
	}
	var originalEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(original, &originalEnvelope); err != nil {
		return nil, fmt.Errorf("decode primary response: %w", err)
	}
	var replacementEnvelope map[string]json.RawMessage
	if err := json.Unmarshal(replacement, &replacementEnvelope); err != nil {
		return nil, fmt.Errorf("decode post guardrail replacement: %w", err)
	}
	field := "choices"
	switch operation {
	case openAIOperationResponses:
		field = "output"
	case openAIOperationEmbeddings:
		field = "data"
	case anthropicOperationMessages:
		field = "content"
	case anthropicOperationCountTokens:
		field = "input_tokens"
	case geminiOperationGenerateContent, geminiOperationStreamContent:
		field = "candidates"
	case geminiOperationCountTokens:
		field = "totalTokens"
	case bedrockOperationConverse, bedrockOperationConverseStream:
		field = "output"
	}
	value, exists := replacementEnvelope[field]
	if !exists {
		return nil, fmt.Errorf(
			"post guardrail replacement must contain %s",
			field,
		)
	}
	if operation == anthropicOperationCountTokens {
		var inputTokens int64
		if err := json.Unmarshal(value, &inputTokens); err != nil ||
			inputTokens < 0 {
			return nil, fmt.Errorf(
				"post guardrail replacement input_tokens must be a non-negative integer",
			)
		}
	} else if operation == geminiOperationCountTokens {
		var totalTokens int64
		if err := json.Unmarshal(value, &totalTokens); err != nil ||
			totalTokens < 0 {
			return nil, fmt.Errorf(
				"post guardrail replacement totalTokens must be a non-negative integer",
			)
		}
	} else if operation == bedrockOperationConverse ||
		operation == bedrockOperationConverseStream {
		if !isJSONObject(value) {
			return nil, fmt.Errorf(
				"post guardrail replacement must contain an output object",
			)
		}
	} else if !isJSONArray(value) {
		return nil, fmt.Errorf(
			"post guardrail replacement must contain a %s array",
			field,
		)
	}
	originalEnvelope[field] = append(json.RawMessage(nil), value...)
	return json.Marshal(originalEnvelope)
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 &&
		trimmed[0] == '{' &&
		trimmed[len(trimmed)-1] == '}' &&
		json.Valid(trimmed)
}

func isJSONArray(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 &&
		trimmed[0] == '[' &&
		trimmed[len(trimmed)-1] == ']' &&
		json.Valid(trimmed)
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newBufferedResponseWriter() *bufferedResponseWriter {
	return &bufferedResponseWriter{header: make(http.Header)}
}

func (w *bufferedResponseWriter) Header() http.Header {
	return w.header
}

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(value []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(value)
}

func (w *bufferedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func writeBufferedResponse(
	destination http.ResponseWriter,
	source *bufferedResponseWriter,
	body []byte,
	operation openAIOperation,
) error {
	for name := range source.header {
		destination.Header().Del(name)
	}
	operation.copyResponseHeaders(destination.Header(), source.header)
	removeRepresentationValidators(destination.Header())
	destination.Header().Del("Content-Length")
	destination.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
	destination.WriteHeader(source.statusCode())
	_, err := destination.Write(body)
	return err
}
