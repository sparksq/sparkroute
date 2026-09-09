// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

type openAIOperation string

const (
	openAIOperationChatCompletions  openAIOperation = "chat_completions"
	openAIOperationResponses        openAIOperation = "responses"
	openAIOperationResponsesCompact openAIOperation = "responses_compact"
	openAIOperationEmbeddings       openAIOperation = "embeddings"
	anthropicOperationMessages      openAIOperation = "messages"
	anthropicOperationCountTokens   openAIOperation = "messages_count_tokens"
	geminiOperationGenerateContent  openAIOperation = "generate_content"
	geminiOperationStreamContent    openAIOperation = "stream_generate_content"
	geminiOperationCountTokens      openAIOperation = "count_tokens"
	geminiOperationEmbedContent     openAIOperation = "embed_content"
	geminiOperationBatchEmbed       openAIOperation = "batch_embed_contents"
	bedrockOperationConverse        openAIOperation = "converse"
	bedrockOperationConverseStream  openAIOperation = "converse_stream"
)

func (o openAIOperation) String() string {
	return string(o)
}

func (o openAIOperation) responseName() string {
	switch o {
	case openAIOperationResponses:
		return "Responses"
	case openAIOperationResponsesCompact:
		return "Responses compact"
	case openAIOperationEmbeddings:
		return "Embeddings"
	case anthropicOperationMessages:
		return "Anthropic Messages"
	case anthropicOperationCountTokens:
		return "Anthropic token-count"
	case geminiOperationGenerateContent:
		return "Gemini GenerateContent"
	case geminiOperationStreamContent:
		return "Gemini streamGenerateContent"
	case geminiOperationCountTokens:
		return "Gemini token-count"
	case geminiOperationEmbedContent:
		return "Gemini EmbedContent"
	case geminiOperationBatchEmbed:
		return "Gemini BatchEmbedContents"
	case bedrockOperationConverse:
		return "Bedrock Converse"
	case bedrockOperationConverseStream:
		return "Bedrock ConverseStream"
	default:
		return "Chat Completions"
	}
}

func (o openAIOperation) decodeRequest(
	raw []byte,
	pathModel string,
) (map[string]json.RawMessage, string, bool, error) {
	if o.isBedrock() {
		return decodeBedrockRequest(raw, pathModel, o)
	}
	if o.isGemini() {
		return decodeGeminiRequest(raw, pathModel, o)
	}
	if o == anthropicOperationMessages {
		return decodeAnthropicMessagesRequest(raw)
	}
	if o == anthropicOperationCountTokens {
		return decodeAnthropicCountTokensRequest(raw)
	}
	if o == openAIOperationEmbeddings {
		return decodeEmbeddingsRequest(raw)
	}
	if o == openAIOperationResponsesCompact {
		return decodeResponsesCompactRequest(raw)
	}
	if o == openAIOperationResponses {
		return decodeResponsesRequest(raw)
	}
	return decodeChatRequest(raw)
}

func (o openAIOperation) detectCapabilities(
	envelope map[string]json.RawMessage,
	streaming bool,
) ([]config.Capability, error) {
	switch o {
	case anthropicOperationMessages, anthropicOperationCountTokens:
		return detectAnthropicCapabilities(envelope, o)
	case geminiOperationGenerateContent, geminiOperationStreamContent,
		geminiOperationCountTokens:
		return detectGeminiCapabilities(envelope, o)
	case bedrockOperationConverse, bedrockOperationConverseStream:
		return detectBedrockCapabilities(envelope)
	case openAIOperationEmbeddings:
		return detectEmbeddingsCapabilities(envelope, streaming)
	case openAIOperationResponsesCompact:
		return detectResponsesCompactCapabilities(envelope, streaming)
	case openAIOperationResponses:
		return detectResponsesCapabilities(envelope, streaming)
	default:
		return detectChatCapabilities(envelope, streaming)
	}
}

