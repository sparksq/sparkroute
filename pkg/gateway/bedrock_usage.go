// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"strings"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const bedrockConverseUsageVersion = "bedrock-converse/v1"

type bedrockUsage struct {
	InputTokens           *int64                `json:"inputTokens"`
	OutputTokens          *int64                `json:"outputTokens"`
	TotalTokens           *int64                `json:"totalTokens"`
	CacheReadInputTokens  *int64                `json:"cacheReadInputTokens"`
	CacheWriteInputTokens *int64                `json:"cacheWriteInputTokens"`
	CacheDetails          []bedrockCacheDetails `json:"cacheDetails"`
}

type bedrockCacheDetails struct {
	InputTokens *int64 `json:"inputTokens"`
	TTL         string `json:"ttl"`
}

func extractBedrockUsage(
	payload []byte,
) (ledger.TokenUsage, bool) {
	missing := missingUsage()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil ||
		!rawNonNull(envelope["usage"]) {
		return missing, false
	}
	var source bedrockUsage
	if err := json.Unmarshal(envelope["usage"], &source); err != nil {
		return missing, false
	}
	usage := ledger.TokenUsage{
		InputTokens:          nonNegative(source.InputTokens),
		OutputTokens:         nonNegative(source.OutputTokens),
		TotalTokens:          nonNegative(source.TotalTokens),
		CachedInputTokens:    nonNegative(source.CacheReadInputTokens),
		CacheCreationTokens:  nonNegative(source.CacheWriteInputTokens),
		ProviderComponents:   bedrockCacheComponents(source.CacheDetails),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: bedrockConverseUsageVersion,
	}
	if len(envelope["usage"]) <= ledger.MaxRawUsageBytes {
		usage.Raw = append(
			json.RawMessage(nil),
			envelope["usage"]...,
		)
	}
	if usage.InputTokens != nil &&
		usage.OutputTokens != nil &&
		usage.TotalTokens != nil {
		usage.Completeness = ledger.UsageComplete
	}
	return usage, true
}

func bedrockCacheComponents(
	details []bedrockCacheDetails,
) map[string]int64 {
	result := make(map[string]int64)
	for _, detail := range details {
		tokens := nonNegative(detail.InputTokens)
		suffix := normalizeBedrockComponent(detail.TTL)
		if tokens == nil || suffix == "" {
			continue
		}
		result["cache_detail."+suffix] += *tokens
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func normalizeBedrockComponent(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || len(value) > 48 {
		return ""
	}
	var result strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' {
			result.WriteRune(char)
			continue
		}
		if char == '-' || char == '_' || char == '.' {
			result.WriteByte('_')
			continue
		}
		return ""
	}
	return strings.Trim(result.String(), "_")
}
