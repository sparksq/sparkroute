// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const (
	chatGeminiStreamFailureClass = "gemini_chat_translation_stream"
	geminiToolCallIDPrefix       = "call_gemini_sig_"
)

type translatedGeminiContent struct {
	Role  string           `json:"role"`
	Parts []map[string]any `json:"parts"`
}

type geminiToolIdentity struct {
	ID   string
	Name string
}

func translateChatCompletionsRequestToGeminiLegacy(
	envelope map[string]json.RawMessage,
) ([]byte, error) {
	allowed := map[string]struct{}{
		"model": {}, "messages": {}, "stream": {}, "stream_options": {},
		"max_tokens": {}, "max_completion_tokens": {},
		"temperature": {}, "top_p": {}, "stop": {},
		"tools": {}, "tool_choice": {}, "parallel_tool_calls": {},
		"n": {}, "response_format": {}, "modalities": {},
		"frequency_penalty": {}, "presence_penalty": {},
		"logprobs": {}, "store": {},
	}
	for field, raw := range envelope {
		if _, exists := allowed[field]; !exists && rawNonNull(raw) {
			return nil, unsupportedFeature(
				"Chat Completions field %q cannot be represented by Gemini GenerateContent",
				field,
			)
		}
	}
	if err := validateChatGeminiNeutralFields(envelope); err != nil {
		return nil, err
	}
	system, contents, err := translateChatGeminiMessages(
		envelope["messages"],
	)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"contents": contents}
	if len(system) != 0 {
		result["systemInstruction"] = translatedGeminiContent{
			Role: "system", Parts: system,
		}
	}
	generation, err := translateChatGeminiGenerationConfig(envelope)
	if err != nil {
		return nil, err
	}
	if len(generation) != 0 {
		result["generationConfig"] = generation
	}
	tools, toolConfig, err := translateChatGeminiTools(envelope)
	if err != nil {
		return nil, err
	}
	if len(tools) != 0 {
		result["tools"] = []any{map[string]any{
			"functionDeclarations": tools,
		}}
	}
	if toolConfig != nil {
		result["toolConfig"] = toolConfig
	}
	return json.Marshal(result)
}

func validateChatGeminiNeutralFields(
	envelope map[string]json.RawMessage,
) error {
	for _, field := range []string{
		"frequency_penalty",
		"presence_penalty",
	} {
		if !rawNonNull(envelope[field]) {
			continue
		}
		var value float64
		if err := json.Unmarshal(envelope[field], &value); err != nil {
			return fmt.Errorf("%s must be a number", field)
		}
		if value != 0 {
			return unsupportedFeature(
				"Chat Completions %s cannot be represented by Gemini GenerateContent unless it is zero",
				field,
			)
		}
	}
	for _, field := range []string{"logprobs", "store"} {
		if !rawNonNull(envelope[field]) {
			continue
		}
		var value bool
		if err := json.Unmarshal(envelope[field], &value); err != nil {
			return fmt.Errorf("%s must be a boolean", field)
		}
		if value {
			return unsupportedFeature(
				"Chat Completions %s=true cannot be represented by Gemini GenerateContent",
				field,
			)
		}
	}
	if rawNonNull(envelope["n"]) {
		var choices int
		if err := json.Unmarshal(envelope["n"], &choices); err != nil ||
			choices < 1 {
			return fmt.Errorf("n must be a positive integer")
		}
		if choices != 1 {
			return unsupportedFeature(
				"Chat Completions n values other than 1 cannot be represented by Gemini GenerateContent",
			)
		}
	}
	if rawNonNull(envelope["response_format"]) {
		format, err := strictJSONObjectFor(
			envelope["response_format"],
			"response_format",
			"Gemini GenerateContent",
			"type",
		)
		if err != nil {
			return err
		}
		var formatType string
		if err := json.Unmarshal(format["type"], &formatType); err != nil ||
			formatType != "text" {
			return unsupportedFeature(
				"Chat Completions response_format cannot be represented by Gemini GenerateContent unless its type is text",
			)
		}
	}
	if rawNonNull(envelope["modalities"]) {
		var modalities []string
		if err := json.Unmarshal(envelope["modalities"], &modalities); err != nil ||
			len(modalities) != 1 || modalities[0] != "text" {
			return unsupportedFeature(
				"Chat Completions modalities cannot be represented by Gemini GenerateContent unless they contain only text",
			)
		}
	}
	var streaming bool
	if rawNonNull(envelope["stream"]) &&
		json.Unmarshal(envelope["stream"], &streaming) != nil {
		return fmt.Errorf("stream must be a boolean")
	}
	if rawNonNull(envelope["stream_options"]) {
		if !streaming {
			return fmt.Errorf(
				"stream_options may only be used when stream is true",
			)
		}
		options, err := strictJSONObjectFor(
			envelope["stream_options"],
			"stream_options",
			"Gemini GenerateContent",
			"include_usage",
		)
		if err != nil {
			return err
		}
		if rawNonNull(options["include_usage"]) {
			var include bool
			if err := json.Unmarshal(
				options["include_usage"],
				&include,
			); err != nil {
				return fmt.Errorf(
					"stream_options.include_usage must be a boolean",
				)
			}
		}
	}
	return nil
}

