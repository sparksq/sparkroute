package gateway

import (
	"bytes"
	"context"
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
	bedrockModelsPathPrefix = "/model/"

	bedrockAdditionalFieldsCapability config.Capability = "x-bedrock-additional-fields"
	bedrockRequestMetadataCapability  config.Capability = "x-bedrock-request-metadata"
)

type bedrockPathModelContextKey struct{}

func withBedrockPathModel(
	ctx context.Context,
	model string,
) context.Context {
	return context.WithValue(ctx, bedrockPathModelContextKey{}, model)
}

func bedrockPathModel(ctx context.Context) string {
	model, _ := ctx.Value(bedrockPathModelContextKey{}).(string)
	return model
}

func newBedrockHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return &bedrockHandler{
		converse: newOpenAIHandler(
			bedrockOperationConverse,
			snapshot,
			targets,
			retryBudget,
			options,
			instrumentation,
		),
		stream: newOpenAIHandler(
			bedrockOperationConverseStream,
			snapshot,
			targets,
			retryBudget,
			options,
			instrumentation,
		),
	}
}

type bedrockHandler struct {
	converse http.Handler
	stream   http.Handler
}

func (h *bedrockHandler) ServeHTTP(
	w http.ResponseWriter,
	request *http.Request,
) {
	model, operation, err := parseBedrockRoute(request.URL.Path)
	if err != nil {
		bedrockOperationConverse.writeError(
			w,
			http.StatusNotFound,
			"route_not_found",
			"route not found",
			"",
		)
		return
	}
	request = request.WithContext(
		withBedrockPathModel(request.Context(), model),
	)
	switch operation {
	case bedrockOperationConverse:
		h.converse.ServeHTTP(w, request)
	case bedrockOperationConverseStream:
		h.stream.ServeHTTP(w, request)
	default:
		bedrockOperationConverse.writeError(
			w,
			http.StatusNotFound,
			"route_not_found",
			"route not found",
			"",
		)
	}
}

func parseBedrockRoute(
	requestPath string,
) (string, openAIOperation, error) {
	if !strings.HasPrefix(requestPath, bedrockModelsPathPrefix) {
		return "", "", fmt.Errorf("route not found")
	}
	resource := strings.TrimPrefix(
		requestPath,
		bedrockModelsPathPrefix,
	)
	var suffix string
	var operation openAIOperation
	switch {
	case strings.HasSuffix(resource, "/converse-stream"):
		suffix = "/converse-stream"
		operation = bedrockOperationConverseStream
	case strings.HasSuffix(resource, "/converse"):
		suffix = "/converse"
		operation = bedrockOperationConverse
	default:
		return "", "", fmt.Errorf("route not found")
	}
	model := strings.TrimSuffix(resource, suffix)
	if model == "" || len(model) > maxRequestedModelBytes ||
		strings.Contains(model, "/") ||
		strings.TrimSpace(model) != model {
		return "", "", fmt.Errorf("route not found")
	}
	return model, operation, nil
}

func (o openAIOperation) isBedrock() bool {
	return o == bedrockOperationConverse ||
		o == bedrockOperationConverseStream
}

func decodeBedrockRequest(
	raw []byte,
	pathModel string,
	operation openAIOperation,
) (map[string]json.RawMessage, string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "", false, fmt.Errorf("request body is required")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", false, fmt.Errorf(
			"request body must be valid JSON",
		)
	}
	if envelope == nil {
		return nil, "", false, fmt.Errorf(
			"request body must be a JSON object",
		)
	}
	if pathModel == "" || len(pathModel) > maxRequestedModelBytes {
		return nil, "", false, fmt.Errorf(
			"model is required in the request path",
		)
	}
	for _, field := range []string{"model", "modelId", "stream"} {
		if rawNonNull(envelope[field]) {
			return nil, "", false, fmt.Errorf(
				"%s is path-owned and must not be in the request body",
				field,
			)
		}
	}
	if operation != bedrockOperationConverse &&
		operation != bedrockOperationConverseStream {
		return nil, "", false, fmt.Errorf(
			"unsupported Bedrock operation",
		)
	}
	if err := validateBedrockMessages(envelope["messages"]); err != nil {
		return nil, "", false, err
	}
	return envelope, pathModel,
		operation == bedrockOperationConverseStream, nil
}

