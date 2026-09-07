package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

const geminiModelsPathPrefix = "/v1beta/models/"

type geminiPathModelContextKey struct{}

func withGeminiPathModel(ctx context.Context, model string) context.Context {
	return context.WithValue(ctx, geminiPathModelContextKey{}, model)
}

func geminiPathModel(ctx context.Context) string {
	model, _ := ctx.Value(geminiPathModelContextKey{}).(string)
	return model
}

func newGeminiHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return &geminiHandler{
		generate: newOpenAIHandler(
			geminiOperationGenerateContent,
			snapshot,
			targets,
			retryBudget,
			options,
			instrumentation,
		),
		stream: newOpenAIHandler(
			geminiOperationStreamContent,
			snapshot,
			targets,
			retryBudget,
			options,
			instrumentation,
		),
		countTokens: newOpenAIHandler(
			geminiOperationCountTokens,
			snapshot,
			targets,
			retryBudget,
			options,
			instrumentation,
		),
	}
}

type geminiHandler struct {
	generate    http.Handler
	stream      http.Handler
	countTokens http.Handler
}

func (h *geminiHandler) ServeHTTP(
	w http.ResponseWriter,
	request *http.Request,
) {
	model, operation, err := parseGeminiRoute(request.URL.Path)
	if err != nil {
		geminiOperationGenerateContent.writeError(
			w,
			http.StatusNotFound,
			"route_not_found",
			err.Error(),
			"",
		)
		return
	}
	request = request.WithContext(
		withGeminiPathModel(request.Context(), model),
	)
	switch operation {
	case geminiOperationGenerateContent:
		h.generate.ServeHTTP(w, request)
	case geminiOperationStreamContent:
		h.stream.ServeHTTP(w, request)
	case geminiOperationCountTokens:
		h.countTokens.ServeHTTP(w, request)
	default:
		geminiOperationGenerateContent.writeError(
			w,
			http.StatusNotFound,
			"route_not_found",
			"route not found",
			"",
		)
	}
}

func parseGeminiRoute(
	path string,
) (string, openAIOperation, error) {
	if !strings.HasPrefix(path, geminiModelsPathPrefix) {
		return "", "", fmt.Errorf("route not found")
	}
	resource := strings.TrimPrefix(path, geminiModelsPathPrefix)
	separator := strings.LastIndexByte(resource, ':')
	if separator <= 0 || separator == len(resource)-1 {
		return "", "", fmt.Errorf("route not found")
	}
	model := resource[:separator]
	if strings.Contains(model, "/") ||
		strings.TrimSpace(model) != model ||
		len(model) > maxRequestedModelBytes {
		return "", "", fmt.Errorf("route not found")
	}
	var operation openAIOperation
	switch resource[separator+1:] {
	case "generateContent":
		operation = geminiOperationGenerateContent
	case "streamGenerateContent":
		operation = geminiOperationStreamContent
	case "countTokens":
		operation = geminiOperationCountTokens
	default:
		return "", "", fmt.Errorf("route not found")
	}
	return model, operation, nil
}

func (o openAIOperation) isGemini() bool {
	switch o {
	case geminiOperationGenerateContent, geminiOperationStreamContent,
		geminiOperationCountTokens, geminiOperationEmbedContent,
		geminiOperationBatchEmbed:
		return true
	default:
		return false
	}
}

func (o openAIOperation) pathModel(ctx context.Context) string {
	if o.isGemini() {
		return geminiPathModel(ctx)
	}
	if o.isBedrock() {
		return bedrockPathModel(ctx)
	}
	return ""
}

func decodeGeminiRequest(
	raw []byte,
	pathModel string,
	operation openAIOperation,
) (map[string]json.RawMessage, string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "", false, fmt.Errorf("request body is required")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", false, fmt.Errorf("request body must be valid JSON")
	}
	if envelope == nil {
		return nil, "", false, fmt.Errorf("request body must be a JSON object")
	}
	if pathModel == "" {
		return nil, "", false, fmt.Errorf("model is required in the request path")
	}
	if len(pathModel) > maxRequestedModelBytes {
		return nil, "", false, fmt.Errorf(
			"model must not exceed %d bytes",
			maxRequestedModelBytes,
		)
	}
	if rawNonNull(envelope["model"]) {
		if err := validateGeminiBodyModel(
			envelope["model"],
			pathModel,
		); err != nil {
			return nil, "", false, err
		}
	}
	streaming := operation == geminiOperationStreamContent
	switch operation {
	case geminiOperationGenerateContent, geminiOperationStreamContent:
		if err := validateGeminiGenerateRequest(
			envelope,
		); err != nil {
			return nil, "", false, err
		}
	case geminiOperationCountTokens:
		if err := validateGeminiCountTokensRequest(
			envelope,
			pathModel,
		); err != nil {
			return nil, "", false, err
		}
	default:
		return nil, "", false, fmt.Errorf("unsupported Gemini operation")
	}
	return envelope, pathModel, streaming, nil
}

