// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const openAIChatUsageVersion = "openai-chat-completions/v1"

type openAIUsageEnvelope struct {
	Usage json.RawMessage `json:"usage"`
}

type openAIUsage struct {
	PromptTokens        *int64 `json:"prompt_tokens"`
	CompletionTokens    *int64 `json:"completion_tokens"`
	TotalTokens         *int64 `json:"total_tokens"`
	PromptTokensDetails struct {
		CachedTokens *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens          *int64 `json:"reasoning_tokens"`
		AcceptedPredictionTokens *int64 `json:"accepted_prediction_tokens"`
		RejectedPredictionTokens *int64 `json:"rejected_prediction_tokens"`
	} `json:"completion_tokens_details"`
}

func extractOpenAIChatUsage(payload []byte) (ledger.TokenUsage, bool) {
	missing := ledger.TokenUsage{Completeness: ledger.UsageMissing}
	var envelope openAIUsageEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil || len(envelope.Usage) == 0 ||
		string(envelope.Usage) == "null" {
		return missing, false
	}
	var source openAIUsage
	if err := json.Unmarshal(envelope.Usage, &source); err != nil {
		return missing, false
	}
	usage := ledger.TokenUsage{
		InputTokens:              nonNegative(source.PromptTokens),
		OutputTokens:             nonNegative(source.CompletionTokens),
		TotalTokens:              nonNegative(source.TotalTokens),
		CachedInputTokens:        nonNegative(source.PromptTokensDetails.CachedTokens),
		ReasoningTokens:          nonNegative(source.CompletionTokensDetails.ReasoningTokens),
		AcceptedPredictionTokens: nonNegative(source.CompletionTokensDetails.AcceptedPredictionTokens),
		RejectedPredictionTokens: nonNegative(source.CompletionTokensDetails.RejectedPredictionTokens),
		Completeness:             ledger.UsagePartial,
		NormalizationVersion:     openAIChatUsageVersion,
	}
	if len(envelope.Usage) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), envelope.Usage...)
	}
	if usage.InputTokens != nil && usage.OutputTokens != nil && usage.TotalTokens != nil {
		usage.Completeness = ledger.UsageComplete
	}
	return usage, true
}

func nonNegative(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	copied := *value
	return &copied
}