func validateBedrockMessages(raw json.RawMessage) error {
	if !rawNonNull(raw) {
		return nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil ||
		len(messages) == 0 {
		return fmt.Errorf(
			"messages must be a non-empty array of objects",
		)
	}
	for index, message := range messages {
		if message == nil {
			return fmt.Errorf(
				"messages item %d must be an object",
				index,
			)
		}
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil ||
			(role != "user" && role != "assistant") {
			return fmt.Errorf(
				"messages item %d role must be user or assistant",
				index,
			)
		}
		var content []json.RawMessage
		if err := json.Unmarshal(
			message["content"],
			&content,
		); err != nil || len(content) == 0 {
			return fmt.Errorf(
				"messages item %d content must be a non-empty array",
				index,
			)
		}
	}
	return nil
}

func rewriteBedrockRequest(
	envelope map[string]json.RawMessage,
) ([]byte, error) {
	rewritten := cloneJSONEnvelope(envelope)
	delete(rewritten, "model")
	delete(rewritten, "modelId")
	delete(rewritten, "stream")
	return json.Marshal(rewritten)
}

func bedrockRuntimeBaseURL(region string) string {
	return "https://bedrock-runtime." + region + ".amazonaws.com"
}

func bedrockOperationURL(
	baseURL string,
	upstreamModel string,
	operation openAIOperation,
) (string, error) {
	if err := validateBedrockModelID(upstreamModel); err != nil {
		return "", err
	}
	var action string
	switch operation {
	case bedrockOperationConverse:
		action = "converse"
	case bedrockOperationConverseStream:
		action = "converse-stream"
	default:
		return "", fmt.Errorf("unsupported Bedrock operation")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	basePath := strings.TrimRight(parsed.Path, "/")
	baseRawPath := strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path = basePath +
		"/model/" + upstreamModel + "/" + action
	// Bedrock's generated SDK treats modelId as a non-greedy URI label.
	// Preserve its decoded Path for net/http while forcing the exact encoded
	// wire path, including an encoded slash in prompt and other resource ARNs.
	parsed.RawPath = baseRawPath +
		"/model/" + awsURIEncode(upstreamModel, false) + "/" + action
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validateBedrockModelID(model string) error {
	if model == "" || len(model) > 2048 ||
		model != strings.TrimSpace(model) ||
		strings.HasPrefix(model, "/") ||
		strings.HasSuffix(model, "/") {
		return fmt.Errorf(
			"Bedrock upstream model must be a valid model identifier",
		)
	}
	for _, segment := range strings.Split(model, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf(
				"Bedrock upstream model must be a valid model identifier",
			)
		}
	}
	for _, char := range model {
		if char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == '-' || char == '_' || char == '.' ||
			char == ':' || char == '/' {
			continue
		}
		return fmt.Errorf(
			"Bedrock upstream model must be a valid model identifier",
		)
	}
	return nil
}

func detectBedrockCapabilities(
	envelope map[string]json.RawMessage,
) ([]config.Capability, error) {
	required := make(map[config.Capability]struct{})
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}
	if rawNonNull(envelope["toolConfig"]) {
		if _, err := bedrockObject(
			envelope["toolConfig"],
			"toolConfig",
		); err != nil {
			return nil, err
		}
		add(config.CapabilityTools)
	}
	if rawNonNull(envelope["outputConfig"]) {
		if _, err := bedrockObject(
			envelope["outputConfig"],
			"outputConfig",
		); err != nil {
			return nil, err
		}
		add(config.CapabilityStructuredOutputs)
	}
	if rawNonNull(envelope["guardrailConfig"]) {
		if _, err := bedrockObject(
			envelope["guardrailConfig"],
			"guardrailConfig",
		); err != nil {
			return nil, err
		}
		add(config.CapabilityProviderGuardrails)
	}
	if rawNonNull(envelope["promptVariables"]) {
		if _, err := bedrockObject(
			envelope["promptVariables"],
			"promptVariables",
		); err != nil {
			return nil, err
		}
		add(config.CapabilityProviderPrompts)
	}
	if rawNonNull(envelope["performanceConfig"]) ||
		rawNonNull(envelope["serviceTier"]) {
		add(config.CapabilityServiceTier)
	}
	if rawNonNull(envelope["requestMetadata"]) {
		if err := validateBedrockRequestMetadata(
			envelope["requestMetadata"],
		); err != nil {
			return nil, err
		}
		add(bedrockRequestMetadataCapability)
	}
	if rawNonNull(envelope["additionalModelRequestFields"]) ||
		rawNonNull(envelope["additionalModelResponseFieldPaths"]) {
		add(bedrockAdditionalFieldsCapability)
	}
	if err := inspectBedrockMessages(
		envelope["messages"],
		add,
	); err != nil {
		return nil, err
	}
	if err := inspectBedrockContentArray(
		envelope["system"],
		"system",
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

func validateBedrockRequestMetadata(raw json.RawMessage) error {
	var metadata map[string]string
	if err := json.Unmarshal(raw, &metadata); err != nil ||
		metadata == nil {
		return fmt.Errorf(
			"requestMetadata must be an object of string values",
		)
	}
	if len(metadata) > 16 {
		return fmt.Errorf(
			"requestMetadata must not contain more than 16 entries",
		)
	}
	for key, value := range metadata {
		if key == "" || len(key) > 256 || len(value) > 256 {
			return fmt.Errorf(
				"requestMetadata keys and values exceed Bedrock limits",
			)
		}
	}
	return nil
}

func inspectBedrockMessages(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawNonNull(raw) {
		return nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return fmt.Errorf("messages must be an array of objects")
	}
	for index, message := range messages {
		if err := inspectBedrockContentArray(
			message["content"],
			fmt.Sprintf("messages item %d content", index),
			add,
		); err != nil {
			return err
		}
	}
	return nil
}

func inspectBedrockContentArray(
	raw json.RawMessage,
	name string,
	add func(...config.Capability),
) error {
	if !rawNonNull(raw) {
		return nil
	}
	var content []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &content); err != nil {
		return fmt.Errorf("%s must be an array of objects", name)
	}
	for index, block := range content {
		if block == nil {
			return fmt.Errorf("%s item %d must be an object", name, index)
		}
		if err := inspectBedrockContentBlock(block, add); err != nil {
			return fmt.Errorf("%s item %d: %w", name, index, err)
		}
	}
	return nil
}

func inspectBedrockContentBlock(
	block map[string]json.RawMessage,
	add func(...config.Capability),
) error {
	active := 0
	for field, raw := range block {
		if !rawNonNull(raw) {
			continue
		}
		active++
		switch field {
		case "text", "json":
		case "image", "video":
			add(config.CapabilityVision)
			if err := inspectBedrockMediaSource(raw, field); err != nil {
				return err
			}
		case "audio":
			add(config.CapabilityAudioInput)
			if err := inspectBedrockMediaSource(raw, field); err != nil {
				return err
			}
		case "document":
			add(config.CapabilityFileInput)
			if err := inspectBedrockMediaSource(raw, field); err != nil {
				return err
			}
		case "toolUse":
			add(config.CapabilityTools)
		case "toolResult":
			add(config.CapabilityTools)
			toolResult, err := bedrockObject(raw, "toolResult")
			if err != nil {
				return err
			}
			if err := inspectBedrockContentArray(
				toolResult["content"],
				"toolResult.content",
				add,
			); err != nil {
				return err
			}
		case "cachePoint":
			add(config.CapabilityPromptCaching)
		case "reasoningContent":
			add(config.CapabilityReasoning)
		case "guardContent":
			add(config.CapabilityProviderGuardrails)
			guardContent, err := bedrockObject(raw, "guardContent")
			if err != nil {
				return err
			}
			if rawNonNull(guardContent["image"]) {
				add(config.CapabilityVision)
				if err := inspectBedrockMediaSource(
					guardContent["image"],
					"guardContent.image",
				); err != nil {
					return err
				}
			}
		case "citationsContent":
			add(config.CapabilityCitations)
		case "searchResult":
			add(config.CapabilityProviderTools)
		default:
			return unsupportedFeature(
				"Bedrock content block field %q is not supported",
				field,
			)
		}
	}
	if active != 1 {
		return fmt.Errorf(
			"content block must contain exactly one non-null union member",
		)
	}
	return nil
}

func inspectBedrockMediaSource(
	raw json.RawMessage,
	name string,
) error {
	object, err := bedrockObject(raw, name)
	if err != nil {
		return err
	}
	if !rawNonNull(object["source"]) {
		return nil
	}
	source, err := bedrockObject(
		object["source"],
		name+".source",
	)
	if err != nil {
		return err
	}
	if rawNonNull(source["s3Location"]) {
		return unsupportedFeature(
			"%s S3 resource references are not yet supported",
			name,
		)
	}
	return nil
}

func bedrockObject(
	raw json.RawMessage,
	name string,
) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil ||
		object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	return object, nil
}

func validateBedrockSuccessResponse(body []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return fmt.Errorf("Bedrock response must be a JSON object")
	}
	if _, err := bedrockObject(envelope["output"], "output"); err != nil {
		return err
	}
	if _, found := extractBedrockUsage(body); !found {
		return fmt.Errorf(
			"Bedrock response must contain valid usage",
		)
	}
	return nil
}

