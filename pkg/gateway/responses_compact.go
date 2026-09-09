// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

func decodeResponsesCompactRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, _, err := decodeChatRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	if !rawNonNull(envelope["input"]) {
		return nil, "", false, fmt.Errorf("input is required")
	}
	if rawNonNull(envelope["instructions"]) {
		var instructions string
		if err := json.Unmarshal(
			envelope["instructions"],
			&instructions,
		); err != nil {
			return nil, "", false, fmt.Errorf(
				"instructions must be a string",
			)
		}
	}
	if rawNonNull(envelope["previous_response_id"]) {
		var responseID string
		if err := json.Unmarshal(
			envelope["previous_response_id"],
			&responseID,
		); err != nil {
			return nil, "", false, fmt.Errorf(
				"previous_response_id must be a string",
			)
		}
		if err := responsesstate.ValidateResponseID(responseID); err != nil {
			return nil, "", false, fmt.Errorf(
				"previous_response_id: %w",
				err,
			)
		}
	}
	for _, field := range []string{
		"background",
		"context_management",
		"conversation",
		"include",
		"max_output_tokens",
		"max_tool_calls",
		"metadata",
		"moderation",
		"multi_agent",
		"parallel_tool_calls",
		"prompt",
		"reasoning",
		"safety_identifier",
		"store",
		"stream",
		"stream_options",
		"temperature",
		"text",
		"tool_choice",
		"tools",
		"top_logprobs",
		"top_p",
		"truncation",
		"user",
	} {
		if rawNonNull(envelope[field]) {
			return nil, "", false, unsupportedFeature(
				"%s is not supported by Responses compact",
				field,
			)
		}
	}
	if err := validateOptionalCompactString(
		envelope,
		"prompt_cache_key",
	); err != nil {
		return nil, "", false, err
	}
	if err := validateOptionalCompactString(
		envelope,
		"prompt_cache_retention",
	); err != nil {
		return nil, "", false, err
	}
	if err := validateOptionalCompactString(
		envelope,
		"service_tier",
	); err != nil {
		return nil, "", false, err
	}
	if rawNonNull(envelope["prompt_cache_options"]) {
		var options map[string]json.RawMessage
		if err := json.Unmarshal(
			envelope["prompt_cache_options"],
			&options,
		); err != nil || options == nil {
			return nil, "", false, fmt.Errorf(
				"prompt_cache_options must be an object",
			)
		}
	}
	return envelope, model, false, nil
}

func validateOptionalCompactString(
	envelope map[string]json.RawMessage,
	field string,
) error {
	if !rawNonNull(envelope[field]) {
		return nil
	}
	var value string
	if err := json.Unmarshal(envelope[field], &value); err != nil {
		return fmt.Errorf("%s must be a string", field)
	}
	return nil
}

func classifyResponsesCompactState(
	envelope map[string]json.RawMessage,
) responsesRequestState {
	state := responsesRequestState{}
	if rawNonNull(envelope["previous_response_id"]) {
		_ = json.Unmarshal(
			envelope["previous_response_id"],
			&state.previousResponseID,
		)
	}
	return state
}

func detectResponsesCompactCapabilities(
	envelope map[string]json.RawMessage,
	_ bool,
) ([]config.Capability, error) {
	required := map[config.Capability]struct{}{
		config.CapabilityResponses:        {},
		config.CapabilityResponsesCompact: {},
	}
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}
	if rawNonNull(envelope["previous_response_id"]) {
		add(config.CapabilityStoredCompletion)
	}
	if err := inspectResponsesInput(envelope["input"], add); err != nil {
		return nil, err
	}
	if err := inspectResponsesInput(
		envelope["instructions"],
		add,
	); err != nil {
		return nil, fmt.Errorf("instructions: %w", err)
	}
	if rawActive(envelope["service_tier"]) {
		add(config.CapabilityServiceTier)
	}
	if responsesPromptCacheActive(envelope) {
		add(config.CapabilityPromptCaching)
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

func responsesPromptCacheActive(
	envelope map[string]json.RawMessage,
) bool {
	for _, field := range []string{
		"prompt_cache_key",
		"prompt_cache_options",
		"prompt_cache_retention",
	} {
		if rawActive(envelope[field]) {
			return true
		}
	}
	return false
}

func responsesCompactURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") +
		"/responses/compact"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func validateResponsesCompactSuccessResponse(body []byte) error {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return fmt.Errorf(
			"the Responses compact response must be a JSON object",
		)
	}
	var object string
	if json.Unmarshal(envelope["object"], &object) != nil ||
		object != "response.compaction" {
		return fmt.Errorf(
			"the Responses compact response object must be response.compaction",
		)
	}
	var responseID string
	if json.Unmarshal(envelope["id"], &responseID) != nil {
		return fmt.Errorf(
			"the Responses compact response id must be a string",
		)
	}
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return fmt.Errorf("the Responses compact response id: %w", err)
	}
	var createdAt int64
	if json.Unmarshal(envelope["created_at"], &createdAt) != nil ||
		createdAt < 0 {
		return fmt.Errorf(
			"the Responses compact response created_at must be a non-negative integer",
		)
	}
	var output []map[string]json.RawMessage
	if json.Unmarshal(envelope["output"], &output) != nil ||
		len(output) == 0 {
		return fmt.Errorf(
			"the Responses compact response output must be a non-empty array of items",
		)
	}
	compactionItems := 0
	for index, item := range output {
		if item == nil {
			return fmt.Errorf(
				"the Responses compact output item %d must be an object",
				index,
			)
		}
		var itemType string
		if json.Unmarshal(item["type"], &itemType) != nil ||
			itemType == "" {
			return fmt.Errorf(
				"the Responses compact output item %d type must be a non-empty string",
				index,
			)
		}
		if itemType != "compaction" {
			continue
		}
		compactionItems++
		var itemID string
		if json.Unmarshal(item["id"], &itemID) != nil {
			return fmt.Errorf(
				"the Responses compact output item %d id must be a string",
				index,
			)
		}
		if err := responsesstate.ValidateResourceID(itemID); err != nil {
			return fmt.Errorf(
				"the Responses compact output item %d id: %w",
				index,
				err,
			)
		}
		var encryptedContent string
		if json.Unmarshal(
			item["encrypted_content"],
			&encryptedContent,
		) != nil || encryptedContent == "" {
			return fmt.Errorf(
				"the Responses compact output item %d encrypted_content must be a non-empty string",
				index,
			)
		}
	}
	if compactionItems != 1 {
		return fmt.Errorf(
			"the Responses compact response must contain exactly one compaction item",
		)
	}
	usage, found := extractOpenAIResponsesUsage(body)
	if !found || usage.Completeness != ledger.UsageComplete {
		return fmt.Errorf(
			"the Responses compact response must contain complete usage",
		)
	}
	return nil
}