func translateChatGeminiGenerationConfig(
	envelope map[string]json.RawMessage,
) (map[string]any, error) {
	result := make(map[string]any)
	rawMax := envelope["max_completion_tokens"]
	if rawNonNull(rawMax) && rawNonNull(envelope["max_tokens"]) {
		return nil, fmt.Errorf(
			"max_tokens and max_completion_tokens cannot both be set",
		)
	}
	if !rawNonNull(rawMax) {
		rawMax = envelope["max_tokens"]
	}
	if rawNonNull(rawMax) {
		var maximum int
		if err := json.Unmarshal(rawMax, &maximum); err != nil ||
			maximum < 1 {
			return nil, fmt.Errorf(
				"maximum completion tokens must be a positive integer",
			)
		}
		result["maxOutputTokens"] = maximum
	}
	if rawNonNull(envelope["temperature"]) {
		var value float64
		if err := json.Unmarshal(
			envelope["temperature"],
			&value,
		); err != nil {
			return nil, fmt.Errorf("temperature must be a number")
		}
		if value < 0 || value > 2 {
			return nil, unsupportedFeature(
				"Chat Completions temperature=%v is outside the Gemini GenerateContent range [0,2]",
				value,
			)
		}
		result["temperature"] = value
	}
	if rawNonNull(envelope["top_p"]) {
		var value float64
		if err := json.Unmarshal(envelope["top_p"], &value); err != nil {
			return nil, fmt.Errorf("top_p must be a number")
		}
		if value < 0 || value > 1 {
			return nil, unsupportedFeature(
				"Chat Completions top_p=%v is outside the Gemini GenerateContent range [0,1]",
				value,
			)
		}
		result["topP"] = value
	}
	if rawNonNull(envelope["stop"]) {
		stops, err := chatStopSequences(envelope["stop"])
		if err != nil {
			return nil, err
		}
		result["stopSequences"] = stops
	}
	return result, nil
}

func translateChatGeminiMessages(
	raw json.RawMessage,
) ([]map[string]any, []translatedGeminiContent, error) {
	var source []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &source); err != nil ||
		len(source) == 0 {
		return nil, nil, fmt.Errorf(
			"messages must be a non-empty array of objects",
		)
	}
	system := make([]map[string]any, 0)
	contents := make([]translatedGeminiContent, 0, len(source))
	toolNames := make(map[string]geminiToolIdentity)
	sawConversation := false
	previousSourceRole := ""
	for index, message := range source {
		if message == nil {
			return nil, nil, fmt.Errorf(
				"messages item %d must be an object",
				index,
			)
		}
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil {
			return nil, nil, fmt.Errorf(
				"messages item %d role must be a string",
				index,
			)
		}
		switch role {
		case "system":
			if sawConversation {
				return nil, nil, unsupportedFeature(
					"system messages after conversation messages cannot be represented by Gemini GenerateContent",
				)
			}
			if err := rejectJSONFieldsFor(
				message,
				fmt.Sprintf("messages item %d", index),
				"Gemini GenerateContent",
				"role", "content",
			); err != nil {
				return nil, nil, err
			}
			parts, err := translateChatGeminiContent(
				message["content"],
				fmt.Sprintf("messages item %d content", index),
				"system",
			)
			if err != nil {
				return nil, nil, err
			}
			system = append(system, parts...)
		case "developer":
			return nil, nil, unsupportedFeature(
				"developer messages cannot be represented distinctly by Gemini GenerateContent",
			)
		case "user", "assistant":
			sawConversation = true
			targetRole := role
			if role == "assistant" {
				targetRole = "model"
			}
			if len(contents) != 0 &&
				contents[len(contents)-1].Role == targetRole {
				return nil, nil, unsupportedFeature(
					"consecutive %s messages cannot be represented without merging message boundaries",
					role,
				)
			}
			parts, calls, err := translateChatGeminiMessage(
				message,
				role,
				index,
			)
			if err != nil {
				return nil, nil, err
			}
			for id, identity := range calls {
				if _, exists := toolNames[id]; exists {
					return nil, nil, fmt.Errorf(
						"tool call id %q is duplicated",
						id,
					)
				}
				toolNames[id] = identity
			}
			contents = append(contents, translatedGeminiContent{
				Role: targetRole, Parts: parts,
			})
		case "tool":
			sawConversation = true
			part, err := translateChatGeminiToolResult(
				message,
				index,
				toolNames,
			)
			if err != nil {
				return nil, nil, err
			}
			if previousSourceRole != "assistant" &&
				previousSourceRole != "tool" {
				return nil, nil, fmt.Errorf(
					"messages item %d tool result must follow an assistant tool call",
					index,
				)
			}
			if previousSourceRole == "tool" {
				contents[len(contents)-1].Parts = append(
					contents[len(contents)-1].Parts,
					part,
				)
			} else {
				contents = append(contents, translatedGeminiContent{
					Role: "user", Parts: []map[string]any{part},
				})
			}
		default:
			return nil, nil, unsupportedFeature(
				"messages item %d role %q cannot be represented by Gemini GenerateContent",
				index,
				role,
			)
		}
		previousSourceRole = role
	}
	if len(contents) == 0 {
		return nil, nil, fmt.Errorf(
			"messages must contain at least one non-system message",
		)
	}
	return system, contents, nil
}

func translateChatGeminiMessage(
	message map[string]json.RawMessage,
	role string,
	index int,
) ([]map[string]any, map[string]geminiToolIdentity, error) {
	name := fmt.Sprintf("messages item %d", index)
	allowed := []string{"role", "content"}
	if role == "assistant" {
		allowed = append(allowed, "tool_calls")
	}
	if err := rejectJSONFieldsFor(
		message,
		name,
		"Gemini GenerateContent",
		allowed...,
	); err != nil {
		return nil, nil, err
	}
	parts, err := translateChatGeminiContent(
		message["content"],
		name+" content",
		role,
	)
	if err != nil {
		return nil, nil, err
	}
	calls := make(map[string]geminiToolIdentity)
	if role == "assistant" && rawNonNull(message["tool_calls"]) {
		toolParts, names, err := translateChatGeminiAssistantToolCalls(
			message["tool_calls"],
			name+" tool_calls",
		)
		if err != nil {
			return nil, nil, err
		}
		parts = append(parts, toolParts...)
		calls = names
	}
	if len(parts) == 0 {
		return nil, nil, fmt.Errorf("%s content must not be empty", name)
	}
	return parts, calls, nil
}

