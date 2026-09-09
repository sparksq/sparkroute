// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const (
	anthropicMessagesUsageVersion    = "anthropic-messages/v1"
	anthropicCountTokensUsageVersion = "anthropic-count-tokens/v1"
)

type anthropicUsage struct {
	InputTokens              *int64                      `json:"input_tokens"`
	OutputTokens             *int64                      `json:"output_tokens"`
	CacheCreationInputTokens *int64                      `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64                      `json:"cache_read_input_tokens"`
	OutputTokensDetails      anthropicOutputTokenDetails `json:"output_tokens_details"`
	CacheCreation            map[string]json.RawMessage  `json:"cache_creation"`
	ServerToolUse            map[string]json.RawMessage  `json:"server_tool_use"`
}

type anthropicOutputTokenDetails struct {
	ThinkingTokens *int64 `json:"thinking_tokens"`
}

func extractAnthropicMessagesUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	missing := missingUsage()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil {
		return missing, false
	}
	rawUsage := envelope["usage"]
	if !rawNonNull(rawUsage) && rawNonNull(envelope["message"]) {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(envelope["message"], &message); err == nil {
			rawUsage = message["usage"]
		}
	}
	if !rawNonNull(rawUsage) {
		return missing, false
	}
	var source anthropicUsage
	if err := json.Unmarshal(rawUsage, &source); err != nil {
		return missing, false
	}
	usage := ledger.TokenUsage{
		InputTokens:         nonNegative(source.InputTokens),
		OutputTokens:        nonNegative(source.OutputTokens),
		CachedInputTokens:   nonNegative(source.CacheReadInputTokens),
		CacheCreationTokens: nonNegative(source.CacheCreationInputTokens),
		ReasoningTokens: nonNegative(
			source.OutputTokensDetails.ThinkingTokens,
		),
		ProviderComponents:   anthropicProviderComponents(source),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: anthropicMessagesUsageVersion,
	}
	if len(rawUsage) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), rawUsage...)
	}
	completeAnthropicUsage(&usage)
	return usage, true
}

func extractAnthropicCountTokensUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	missing := missingUsage()
	var source struct {
		InputTokens *int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(payload, &source); err != nil {
		return missing, false
	}
	inputTokens := nonNegative(source.InputTokens)
	if inputTokens == nil {
		return missing, false
	}
	zero := int64(0)
	total := *inputTokens
	usage := ledger.TokenUsage{
		InputTokens:          inputTokens,
		OutputTokens:         &zero,
		TotalTokens:          &total,
		Completeness:         ledger.UsageComplete,
		NormalizationVersion: anthropicCountTokensUsageVersion,
	}
	if len(payload) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), payload...)
	}
	return usage, true
}

func anthropicProviderComponents(
	source anthropicUsage,
) map[string]int64 {
	result := make(map[string]int64)
	appendRawNumericComponents(
		result,
		"cache_creation.",
		source.CacheCreation,
	)
	appendRawNumericComponents(
		result,
		"server_tool_use.",
		source.ServerToolUse,
	)
	if len(result) == 0 {
		return nil
	}
	return result
}

func appendRawNumericComponents(
	target map[string]int64,
	prefix string,
	source map[string]json.RawMessage,
) {
	for name, raw := range source {
		var value int64
		if json.Unmarshal(raw, &value) == nil && value >= 0 {
			target[prefix+name] = value
		}
	}
}

func (o openAIOperation) mergeUsage(
	current ledger.TokenUsage,
	next ledger.TokenUsage,
) ledger.TokenUsage {
	if o == geminiOperationGenerateContent ||
		o == geminiOperationStreamContent {
		return mergeGeminiGenerateContentUsage(current, next)
	}
	if o != anthropicOperationMessages {
		return next
	}
	merged := current
	mergeUsageValue := func(target **int64, source *int64) {
		if source != nil {
			value := *source
			*target = &value
		}
	}
	mergeUsageValue(&merged.InputTokens, next.InputTokens)
	mergeUsageValue(&merged.OutputTokens, next.OutputTokens)
	mergeUsageValue(&merged.CachedInputTokens, next.CachedInputTokens)
	mergeUsageValue(&merged.CacheCreationTokens, next.CacheCreationTokens)
	mergeUsageValue(&merged.ReasoningTokens, next.ReasoningTokens)
	if len(next.ProviderComponents) != 0 {
		if merged.ProviderComponents == nil {
			merged.ProviderComponents = make(map[string]int64)
		}
		for name, value := range next.ProviderComponents {
			merged.ProviderComponents[name] = value
		}
	}
	merged.Raw = mergeRawUsageObjects(current.Raw, next.Raw)
	merged.NormalizationVersion = anthropicMessagesUsageVersion
	merged.Completeness = ledger.UsagePartial
	completeAnthropicUsage(&merged)
	return merged
}

func mergeRawUsageObjects(
	current json.RawMessage,
	next json.RawMessage,
) json.RawMessage {
	if len(current) == 0 {
		return append(json.RawMessage(nil), next...)
	}
	if len(next) == 0 {
		return append(json.RawMessage(nil), current...)
	}
	var left, right map[string]json.RawMessage
	if json.Unmarshal(current, &left) != nil ||
		json.Unmarshal(next, &right) != nil {
		return append(json.RawMessage(nil), next...)
	}
	for name, value := range right {
		left[name] = value
	}
	encoded, err := json.Marshal(left)
	if err != nil || len(encoded) > ledger.MaxRawUsageBytes {
		return append(json.RawMessage(nil), next...)
	}
	return encoded
}

func completeAnthropicUsage(usage *ledger.TokenUsage) {
	if usage.InputTokens == nil || usage.OutputTokens == nil {
		return
	}
	total := *usage.InputTokens + *usage.OutputTokens
	if usage.CachedInputTokens != nil {
		total += *usage.CachedInputTokens
	}
	if usage.CacheCreationTokens != nil {
		total += *usage.CacheCreationTokens
	}
	usage.TotalTokens = &total
	usage.Completeness = ledger.UsageComplete
}