type bedrockErrorEnvelope struct {
	Message string `json:"message"`
}

func writeBedrockError(
	w http.ResponseWriter,
	status int,
	code string,
	message string,
) {
	w.Header().Set(
		"X-Amzn-Errortype",
		bedrockErrorType(status, code),
	)
	writeJSON(w, status, bedrockErrorEnvelope{Message: message})
}

func bedrockErrorType(status int, code string) string {
	switch code {
	case "model_not_found", "route_not_found":
		return "ResourceNotFoundException"
	case "request_timeout", "upstream_timeout":
		return "ModelTimeoutException"
	}
	switch status {
	case http.StatusUnauthorized:
		return "UnrecognizedClientException"
	case http.StatusForbidden:
		return "AccessDeniedException"
	case http.StatusNotFound:
		return "ResourceNotFoundException"
	case http.StatusTooManyRequests:
		return "ThrottlingException"
	case http.StatusServiceUnavailable:
		return "ServiceUnavailableException"
	default:
		if status >= 500 {
			return "InternalServerException"
		}
		return "ValidationException"
	}
}

func inspectBedrockStreamMessage(
	message awsEventMessage,
) (bool, ledger.Outcome, string) {
	messageType := message.header(":message-type")
	if messageType == "exception" {
		suffix := bedrockFailureSuffix(
			message.header(":exception-type"),
		)
		failureClass := "bedrock_stream_exception"
		if suffix != "" {
			failureClass += "_" + suffix
		}
		return true, ledger.OutcomeUpstreamError, failureClass
	}
	if messageType != "" && messageType != "event" {
		return true, ledger.OutcomeStreamError,
			"bedrock_stream_invalid_message"
	}
	if !json.Valid(message.payload) {
		return true, ledger.OutcomeStreamError,
			"bedrock_stream_invalid_event"
	}
	if message.header(":event-type") == "messageStop" {
		return true, ledger.OutcomeSuccess, ""
	}
	return false, ledger.OutcomeSuccess, ""
}

func bedrockFailureSuffix(value string) string {
	var result strings.Builder
	for index, char := range value {
		if char >= 'A' && char <= 'Z' {
			if index != 0 {
				result.WriteByte('_')
			}
			result.WriteRune(char - 'A' + 'a')
			continue
		}
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '_' {
			result.WriteRune(char)
			continue
		}
		return ""
	}
	suffix := result.String()
	if !validFailureClassSuffix(suffix) {
		return ""
	}
	return suffix
}