func translateChatGeminiContent(
	raw json.RawMessage,
	name string,
	role string,
) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		if role == "assistant" {
			return nil, nil
		}
		return nil, fmt.Errorf("%s is required", name)
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		if text == "" {
			return nil, fmt.Errorf("%s must not be empty", name)
		}
		return []map[string]any{{"text": text}}, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &parts); err != nil ||
		len(parts) == 0 {
		return nil, fmt.Errorf(
			"%s must be a string or a non-empty array of content parts",
			name,
		)
	}
	result := make([]map[string]any, 0, len(parts))
	for index, part := range parts {
		if part == nil {
			return nil, fmt.Errorf(
				"%s item %d must be an object",
				name,
				index,
			)
		}
		var partType string
		if err := json.Unmarshal(part["type"], &partType); err != nil {
			return nil, fmt.Errorf(
				"%s item %d type must be a string",
				name,
				index,
			)
		}
		partName := fmt.Sprintf("%s item %d", name, index)
		switch partType {
		case "text":
			if err := rejectJSONFieldsFor(
				part,
				partName,
				"Gemini GenerateContent",
				"type", "text",
			); err != nil {
				return nil, err
			}
			var value string
			if err := json.Unmarshal(part["text"], &value); err != nil ||
				value == "" {
				return nil, fmt.Errorf(
					"%s text must be a non-empty string",
					partName,
				)
			}
			result = append(result, map[string]any{"text": value})
		case "image_url":
			if role != "user" {
				return nil, unsupportedFeature(
					"%s image content is only representable for user messages",
					partName,
				)
			}
			image, err := translateChatGeminiImagePart(part, partName)
			if err != nil {
				return nil, err
			}
			result = append(result, image)
		default:
			return nil, unsupportedFeature(
				"%s type %q cannot be represented by Gemini GenerateContent",
				partName,
				partType,
			)
		}
	}
	return result, nil
}