func validateGeminiBodyModel(
	raw json.RawMessage,
	pathModel string,
) error {
	var bodyModel string
	if err := json.Unmarshal(raw, &bodyModel); err != nil ||
		strings.TrimSpace(bodyModel) == "" {
		return fmt.Errorf("model must be a non-empty string")
	}
	bodyModel = strings.TrimPrefix(bodyModel, "models/")
	if bodyModel != pathModel {
		return fmt.Errorf("request body model must match the request path model")
	}
	return nil
}

func validateGeminiGenerateRequest(
	envelope map[string]json.RawMessage,
) error {
	if err := validateGeminiAliasGroups(
		envelope,
		[]string{"cachedContent", "cached_content"},
		[]string{"toolConfig", "tool_config"},
		[]string{"serviceTier", "service_tier"},
		[]string{"systemInstruction", "system_instruction"},
		[]string{"generationConfig", "generation_config"},
	); err != nil {
		return err
	}
	if err := validateGeminiContents(envelope["contents"]); err != nil {
		return fmt.Errorf("contents: %w", err)
	}
	if geminiRawNonNull(
		envelope,
		"cachedContent",
		"cached_content",
	) {
		return unsupportedFeature(
			"cachedContent resource affinity is not yet supported",
		)
	}
	if rawStore, exists := envelope["store"]; exists {
		var store bool
		if err := json.Unmarshal(rawStore, &store); err != nil ||
			bytes.Equal(bytes.TrimSpace(rawStore), []byte("null")) {
			return fmt.Errorf("store must be a boolean")
		}
	}
	return nil
}

func validateGeminiCountTokensRequest(
	envelope map[string]json.RawMessage,
	pathModel string,
) error {
	if err := validateGeminiAliasGroups(
		envelope,
		[]string{
			"generateContentRequest",
			"generate_content_request",
		},
	); err != nil {
		return err
	}
	hasContents := rawNonNull(envelope["contents"])
	rawGenerateRequest, _ := geminiRawField(
		envelope,
		"generateContentRequest",
		"generate_content_request",
	)
	hasGenerateRequest := rawNonNull(rawGenerateRequest)
	if hasContents == hasGenerateRequest {
		return fmt.Errorf(
			"exactly one of contents or generateContentRequest is required",
		)
	}
	if hasContents {
		if err := validateGeminiContents(envelope["contents"]); err != nil {
			return fmt.Errorf("contents: %w", err)
		}
		return nil
	}
	var generateRequest map[string]json.RawMessage
	if err := json.Unmarshal(
		rawGenerateRequest,
		&generateRequest,
	); err != nil || generateRequest == nil {
		return fmt.Errorf("generateContentRequest must be a JSON object")
	}
	if rawNonNull(generateRequest["model"]) {
		if err := validateGeminiBodyModel(
			generateRequest["model"],
			pathModel,
		); err != nil {
			return fmt.Errorf("generateContentRequest: %w", err)
		}
	}
	return validateGeminiGenerateRequest(generateRequest)
}

func validateGeminiContents(raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("is required")
	}
	switch trimmed[0] {
	case '{':
		var content map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &content); err != nil ||
			content == nil {
			return fmt.Errorf("must be an object or array of objects")
		}
	case '[':
		var contents []json.RawMessage
		if err := json.Unmarshal(trimmed, &contents); err != nil ||
			len(contents) == 0 {
			return fmt.Errorf("must be a non-empty array of objects")
		}
		for index, content := range contents {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(content, &object); err != nil ||
				object == nil {
				return fmt.Errorf("item %d must be an object", index)
			}
		}
	default:
		return fmt.Errorf("must be an object or array of objects")
	}
	return nil
}

