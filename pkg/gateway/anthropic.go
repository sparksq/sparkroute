// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

const (
	anthropicAPIVersion       = "2023-06-01"
	maxAnthropicBetas         = 16
	anthropicBetaCapability   = "x-anthropic-beta."
	maxAnthropicMessages      = 100000
	anthropicStreamErrorClass = "anthropic_stream_error"
)

func newAnthropicMessagesHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return newOpenAIHandler(
		anthropicOperationMessages,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
}

func newAnthropicCountTokensHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return newOpenAIHandler(
		anthropicOperationCountTokens,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
}

func (o openAIOperation) isAnthropic() bool {
	return o == anthropicOperationMessages ||
		o == anthropicOperationCountTokens
}

func (o openAIOperation) protocol() string {
	if o.isAnthropic() {
		return "anthropic"
	}
	if o.isGemini() {
		return "gemini"
	}
	if o.isBedrock() {
		return "bedrock"
	}
	return "openai"
}

func (o openAIOperation) upstreamOperationForProtocol(
	protocol config.Protocol,
	streaming bool,
) openAIOperation {
	if o == openAIOperationChatCompletions &&
		protocol == config.ProtocolBedrock {
		if streaming {
			return bedrockOperationConverseStream
		}
		return bedrockOperationConverse
	}
	if o == openAIOperationChatCompletions &&
		protocol == config.ProtocolAnthropic {
		return anthropicOperationMessages
	}
	if o == openAIOperationChatCompletions &&
		protocol == config.ProtocolGemini {
		if streaming {
			return geminiOperationStreamContent
		}
		return geminiOperationGenerateContent
	}
	if o == openAIOperationEmbeddings &&
		protocol == config.ProtocolGemini {
		return geminiOperationEmbedContent
	}
	return o
}

func upstreamOperationForSelection(
	operation openAIOperation,
	selection routing.Selection,
	streaming bool,
) openAIOperation {
	protocol := selectionUpstreamProtocol(selection)
	if operation == openAIOperationResponses &&
		protocol == config.ProtocolOpenAI &&
		!selection.NativeProtocol {
		return openAIOperationChatCompletions
	}
	return operation.upstreamOperationForProtocol(protocol, streaming)
}

func (o openAIOperation) protocolHeaderCapabilities(
	header http.Header,
) ([]config.Capability, error) {
	if !o.isAnthropic() {
		return nil, nil
	}
	_, betas, err := parseAnthropicProtocolHeaders(header)
	if err != nil {
		return nil, err
	}
	result := make([]config.Capability, 0, len(betas))
	for _, beta := range betas {
		result = append(
			result,
			config.Capability(anthropicBetaCapability+beta),
		)
	}
	return result, nil
}

func (o openAIOperation) applyProtocolHeaders(
	target http.Header,
	source http.Header,
) error {
	if !o.isAnthropic() {
		return nil
	}
	version, betas, err := parseAnthropicProtocolHeaders(source)
	if err != nil {
		return err
	}
	target.Set("Anthropic-Version", version)
	target.Del("Anthropic-Beta")
	if len(betas) != 0 {
		target.Set("Anthropic-Beta", strings.Join(betas, ","))
	}
	return nil
}

