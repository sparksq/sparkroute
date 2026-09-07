package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const (
	geminiEmbedContentUsageVersion       = "gemini-embed-content/v1"
	geminiBatchEmbedContentsUsageVersion = "gemini-batch-embed-contents/v1"
	maxGeminiEmbeddingDimensions         = int64(1<<31 - 1)
	maxOpenAIEmbeddingInputs             = 2048
)

func translateOpenAIEmbeddingsRequestToGemini(
	envelope map[string]json.RawMessage,
) ([]byte, int64, int, error) {
	if err := rejectJSONFieldsFor(
		envelope,
		"Embeddings request",
		"Gemini EmbedContent",
		"model", "input", "encoding_format", "dimensions", "user",
		"stream",
	); err != nil {
		return nil, 0, 0, err
	}
	inputs, err := geminiEmbeddingInputs(envelope["input"])
	if err != nil {
		return nil, 0, 0, err
	}
	if rawNonNull(envelope["encoding_format"]) {
		var encoding string
		if err := json.Unmarshal(
			envelope["encoding_format"],
			&encoding,
		); err != nil || encoding == "" {
			return nil, 0, 0, fmt.Errorf(
				"encoding_format must be a non-empty string",
			)
		}
		if encoding != "float" {
			return nil, 0, 0, unsupportedFeature(
				"encoding_format %q cannot be represented by Gemini EmbedContent",
				encoding,
			)
		}
	}
	var dimensions int64
	if rawNonNull(envelope["dimensions"]) {
		if err := json.Unmarshal(
			envelope["dimensions"],
			&dimensions,
		); err != nil || dimensions <= 0 {
			return nil, 0, 0, fmt.Errorf(
				"dimensions must be a positive integer",
			)
		}
		if dimensions > maxGeminiEmbeddingDimensions {
			return nil, 0, 0, unsupportedFeature(
				"dimensions exceeds the Gemini EmbedContent integer range",
			)
		}
	}
	if rawNonNull(envelope["user"]) {
		return nil, 0, 0, unsupportedFeature(
			"user attribution cannot be represented by Gemini EmbedContent",
		)
	}
	embeddingConfig := map[string]any{"autoTruncate": false}
	if dimensions != 0 {
		embeddingConfig["outputDimensionality"] = dimensions
	}
	var requestBody map[string]any
	if len(inputs) == 1 {
		requestBody = map[string]any{
			"content":            geminiEmbeddingContent(inputs[0]),
			"embedContentConfig": embeddingConfig,
		}
	} else {
		requests := make([]any, 0, len(inputs))
		for _, text := range inputs {
			requests = append(requests, map[string]any{
				"content":            geminiEmbeddingContent(text),
				"embedContentConfig": embeddingConfig,
			})
		}
		requestBody = map[string]any{"requests": requests}
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return nil, 0, 0, err
	}
	return body, dimensions, len(inputs), nil
}

func geminiEmbeddingContent(text string) map[string]any {
	return map[string]any{
		"parts": []any{map[string]any{"text": text}},
	}
}

func geminiEmbeddingInputs(
	raw json.RawMessage,
) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, fmt.Errorf("input is required")
	}
	if trimmed[0] == '"' {
		var text string
		if err := json.Unmarshal(trimmed, &text); err != nil {
			return nil, fmt.Errorf("input must be valid text")
		}
		if text == "" {
			return nil, fmt.Errorf("input must not be empty")
		}
		return []string{text}, nil
	}
	if trimmed[0] != '[' {
		return nil, unsupportedFeature(
			"input must be text strings for Gemini embedding translation",
		)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("input must be a valid array")
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("input must not be empty")
	}
	if len(items) > maxOpenAIEmbeddingInputs {
		return nil, fmt.Errorf(
			"input must not contain more than %d items",
			maxOpenAIEmbeddingInputs,
		)
	}
	inputs := make([]string, 0, len(items))
	for index, item := range items {
		var text string
		if err := json.Unmarshal(item, &text); err != nil {
			return nil, unsupportedFeature(
				"token-array embedding input cannot be represented by Gemini embedding translation",
			)
		}
		if text == "" {
			return nil, fmt.Errorf(
				"input item %d must not be an empty string",
				index,
			)
		}
		inputs = append(inputs, text)
	}
	return inputs, nil
}