func (o openAIOperation) extractUsage(payload []byte) (ledger.TokenUsage, bool) {
	switch o {
	case anthropicOperationMessages:
		return extractAnthropicMessagesUsage(payload)
	case anthropicOperationCountTokens:
		return extractAnthropicCountTokensUsage(payload)
	case geminiOperationGenerateContent, geminiOperationStreamContent:
		return extractGeminiGenerateContentUsage(payload)
	case geminiOperationCountTokens:
		return extractGeminiCountTokensUsage(payload)
	case geminiOperationEmbedContent:
		return extractGeminiEmbedContentUsage(payload)
	case geminiOperationBatchEmbed:
		return extractGeminiBatchEmbedContentsUsage(payload)
	case bedrockOperationConverse, bedrockOperationConverseStream:
		return extractBedrockUsage(payload)
	case openAIOperationEmbeddings:
		return extractOpenAIEmbeddingsUsage(payload)
	case openAIOperationResponses, openAIOperationResponsesCompact:
		return extractOpenAIResponsesUsage(payload)
	default:
		return extractOpenAIChatUsage(payload)
	}
}

func (o openAIOperation) sseEventTransformer(
	model string,
	rewrite bool,
) func([]byte) []byte {
	if !rewrite {
		return nil
	}
	if o == anthropicOperationMessages {
		return func(event []byte) []byte {
			return rewriteAnthropicSSEEventModel(event, model)
		}
	}
	if o == geminiOperationStreamContent {
		return func(event []byte) []byte {
			return rewriteGeminiSSEEventModel(event, model)
		}
	}
	if o == openAIOperationResponses {
		return func(event []byte) []byte {
			return rewriteResponsesSSEEventModel(event, model)
		}
	}
	return func(event []byte) []byte {
		return rewriteSSEEventModel(event, model)
	}
}

func (o openAIOperation) upstreamURL(
	baseURL string,
	upstreamModel string,
) (string, error) {
	switch o {
	case geminiOperationGenerateContent, geminiOperationStreamContent,
		geminiOperationCountTokens, geminiOperationEmbedContent,
		geminiOperationBatchEmbed:
		return geminiOperationURL(baseURL, upstreamModel, o)
	case bedrockOperationConverse, bedrockOperationConverseStream:
		return bedrockOperationURL(baseURL, upstreamModel, o)
	case anthropicOperationMessages:
		return anthropicMessagesURL(baseURL)
	case anthropicOperationCountTokens:
		return anthropicCountTokensURL(baseURL)
	case openAIOperationEmbeddings:
		return embeddingsURL(baseURL)
	case openAIOperationResponsesCompact:
		return responsesCompactURL(baseURL)
	case openAIOperationResponses:
		return responsesURL(baseURL)
	default:
		return chatCompletionsURL(baseURL)
	}
}

func newResponsesHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	core := newOpenAIHandler(
		openAIOperationResponses,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
	compact := newOpenAIHandler(
		openAIOperationResponsesCompact,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
	return &responsesHandler{
		create:   core,
		compact:  compact,
		resource: &responsesResourceHandler{core: core},
	}
}

type responsesHandler struct {
	create   http.Handler
	compact  http.Handler
	resource http.Handler
}

func (h *responsesHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/v1/responses" {
		h.create.ServeHTTP(w, request)
		return
	}
	if request.URL.Path == "/v1/responses/compact" {
		h.compact.ServeHTTP(w, request)
		return
	}
	h.resource.ServeHTTP(w, request)
}

func (o openAIOperation) isResponsesOperation() bool {
	return o == openAIOperationResponses ||
		o == openAIOperationResponsesCompact
}

type unsupportedFeatureError struct {
	message string
}

func (e *unsupportedFeatureError) Error() string {
	return e.message
}

func unsupportedFeature(format string, values ...any) error {
	return &unsupportedFeatureError{message: fmt.Sprintf(format, values...)}
}