func parseAnthropicProtocolHeaders(
	header http.Header,
) (string, []string, error) {
	versions := header.Values("Anthropic-Version")
	if len(versions) != 1 || strings.TrimSpace(versions[0]) == "" {
		return "", nil, fmt.Errorf(
			"anthropic-version must be set to %s",
			anthropicAPIVersion,
		)
	}
	version := strings.TrimSpace(versions[0])
	if version != anthropicAPIVersion {
		return "", nil, unsupportedFeature(
			"anthropic-version %q is not supported; use %s",
			version,
			anthropicAPIVersion,
		)
	}
	if len(header.Values("Anthropic-User-Profile-Id")) != 0 {
		return "", nil, unsupportedFeature(
			"anthropic-user-profile-id forwarding is not enabled",
		)
	}

	seen := make(map[string]struct{})
	betas := make([]string, 0)
	for _, value := range header.Values("Anthropic-Beta") {
		for item := range strings.SplitSeq(value, ",") {
			beta := strings.TrimSpace(item)
			if beta == "" {
				return "", nil, fmt.Errorf(
					"anthropic-beta contains an empty feature name",
				)
			}
			if err := validateAnthropicBeta(beta); err != nil {
				return "", nil, err
			}
			if _, exists := seen[beta]; exists {
				continue
			}
			if len(betas) == maxAnthropicBetas {
				return "", nil, fmt.Errorf(
					"anthropic-beta must not contain more than %d features",
					maxAnthropicBetas,
				)
			}
			seen[beta] = struct{}{}
			betas = append(betas, beta)
		}
	}
	return version, betas, nil
}

func validateAnthropicBeta(beta string) error {
	capability := anthropicBetaCapability + beta
	if len(capability) > 64 {
		return fmt.Errorf("anthropic-beta feature name is too long")
	}
	if beta[0] == '-' || beta[len(beta)-1] == '-' {
		return fmt.Errorf(
			"anthropic-beta feature %q has invalid surrounding hyphens",
			beta,
		)
	}
	for _, value := range beta {
		if value >= 'a' && value <= 'z' ||
			value >= '0' && value <= '9' ||
			value == '-' {
			continue
		}
		return fmt.Errorf(
			"anthropic-beta feature %q contains an invalid character",
			beta,
		)
	}
	return nil
}

func decodeAnthropicMessagesRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, streaming, err := decodeAnthropicRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	var maxTokens int64
	if rawMaxTokens := envelope["max_tokens"]; !rawNonNull(rawMaxTokens) {
		return nil, "", false, fmt.Errorf("max_tokens is required")
	} else if err := json.Unmarshal(rawMaxTokens, &maxTokens); err != nil ||
		maxTokens < 0 {
		return nil, "", false, fmt.Errorf(
			"max_tokens must be a non-negative integer",
		)
	}
	return envelope, model, streaming, nil
}

func decodeAnthropicCountTokensRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, streaming, err := decodeAnthropicRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	if streaming {
		return nil, "", false, unsupportedFeature(
			"streaming is not supported by the token-counting API",
		)
	}
	return envelope, model, false, nil
}

func decodeAnthropicRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, streaming, err := decodeChatRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	rawMessages := envelope["messages"]
	if !rawNonNull(rawMessages) {
		return nil, "", false, fmt.Errorf("messages is required")
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return nil, "", false, fmt.Errorf(
			"messages must be an array of message objects",
		)
	}
	if len(messages) == 0 {
		return nil, "", false, fmt.Errorf(
			"messages must contain at least one message",
		)
	}
	if len(messages) > maxAnthropicMessages {
		return nil, "", false, fmt.Errorf(
			"messages must not contain more than %d entries",
			maxAnthropicMessages,
		)
	}
	for index, message := range messages {
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil ||
			role != "user" && role != "assistant" {
			return nil, "", false, fmt.Errorf(
				"messages[%d].role must be user or assistant",
				index,
			)
		}
		if !rawNonNull(message["content"]) {
			return nil, "", false, fmt.Errorf(
				"messages[%d].content is required",
				index,
			)
		}
	}
	return envelope, model, streaming, nil
}

