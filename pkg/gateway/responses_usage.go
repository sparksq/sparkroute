package gateway

import (
	"encoding/json"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const openAIResponsesUsageVersion = "openai-responses/v1"

type openAIResponsesUsage struct {
	InputTokens        *int64 `json:"input_tokens"`
	OutputTokens       *int64 `json:"output_tokens"`
	TotalTokens        *int64 `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func extractOpenAIResponsesUsage(payload []byte) (ledger.TokenUsage, bool) {
	missing := ledger.TokenUsage{Completeness: ledger.UsageMissing}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		return missing, false
	}
	if rawResponse, exists := envelope["response"]; exists {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(rawResponse, &response); err != nil || response == nil {
			return missing, false
		}
		envelope = response
	}
	rawUsage := envelope["usage"]
	if len(rawUsage) == 0 || string(rawUsage) == "null" {
		return missing, false
	}
	var source openAIResponsesUsage
	if err := json.Unmarshal(rawUsage, &source); err != nil {
		return missing, false
	}
	usage := ledger.TokenUsage{
		InputTokens:          nonNegative(source.InputTokens),
		OutputTokens:         nonNegative(source.OutputTokens),
		TotalTokens:          nonNegative(source.TotalTokens),
		CachedInputTokens:    nonNegative(source.InputTokensDetails.CachedTokens),
		CacheCreationTokens:  nonNegative(source.InputTokensDetails.CacheWriteTokens),
		ReasoningTokens:      nonNegative(source.OutputTokensDetails.ReasoningTokens),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: openAIResponsesUsageVersion,
	}
	if len(rawUsage) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), rawUsage...)
	}
	if usage.InputTokens != nil && usage.OutputTokens != nil && usage.TotalTokens != nil {
		usage.Completeness = ledger.UsageComplete
	}
	return usage, true
}
