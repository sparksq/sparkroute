package gateway

import (
	"encoding/json"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const openAIEmbeddingsUsageVersion = "openai-embeddings/v1"

type openAIEmbeddingsUsage struct {
	PromptTokens *int64 `json:"prompt_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
}

func extractOpenAIEmbeddingsUsage(payload []byte) (ledger.TokenUsage, bool) {
	missing := ledger.TokenUsage{Completeness: ledger.UsageMissing}
	var envelope openAIUsageEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Usage) == 0 ||
		string(envelope.Usage) == "null" {
		return missing, false
	}
	var source openAIEmbeddingsUsage
	if err := json.Unmarshal(envelope.Usage, &source); err != nil {
		return missing, false
	}
	zero := int64(0)
	usage := ledger.TokenUsage{
		InputTokens:          nonNegative(source.PromptTokens),
		OutputTokens:         &zero,
		TotalTokens:          nonNegative(source.TotalTokens),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: openAIEmbeddingsUsageVersion,
	}
	if len(envelope.Usage) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), envelope.Usage...)
	}
	if usage.InputTokens != nil && usage.TotalTokens != nil {
		usage.Completeness = ledger.UsageComplete
	}
	return usage, true
}
