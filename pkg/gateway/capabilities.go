package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sparksq/sparkroute/pkg/config"
)

func detectChatCapabilities(
	envelope map[string]json.RawMessage,
	streaming bool,
) ([]config.Capability, error) {
	required := make(map[config.Capability]struct{})
	add := func(capabilities ...config.Capability) {
		for _, capability := range capabilities {
			required[capability] = struct{}{}
		}
	}

	if rawActive(envelope["tools"]) || rawActive(envelope["functions"]) {
		add(config.CapabilityTools)
	}
	if raw := envelope["tool_choice"]; rawActive(raw) && !rawJSONStringEquals(raw, "none") {
		add(config.CapabilityTools)
	}
	if raw := envelope["function_call"]; rawActive(raw) && !rawJSONStringEquals(raw, "none") {
		add(config.CapabilityTools)
	}
	if rawJSONBool(envelope["parallel_tool_calls"]) {
		add(config.CapabilityTools, config.CapabilityParallelTools)
	}

	if raw, exists := envelope["messages"]; exists && rawActive(raw) {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, fmt.Errorf("messages must be an array of objects")
		}
		for _, message := range messages {
			var role string
			if rawRole := message["role"]; rawActive(rawRole) {
				if err := json.Unmarshal(rawRole, &role); err != nil {
					return nil, fmt.Errorf("message role must be a string")
				}
			}
			switch role {
			case "developer":
				add(config.CapabilityDeveloperMessages)
			case "tool":
				add(config.CapabilityTools)
			}
			if rawActive(message["tool_calls"]) ||
				rawActive(message["function_call"]) {
				add(config.CapabilityTools)
			}
			if rawActive(message["audio"]) {
				add(config.CapabilityAudioInput)
			}
			if err := inspectMessageContent(message["content"], add); err != nil {
				return nil, err
			}
		}
	}

	if raw, exists := envelope["response_format"]; exists && rawActive(raw) {
		var responseFormat map[string]json.RawMessage
		if err := json.Unmarshal(raw, &responseFormat); err != nil {
			return nil, fmt.Errorf("response_format must be an object")
		}
		var formatType string
		if rawType := responseFormat["type"]; rawActive(rawType) {
			if err := json.Unmarshal(rawType, &formatType); err != nil {
				return nil, fmt.Errorf("response_format.type must be a string")
			}
		}
		switch formatType {
		case "", "text":
		case "json_object":
			add(config.CapabilityJSONMode)
		default:
			add(config.CapabilityStructuredOutputs)
		}
	}
	if rawActive(envelope["reasoning_effort"]) {
		add(config.CapabilityReasoning)
	}
	if rawJSONBool(envelope["logprobs"]) || rawJSONIntGreaterThan(envelope["top_logprobs"], 0) {
		add(config.CapabilityLogprobs)
	}
	if rawActive(envelope["seed"]) {
		add(config.CapabilitySeed)
	}
	if rawJSONIntGreaterThan(envelope["n"], 1) {
		add(config.CapabilityMultipleChoices)
	}
	if rawActive(envelope["prediction"]) {
		add(config.CapabilityPrediction)
	}
	if rawActive(envelope["service_tier"]) {
		add(config.CapabilityServiceTier)
	}
	if rawNonNull(envelope["web_search_options"]) {
		add(config.CapabilityProviderTools)
	}
	if rawJSONBool(envelope["store"]) {
		add(config.CapabilityStoredCompletion)
	}
	if rawActive(envelope["audio"]) {
		add(config.CapabilityAudioOutput)
	}
	if raw, exists := envelope["modalities"]; exists && rawActive(raw) {
		var modalities []string
		if err := json.Unmarshal(raw, &modalities); err != nil {
			return nil, fmt.Errorf("modalities must be an array of strings")
		}
		for _, modality := range modalities {
			if modality == "audio" {
				add(config.CapabilityAudioOutput)
			}
		}
	}
	if streaming && streamIncludesUsage(envelope["stream_options"]) {
		add(config.CapabilityStreamUsage)
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

func inspectMessageContent(
	raw json.RawMessage,
	add func(...config.Capability),
) error {
	if !rawActive(raw) || len(bytes.TrimSpace(raw)) == 0 ||
		bytes.TrimSpace(raw)[0] != '[' {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("message content parts must be objects")
	}
	for _, part := range parts {
		var partType string
		if rawType := part["type"]; rawActive(rawType) {
			if err := json.Unmarshal(rawType, &partType); err != nil {
				return fmt.Errorf("message content part type must be a string")
			}
		}
		switch partType {
		case "image_url", "input_image", "input_video", "video":
			add(config.CapabilityVision)
		case "input_audio", "audio":
			add(config.CapabilityAudioInput)
		case "file", "input_file", "document":
			add(config.CapabilityFileInput)
		}
	}
	return nil
}

func streamIncludesUsage(raw json.RawMessage) bool {
	if !rawActive(raw) {
		return false
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil {
		return false
	}
	return rawJSONBool(options["include_usage"])
}

func rawActive(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null")) &&
		!bytes.Equal(trimmed, []byte("[]")) &&
		!bytes.Equal(trimmed, []byte("{}"))
}

func rawNonNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func rawJSONStringEquals(raw json.RawMessage, expected string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && value == expected
}

func rawJSONBool(raw json.RawMessage) bool {
	var value bool
	return json.Unmarshal(raw, &value) == nil && value
}

func rawJSONIntGreaterThan(raw json.RawMessage, threshold int64) bool {
	var value int64
	return json.Unmarshal(raw, &value) == nil && value > threshold
}

func capabilityStrings(capabilities []config.Capability) []string {
	result := make([]string, len(capabilities))
	for index, capability := range capabilities {
		result[index] = string(capability)
	}
	return result
}
