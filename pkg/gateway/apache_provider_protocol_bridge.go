package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	llmprotocol "github.com/scitrera/go-llm/protocol"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

// These wrappers keep SparkRoute's routing, provider authentication, response
// presentation, ledger, and retry ownership while delegating portable wire
// semantics to the standalone Apache-2.0 protocol module.
func translateChatCompletionsRequestToGemini(envelope map[string]json.RawMessage) ([]byte, error) {
	// Retain SparkRoute's deployment-specific eligibility checks while moving the
	// portable encoding itself into the Apache codec.
	legacyBody, err := translateChatCompletionsRequestToGeminiLegacy(envelope)
	if err != nil {
		return nil, err
	}
	for _, raw := range envelope {
		if bytes.Contains(raw, []byte(geminiToolCallIDPrefix)) {
			return legacyBody, nil
		}
	}
	return translateChatRequestWithApacheCodec(envelope, llmprotocol.FormatGemini)
}

func translateChatCompletionsRequestToBedrock(envelope map[string]json.RawMessage) ([]byte, error) {
	if _, err := translateChatCompletionsRequestToBedrockLegacy(envelope); err != nil {
		return nil, err
	}
	return translateChatRequestWithApacheCodec(envelope, llmprotocol.FormatBedrock)
}

func translateChatRequestWithApacheCodec(envelope map[string]json.RawMessage, target llmprotocol.Format) ([]byte, error) {
	body, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	translated, err := openAIProtocolRegistry.TranslateRequest(llmprotocol.FormatOpenAIChat, target, body, llmprotocol.StrictPolicy())
	if err != nil {
		return nil, err
	}
	return translated.Body, nil
}

func translateGeminiResponseToChatCompletions(body []byte, requestID string, model string, created int64) ([]byte, ledger.TokenUsage, error) {
	legacyBody, usage, legacyErr := translateGeminiResponseToChatCompletionsLegacy(body, requestID, model, created)
	if legacyErr != nil {
		return nil, usage, legacyErr
	}
	translated, err := translateProviderResponseWithApacheCodec(body, llmprotocol.FormatGemini, requestID, model, created)
	if err != nil {
		// Gemini thought signatures need SparkRoute's opaque, target-affine carrier;
		// the provider-neutral codec correctly refuses to forge or discard them.
		var translationErr *llmprotocol.TranslationError
		if errors.As(err, &translationErr) && translationErr.Code == "reasoning_signature_not_supported" {
			return legacyBody, usage, nil
		}
		return nil, usage, err
	}
	return translated, usage, nil
}

func translateBedrockResponseToChatCompletions(body []byte, requestID string, model string, created int64) ([]byte, ledger.TokenUsage, error) {
	_, usage, legacyErr := translateBedrockResponseToChatCompletionsLegacy(body, requestID, model, created)
	if legacyErr != nil {
		return nil, usage, legacyErr
	}
	translated, err := translateProviderResponseWithApacheCodec(body, llmprotocol.FormatBedrock, requestID, model, created)
	if err != nil {
		return nil, usage, err
	}
	return translated, usage, nil
}

func translateProviderResponseWithApacheCodec(body []byte, source llmprotocol.Format, requestID string, model string, created int64) ([]byte, error) {
	sourceCodec, err := openAIProtocolRegistry.Codec(source)
	if err != nil {
		return nil, err
	}
	decoded, err := sourceCodec.DecodeResponse(body, llmprotocol.StrictPolicy())
	if err != nil {
		return nil, err
	}
	if len(decoded.Response.Outputs) != 1 || decoded.Response.Outputs[0].StopReason == "" || decoded.Response.Outputs[0].StopReason == llmprotocol.StopUnknown {
		return nil, fmt.Errorf("%s response must contain one finished output", source)
	}
	translated, err := openAIProtocolRegistry.TranslateResponse(source, llmprotocol.FormatOpenAIChat, body, llmprotocol.StrictPolicy())
	if err != nil {
		return nil, err
	}
	var object map[string]any
	if json.Unmarshal(translated.Body, &object) != nil || object == nil {
		return nil, fmt.Errorf("translated Chat Completions response is invalid")
	}
	object["id"] = "chatcmpl-" + requestID
	object["object"] = "chat.completion"
	object["created"] = created
	object["model"] = model
	if choices, ok := object["choices"].([]any); ok {
		for _, rawChoice := range choices {
			if choice, ok := rawChoice.(map[string]any); ok {
				choice["logprobs"] = nil
			}
		}
	}
	return json.Marshal(object)
}