func translateChatGeminiImagePart(
	part map[string]json.RawMessage,
	name string,
) (map[string]any, error) {
	if err := rejectJSONFieldsFor(
		part,
		name,
		"Gemini GenerateContent",
		"type", "image_url",
	); err != nil {
		return nil, err
	}
	image, err := strictJSONObjectFor(
		part["image_url"],
		name+".image_url",
		"Gemini GenerateContent",
		"url", "detail",
	)
	if err != nil {
		return nil, err
	}
	if rawNonNull(image["detail"]) {
		return nil, unsupportedFeature(
			"%s detail cannot be represented by Gemini GenerateContent",
			name,
		)
	}
	var source string
	if err := json.Unmarshal(image["url"], &source); err != nil ||
		source == "" {
		return nil, fmt.Errorf(
			"%s image_url.url must be a non-empty string",
			name,
		)
	}
	mediaType, data, err := decodeBase64DataURL(source)
	if err != nil {
		return nil, unsupportedFeature(
			"%s must use an inline base64 data URL: %v",
			name,
			err,
		)
	}
	switch mediaType {
	case "image/jpeg", "image/png", "image/webp":
	default:
		return nil, unsupportedFeature(
			"%s media type %q is not supported by Gemini GenerateContent",
			name,
			mediaType,
		)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s image data must not be empty", name)
	}
	return map[string]any{
		"inlineData": map[string]any{
			"mimeType": mediaType,
			"data":     base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

func translateChatGeminiAssistantToolCalls(
	raw json.RawMessage,
	name string,
) ([]map[string]any, map[string]geminiToolIdentity, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil ||
		len(calls) == 0 {
		return nil, nil, fmt.Errorf(
			"%s must be a non-empty array of objects",
			name,
		)
	}
	result := make([]map[string]any, 0, len(calls))
	names := make(map[string]geminiToolIdentity, len(calls))
	for index, call := range calls {
		callName := fmt.Sprintf("%s item %d", name, index)
		if err := rejectJSONFieldsFor(
			call,
			callName,
			"Gemini GenerateContent",
			"id", "type", "function",
		); err != nil {
			return nil, nil, err
		}
		var id, callType string
		if err := json.Unmarshal(call["id"], &id); err != nil ||
			id == "" {
			return nil, nil, fmt.Errorf(
				"%s id must be a non-empty string",
				callName,
			)
		}
		if _, exists := names[id]; exists {
			return nil, nil, fmt.Errorf(
				"%s id %q is duplicated",
				callName,
				id,
			)
		}
		if err := json.Unmarshal(call["type"], &callType); err != nil ||
			callType != "function" {
			return nil, nil, unsupportedFeature(
				"%s must be a function tool call",
				callName,
			)
		}
		function, err := strictJSONObjectFor(
			call["function"],
			callName+".function",
			"Gemini GenerateContent",
			"name", "arguments",
		)
		if err != nil {
			return nil, nil, err
		}
		var functionName, arguments string
		if err := json.Unmarshal(
			function["name"],
			&functionName,
		); err != nil || functionName == "" {
			return nil, nil, fmt.Errorf(
				"%s function.name must be a non-empty string",
				callName,
			)
		}
		if err := json.Unmarshal(
			function["arguments"],
			&arguments,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"%s function.arguments must be a JSON string",
				callName,
			)
		}
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(arguments), &args); err != nil ||
			args == nil {
			return nil, nil, fmt.Errorf(
				"%s function.arguments must encode a JSON object",
				callName,
			)
		}
		providerID, signature, wrapped, err :=
			decodeGeminiToolCallID(id)
		if err != nil {
			return nil, nil, fmt.Errorf("%s id: %w", callName, err)
		}
		if !wrapped {
			providerID = id
		}
		functionCall := map[string]any{
			"id": providerID, "name": functionName,
			"args": json.RawMessage(arguments),
		}
		part := map[string]any{"functionCall": functionCall}
		if signature != "" {
			part["thoughtSignature"] = signature
		}
		result = append(result, part)
		names[id] = geminiToolIdentity{
			ID: providerID, Name: functionName,
		}
	}
	return result, names, nil
}

func translateChatGeminiToolResult(
	message map[string]json.RawMessage,
	index int,
	toolNames map[string]geminiToolIdentity,
) (map[string]any, error) {
	name := fmt.Sprintf("messages item %d", index)
	if err := rejectJSONFieldsFor(
		message,
		name,
		"Gemini GenerateContent",
		"role", "tool_call_id", "content",
	); err != nil {
		return nil, err
	}
	var toolCallID string
	if err := json.Unmarshal(
		message["tool_call_id"],
		&toolCallID,
	); err != nil || toolCallID == "" {
		return nil, fmt.Errorf(
			"%s tool_call_id must be a non-empty string",
			name,
		)
	}
	identity, exists := toolNames[toolCallID]
	if !exists {
		return nil, fmt.Errorf(
			"%s tool_call_id %q has no matching assistant tool call",
			name,
			toolCallID,
		)
	}
	var content string
	if err := json.Unmarshal(message["content"], &content); err != nil {
		return nil, unsupportedFeature(
			"%s content must be a string containing a JSON object for Gemini GenerateContent",
			name,
		)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &response); err != nil ||
		response == nil {
		return nil, unsupportedFeature(
			"%s content must encode a JSON object for Gemini GenerateContent",
			name,
		)
	}
	return map[string]any{
		"functionResponse": map[string]any{
			"id": identity.ID, "name": identity.Name,
			"response": json.RawMessage(content),
		},
	}, nil
}

type geminiEncodedToolCallID struct {
	Version          int    `json:"v"`
	ID               string `json:"id"`
	ThoughtSignature string `json:"thought_signature"`
}

func encodeGeminiToolCallID(
	providerID string,
	thoughtSignature string,
) (string, error) {
	if providerID == "" || thoughtSignature == "" {
		return "", fmt.Errorf(
			"the Gemini signed tool-call identity is incomplete",
		)
	}
	encoded, err := json.Marshal(geminiEncodedToolCallID{
		Version:          1,
		ID:               providerID,
		ThoughtSignature: thoughtSignature,
	})
	if err != nil {
		return "", err
	}
	return geminiToolCallIDPrefix +
		base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeGeminiToolCallID(
	value string,
) (string, string, bool, error) {
	if !strings.HasPrefix(value, geminiToolCallIDPrefix) {
		return "", "", false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(
		strings.TrimPrefix(value, geminiToolCallIDPrefix),
	)
	if err != nil {
		return "", "", true, fmt.Errorf(
			"signed Gemini tool-call ID is malformed",
		)
	}
	var decoded geminiEncodedToolCallID
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil ||
		decoder.Decode(&struct{}{}) != io.EOF ||
		decoded.Version != 1 ||
		decoded.ID == "" ||
		decoded.ThoughtSignature == "" {
		return "", "", true, fmt.Errorf(
			"signed Gemini tool-call ID is malformed",
		)
	}
	return decoded.ID, decoded.ThoughtSignature, true, nil
}

func translateChatGeminiTools(
	envelope map[string]json.RawMessage,
) ([]map[string]any, map[string]any, error) {
	var translated []map[string]any
	toolNames := make(map[string]struct{})
	if rawNonNull(envelope["tools"]) {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(envelope["tools"], &tools); err != nil ||
			len(tools) == 0 {
			return nil, nil, fmt.Errorf(
				"tools must be a non-empty array of objects",
			)
		}
		translated = make([]map[string]any, 0, len(tools))
		for index, tool := range tools {
			name := fmt.Sprintf("tools item %d", index)
			if err := rejectJSONFieldsFor(
				tool,
				name,
				"Gemini GenerateContent",
				"type", "function",
			); err != nil {
				return nil, nil, err
			}
			var toolType string
			if err := json.Unmarshal(
				tool["type"],
				&toolType,
			); err != nil || toolType != "function" {
				return nil, nil, unsupportedFeature(
					"%s must be a function tool",
					name,
				)
			}
			function, err := strictJSONObjectFor(
				tool["function"],
				name+".function",
				"Gemini GenerateContent",
				"name", "description", "parameters", "strict",
			)
			if err != nil {
				return nil, nil, err
			}
			var functionName string
			if err := json.Unmarshal(
				function["name"],
				&functionName,
			); err != nil || functionName == "" {
				return nil, nil, fmt.Errorf(
					"%s function.name must be a non-empty string",
					name,
				)
			}
			if _, exists := toolNames[functionName]; exists {
				return nil, nil, fmt.Errorf(
					"tool function name %q is duplicated",
					functionName,
				)
			}
			toolNames[functionName] = struct{}{}
			declaration := map[string]any{"name": functionName}
			if rawNonNull(function["description"]) {
				var description string
				if err := json.Unmarshal(
					function["description"],
					&description,
				); err != nil {
					return nil, nil, fmt.Errorf(
						"%s function.description must be a string",
						name,
					)
				}
				if description != "" {
					declaration["description"] = description
				}
			}
			if rawNonNull(function["parameters"]) {
				var schema map[string]json.RawMessage
				if err := json.Unmarshal(
					function["parameters"],
					&schema,
				); err != nil || schema == nil {
					return nil, nil, fmt.Errorf(
						"%s function.parameters must be a JSON object",
						name,
					)
				}
				declaration["parametersJsonSchema"] =
					json.RawMessage(append(
						[]byte(nil),
						function["parameters"]...,
					))
			}
			if rawNonNull(function["strict"]) {
				var strict bool
				if err := json.Unmarshal(
					function["strict"],
					&strict,
				); err != nil {
					return nil, nil, fmt.Errorf(
						"%s function.strict must be a boolean",
						name,
					)
				}
				if strict {
					return nil, nil, unsupportedFeature(
						"%s function.strict=true cannot be represented without validating the common strict-schema subset",
						name,
					)
				}
			}
			translated = append(translated, declaration)
		}
	}
	if rawNonNull(envelope["parallel_tool_calls"]) {
		var parallel bool
		if err := json.Unmarshal(
			envelope["parallel_tool_calls"],
			&parallel,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"parallel_tool_calls must be a boolean",
			)
		}
		if len(translated) == 0 {
			return nil, nil, fmt.Errorf(
				"parallel_tool_calls requires tools",
			)
		}
		if !parallel {
			return nil, nil, unsupportedFeature(
				"parallel_tool_calls=false cannot be represented by Gemini GenerateContent",
			)
		}
	}
	var functionConfig map[string]any
	forcedName := ""
	if rawNonNull(envelope["tool_choice"]) {
		var choice string
		if json.Unmarshal(envelope["tool_choice"], &choice) == nil {
			switch choice {
			case "none":
				if len(translated) != 0 {
					functionConfig = map[string]any{"mode": "NONE"}
				}
			case "auto":
				functionConfig = map[string]any{"mode": "AUTO"}
			case "required":
				functionConfig = map[string]any{"mode": "ANY"}
			default:
				return nil, nil, fmt.Errorf(
					"tool_choice must be none, auto, required, or a function tool object",
				)
			}
		} else {
			choice, err := strictJSONObjectFor(
				envelope["tool_choice"],
				"tool_choice",
				"Gemini GenerateContent",
				"type", "function",
			)
			if err != nil {
				return nil, nil, err
			}
			var choiceType string
			if err := json.Unmarshal(
				choice["type"],
				&choiceType,
			); err != nil || choiceType != "function" {
				return nil, nil, unsupportedFeature(
					"only function tool_choice objects can be represented by Gemini GenerateContent",
				)
			}
			function, err := strictJSONObjectFor(
				choice["function"],
				"tool_choice.function",
				"Gemini GenerateContent",
				"name",
			)
			if err != nil {
				return nil, nil, err
			}
			if err := json.Unmarshal(
				function["name"],
				&forcedName,
			); err != nil || forcedName == "" {
				return nil, nil, fmt.Errorf(
					"tool_choice.function.name must be a non-empty string",
				)
			}
			functionConfig = map[string]any{
				"mode":                 "ANY",
				"allowedFunctionNames": []string{forcedName},
			}
		}
	}
	if functionConfig != nil && len(translated) == 0 {
		return nil, nil, fmt.Errorf("tool_choice requires tools")
	}
	if forcedName != "" {
		if _, exists := toolNames[forcedName]; !exists {
			return nil, nil, fmt.Errorf(
				"tool_choice references unknown function %q",
				forcedName,
			)
		}
	}
	if functionConfig == nil {
		return translated, nil, nil
	}
	return translated, map[string]any{
		"functionCallingConfig": functionConfig,
	}, nil
}

type geminiChatToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type geminiChatCandidate struct {
	Text         string
	ToolCalls    []geminiChatToolCall
	FinishReason string
}

func parseGeminiChatPayload(
	payload []byte,
) (
	map[string]json.RawMessage,
	*geminiChatCandidate,
	ledger.TokenUsage,
	bool,
	error,
) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil ||
		envelope == nil {
		return nil, nil, missingUsage(), false, fmt.Errorf(
			"the Gemini response must be a JSON object",
		)
	}
	if err := rejectGeminiChatResponseFields(
		envelope,
		"Gemini response",
		"candidates", "promptFeedback", "usageMetadata",
		"modelVersion", "responseId", "modelStatus", "error",
	); err != nil {
		return nil, nil, missingUsage(), false, err
	}
	if rawNonNull(envelope["error"]) {
		return envelope, nil, missingUsage(), false, fmt.Errorf(
			"the Gemini success response must not contain an error",
		)
	}
	for _, field := range []string{"modelVersion", "responseId"} {
		if !rawNonNull(envelope[field]) {
			continue
		}
		var value string
		if json.Unmarshal(envelope[field], &value) != nil {
			return nil, nil, missingUsage(), false, fmt.Errorf(
				"the Gemini response %s must be a string",
				field,
			)
		}
	}
	usage, foundUsage := extractGeminiGenerateContentUsage(payload)
	blocked, err := geminiPromptBlocked(envelope["promptFeedback"])
	if err != nil {
		return nil, nil, usage, foundUsage, err
	}
	var candidates []map[string]json.RawMessage
	if rawNonNull(envelope["candidates"]) {
		if err := json.Unmarshal(
			envelope["candidates"],
			&candidates,
		); err != nil {
			return nil, nil, usage, foundUsage, fmt.Errorf(
				"the Gemini candidates must be an array of objects",
			)
		}
	}
	if len(candidates) > 1 {
		return nil, nil, usage, foundUsage, fmt.Errorf(
			"upstream Gemini returned multiple candidates that cannot be represented for a single-choice Chat Completions request",
		)
	}
	if blocked {
		if len(candidates) != 0 {
			return nil, nil, usage, foundUsage, fmt.Errorf(
				"the Gemini prompt feedback and candidates are inconsistent",
			)
		}
		return envelope, &geminiChatCandidate{
			FinishReason: "SAFETY",
		}, usage, foundUsage, nil
	}
	if len(candidates) == 0 {
		return envelope, nil, usage, foundUsage, nil
	}
	candidate, err := parseGeminiChatCandidate(candidates[0])
	return envelope, candidate, usage, foundUsage, err
}

func geminiPromptBlocked(raw json.RawMessage) (bool, error) {
	if !rawNonNull(raw) {
		return false, nil
	}
	feedback, err := strictJSONObjectFor(
		raw,
		"Gemini promptFeedback",
		"Chat Completions",
		"blockReason", "safetyRatings",
	)
	if err != nil {
		return false, err
	}
	if !rawNonNull(feedback["blockReason"]) {
		return false, nil
	}
	var reason string
	if json.Unmarshal(feedback["blockReason"], &reason) != nil {
		return false, fmt.Errorf(
			"the Gemini promptFeedback.blockReason must be a string",
		)
	}
	switch reason {
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY":
		return true, nil
	default:
		return false, fmt.Errorf(
			"the Gemini prompt block reason %q cannot be represented in Chat Completions",
			reason,
		)
	}
}

func parseGeminiChatCandidate(
	raw map[string]json.RawMessage,
) (*geminiChatCandidate, error) {
	if raw == nil {
		return nil, fmt.Errorf("the Gemini candidate must be an object")
	}
	if err := rejectGeminiChatResponseFields(
		raw,
		"Gemini candidate",
		"content", "finishReason", "safetyRatings",
		"citationMetadata", "tokenCount", "groundingAttributions",
		"groundingMetadata", "avgLogprobs", "logprobsResult",
		"urlContextMetadata", "index", "finishMessage",
	); err != nil {
		return nil, err
	}
	for _, field := range []string{
		"citationMetadata", "groundingAttributions",
		"groundingMetadata", "avgLogprobs", "logprobsResult",
		"urlContextMetadata",
	} {
		if rawActive(raw[field]) {
			return nil, fmt.Errorf(
				"the Gemini candidate field %q cannot be represented in Chat Completions",
				field,
			)
		}
	}
	if rawNonNull(raw["index"]) {
		var index int
		if json.Unmarshal(raw["index"], &index) != nil || index != 0 {
			return nil, fmt.Errorf(
				"the Gemini candidate index must be zero",
			)
		}
	}
	result := &geminiChatCandidate{}
	if rawNonNull(raw["finishReason"]) {
		if json.Unmarshal(
			raw["finishReason"],
			&result.FinishReason,
		) != nil || result.FinishReason == "" {
			return nil, fmt.Errorf(
				"the Gemini candidate finishReason must be a string",
			)
		}
	}
	if !rawNonNull(raw["content"]) {
		return result, nil
	}
	var content map[string]json.RawMessage
	if json.Unmarshal(raw["content"], &content) != nil ||
		content == nil {
		return nil, fmt.Errorf(
			"the Gemini candidate content must be an object",
		)
	}
	if err := rejectGeminiChatResponseFields(
		content,
		"Gemini candidate content",
		"role", "parts",
	); err != nil {
		return nil, err
	}
	var role string
	if json.Unmarshal(content["role"], &role) != nil || role != "model" {
		return nil, fmt.Errorf(
			"the Gemini candidate content must have role model",
		)
	}
	var parts []map[string]json.RawMessage
	if rawNonNull(content["parts"]) {
		if json.Unmarshal(content["parts"], &parts) != nil {
			return nil, fmt.Errorf(
				"the Gemini candidate parts must be an array of objects",
			)
		}
	}
	seenIDs := make(map[string]struct{})
	var text strings.Builder
	for index, part := range parts {
		if part == nil {
			return nil, fmt.Errorf(
				"the Gemini candidate part %d must be an object",
				index,
			)
		}
		if err := rejectGeminiChatResponseFields(
			part,
			fmt.Sprintf("Gemini candidate part %d", index),
			"text", "inlineData", "functionCall",
			"functionResponse", "fileData", "executableCode",
			"codeExecutionResult", "thought", "thoughtSignature",
			"videoMetadata",
		); err != nil {
			return nil, err
		}
		if rawJSONBool(part["thought"]) {
			return nil, fmt.Errorf(
				"the Gemini thought content cannot be represented in Chat Completions",
			)
		}
		var thoughtSignature string
		if rawNonNull(part["thoughtSignature"]) {
			if json.Unmarshal(
				part["thoughtSignature"],
				&thoughtSignature,
			) != nil || thoughtSignature == "" {
				return nil, fmt.Errorf(
					"the Gemini thoughtSignature must be a non-empty string",
				)
			}
			if !rawNonNull(part["functionCall"]) {
				return nil, fmt.Errorf(
					"the Gemini non-tool thoughtSignature cannot be represented in Chat Completions",
				)
			}
		}
		active := 0
		if rawNonNull(part["text"]) {
			active++
			var value string
			if json.Unmarshal(part["text"], &value) != nil {
				return nil, fmt.Errorf(
					"the Gemini candidate part %d text must be a string",
					index,
				)
			}
			text.WriteString(value)
		}
		if rawNonNull(part["functionCall"]) {
			active++
			call, err := parseGeminiChatFunctionCall(
				part["functionCall"],
				index,
			)
			if err != nil {
				return nil, err
			}
			if thoughtSignature != "" {
				call.ID, err = encodeGeminiToolCallID(
					call.ID,
					thoughtSignature,
				)
				if err != nil {
					return nil, err
				}
			}
			if _, exists := seenIDs[call.ID]; exists {
				return nil, fmt.Errorf(
					"the Gemini function call id %q is duplicated",
					call.ID,
				)
			}
			seenIDs[call.ID] = struct{}{}
			result.ToolCalls = append(result.ToolCalls, call)
		}
		for _, field := range []string{
			"inlineData", "functionResponse", "fileData",
			"executableCode", "codeExecutionResult", "videoMetadata",
		} {
			if rawNonNull(part[field]) {
				return nil, fmt.Errorf(
					"the Gemini candidate part field %q cannot be represented in Chat Completions",
					field,
				)
			}
		}
		if active != 1 {
			return nil, fmt.Errorf(
				"the Gemini candidate part %d must contain exactly one representable data field",
				index,
			)
		}
	}
	result.Text = text.String()
	return result, nil
}

func parseGeminiChatFunctionCall(
	raw json.RawMessage,
	index int,
) (geminiChatToolCall, error) {
	call, err := strictJSONObjectFor(
		raw,
		fmt.Sprintf("Gemini candidate part %d functionCall", index),
		"Chat Completions",
		"id", "name", "args",
	)
	if err != nil {
		return geminiChatToolCall{}, err
	}
	var result geminiChatToolCall
	if json.Unmarshal(call["id"], &result.ID) != nil || result.ID == "" ||
		json.Unmarshal(call["name"], &result.Name) != nil ||
		result.Name == "" {
		return geminiChatToolCall{}, fmt.Errorf(
			"the Gemini function call identity is invalid",
		)
	}
	var args map[string]json.RawMessage
	if json.Unmarshal(call["args"], &args) != nil || args == nil {
		return geminiChatToolCall{}, fmt.Errorf(
			"the Gemini function call args must be a JSON object",
		)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return geminiChatToolCall{}, err
	}
	result.Arguments = string(encoded)
	return result, nil
}

func rejectGeminiChatResponseFields(
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
				"%s field %q cannot be represented in Chat Completions",
				name,
				field,
			)
		}
	}
	return nil
}

