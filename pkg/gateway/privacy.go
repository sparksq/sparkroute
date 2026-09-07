package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/privacy"
)

const (
	maximumPIITextSegments = 65_536
	maximumPIITextBytes    = 64 << 20
)

type piiJSONTransformer struct {
	ctx       context.Context
	transform func(context.Context, string) (string, error)
	segments  int
	bytes     int
}

func (t *piiJSONTransformer) text(value string) (string, error) {
	if err := t.ctx.Err(); err != nil {
		return "", err
	}
	if t.segments >= maximumPIITextSegments || len(value) > maximumPIITextBytes-t.bytes {
		return "", fmt.Errorf("PII text traversal exceeds its segment or byte limit")
	}
	t.segments++
	t.bytes += len(value)
	return t.transform(t.ctx, value)
}

func piiEntities(values []config.PIIEntity) []privacy.Entity {
	result := make([]privacy.Entity, len(values))
	for index, value := range values {
		result[index] = privacy.Entity(value)
	}
	return result
}

func recordPrivacyInspection(provider privacy.Provider) {
	if provider != nil {
		provider.RecordInspection()
	}
}

func recordPrivacyFailure(provider privacy.Provider) {
	if provider != nil {
		provider.RecordFailure()
	}
}

func newPIISession(
	ctx context.Context,
	provider privacy.Provider,
	policy config.EffectivePIIPolicy,
	caller identity.Identity,
) (privacy.Session, error) {
	if provider == nil {
		return nil, fmt.Errorf("PII policy requires a privacy provider")
	}
	scope := privacy.Scope{}
	if policy.Scope == config.PIIScopeConversation {
		var err error
		scope, err = piiConversationScope(caller)
		if err != nil {
			return nil, err
		}
	}
	return provider.NewSession(ctx, piiEntities(policy.Entities), scope)
}

func piiConversationScope(caller identity.Identity) (privacy.Scope, error) {
	principal := strings.TrimSpace(caller.Principal.ID)
	if principal == "" {
		principal = strings.TrimSpace(caller.Principal.Subject)
	}
	if principal == "" {
		return privacy.Scope{}, fmt.Errorf("PII conversation scope requires an authenticated principal")
	}
	tenant := strings.TrimSpace(caller.Principal.Tenant)
	if tenant == "" {
		tenant = trustedIdentityValue(caller, identity.AttributeTenant)
	}
	if tenant == "" {
		tenant = "standalone"
	}
	conversation := trustedIdentityValue(caller, identity.AttributeThreadID)
	if conversation == "" {
		conversation = trustedIdentityValue(caller, identity.AttributeSession)
	}
	if conversation == "" {
		return privacy.Scope{}, fmt.Errorf("PII conversation scope requires trusted thread_id or session attribution")
	}
	return privacy.Scope{
		Tenant: tenant, Principal: principal, Conversation: conversation,
	}, nil
}

func trustedIdentityValue(caller identity.Identity, key string) string {
	if value := strings.TrimSpace(caller.Attribution[key]); value != "" {
		return value
	}
	return strings.TrimSpace(caller.Principal.FixedAttribution[key])
}

func (o openAIOperation) piiMediaInput(
	envelope map[string]json.RawMessage,
	streaming bool,
) (bool, error) {
	capabilities, err := o.detectCapabilities(envelope, streaming)
	if err != nil {
		return false, err
	}
	for _, capability := range capabilities {
		switch capability {
		case config.CapabilityVision, config.CapabilityAudioInput, config.CapabilityFileInput:
			return true, nil
		}
	}
	return false, nil
}