func (o openAIOperation) guardrailRequestBody(
	raw []byte,
	envelope map[string]json.RawMessage,
	requestedModel string,
) ([]byte, error) {
	if !o.isGemini() {
		return append([]byte(nil), raw...), nil
	}
	guardrailEnvelope := cloneJSONEnvelope(envelope)
	rawModel, err := json.Marshal(requestedModel)
	if err != nil {
		return nil, err
	}
	guardrailEnvelope["model"] = rawModel
	return json.Marshal(guardrailEnvelope)
}

func (o openAIOperation) rewriteRequest(
	envelope map[string]json.RawMessage,
	upstreamModel string,
) ([]byte, error) {
	if o.isBedrock() {
		return rewriteBedrockRequest(envelope)
	}
	if !o.isGemini() {
		return rewriteModel(envelope, upstreamModel)
	}
	rewritten := cloneJSONEnvelope(envelope)
	delete(rewritten, "model")
	rawGenerateRequest, generateRequestField := geminiRawField(
		rewritten,
		"generateContentRequest",
		"generate_content_request",
	)
	if o == geminiOperationCountTokens &&
		rawNonNull(rawGenerateRequest) {
		var generateRequest map[string]json.RawMessage
		if err := json.Unmarshal(
			rawGenerateRequest,
			&generateRequest,
		); err != nil || generateRequest == nil {
			return nil, fmt.Errorf(
				"generateContentRequest must be a JSON object",
			)
		}
		if _, exists := generateRequest["model"]; exists {
			rawModel, err := json.Marshal(
				"models/" + geminiModelID(upstreamModel),
			)
			if err != nil {
				return nil, err
			}
			generateRequest["model"] = rawModel
			rewritten[generateRequestField], err = json.Marshal(
				generateRequest,
			)
			if err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(rewritten)
}

func geminiModelID(model string) string {
	return strings.TrimPrefix(model, "models/")
}

func geminiOperationURL(
	baseURL string,
	upstreamModel string,
	operation openAIOperation,
) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	model := geminiModelID(upstreamModel)
	if strings.TrimSpace(model) == "" ||
		strings.TrimSpace(model) != model ||
		strings.Contains(model, "/") {
		return "", fmt.Errorf(
			"the Gemini upstream model must be a non-empty models/* identifier",
		)
	}
	var action string
	switch operation {
	case geminiOperationGenerateContent:
		action = "generateContent"
	case geminiOperationStreamContent:
		action = "streamGenerateContent"
	case geminiOperationCountTokens:
		action = "countTokens"
	case geminiOperationEmbedContent:
		action = "embedContent"
	case geminiOperationBatchEmbed:
		action = "batchEmbedContents"
	default:
		return "", fmt.Errorf("unsupported Gemini operation")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") +
		"/models/" + model + ":" + action
	parsed.RawPath = ""
	query := make(url.Values)
	if operation == geminiOperationStreamContent {
		query.Set("alt", "sse")
	}
	parsed.RawQuery = query.Encode()
	parsed.Fragment = ""
	return parsed.String(), nil
}

func detectGeminiCapabilities(
	envelope map[string]json.RawMessage,
	operation openAIOperation,
) ([]config.Capability, error) {
	inspected := envelope
	rawGenerateRequest, _ := geminiRawField(
		envelope,
		"generateContentRequest",
		"generate_content_request",
	)
	if operation == geminiOperationCountTokens &&
		rawNonNull(rawGenerateRequest) {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(
			rawGenerateRequest,
			&nested,
		); err != nil || nested == nil {
			return nil, fmt.Errorf(
				"generateContentRequest must be a JSON object",
			)
		}
		inspected = nested
	}
	required := make(map[config.Capability]struct{})
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}
	if operation == geminiOperationCountTokens {
		add(config.CapabilityTokenCounting)
	}
	if geminiRawNonNull(inspected, "toolConfig", "tool_config") {
		add(config.CapabilityTools)
	}
	if rawJSONBool(inspected["store"]) {
		add(config.CapabilityStoredCompletion)
	}
	if geminiRawActive(inspected, "serviceTier", "service_tier") {
		add(config.CapabilityServiceTier)
	}
	if err := inspectGeminiTools(inspected["tools"], add); err != nil {
		return nil, err
	}
	if err := inspectGeminiContents(inspected["contents"], add); err != nil {
		return nil, err
	}
	rawSystemInstruction, _ := geminiRawField(
		inspected,
		"systemInstruction",
		"system_instruction",
	)
	if err := inspectGeminiContent(rawSystemInstruction, add); err != nil {
		return nil, fmt.Errorf("systemInstruction: %w", err)
	}
	rawGenerationConfig, _ := geminiRawField(
		inspected,
		"generationConfig",
		"generation_config",
	)
	if err := inspectGeminiGenerationConfig(
		rawGenerationConfig,
		add,
	); err != nil {
		return nil, err
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

func inspectGeminiTools(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	tools, err := geminiObjectList(raw, "tools")
	if err != nil {
		return err
	}
	for _, tool := range tools {
		for name, value := range tool {
			if !rawNonNull(value) {
				continue
			}
			normalizedName := strings.ReplaceAll(
				strings.ToLower(name),
				"_",
				"",
			)
			if normalizedName == "functiondeclarations" {
				add(config.CapabilityTools)
				continue
			}
			add(config.CapabilityProviderTools)
		}
	}
	return nil
}

func inspectGeminiContents(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	contents, err := geminiObjectList(raw, "contents")
	if err != nil {
		return err
	}
	for index, content := range contents {
		encoded, err := json.Marshal(content)
		if err != nil {
			return err
		}
		if err := inspectGeminiContent(encoded, add); err != nil {
			return fmt.Errorf("contents item %d: %w", index, err)
		}
	}
	return nil
}

func inspectGeminiContent(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	var content map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil ||
		content == nil {
		return fmt.Errorf("must be a JSON object")
	}
	if !rawActive(content["parts"]) {
		return nil
	}
	parts, err := geminiObjectList(content["parts"], "parts")
	if err != nil {
		return err
	}
	for index, part := range parts {
		if err := validateGeminiAliasGroups(
			part,
			[]string{"fileData", "file_data"},
			[]string{"functionCall", "function_call"},
			[]string{"functionResponse", "function_response"},
			[]string{"executableCode", "executable_code"},
			[]string{
				"codeExecutionResult",
				"code_execution_result",
			},
			[]string{"inlineData", "inline_data"},
		); err != nil {
			return fmt.Errorf("parts item %d: %w", index, err)
		}
		if geminiRawNonNull(part, "fileData", "file_data") {
			return unsupportedFeature(
				"parts item %d fileData resource affinity is not yet supported",
				index,
			)
		}
		if geminiRawNonNull(part, "functionCall", "function_call") ||
			geminiRawNonNull(
				part,
				"functionResponse",
				"function_response",
			) {
			add(config.CapabilityTools)
		}
		if geminiRawNonNull(
			part,
			"executableCode",
			"executable_code",
		) ||
			geminiRawNonNull(
				part,
				"codeExecutionResult",
				"code_execution_result",
			) {
			add(config.CapabilityProviderTools)
		}
		if rawJSONBool(part["thought"]) {
			add(config.CapabilityReasoning)
		}
		rawInlineData, _ := geminiRawField(
			part,
			"inlineData",
			"inline_data",
		)
		if rawNonNull(rawInlineData) {
			if err := inspectGeminiInlineData(
				rawInlineData,
				add,
			); err != nil {
				return fmt.Errorf("parts item %d: %w", index, err)
			}
		}
	}
	return nil
}

func inspectGeminiInlineData(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil || data == nil {
		return fmt.Errorf("inlineData must be a JSON object")
	}
	if err := validateGeminiAliasGroups(
		data,
		[]string{"mimeType", "mime_type"},
	); err != nil {
		return fmt.Errorf("inlineData: %w", err)
	}
	var mimeType string
	rawMIMEType, _ := geminiRawField(data, "mimeType", "mime_type")
	if rawActive(rawMIMEType) {
		if err := json.Unmarshal(rawMIMEType, &mimeType); err != nil {
			return fmt.Errorf("inlineData.mimeType must be a string")
		}
	}
	if mimeType == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return fmt.Errorf("inlineData.mimeType must be a valid media type")
	}
	switch {
	case strings.HasPrefix(mediaType, "image/"),
		strings.HasPrefix(mediaType, "video/"):
		add(config.CapabilityVision)
	case strings.HasPrefix(mediaType, "audio/"):
		add(config.CapabilityAudioInput)
	default:
		add(config.CapabilityFileInput)
	}
	return nil
}

func inspectGeminiGenerationConfig(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) {
		return nil
	}
	var generation map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generation); err != nil ||
		generation == nil {
		return fmt.Errorf("generationConfig must be a JSON object")
	}
	if err := validateGeminiAliasGroups(
		generation,
		[]string{"responseMimeType", "response_mime_type"},
		[]string{"responseSchema", "response_schema"},
		[]string{
			"responseJsonSchema",
			"response_json_schema",
			"_responseJsonSchema",
			"_response_json_schema",
		},
		[]string{"responseFormat", "response_format"},
		[]string{"thinkingConfig", "thinking_config"},
		[]string{"responseLogprobs", "response_logprobs"},
		[]string{"candidateCount", "candidate_count"},
		[]string{"speechConfig", "speech_config"},
		[]string{"imageConfig", "image_config"},
		[]string{"responseModalities", "response_modalities"},
	); err != nil {
		return fmt.Errorf("generationConfig: %w", err)
	}
	var responseMIME string
	rawResponseMIME, _ := geminiRawField(
		generation,
		"responseMimeType",
		"response_mime_type",
	)
	if rawActive(rawResponseMIME) {
		if err := json.Unmarshal(
			rawResponseMIME,
			&responseMIME,
		); err != nil {
			return fmt.Errorf(
				"generationConfig.responseMimeType must be a string",
			)
		}
	}
	if responseMIME == "application/json" ||
		responseMIME == "text/x.enum" {
		add(config.CapabilityJSONMode)
	}
	if geminiRawNonNull(
		generation,
		"responseSchema",
		"response_schema",
	) ||
		geminiRawNonNull(
			generation,
			"responseJsonSchema",
			"response_json_schema",
			"_responseJsonSchema",
			"_response_json_schema",
		) ||
		geminiRawNonNull(
			generation,
			"responseFormat",
			"response_format",
		) {
		add(config.CapabilityStructuredOutputs)
	}
	if geminiRawNonNull(
		generation,
		"thinkingConfig",
		"thinking_config",
	) {
		add(config.CapabilityReasoning)
	}
	if rawActive(generation["seed"]) {
		add(config.CapabilitySeed)
	}
	rawResponseLogprobs, _ := geminiRawField(
		generation,
		"responseLogprobs",
		"response_logprobs",
	)
	if rawJSONBool(rawResponseLogprobs) ||
		rawJSONIntGreaterThan(generation["logprobs"], 0) {
		add(config.CapabilityLogprobs)
	}
	rawCandidateCount, _ := geminiRawField(
		generation,
		"candidateCount",
		"candidate_count",
	)
	if rawJSONIntGreaterThan(rawCandidateCount, 1) {
		add(config.CapabilityMultipleChoices)
	}
	if geminiRawNonNull(
		generation,
		"speechConfig",
		"speech_config",
	) {
		add(config.CapabilityAudioOutput)
	}
	if geminiRawNonNull(
		generation,
		"imageConfig",
		"image_config",
	) {
		add(config.CapabilityVision)
	}
	rawResponseModalities, _ := geminiRawField(
		generation,
		"responseModalities",
		"response_modalities",
	)
	if rawActive(rawResponseModalities) {
		var modalities []string
		if err := json.Unmarshal(
			rawResponseModalities,
			&modalities,
		); err != nil {
			return fmt.Errorf(
				"generationConfig.responseModalities must be an array of strings",
			)
		}
		for _, modality := range modalities {
			switch strings.ToUpper(modality) {
			case "AUDIO":
				add(config.CapabilityAudioOutput)
			case "IMAGE":
				add(config.CapabilityVision)
			}
		}
	}
	return nil
}

