// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"strings"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const (
	geminiGenerateContentUsageVersion = "gemini-generate-content/v1"
	geminiCountTokensUsageVersion     = "gemini-count-tokens/v1"
)

type geminiUsageMetadata struct {
	PromptTokenCount           *int64                `json:"promptTokenCount"`
	CachedContentTokenCount    *int64                `json:"cachedContentTokenCount"`
	CandidatesTokenCount       *int64                `json:"candidatesTokenCount"`
	ToolUsePromptTokenCount    *int64                `json:"toolUsePromptTokenCount"`
	ThoughtsTokenCount         *int64                `json:"thoughtsTokenCount"`
	TotalTokenCount            *int64                `json:"totalTokenCount"`
	PromptTokensDetails        []geminiModalityUsage `json:"promptTokensDetails"`
	CacheTokensDetails         []geminiModalityUsage `json:"cacheTokensDetails"`
	CandidatesTokensDetails    []geminiModalityUsage `json:"candidatesTokensDetails"`
	ToolUsePromptTokensDetails []geminiModalityUsage `json:"toolUsePromptTokensDetails"`
}

type geminiModalityUsage struct {
	Modality   string `json:"modality"`
	TokenCount *int64 `json:"tokenCount"`
}

func extractGeminiGenerateContentUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	missing := missingUsage()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil ||
		!rawNonNull(envelope["usageMetadata"]) {
		return missing, false
	}
	var source geminiUsageMetadata
	if err := json.Unmarshal(
		envelope["usageMetadata"],
		&source,
	); err != nil {
		return missing, false
	}
	usage := ledger.TokenUsage{
		InputTokens:          nonNegative(source.PromptTokenCount),
		OutputTokens:         nonNegative(source.CandidatesTokenCount),
		TotalTokens:          nonNegative(source.TotalTokenCount),
		CachedInputTokens:    nonNegative(source.CachedContentTokenCount),
		ReasoningTokens:      nonNegative(source.ThoughtsTokenCount),
		ToolUsePromptTokens:  nonNegative(source.ToolUsePromptTokenCount),
		ProviderComponents:   geminiProviderComponents(source),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: geminiGenerateContentUsageVersion,
	}
	rawUsage := envelope["usageMetadata"]
	if len(rawUsage) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), rawUsage...)
	}
	completeGeminiGenerateContentUsage(&usage)
	return usage, true
}

func extractGeminiCountTokensUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	missing := missingUsage()
	var source struct {
		TotalTokens             *int64                `json:"totalTokens"`
		CachedContentTokenCount *int64                `json:"cachedContentTokenCount"`
		PromptTokensDetails     []geminiModalityUsage `json:"promptTokensDetails"`
		CacheTokensDetails      []geminiModalityUsage `json:"cacheTokensDetails"`
	}
	if err := json.Unmarshal(payload, &source); err != nil {
		return missing, false
	}
	total := nonNegative(source.TotalTokens)
	if total == nil {
		return missing, false
	}
	zero := int64(0)
	usage := ledger.TokenUsage{
		InputTokens:       total,
		OutputTokens:      &zero,
		TotalTokens:       total,
		CachedInputTokens: nonNegative(source.CachedContentTokenCount),
		ProviderComponents: geminiModalityComponents(
			source.PromptTokensDetails,
			source.CacheTokensDetails,
			nil,
			nil,
		),
		Completeness:         ledger.UsageComplete,
		NormalizationVersion: geminiCountTokensUsageVersion,
	}
	if len(payload) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(json.RawMessage(nil), payload...)
	}
	return usage, true
}

func geminiProviderComponents(
	source geminiUsageMetadata,
) map[string]int64 {
	return geminiModalityComponents(
		source.PromptTokensDetails,
		source.CacheTokensDetails,
		source.CandidatesTokensDetails,
		source.ToolUsePromptTokensDetails,
	)
}

func geminiModalityComponents(
	prompt []geminiModalityUsage,
	cache []geminiModalityUsage,
	candidates []geminiModalityUsage,
	toolUse []geminiModalityUsage,
) map[string]int64 {
	result := make(map[string]int64)
	appendDetails := func(
		prefix string,
		details []geminiModalityUsage,
	) {
		for _, detail := range details {
			count := nonNegative(detail.TokenCount)
			modality := strings.ToLower(strings.TrimSpace(detail.Modality))
			if count == nil || !validFailureClassSuffix(modality) {
				continue
			}
			result[prefix+modality] = *count
		}
	}
	appendDetails("prompt.", prompt)
	appendDetails("cache.", cache)
	appendDetails("candidates.", candidates)
	appendDetails("tool_use_prompt.", toolUse)
	if len(result) == 0 {
		return nil
	}
	return result
}

func mergeGeminiGenerateContentUsage(
	current ledger.TokenUsage,
	next ledger.TokenUsage,
) ledger.TokenUsage {
	merged := current
	mergeUsageValue := func(target **int64, source *int64) {
		if source != nil {
			value := *source
			*target = &value
		}
	}
	mergeUsageValue(&merged.InputTokens, next.InputTokens)
	mergeUsageValue(&merged.OutputTokens, next.OutputTokens)
	mergeUsageValue(&merged.TotalTokens, next.TotalTokens)
	mergeUsageValue(&merged.CachedInputTokens, next.CachedInputTokens)
	mergeUsageValue(&merged.ReasoningTokens, next.ReasoningTokens)
	mergeUsageValue(
		&merged.ToolUsePromptTokens,
		next.ToolUsePromptTokens,
	)
	if len(next.ProviderComponents) != 0 {
		if merged.ProviderComponents == nil {
			merged.ProviderComponents = make(map[string]int64)
		}
		for name, value := range next.ProviderComponents {
			merged.ProviderComponents[name] = value
		}
	}
	merged.Raw = mergeRawUsageObjects(current.Raw, next.Raw)
	merged.NormalizationVersion = geminiGenerateContentUsageVersion
	merged.Completeness = ledger.UsagePartial
	completeGeminiGenerateContentUsage(&merged)
	return merged
}

func completeGeminiGenerateContentUsage(usage *ledger.TokenUsage) {
	if usage.InputTokens != nil &&
		usage.OutputTokens != nil &&
		usage.TotalTokens != nil {
		usage.Completeness = ledger.UsageComplete
	}
}