func requestValidationCode(err error) string {
	var unsupported *unsupportedFeatureError
	if errors.As(err, &unsupported) {
		return "unsupported_feature"
	}
	return "invalid_request_body"
}

func decodeResponsesRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, streaming, err := decodeChatRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	if rawStore, exists := envelope["store"]; exists {
		var store bool
		if err := json.Unmarshal(rawStore, &store); err != nil ||
			bytes.Equal(bytes.TrimSpace(rawStore), []byte("null")) {
			return nil, "", false, fmt.Errorf("store must be a boolean")
		}
	}
	if rawNonNull(envelope["previous_response_id"]) {
		var responseID string
		if err := json.Unmarshal(envelope["previous_response_id"], &responseID); err != nil {
			return nil, "", false, fmt.Errorf("previous_response_id must be a string")
		}
		if err := responsesstate.ValidateResponseID(responseID); err != nil {
			return nil, "", false, fmt.Errorf("previous_response_id: %w", err)
		}
	}
	conversationID, err := responsesConversationID(envelope["conversation"])
	if err != nil {
		return nil, "", false, err
	}
	if conversationID != "" && rawNonNull(envelope["previous_response_id"]) {
		return nil, "", false, fmt.Errorf(
			"conversation and previous_response_id cannot be used together",
		)
	}
	if rawNonNull(envelope["prompt"]) {
		return nil, "", false, unsupportedFeature(
			"prompt resource affinity is not yet supported",
		)
	}
	if rawBackground, exists := envelope["background"]; exists {
		var background bool
		if err := json.Unmarshal(rawBackground, &background); err != nil ||
			bytes.Equal(bytes.TrimSpace(rawBackground), []byte("null")) {
			return nil, "", false, fmt.Errorf("background must be a boolean")
		}
	}
	return envelope, model, streaming, nil
}

type responsesRequestState struct {
	ownerScope          string
	previousResponseID  string
	conversationID      string
	fileIDs             []string
	itemIDs             []string
	itemExpiresAt       time.Time
	durableItems        bool
	storesResponse      bool
	background          bool
	providerHostedTool  bool
	embeddingDimensions int64
	embeddingInputCount int
}

func responsesCallerScope(caller identity.Identity) string {
	principal := caller.Principal
	raw := "v1\x00" + principal.Tenant + "\x00" + principal.ID
	digest := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(digest[:])
}

func (s responsesRequestState) singleAttempt() bool {
	return s.previousResponseID != "" ||
		s.conversationID != "" ||
		len(s.fileIDs) != 0 ||
		len(s.itemIDs) != 0 ||
		s.bindsResponse() ||
		s.providerHostedTool
}

func (s responsesRequestState) bindsResponse() bool {
	return s.storesResponse || s.background || s.conversationID != ""
}

func (s responsesRequestState) returnedItemExpiry() time.Time {
	if s.durableItems || s.conversationID != "" {
		return time.Time{}
	}
	if !s.itemExpiresAt.IsZero() {
		return s.itemExpiresAt
	}
	return time.Now().UTC().Add(responsesstate.DefaultTTL)
}

func classifyResponsesState(
	envelope map[string]json.RawMessage,
) responsesRequestState {
	state := responsesRequestState{storesResponse: true}
	if rawStore, exists := envelope["store"]; exists {
		_ = json.Unmarshal(rawStore, &state.storesResponse)
	}
	if rawBackground, exists := envelope["background"]; exists {
		_ = json.Unmarshal(rawBackground, &state.background)
	}
	if rawNonNull(envelope["previous_response_id"]) {
		_ = json.Unmarshal(envelope["previous_response_id"], &state.previousResponseID)
	}
	state.conversationID, _ = responsesConversationID(envelope["conversation"])
	state.providerHostedTool = responsesUsesProviderHostedTools(envelope)
	return state
}