func rewriteGeminiBatchEmbeddingModel(
	body []byte,
	upstreamModel string,
) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil ||
		envelope == nil {
		return nil, fmt.Errorf(
			"translated Gemini batch embedding request is invalid",
		)
	}
	var requests []map[string]json.RawMessage
	if json.Unmarshal(envelope["requests"], &requests) != nil ||
		len(requests) < 2 {
		return nil, fmt.Errorf(
			"translated Gemini batch embedding request must contain at least two requests",
		)
	}
	model := "models/" + geminiModelID(upstreamModel)
	rawModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	for _, request := range requests {
		if request == nil {
			return nil, fmt.Errorf(
				"translated Gemini batch embedding request contains an invalid item",
			)
		}
		request["model"] = rawModel
	}
	envelope["requests"], err = json.Marshal(requests)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope)
}

func isGeminiBatchEmbeddingRequest(body []byte) bool {
	var envelope map[string]json.RawMessage
	return json.Unmarshal(body, &envelope) == nil &&
		envelope != nil &&
		rawNonNull(envelope["requests"])
}

func translateGeminiEmbeddingResponseToOpenAI(
	body []byte,
	model string,
	expectedDimensions int64,
	expectedInputs int,
) ([]byte, ledger.TokenUsage, error) {
	if expectedInputs <= 0 {
		return nil, missingUsage(), fmt.Errorf(
			"translated Gemini embedding input count is invalid",
		)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return nil, missingUsage(), fmt.Errorf(
			"the Gemini embedding response must be a JSON object",
		)
	}
	responseName := "Gemini EmbedContent response"
	embeddingField := "embedding"
	usageVersion := geminiEmbedContentUsageVersion
	if expectedInputs > 1 {
		responseName = "Gemini BatchEmbedContents response"
		embeddingField = "embeddings"
		usageVersion = geminiBatchEmbedContentsUsageVersion
	}
	if err := rejectGeminiEmbeddingResponseFields(
		envelope,
		responseName,
		embeddingField, "usageMetadata",
	); err != nil {
		return nil, missingUsage(), err
	}
	rawEmbeddings := make([]json.RawMessage, 0, expectedInputs)
	if expectedInputs == 1 {
		if !rawNonNull(envelope["embedding"]) {
			return nil, missingUsage(), fmt.Errorf(
				"the Gemini EmbedContent response must contain embedding",
			)
		}
		rawEmbeddings = append(
			rawEmbeddings,
			envelope["embedding"],
		)
	} else if json.Unmarshal(
		envelope["embeddings"],
		&rawEmbeddings,
	) != nil || len(rawEmbeddings) != expectedInputs {
		return nil, missingUsage(), fmt.Errorf(
			"the Gemini BatchEmbedContents response must contain %d ordered embeddings",
			expectedInputs,
		)
	}
	data := make([]any, 0, len(rawEmbeddings))
	for index, rawEmbedding := range rawEmbeddings {
		values, err := parseGeminiContentEmbedding(
			rawEmbedding,
			expectedDimensions,
			index,
		)
		if err != nil {
			return nil, missingUsage(), err
		}
		data = append(data, map[string]any{
			"object":    "embedding",
			"embedding": values,
			"index":     index,
		})
	}
	usage, err := parseGeminiEmbeddingUsage(
		envelope["usageMetadata"],
		responseName,
		usageVersion,
	)
	if err != nil {
		return nil, missingUsage(), err
	}
	result := map[string]any{
		"object": "list",
		"data":   data,
		"model":  model,
		"usage": map[string]any{
			"prompt_tokens": *usage.InputTokens,
			"total_tokens":  *usage.TotalTokens,
		},
	}
	translated, err := json.Marshal(result)
	if err != nil {
		return nil, missingUsage(), err
	}
	return translated, usage, nil
}

func parseGeminiContentEmbedding(
	raw json.RawMessage,
	expectedDimensions int64,
	index int,
) ([]float64, error) {
	var embedding map[string]json.RawMessage
	if !rawNonNull(raw) ||
		json.Unmarshal(raw, &embedding) != nil ||
		embedding == nil {
		return nil, fmt.Errorf(
			"the Gemini embedding item %d must be an object",
			index,
		)
	}
	if err := rejectGeminiEmbeddingResponseFields(
		embedding,
		fmt.Sprintf("Gemini embedding item %d", index),
		"values", "shape",
	); err != nil {
		return nil, err
	}
	if rawNonNull(embedding["shape"]) {
		return nil, fmt.Errorf(
			"the Gemini embedding item %d shape cannot be represented by the OpenAI Embeddings API",
			index,
		)
	}
	var values []float64
	if !rawNonNull(embedding["values"]) ||
		json.Unmarshal(embedding["values"], &values) != nil ||
		len(values) == 0 {
		return nil, fmt.Errorf(
			"the Gemini embedding item %d values must be a non-empty number array",
			index,
		)
	}
	if expectedDimensions != 0 &&
		int64(len(values)) != expectedDimensions {
		return nil, fmt.Errorf(
			"the Gemini embedding item %d dimension is %d, expected %d",
			index,
			len(values),
			expectedDimensions,
		)
	}
	return values, nil
}