func geminiChatFinishReason(
	reason string,
	hasToolCalls bool,
) (string, error) {
	switch reason {
	case "STOP":
		if hasToolCalls {
			return "tool_calls", nil
		}
		return "stop", nil
	case "MAX_TOKENS":
		if hasToolCalls {
			return "", fmt.Errorf(
				"the Gemini MAX_TOKENS finish reason is inconsistent with function calls",
			)
		}
		return "length", nil
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT",
		"SPII", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT",
		"IMAGE_RECITATION", "ESCALATION":
		if hasToolCalls {
			return "", fmt.Errorf(
				"the Gemini filtered finish reason is inconsistent with function calls",
			)
		}
		return "content_filter", nil
	default:
		return "", fmt.Errorf(
			"the Gemini finish reason %q cannot be represented in Chat Completions",
			reason,
		)
	}
}

func translateGeminiResponseToChatCompletionsLegacy(
	body []byte,
	requestID string,
	model string,
	created int64,
) ([]byte, ledger.TokenUsage, error) {
	_, candidate, usage, foundUsage, err := parseGeminiChatPayload(body)
	if err != nil {
		return nil, missingUsage(), err
	}
	if candidate == nil || candidate.FinishReason == "" {
		return nil, usage, fmt.Errorf(
			"the Gemini response must contain one finished candidate",
		)
	}
	if !foundUsage || usage.Completeness != ledger.UsageComplete {
		return nil, usage, fmt.Errorf(
			"the Gemini response must contain complete usage",
		)
	}
	finishReason, err := geminiChatFinishReason(
		candidate.FinishReason,
		len(candidate.ToolCalls) != 0,
	)
	if err != nil {
		return nil, usage, err
	}
	message := map[string]any{"role": "assistant"}
	if candidate.Text == "" {
		message["content"] = nil
	} else {
		message["content"] = candidate.Text
	}
	if len(candidate.ToolCalls) != 0 {
		toolCalls := make([]any, 0, len(candidate.ToolCalls))
		for _, call := range candidate.ToolCalls {
			toolCalls = append(toolCalls, map[string]any{
				"id": call.ID, "type": "function",
				"function": map[string]any{
					"name": call.Name, "arguments": call.Arguments,
				},
			})
		}
		message["tool_calls"] = toolCalls
	}
	result := map[string]any{
		"id":      "chatcmpl-" + requestID,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
			"logprobs":      nil,
		}},
		"usage": chatUsageFromGeminiLedger(usage),
	}
	translated, err := json.Marshal(result)
	return translated, usage, err
}