func responsesConversationID(raw json.RawMessage) (string, error) {
	if !rawNonNull(raw) {
		return "", nil
	}
	trimmed := bytes.TrimSpace(raw)
	var conversationID string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if err := json.Unmarshal(trimmed, &conversationID); err != nil {
			return "", fmt.Errorf("conversation must be a string or object")
		}
	} else {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil || object == nil {
			return "", fmt.Errorf("conversation must be a string or object")
		}
		if err := json.Unmarshal(object["id"], &conversationID); err != nil {
			return "", fmt.Errorf("conversation.id must be a string")
		}
	}
	if err := responsesstate.ValidateResourceID(conversationID); err != nil {
		return "", fmt.Errorf("conversation: %w", err)
	}
	return conversationID, nil
}

func responsesUsesProviderHostedTools(
	envelope map[string]json.RawMessage,
) bool {
	var tools []map[string]json.RawMessage
	if rawActive(envelope["tools"]) && json.Unmarshal(envelope["tools"], &tools) == nil {
		for _, tool := range tools {
			var toolType string
			_ = json.Unmarshal(tool["type"], &toolType)
			if toolType != "" && toolType != "function" && toolType != "custom" {
				return true
			}
		}
	}
	var choice map[string]json.RawMessage
	if rawActive(envelope["tool_choice"]) &&
		json.Unmarshal(envelope["tool_choice"], &choice) == nil {
		var toolType string
		_ = json.Unmarshal(choice["type"], &toolType)
		return toolType != "" && toolType != "function" && toolType != "custom"
	}
	return false
}

func responsesIDFromPayload(payload []byte) (string, bool, error) {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return "", false, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = fmt.Errorf("payload must be a JSON object")
		}
		return "", false, err
	}
	response := envelope
	if rawResponse := envelope["response"]; rawNonNull(rawResponse) {
		if err := json.Unmarshal(rawResponse, &response); err != nil || response == nil {
			if err == nil {
				err = fmt.Errorf("response must be a JSON object")
			}
			return "", false, err
		}
	}
	if !rawNonNull(response["id"]) {
		return "", false, nil
	}
	var responseID string
	if err := json.Unmarshal(response["id"], &responseID); err != nil {
		return "", false, fmt.Errorf("response id must be a string")
	}
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return "", false, err
	}
	return responseID, true, nil
}

func (h *chatCompletionsHandler) bindResponsesAffinity(
	ctx context.Context,
	ownerScope string,
	responseID string,
	selection routing.Selection,
	durable bool,
) error {
	now := time.Now().UTC()
	if durable {
		resourceStore, ok := h.responsesState.(responsesstate.ResourceStore)
		if !ok {
			return fmt.Errorf("durable response affinity storage is not configured")
		}
		return resourceStore.BindResource(ctx, responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceResponse,
				ResourceID: responseID,
			},
			VirtualModel:  selection.VirtualModel,
			Provider:      selection.Provider.Name,
			Deployment:    selection.Deployment.Name,
			UpstreamModel: selection.Deployment.Model,
			BoundAt:       now,
		})
	}
	return h.responsesState.Bind(ctx, responsesstate.Affinity{
		Scope:         ownerScope,
		ResponseID:    responseID,
		VirtualModel:  selection.VirtualModel,
		Provider:      selection.Provider.Name,
		Deployment:    selection.Deployment.Name,
		UpstreamModel: selection.Deployment.Model,
		BoundAt:       now,
		ExpiresAt:     now.Add(responsesstate.DefaultTTL),
	})
}