func parseGeminiEmbeddingUsage(
	raw json.RawMessage,
	responseName string,
	normalizationVersion string,
) (ledger.TokenUsage, error) {
	if !rawNonNull(raw) {
		return missingUsage(), fmt.Errorf(
			"%s must contain usageMetadata",
			responseName,
		)
	}
	var usageFields map[string]json.RawMessage
	if json.Unmarshal(raw, &usageFields) != nil ||
		usageFields == nil {
		return missingUsage(), fmt.Errorf(
			"the Gemini usageMetadata must be an object",
		)
	}
	if err := rejectGeminiEmbeddingResponseFields(
		usageFields,
		responseName+" usageMetadata",
		"promptTokenCount", "promptTokenDetails",
	); err != nil {
		return missingUsage(), err
	}
	var promptTokens int64
	if json.Unmarshal(
		usageFields["promptTokenCount"],
		&promptTokens,
	) != nil || promptTokens < 0 {
		return missingUsage(), fmt.Errorf(
			"the Gemini usageMetadata.promptTokenCount must be a non-negative integer",
		)
	}
	var details []geminiModalityUsage
	if rawNonNull(usageFields["promptTokenDetails"]) {
		var rawDetails []map[string]json.RawMessage
		if json.Unmarshal(
			usageFields["promptTokenDetails"],
			&rawDetails,
		) != nil {
			return missingUsage(), fmt.Errorf(
				"the Gemini usageMetadata.promptTokenDetails must be an array of objects",
			)
		}
		details = make([]geminiModalityUsage, 0, len(rawDetails))
		for index, detail := range rawDetails {
			if detail == nil {
				return missingUsage(), fmt.Errorf(
					"the Gemini promptTokenDetails item %d must be an object",
					index,
				)
			}
			if err := rejectGeminiEmbeddingResponseFields(
				detail,
				fmt.Sprintf(
					"Gemini promptTokenDetails item %d",
					index,
				),
				"modality", "tokenCount",
			); err != nil {
				return missingUsage(), err
			}
			var parsed geminiModalityUsage
			if json.Unmarshal(
				detail["modality"],
				&parsed.Modality,
			) != nil || parsed.Modality == "" {
				return missingUsage(), fmt.Errorf(
					"the Gemini promptTokenDetails item %d modality must be a non-empty string",
					index,
				)
			}
			var count int64
			if json.Unmarshal(
				detail["tokenCount"],
				&count,
			) != nil || count < 0 {
				return missingUsage(), fmt.Errorf(
					"the Gemini promptTokenDetails item %d tokenCount must be a non-negative integer",
					index,
				)
			}
			parsed.TokenCount = &count
			details = append(details, parsed)
		}
	}
	zero := int64(0)
	total := promptTokens
	usage := ledger.TokenUsage{
		InputTokens:  &promptTokens,
		OutputTokens: &zero,
		TotalTokens:  &total,
		ProviderComponents: geminiModalityComponents(
			details,
			nil,
			nil,
			nil,
		),
		Completeness:         ledger.UsageComplete,
		NormalizationVersion: normalizationVersion,
	}
	if len(raw) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), raw...)
	}
	return usage, nil
}

func extractGeminiEmbedContentUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(payload, &envelope) != nil ||
		envelope == nil {
		return missingUsage(), false
	}
	usage, err := parseGeminiEmbeddingUsage(
		envelope["usageMetadata"],
		"Gemini EmbedContent response",
		geminiEmbedContentUsageVersion,
	)
	if err != nil {
		return missingUsage(), false
	}
	return usage, true
}

func extractGeminiBatchEmbedContentsUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(payload, &envelope) != nil ||
		envelope == nil {
		return missingUsage(), false
	}
	usage, err := parseGeminiEmbeddingUsage(
		envelope["usageMetadata"],
		"Gemini BatchEmbedContents response",
		geminiBatchEmbedContentsUsageVersion,
	)
	if err != nil {
		return missingUsage(), false
	}
	return usage, true
}

func rejectGeminiEmbeddingResponseFields(
	object map[string]json.RawMessage,
	name string,
	allowed ...string,
) error {
	set := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		set[field] = struct{}{}
	}
	for field, raw := range object {
		if _, exists := set[field]; !exists && rawNonNull(raw) {
			return fmt.Errorf(
				"%s field %q cannot be represented by the OpenAI Embeddings API",
				name,
				field,
			)
		}
	}
	return nil
}