func geminiRawField(
	object map[string]json.RawMessage,
	names ...string,
) (json.RawMessage, string) {
	var fallback json.RawMessage
	var fallbackName string
	for _, name := range names {
		if value, exists := object[name]; exists {
			if rawNonNull(value) {
				return value, name
			}
			if fallbackName == "" {
				fallback = value
				fallbackName = name
			}
		}
	}
	return fallback, fallbackName
}

func geminiRawNonNull(
	object map[string]json.RawMessage,
	names ...string,
) bool {
	for _, name := range names {
		if rawNonNull(object[name]) {
			return true
		}
	}
	return false
}

func geminiRawActive(
	object map[string]json.RawMessage,
	names ...string,
) bool {
	for _, name := range names {
		if rawActive(object[name]) {
			return true
		}
	}
	return false
}

func validateGeminiAliasGroups(
	object map[string]json.RawMessage,
	groups ...[]string,
) error {
	for _, names := range groups {
		found := make([]string, 0, len(names))
		for _, name := range names {
			if _, exists := object[name]; exists {
				found = append(found, name)
			}
		}
		if len(found) > 1 {
			return fmt.Errorf(
				"fields %s are aliases and cannot be used together",
				strings.Join(found, ", "),
			)
		}
	}
	return nil
}

func geminiObjectList(
	raw json.RawMessage,
	name string,
) ([]map[string]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}
	switch trimmed[0] {
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil ||
			object == nil {
			return nil, fmt.Errorf(
				"%s must be an object or array of objects",
				name,
			)
		}
		return []map[string]json.RawMessage{object}, nil
	case '[':
		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &objects); err != nil {
			return nil, fmt.Errorf(
				"%s must be an object or array of objects",
				name,
			)
		}
		for index, object := range objects {
			if object == nil {
				return nil, fmt.Errorf(
					"%s item %d must be an object",
					name,
					index,
				)
			}
		}
		return objects, nil
	default:
		return nil, fmt.Errorf(
			"%s must be an object or array of objects",
			name,
		)
	}
}