func resolveResponsesAffinity(
	ctx context.Context,
	store responsesstate.Store,
	ownerScope string,
	responseID string,
) (responsesstate.Affinity, bool, bool, error) {
	if resourceStore, ok := store.(responsesstate.ResourceStore); ok {
		resource, found, err := resourceStore.ResolveResource(
			ctx,
			responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceResponse,
				ResourceID: responseID,
			},
		)
		if err != nil {
			return responsesstate.Affinity{}, false, false, err
		}
		if found {
			return responsesstate.Affinity{
				Scope:         resource.Scope,
				ResponseID:    resource.ResourceID,
				VirtualModel:  resource.VirtualModel,
				Provider:      resource.Provider,
				Deployment:    resource.Deployment,
				UpstreamModel: resource.UpstreamModel,
				BoundAt:       resource.BoundAt,
				ExpiresAt:     resource.ExpiresAt,
			}, true, resource.ExpiresAt.IsZero(), nil
		}
	}
	affinity, found, err := store.Resolve(ctx, ownerScope, responseID)
	return affinity, found, false, err
}

func detectResponsesCapabilities(
	envelope map[string]json.RawMessage,
	_ bool,
) ([]config.Capability, error) {
	required := map[config.Capability]struct{}{
		config.CapabilityResponses: {},
	}
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}
	state := classifyResponsesState(envelope)
	if state.bindsResponse() || state.previousResponseID != "" {
		add(config.CapabilityStoredCompletion)
	}
	if state.conversationID != "" {
		add(config.CapabilityConversations)
	}
	if state.background {
		add(config.CapabilityBackgroundResponses)
	}

	if err := inspectResponsesInput(envelope["input"], add); err != nil {
		return nil, err
	}
	if err := inspectResponsesInput(envelope["instructions"], add); err != nil {
		return nil, fmt.Errorf("instructions: %w", err)
	}
	if err := inspectResponsesTools(envelope["tools"], add); err != nil {
		return nil, err
	}
	if err := inspectResponsesToolChoice(envelope["tool_choice"], add); err != nil {
		return nil, err
	}
	if rawJSONBool(envelope["parallel_tool_calls"]) {
		add(config.CapabilityTools, config.CapabilityParallelTools)
	}
	if err := inspectResponsesText(envelope["text"], add); err != nil {
		return nil, err
	}
	if rawActive(envelope["reasoning"]) {
		add(config.CapabilityReasoning)
	}
	if rawJSONIntGreaterThan(envelope["top_logprobs"], 0) {
		add(config.CapabilityLogprobs)
	}
	if rawActive(envelope["service_tier"]) {
		add(config.CapabilityServiceTier)
	}
	if responsesPromptCacheActive(envelope) {
		add(config.CapabilityPromptCaching)
	}
	if raw := envelope["include"]; rawActive(raw) {
		var include []string
		if err := json.Unmarshal(raw, &include); err != nil {
			return nil, fmt.Errorf("include must be an array of strings")
		}
		for _, value := range include {
			if value == "message.output_text.logprobs" {
				add(config.CapabilityLogprobs)
			}
		}
	}

	result := make([]config.Capability, 0, len(required))
	for capability := range required {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return result, nil
}

func inspectResponsesTools(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("tools must be an array of objects")
	}
	for index, tool := range tools {
		var toolType string
		if err := json.Unmarshal(tool["type"], &toolType); err != nil || toolType == "" {
			return fmt.Errorf("tools[%d].type must be a non-empty string", index)
		}
		switch toolType {
		case "function", "custom":
			add(config.CapabilityTools)
		default:
			add(config.CapabilityProviderTools)
		}
	}
	return nil
}

func inspectResponsesToolChoice(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var choice string
		if err := json.Unmarshal(trimmed, &choice); err != nil {
			return fmt.Errorf("tool_choice must be a string or object")
		}
		if choice != "none" {
			add(config.CapabilityTools)
		}
		return nil
	}
	var choice map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &choice); err != nil || choice == nil {
		return fmt.Errorf("tool_choice must be a string or object")
	}
	var toolType string
	if err := json.Unmarshal(choice["type"], &toolType); err != nil || toolType == "" {
		return fmt.Errorf("tool_choice.type must be a non-empty string")
	}
	switch toolType {
	case "function", "custom":
		add(config.CapabilityTools)
	default:
		add(config.CapabilityProviderTools)
	}
	return nil
}