func detectAnthropicCapabilities(
	envelope map[string]json.RawMessage,
	operation openAIOperation,
) ([]config.Capability, error) {
	required := make(map[config.Capability]struct{})
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}
	if operation == anthropicOperationCountTokens {
		add(config.CapabilityTokenCounting)
	}
	if rawActive(envelope["container"]) {
		return nil, unsupportedFeature(
			"provider container references require gateway affinity support",
		)
	}
	if rawActive(envelope["thinking"]) {
		add(config.CapabilityReasoning)
	}
	if rawActive(envelope["service_tier"]) ||
		rawActive(envelope["speed"]) ||
		rawActive(envelope["inference_geo"]) {
		add(config.CapabilityServiceTier)
	}
	if rawActive(envelope["mcp_servers"]) ||
		rawActive(envelope["context_management"]) {
		add(config.CapabilityProviderTools)
	}
	if err := inspectAnthropicOutputConfig(envelope, add); err != nil {
		return nil, err
	}
	if err := inspectAnthropicContent(envelope["system"], add); err != nil {
		return nil, fmt.Errorf("system: %w", err)
	}
	if raw := envelope["messages"]; rawNonNull(raw) {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, fmt.Errorf(
				"messages must be an array of message objects",
			)
		}
		for index, message := range messages {
			if err := inspectAnthropicContent(
				message["content"],
				add,
			); err != nil {
				return nil, fmt.Errorf(
					"messages[%d].content: %w",
					index,
					err,
				)
			}
		}
	}
	if rawActive(envelope["tool_choice"]) {
		add(config.CapabilityTools)
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(envelope["tool_choice"], &choice); err != nil {
			return nil, fmt.Errorf("tool_choice must be an object")
		}
		if raw, exists := choice["disable_parallel_tool_use"]; exists &&
			rawNonNull(raw) {
			var disabled bool
			if err := json.Unmarshal(raw, &disabled); err != nil {
				return nil, fmt.Errorf(
					"tool_choice.disable_parallel_tool_use must be a boolean",
				)
			}
			if !disabled {
				add(config.CapabilityParallelTools)
			}
		}
	}
	if raw := envelope["tools"]; rawActive(raw) {
		add(config.CapabilityTools)
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, fmt.Errorf("tools must be an array of tool objects")
		}
		for index, tool := range tools {
			var toolType string
			if rawType := tool["type"]; rawActive(rawType) {
				if err := json.Unmarshal(rawType, &toolType); err != nil {
					return nil, fmt.Errorf(
						"tools[%d].type must be a string",
						index,
					)
				}
			}
			if toolType != "" && toolType != "custom" {
				add(config.CapabilityProviderTools)
			}
			inspectAnthropicObjectFeatures(tool, add)
		}
	}
	inspectAnthropicObjectFeatures(envelope, add)

	result := make([]config.Capability, 0, len(required))
	for capability := range required {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return result, nil
}

func inspectAnthropicOutputConfig(
	envelope map[string]json.RawMessage,
	add func(...config.Capability),
) error {
	if rawActive(envelope["output_format"]) {
		add(config.CapabilityStructuredOutputs)
	}
	if raw := envelope["output_config"]; rawActive(raw) {
		var outputConfig map[string]json.RawMessage
		if err := json.Unmarshal(raw, &outputConfig); err != nil {
			return fmt.Errorf("output_config must be an object")
		}
		if rawActive(outputConfig["format"]) {
			add(config.CapabilityStructuredOutputs)
		}
	}
	return nil
}

func inspectAnthropicContent(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawNonNull(raw) {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("must be a string or an array of content blocks")
	}
	if trimmed[0] == '"' {
		var content string
		if err := json.Unmarshal(trimmed, &content); err != nil {
			return fmt.Errorf("must be a string or an array of content blocks")
		}
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return fmt.Errorf("must be a string or an array of content blocks")
	}
	for index, block := range blocks {
		var blockType string
		if rawType := block["type"]; rawActive(rawType) {
			if err := json.Unmarshal(rawType, &blockType); err != nil {
				return fmt.Errorf("block %d type must be a string", index)
			}
		}
		switch blockType {
		case "image":
			add(config.CapabilityVision)
		case "document":
			add(config.CapabilityFileInput)
		case "tool_use", "tool_result":
			add(config.CapabilityTools)
		case "thinking", "redacted_thinking":
			add(config.CapabilityReasoning)
		default:
			if strings.Contains(blockType, "tool_use") ||
				strings.Contains(blockType, "tool_result") {
				add(
					config.CapabilityTools,
					config.CapabilityProviderTools,
				)
			}
		}
		if rawSource := block["source"]; rawNonNull(rawSource) {
			var source map[string]json.RawMessage
			if json.Unmarshal(rawSource, &source) == nil &&
				rawNonNull(source["file_id"]) {
				return unsupportedFeature(
					"Anthropic file_id inputs require gateway file affinity support",
				)
			}
		}
		inspectAnthropicObjectFeatures(block, add)
		if blockType == "tool_result" && rawNonNull(block["content"]) {
			if err := inspectAnthropicContent(block["content"], add); err != nil {
				return fmt.Errorf("block %d tool result: %w", index, err)
			}
		}
	}
	return nil
}