func chatUsageFromGeminiLedger(
	usage ledger.TokenUsage,
) map[string]any {
	result := chatUsageFromLedger(usage)
	if usage.ReasoningTokens != nil {
		result["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": *usage.ReasoningTokens,
		}
	}
	return result
}

func translateGeminiErrorToOpenAI(
	w http.ResponseWriter,
	response *http.Response,
) {
	body, _ := readBoundedResponse(response.Body, 1<<20)
	var source geminiErrorEnvelope
	_ = json.Unmarshal(body, &source)
	message := strings.TrimSpace(source.Error.Message)
	if message == "" {
		message = "the upstream Gemini request failed"
	}
	writeOpenAIError(
		w,
		response.StatusCode,
		"upstream_error",
		message,
	)
}

type geminiChatStreamState struct {
	requestID    string
	model        string
	created      int64
	includeUsage bool
	started      bool
	finished     bool
	failed       bool
	nextTool     int
	toolIDs      map[string]struct{}
	usage        ledger.TokenUsage
	result       proxyResult
}

func proxyGeminiChatCompletionsStream(
	destination io.Writer,
	source io.Reader,
	requestID string,
	model string,
	includeUsage bool,
	result proxyResult,
) (proxyResult, error) {
	state := &geminiChatStreamState{
		requestID: requestID, model: model,
		created: time.Now().Unix(), includeUsage: includeUsage,
		toolIDs: make(map[string]struct{}),
		usage:   missingUsage(),
		result:  result,
	}
	err := proxySSEEvents(
		io.Discard,
		source,
		nil,
		func(payload []byte) error {
			return state.handle(destination, payload)
		},
	)
	if err != nil {
		return state.result, err
	}
	if state.failed {
		return state.result, nil
	}
	if !state.started || !state.finished {
		return state.result, fmt.Errorf(
			"the Gemini GenerateContent stream ended before a finished candidate",
		)
	}
	if state.usage.Completeness != ledger.UsageComplete {
		return state.result, fmt.Errorf(
			"the Gemini GenerateContent stream ended before complete usage",
		)
	}
	state.result.usage = state.usage
	if state.includeUsage {
		if err := state.writeUsageChunk(destination); err != nil {
			return state.result, err
		}
	}
	if err := writeSSEData(destination, []byte("[DONE]")); err != nil {
		return state.result, err
	}
	return state.result, nil
}