// ValidatePrivacyProvider verifies that every enabled document policy can be
// served by the optional downstream provider. Command/configuration surfaces
// use it before accepting a document; NewDataPlane enforces it again.
func ValidatePrivacyProvider(document config.Document, provider privacy.Provider) error {
	var required []privacy.Entity
	seen := make(map[privacy.Entity]struct{})
	for _, model := range document.VirtualModels {
		if model.Privacy == nil {
			continue
		}
		policy := model.Privacy.PII.Effective()
		if policy.Mode == config.PIIModeDisabled {
			continue
		}
		for _, entity := range piiEntities(policy.Entities) {
			if _, exists := seen[entity]; exists {
				continue
			}
			seen[entity] = struct{}{}
			required = append(required, entity)
		}
	}
	if len(required) == 0 {
		return nil
	}
	if provider == nil {
		return fmt.Errorf("PII configuration requires a privacy provider; the OSS distribution ships only the extension contract")
	}
	for _, entity := range required {
		if !provider.Supports(entity) {
			return fmt.Errorf("PII provider does not support configured entity %q", entity)
		}
	}
	return nil
}

func (o openAIOperation) substitutePIIRequest(
	ctx context.Context,
	raw []byte,
	session privacy.Session,
) ([]byte, error) {
	return o.transformPIIRequest(ctx, raw, session.Substitute)
}

func (o openAIOperation) substituteAndValidatePIIRequest(
	ctx context.Context,
	raw []byte,
	pathModel string,
	streaming bool,
	session privacy.Session,
) ([]byte, map[string]json.RawMessage, error) {
	originalEnvelope, originalModel, originalStreaming, originalErr := o.decodeRequest(raw, pathModel)
	if originalErr != nil {
		return raw, nil, originalErr
	}
	transformed, err := o.substitutePIIRequest(ctx, raw, session)
	if err != nil {
		return raw, originalEnvelope, err
	}
	envelope, transformedModel, transformedStreaming, err := o.decodeRequest(transformed, pathModel)
	if err != nil {
		return raw, originalEnvelope, fmt.Errorf("PII substitution produced an invalid request: %w", err)
	}
	if transformedModel != originalModel || transformedStreaming != originalStreaming ||
		transformedStreaming != streaming {
		return raw, originalEnvelope, fmt.Errorf("PII substitution changed request routing controls")
	}
	return transformed, envelope, nil
}

func (o openAIOperation) transformPIIRequest(
	ctx context.Context,
	raw []byte,
	transform func(context.Context, string) (string, error),
) ([]byte, error) {
	document, err := decodePIIJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := document.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("PII request must be a JSON object")
	}
	walker := &piiJSONTransformer{ctx: ctx, transform: transform}
	switch o {
	case openAIOperationChatCompletions:
		err = transformChatPIIRequest(walker, root)
	case openAIOperationResponses, openAIOperationResponsesCompact:
		err = transformResponsesPIIRequest(walker, root)
	case openAIOperationEmbeddings:
		err = transformObjectFieldAllStrings(walker, root, "input")
	case anthropicOperationMessages, anthropicOperationCountTokens:
		err = transformAnthropicPIIRequest(walker, root)
	case geminiOperationGenerateContent, geminiOperationStreamContent,
		geminiOperationCountTokens, geminiOperationEmbedContent,
		geminiOperationBatchEmbed:
		err = transformGeminiPIIRequest(walker, root, o)
	case bedrockOperationConverse, bedrockOperationConverseStream:
		err = transformBedrockPIIRequest(walker, root)
	default:
		err = fmt.Errorf("PII substitution does not support operation %q", o)
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

func (o openAIOperation) restorePIIResponse(
	ctx context.Context,
	raw []byte,
	session privacy.Session,
) ([]byte, error) {
	return o.transformPIIResponse(ctx, raw, func(_ context.Context, value string) (string, error) {
		return session.Restore(value), nil
	})
}

func (o openAIOperation) redactPIIResponse(
	ctx context.Context,
	raw []byte,
	session privacy.Session,
) ([]byte, error) {
	return o.transformPIIResponse(ctx, raw, session.Redact)
}

func (o openAIOperation) transformPIIResponse(
	ctx context.Context,
	raw []byte,
	transform func(context.Context, string) (string, error),
) ([]byte, error) {
	document, err := decodePIIJSON(raw)
	if err != nil {
		return nil, err
	}
	root, ok := document.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("PII response must be a JSON object")
	}
	field := ""
	switch o {
	case openAIOperationChatCompletions:
		field = "choices"
	case openAIOperationResponses, openAIOperationResponsesCompact:
		field = "output"
	case anthropicOperationMessages:
		field = "content"
	case geminiOperationGenerateContent, geminiOperationStreamContent:
		field = "candidates"
	case bedrockOperationConverse, bedrockOperationConverseStream:
		field = "output"
	case openAIOperationEmbeddings, anthropicOperationCountTokens,
		geminiOperationCountTokens, geminiOperationEmbedContent,
		geminiOperationBatchEmbed:
		return append([]byte(nil), raw...), nil
	default:
		return nil, fmt.Errorf("PII response transformation does not support operation %q", o)
	}
	walker := &piiJSONTransformer{ctx: ctx, transform: transform}
	if err := transformObjectFieldAllStrings(walker, root, field); err != nil {
		return nil, err
	}
	return json.Marshal(root)
}