func inspectAnthropicObjectFeatures(
	object map[string]json.RawMessage,
	add func(...config.Capability),
) {
	if rawActive(object["cache_control"]) {
		add(config.CapabilityPromptCaching)
	}
	if rawActive(object["citations"]) {
		add(config.CapabilityCitations)
	}
}

func anthropicMessagesURL(baseURL string) (string, error) {
	return anthropicOperationURL(baseURL, "messages")
}

func anthropicCountTokensURL(baseURL string) (string, error) {
	return anthropicOperationURL(baseURL, "messages/count_tokens")
}

func anthropicOperationURL(baseURL, operation string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + operation
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (o openAIOperation) validateSuccessResponse(body []byte) error {
	if o.isBedrock() {
		return validateBedrockSuccessResponse(body)
	}
	if o.isGemini() {
		return validateGeminiSuccessResponse(o, body)
	}
	if o == openAIOperationResponsesCompact {
		return validateResponsesCompactSuccessResponse(body)
	}
	if err := validateJSONResponse(body); err != nil {
		return err
	}
	if !o.isAnthropic() {
		return nil
	}
	if o == anthropicOperationCountTokens {
		if _, found := extractAnthropicCountTokensUsage(body); !found {
			return fmt.Errorf(
				"token-count response must contain non-negative input_tokens",
			)
		}
		return nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return fmt.Errorf("message response must be a JSON object")
	}
	var messageType, messageID, role, model string
	if json.Unmarshal(envelope["type"], &messageType) != nil ||
		messageType != "message" {
		return fmt.Errorf("message response type must be message")
	}
	if json.Unmarshal(envelope["id"], &messageID) != nil ||
		messageID == "" {
		return fmt.Errorf("message response id must be a non-empty string")
	}
	if json.Unmarshal(envelope["role"], &role) != nil ||
		role != "assistant" {
		return fmt.Errorf("message response role must be assistant")
	}
	if json.Unmarshal(envelope["model"], &model) != nil ||
		model == "" {
		return fmt.Errorf("message response model must be a non-empty string")
	}
	var content []json.RawMessage
	if json.Unmarshal(envelope["content"], &content) != nil {
		return fmt.Errorf("message response content must be an array")
	}
	if _, found := extractAnthropicMessagesUsage(body); !found {
		return fmt.Errorf("message response must contain valid usage")
	}
	return nil
}

type anthropicErrorEnvelope struct {
	Type      string         `json:"type"`
	Error     anthropicError `json:"error"`
	RequestID string         `json:"request_id,omitempty"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (o openAIOperation) writeError(
	w http.ResponseWriter,
	status int,
	code string,
	message string,
	requestID string,
) {
	if o.isBedrock() {
		writeBedrockError(w, status, code, message)
		return
	}
	if o.isGemini() {
		writeGeminiError(w, status, message)
		return
	}
	if !o.isAnthropic() {
		writeOpenAIError(w, status, code, message)
		return
	}
	writeJSON(w, status, anthropicErrorEnvelope{
		Type: "error",
		Error: anthropicError{
			Type:    anthropicErrorType(status),
			Message: message,
		},
		RequestID: requestID,
	})
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusConflict:
		return "conflict_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusGatewayTimeout:
		return "timeout_error"
	case 529:
		return "overloaded_error"
	default:
		if status >= 400 && status < 500 {
			return "invalid_request_error"
		}
		return "api_error"
	}
}

func (o openAIOperation) setRequestIDHeaders(
	header http.Header,
	configuredName string,
	requestID string,
) {
	header.Set(configuredName, requestID)
	if o.isBedrock() && header.Get("X-Amzn-Requestid") == "" {
		header.Set("X-Amzn-Requestid", requestID)
	}
	if o.isAnthropic() && header.Get("Request-Id") == "" {
		header.Set("Request-Id", requestID)
	}
}

func (o openAIOperation) copyResponseHeaders(
	target http.Header,
	source http.Header,
) {
	if o.isAnthropic() && source.Get("Request-Id") != "" {
		target.Del("Request-Id")
	}
	if o.isBedrock() && source.Get("X-Amzn-Requestid") != "" {
		target.Del("X-Amzn-Requestid")
	}
	copyResponseHeaders(target, source)
}

func (o openAIOperation) streamRequiresTerminal() bool {
	return o == openAIOperationResponses ||
		o == anthropicOperationMessages ||
		o == bedrockOperationConverseStream
}

func (o openAIOperation) inspectStreamEvent(
	payload []byte,
) (bool, ledger.Outcome, string) {
	if o == openAIOperationResponses {
		return inspectResponsesStreamEvent(payload)
	}
	if o == geminiOperationStreamContent {
		return inspectGeminiStreamEvent(payload)
	}
	if o != anthropicOperationMessages {
		return false, ledger.OutcomeSuccess, ""
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil {
		return false, ledger.OutcomeSuccess, ""
	}
	var eventType string
	_ = json.Unmarshal(envelope["type"], &eventType)
	switch eventType {
	case "message_stop":
		return true, ledger.OutcomeSuccess, ""
	case "error":
		failureClass := anthropicStreamErrorClass
		var source struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(envelope["error"], &source) == nil &&
			validFailureClassSuffix(source.Type) {
			failureClass += "_" + source.Type
		}
		return true, ledger.OutcomeUpstreamError, failureClass
	default:
		return false, ledger.OutcomeSuccess, ""
	}
}

func validFailureClassSuffix(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '_' {
			continue
		}
		return false
	}
	return true
}

func (o openAIOperation) streamIncompleteFailureClass() string {
	if o == bedrockOperationConverseStream {
		return "bedrock_stream_incomplete"
	}
	if o == anthropicOperationMessages {
		return "anthropic_stream_incomplete"
	}
	return "responses_stream_incomplete"
}

func (o openAIOperation) requiresSingleAttempt(
	required []config.Capability,
) bool {
	if o.isGemini() {
		for _, capability := range required {
			if capability == config.CapabilityProviderTools ||
				capability == config.CapabilityStoredCompletion ||
				capability == config.CapabilityPromptCaching {
				return true
			}
		}
		return false
	}
	if o.isBedrock() {
		for _, capability := range required {
			if capability == config.CapabilityProviderTools ||
				capability == config.CapabilityPromptCaching {
				return true
			}
		}
		return false
	}
	if o != anthropicOperationMessages {
		return false
	}
	for _, capability := range required {
		if capability == config.CapabilityProviderTools ||
			capability == config.CapabilityPromptCaching ||
			strings.HasPrefix(
				string(capability),
				anthropicBetaCapability,
			) {
			return true
		}
	}
	return false
}

func rewriteAnthropicSSEEventModel(event []byte, model string) []byte {
	rawModel, err := json.Marshal(model)
	if err != nil {
		return event
	}
	return rewriteSSEEventJSON(event, len(model), func(
		envelope map[string]json.RawMessage,
	) bool {
		rawMessage, exists := envelope["message"]
		if !exists {
			return false
		}
		var message map[string]json.RawMessage
		if err := json.Unmarshal(rawMessage, &message); err != nil ||
			message == nil {
			return false
		}
		if _, exists := message["model"]; !exists {
			return false
		}
		message["model"] = rawModel
		encoded, err := json.Marshal(message)
		if err != nil {
			return false
		}
		envelope["message"] = encoded
		return true
	})
}