func (s *geminiChatStreamState) handle(
	destination io.Writer,
	payload []byte,
) error {
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil || raw == nil {
		return fmt.Errorf("the Gemini stream event must be a JSON object")
	}
	if rawNonNull(raw["error"]) {
		if s.failed || s.finished {
			return fmt.Errorf(
				"the Gemini stream error arrived after a terminal candidate",
			)
		}
		return s.handleError(destination, raw["error"])
	}
	if s.failed {
		return fmt.Errorf(
			"the Gemini stream event arrived after an error",
		)
	}
	_, candidate, usage, foundUsage, err :=
		parseGeminiChatPayload(payload)
	if err != nil {
		return err
	}
	if foundUsage {
		s.usage = geminiOperationStreamContent.mergeUsage(
			s.usage,
			usage,
		)
	}
	if candidate == nil {
		if !foundUsage {
			return fmt.Errorf(
				"the Gemini stream event contained neither a candidate nor usage",
			)
		}
		return nil
	}
	if s.finished {
		return fmt.Errorf(
			"the Gemini candidate arrived after a terminal candidate",
		)
	}
	if !s.started {
		s.started = true
		if err := s.writeChunk(
			destination,
			map[string]any{"role": "assistant"},
			nil,
		); err != nil {
			return err
		}
	}
	if candidate.Text != "" {
		if err := s.writeChunk(
			destination,
			map[string]any{"content": candidate.Text},
			nil,
		); err != nil {
			return err
		}
	}
	for _, call := range candidate.ToolCalls {
		if _, exists := s.toolIDs[call.ID]; exists {
			return fmt.Errorf(
				"the Gemini streamed function call id %q is duplicated",
				call.ID,
			)
		}
		s.toolIDs[call.ID] = struct{}{}
		toolIndex := s.nextTool
		s.nextTool++
		if err := s.writeChunk(destination, map[string]any{
			"tool_calls": []any{map[string]any{
				"index": toolIndex, "id": call.ID, "type": "function",
				"function": map[string]any{
					"name": call.Name, "arguments": call.Arguments,
				},
			}},
		}, nil); err != nil {
			return err
		}
	}
	if candidate.FinishReason == "" {
		return nil
	}
	finishReason, err := geminiChatFinishReason(
		candidate.FinishReason,
		s.nextTool != 0,
	)
	if err != nil {
		return err
	}
	s.finished = true
	return s.writeChunk(
		destination,
		map[string]any{},
		&finishReason,
	)
}