func inspectResponsesText(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	var textOptions map[string]json.RawMessage
	if err := json.Unmarshal(raw, &textOptions); err != nil {
		return fmt.Errorf("text must be an object")
	}
	rawFormat := textOptions["format"]
	if !rawActive(rawFormat) {
		return nil
	}
	var format map[string]json.RawMessage
	if err := json.Unmarshal(rawFormat, &format); err != nil {
		return fmt.Errorf("text.format must be an object")
	}
	var formatType string
	if rawType := format["type"]; rawActive(rawType) {
		if err := json.Unmarshal(rawType, &formatType); err != nil {
			return fmt.Errorf("text.format.type must be a string")
		}
	}
	switch formatType {
	case "", "text":
	case "json_object":
		add(config.CapabilityJSONMode)
	default:
		add(config.CapabilityStructuredOutputs)
	}
	return nil
}

func inspectResponsesInput(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '"':
		var value string
		if err := json.Unmarshal(trimmed, &value); err != nil {
			return fmt.Errorf("must be a string or an array of input items")
		}
		return nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return fmt.Errorf("must be an array of input items")
		}
		for index, item := range items {
			if err := inspectResponsesInputItem(item, add); err != nil {
				return fmt.Errorf("item %d: %w", index, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("must be a string or an array of input items")
	}
}

func inspectResponsesInputItem(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return fmt.Errorf("must be an object")
	}
	itemType, err := responsesInputItemType(item)
	if err != nil {
		return err
	}
	var role string
	if rawRole := item["role"]; rawActive(rawRole) {
		if err := json.Unmarshal(rawRole, &role); err != nil {
			return fmt.Errorf("role must be a string")
		}
	}
	if role == "developer" || role == "system" {
		add(config.CapabilityDeveloperMessages)
	}
	if rawNonNull(item["file_id"]) {
		if _, err := decodeProviderFileID(item["file_id"]); err != nil {
			return err
		}
		add(config.CapabilityFileInput, config.CapabilityFiles)
	}
	switch itemType {
	case "input_image", "image_url", "input_video", "video":
		add(config.CapabilityVision)
	case "input_audio", "audio":
		add(config.CapabilityAudioInput)
	case "input_file", "file", "document":
		add(config.CapabilityFileInput)
	case "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
		add(config.CapabilityTools)
	case "reasoning":
		add(config.CapabilityReasoning)
	case "item_reference":
		add(config.CapabilityStoredCompletion)
	}
	if _, requiresAffinity := responsesItemAffinityField(itemType, item); requiresAffinity {
		add(config.CapabilityStoredCompletion)
	}
	if responsesProviderToolItem(itemType) {
		add(config.CapabilityProviderTools)
	}
	if rawContent := item["content"]; rawActive(rawContent) {
		if err := inspectResponsesInput(rawContent, add); err != nil {
			return fmt.Errorf("content: %w", err)
		}
	}
	if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
		if rawOutput := item["output"]; rawActive(rawOutput) {
			if err := inspectResponsesInput(rawOutput, add); err != nil {
				return fmt.Errorf("output: %w", err)
			}
		}
	}
	return nil
}

func inspectResponsesStreamEvent(
	payload []byte,
) (bool, ledger.Outcome, string) {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		return false, "", ""
	}
	switch event.Type {
	case "response.completed":
		return true, ledger.OutcomeSuccess, ""
	case "response.failed":
		return true, ledger.OutcomeUpstreamError, "response_failed"
	case "response.incomplete":
		return true, ledger.OutcomeUpstreamError, "response_incomplete"
	case "response.cancelled":
		return true, ledger.OutcomeUpstreamError, "response_cancelled"
	case "error":
		return true, ledger.OutcomeStreamError, "response_stream_error"
	default:
		return false, "", ""
	}
}

func responsesURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/responses"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