func (o openAIOperation) rewriteJSONResponseModel(
	body []byte,
	model string,
) ([]byte, error) {
	if o.isBedrock() {
		return append([]byte(nil), body...), nil
	}
	if !o.isGemini() {
		return rewriteJSONResponseModel(body, model)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		if err == nil {
			err = fmt.Errorf("response must be a JSON object")
		}
		return nil, err
	}
	if _, exists := envelope["modelVersion"]; !exists {
		return body, nil
	}
	rawModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	envelope["modelVersion"] = rawModel
	return json.Marshal(envelope)
}

func rewriteGeminiSSEEventModel(event []byte, model string) []byte {
	rawModel, err := json.Marshal(model)
	if err != nil {
		return event
	}
	return rewriteSSEEventJSON(
		event,
		len(model),
		func(envelope map[string]json.RawMessage) bool {
			if _, exists := envelope["modelVersion"]; !exists {
				return false
			}
			envelope["modelVersion"] = rawModel
			return true
		},
	)
}

type geminiErrorEnvelope struct {
	Error geminiError `json:"error"`
}

type geminiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func writeGeminiError(
	w http.ResponseWriter,
	status int,
	message string,
) {
	writeJSON(w, status, geminiErrorEnvelope{
		Error: geminiError{
			Code:    status,
			Message: message,
			Status:  geminiErrorStatus(status),
		},
	})
}

func geminiErrorStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "ABORTED"
	case http.StatusTooManyRequests,
		http.StatusRequestEntityTooLarge:
		return "RESOURCE_EXHAUSTED"
	case http.StatusNotImplemented:
		return "UNIMPLEMENTED"
	case http.StatusBadGateway,
		http.StatusServiceUnavailable:
		return "UNAVAILABLE"
	case http.StatusGatewayTimeout:
		return "DEADLINE_EXCEEDED"
	default:
		if status >= 500 {
			return "INTERNAL"
		}
		return "INVALID_ARGUMENT"
	}
}

func validateGeminiSuccessResponse(
	operation openAIOperation,
	body []byte,
) error {
	if err := validateJSONResponse(body); err != nil {
		return err
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return fmt.Errorf("the Gemini response must be a JSON object")
	}
	if rawNonNull(envelope["error"]) {
		return fmt.Errorf("the Gemini success response must not contain an error")
	}
	if operation == geminiOperationCountTokens {
		if _, found := extractGeminiCountTokensUsage(body); !found {
			return fmt.Errorf(
				"token-count response must contain non-negative totalTokens",
			)
		}
	}
	return nil
}

func inspectGeminiStreamEvent(
	payload []byte,
) (bool, ledger.Outcome, string) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil {
		return true, ledger.OutcomeStreamError,
			"gemini_stream_invalid_event"
	}
	if !rawNonNull(envelope["error"]) {
		return false, ledger.OutcomeSuccess, ""
	}
	failureClass := "gemini_stream_error"
	var source struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(envelope["error"], &source) == nil {
		suffix := strings.ToLower(source.Status)
		if validFailureClassSuffix(suffix) {
			failureClass += "_" + suffix
		}
	}
	return true, ledger.OutcomeUpstreamError, failureClass
}