func (s *geminiChatStreamState) handleError(
	destination io.Writer,
	raw json.RawMessage,
) error {
	var source geminiError
	_ = json.Unmarshal(raw, &source)
	message := strings.TrimSpace(source.Message)
	if message == "" {
		message = "the upstream Gemini stream failed"
	}
	payload, _ := json.Marshal(openAIErrorEnvelope{
		Error: openAIError{
			Message: message,
			Type:    "api_error",
			Code:    "upstream_stream_error",
		},
	})
	if err := writeSSEData(destination, payload); err != nil {
		return err
	}
	s.failed = true
	s.result.outcome = ledger.OutcomeUpstreamError
	s.result.failureClass = "gemini_stream_error"
	suffix := strings.ToLower(source.Status)
	if validFailureClassSuffix(suffix) {
		s.result.failureClass += "_" + suffix
	}
	return nil
}

func (s *geminiChatStreamState) writeChunk(
	destination io.Writer,
	delta map[string]any,
	finishReason *string,
) error {
	choice := map[string]any{
		"index":         0,
		"delta":         delta,
		"finish_reason": nil,
		"logprobs":      nil,
	}
	if finishReason != nil {
		choice["finish_reason"] = *finishReason
	}
	chunk := map[string]any{
		"id":      "chatcmpl-" + s.requestID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{choice},
	}
	if s.includeUsage {
		chunk["usage"] = nil
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return writeSSEData(destination, payload)
}

func (s *geminiChatStreamState) writeUsageChunk(
	destination io.Writer,
) error {
	chunk := map[string]any{
		"id":      "chatcmpl-" + s.requestID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   chatUsageFromGeminiLedger(s.usage),
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return writeSSEData(destination, payload)
}