func redactAllPIIJSONStrings(
	ctx context.Context,
	raw []byte,
	session privacy.Session,
) ([]byte, error) {
	document, err := decodePIIJSON(raw)
	if err != nil {
		return nil, err
	}
	walker := &piiJSONTransformer{ctx: ctx, transform: session.Redact}
	if err := transformAllStrings(walker, &document); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func restoreAllPIIJSONStrings(
	ctx context.Context,
	raw []byte,
	session privacy.Session,
) ([]byte, error) {
	document, err := decodePIIJSON(raw)
	if err != nil {
		return nil, err
	}
	walker := &piiJSONTransformer{
		ctx: ctx,
		transform: func(_ context.Context, value string) (string, error) {
			return session.Restore(value), nil
		},
	}
	if err := transformAllStrings(walker, &document); err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

func decodePIIJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode PII JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("PII JSON has a trailing value")
		}
		return nil, fmt.Errorf("decode PII JSON trailer: %w", err)
	}
	return document, nil
}

func transformChatPIIRequest(w *piiJSONTransformer, root map[string]any) error {
	messages, _ := root["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		if err := transformContentField(w, message, "content"); err != nil {
			return err
		}
		if err := transformFunctionArguments(w, message["function_call"]); err != nil {
			return err
		}
		toolCalls, _ := message["tool_calls"].([]any)
		for _, rawToolCall := range toolCalls {
			toolCall, _ := rawToolCall.(map[string]any)
			if err := transformFunctionArguments(w, toolCall["function"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func transformResponsesPIIRequest(w *piiJSONTransformer, root map[string]any) error {
	if err := transformStringField(w, root, "instructions"); err != nil {
		return err
	}
	if value, exists := root["input"]; exists {
		if err := transformContentValue(w, &value); err != nil {
			return err
		}
		root["input"] = value
	}
	return nil
}

func transformAnthropicPIIRequest(w *piiJSONTransformer, root map[string]any) error {
	if value, exists := root["system"]; exists {
		if err := transformContentValue(w, &value); err != nil {
			return err
		}
		root["system"] = value
	}
	messages, _ := root["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if err := transformContentField(w, message, "content"); err != nil {
			return err
		}
	}
	return nil
}

func transformGeminiPIIRequest(w *piiJSONTransformer, root map[string]any, operation openAIOperation) error {
	if operation == geminiOperationCountTokens {
		for _, field := range []string{"generateContentRequest", "generate_content_request"} {
			if nested, ok := root[field].(map[string]any); ok {
				if err := transformGeminiPIIRequest(w, nested, geminiOperationGenerateContent); err != nil {
					return err
				}
			}
		}
	}
	if operation == geminiOperationBatchEmbed {
		requests, _ := root["requests"].([]any)
		for _, rawRequest := range requests {
			request, _ := rawRequest.(map[string]any)
			if err := transformContentField(w, request, "content"); err != nil {
				return err
			}
		}
		return nil
	}
	for _, field := range []string{"systemInstruction", "system_instruction", "content"} {
		if err := transformContentField(w, root, field); err != nil {
			return err
		}
	}
	contents, _ := root["contents"].([]any)
	for _, rawContent := range contents {
		content, _ := rawContent.(map[string]any)
		if err := transformContentField(w, content, "parts"); err != nil {
			return err
		}
	}
	return nil
}

func transformBedrockPIIRequest(w *piiJSONTransformer, root map[string]any) error {
	if err := transformContentField(w, root, "system"); err != nil {
		return err
	}
	messages, _ := root["messages"].([]any)
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if err := transformContentField(w, message, "content"); err != nil {
			return err
		}
	}
	return nil
}

func transformContentField(w *piiJSONTransformer, object map[string]any, field string) error {
	if object == nil {
		return nil
	}
	value, exists := object[field]
	if !exists {
		return nil
	}
	if err := transformContentValue(w, &value); err != nil {
		return err
	}
	object[field] = value
	return nil
}

func transformContentValue(w *piiJSONTransformer, value *any) error {
	switch typed := (*value).(type) {
	case string:
		transformed, err := w.text(typed)
		if err != nil {
			return err
		}
		*value = transformed
	case []any:
		for index := range typed {
			if err := transformContentValue(w, &typed[index]); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, field := range []string{"text", "input_text", "output_text", "arguments"} {
			if err := transformStringField(w, typed, field); err != nil {
				return err
			}
		}
		for _, field := range []string{"content", "parts"} {
			if err := transformContentField(w, typed, field); err != nil {
				return err
			}
		}
		itemType, _ := typed["type"].(string)
		for _, field := range []string{"input", "args", "response", "json"} {
			if _, exists := typed[field]; !exists {
				continue
			}
			if itemType == "" || itemType == "tool_use" || itemType == "function_call" ||
				field == "args" || field == "response" || field == "json" {
				if err := transformObjectFieldAllStrings(w, typed, field); err != nil {
					return err
				}
			}
		}
		if itemType == "function_call_output" {
			if err := transformObjectFieldAllStrings(w, typed, "output"); err != nil {
				return err
			}
		}
		if functionCall, ok := typed["functionCall"].(map[string]any); ok {
			if err := transformObjectFieldAllStrings(w, functionCall, "args"); err != nil {
				return err
			}
		}
		if functionResponse, ok := typed["functionResponse"].(map[string]any); ok {
			if err := transformObjectFieldAllStrings(w, functionResponse, "response"); err != nil {
				return err
			}
		}
		if toolUse, ok := typed["toolUse"].(map[string]any); ok {
			if err := transformObjectFieldAllStrings(w, toolUse, "input"); err != nil {
				return err
			}
		}
		if toolResult, ok := typed["toolResult"].(map[string]any); ok {
			if err := transformContentField(w, toolResult, "content"); err != nil {
				return err
			}
		}
	}
	return nil
}

func transformFunctionArguments(w *piiJSONTransformer, value any) error {
	function, _ := value.(map[string]any)
	if function == nil {
		return nil
	}
	return transformStringField(w, function, "arguments")
}

func transformStringField(w *piiJSONTransformer, object map[string]any, field string) error {
	value, ok := object[field].(string)
	if !ok {
		return nil
	}
	transformed, err := w.text(value)
	if err != nil {
		return err
	}
	object[field] = transformed
	return nil
}

func transformObjectFieldAllStrings(w *piiJSONTransformer, object map[string]any, field string) error {
	value, exists := object[field]
	if !exists {
		return nil
	}
	if err := transformAllStrings(w, &value); err != nil {
		return err
	}
	object[field] = value
	return nil
}

func transformAllStrings(w *piiJSONTransformer, value *any) error {
	switch typed := (*value).(type) {
	case string:
		transformed, err := w.text(typed)
		if err != nil {
			return err
		}
		*value = transformed
	case []any:
		for index := range typed {
			if err := transformAllStrings(w, &typed[index]); err != nil {
				return err
			}
		}
	case map[string]any:
		for key, nested := range typed {
			if err := transformAllStrings(w, &nested); err != nil {
				return err
			}
			typed[key] = nested
		}
	}
	return nil
}
