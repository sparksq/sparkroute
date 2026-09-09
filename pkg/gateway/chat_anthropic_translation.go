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

const chatAnthropicStreamFailureClass = "anthropic_chat_translation_stream"

type translatedAnthropicMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

func translateChatCompletionsRequestToAnthropic(
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
				"Chat Completions field %q cannot be represented by Anthropic Messages",
				field,
			)
		}
	}
	if err := validateChatAnthropicNeutralFields(envelope); err != nil {
		return nil, err
	}

	system, messages, err := translateChatAnthropicMessages(
		envelope["messages"],
	)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"messages": messages}
	if len(system) != 0 {
		result["system"] = system
	}
	if rawNonNull(envelope["stream"]) {
		var stream bool
		if err := json.Unmarshal(envelope["stream"], &stream); err != nil {
			return nil, fmt.Errorf("stream must be a boolean")
		}
		result["stream"] = stream
	}
	if err := translateChatAnthropicInference(envelope, result); err != nil {
		return nil, err
	}
	tools, choice, err := translateChatAnthropicTools(envelope)
	if err != nil {
		return nil, err
	}
	if len(tools) != 0 {
		result["tools"] = tools
	}
	if choice != nil {
		result["tool_choice"] = choice
	}
	return json.Marshal(result)
}

func rewriteTranslatedAnthropicModel(
	body []byte,
	model string,
) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return nil, fmt.Errorf(
			"translated Anthropic request must be a JSON object",
		)
	}
	return rewriteModel(envelope, model)
}

func validateChatAnthropicNeutralFields(
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
				"Chat Completions %s cannot be represented by Anthropic Messages unless it is zero",
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
				"Chat Completions %s=true cannot be represented by Anthropic Messages",
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
				"Chat Completions n values other than 1 cannot be represented by Anthropic Messages",
			)
		}
	}
	if rawNonNull(envelope["response_format"]) {
		format, err := strictJSONObjectFor(
			envelope["response_format"],
			"response_format",
			"Anthropic Messages",
			"type",
		)
		if err != nil {
			return err
		}
		var formatType string
		if err := json.Unmarshal(format["type"], &formatType); err != nil ||
			formatType != "text" {
			return unsupportedFeature(
				"Chat Completions response_format cannot be represented by Anthropic Messages unless its type is text",
			)
		}
	}
	if rawNonNull(envelope["modalities"]) {
		var modalities []string
		if err := json.Unmarshal(envelope["modalities"], &modalities); err != nil ||
			len(modalities) != 1 || modalities[0] != "text" {
			return unsupportedFeature(
				"Chat Completions modalities cannot be represented by Anthropic Messages unless they contain only text",
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
			"Anthropic Messages",
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

func translateChatAnthropicInference(
	envelope map[string]json.RawMessage,
	result map[string]any,
) error {
	maxCompletion := envelope["max_completion_tokens"]
	maxTokens := envelope["max_tokens"]
	if rawNonNull(maxCompletion) && rawNonNull(maxTokens) {
		return fmt.Errorf(
			"max_tokens and max_completion_tokens cannot both be set",
		)
	}
	rawMax := maxCompletion
	if !rawNonNull(rawMax) {
		rawMax = maxTokens
	}
	if !rawNonNull(rawMax) {
		return unsupportedFeature(
			"Anthropic Messages requires max_tokens or max_completion_tokens to be set explicitly",
		)
	}
	var maximum int
	if err := json.Unmarshal(rawMax, &maximum); err != nil || maximum < 1 {
		return fmt.Errorf(
			"maximum completion tokens must be a positive integer",
		)
	}
	result["max_tokens"] = maximum

	for _, field := range []string{"temperature", "top_p"} {
		raw := envelope[field]
		if !rawNonNull(raw) {
			continue
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return fmt.Errorf("%s must be a number", field)
		}
		if value < 0 || value > 1 {
			return unsupportedFeature(
				"Chat Completions %s=%v is outside the Anthropic Messages range [0,1]",
				field,
				value,
			)
		}
		result[field] = value
	}
	if rawNonNull(envelope["stop"]) {
		stops, err := chatStopSequences(envelope["stop"])
		if err != nil {
			return err
		}
		result["stop_sequences"] = stops
	}
	return nil
}

func translateChatAnthropicMessages(
	raw json.RawMessage,
) ([]map[string]any, []translatedAnthropicMessage, error) {
	var source []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &source); err != nil ||
		len(source) == 0 {
		return nil, nil, fmt.Errorf(
			"messages must be a non-empty array of objects",
		)
	}
	system := make([]map[string]any, 0)
	messages := make([]translatedAnthropicMessage, 0, len(source))
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
					"system messages after conversation messages cannot be represented by Anthropic Messages",
				)
			}
			if err := rejectJSONFieldsFor(
				message,
				fmt.Sprintf("messages item %d", index),
				"Anthropic Messages",
				"role", "content",
			); err != nil {
				return nil, nil, err
			}
			blocks, err := translateChatAnthropicTextContent(
				message["content"],
				fmt.Sprintf("messages item %d content", index),
				false,
			)
			if err != nil {
				return nil, nil, err
			}
			system = append(system, blocks...)
		case "developer":
			return nil, nil, unsupportedFeature(
				"developer messages cannot be represented distinctly by Anthropic Messages",
			)
		case "user", "assistant":
			sawConversation = true
			if len(messages) != 0 &&
				messages[len(messages)-1].Role == role {
				return nil, nil, unsupportedFeature(
					"consecutive %s messages cannot be represented without merging message boundaries",
					role,
				)
			}
			blocks, err := translateChatAnthropicConversationMessage(
				message,
				role,
				index,
			)
			if err != nil {
				return nil, nil, err
			}
			messages = append(messages, translatedAnthropicMessage{
				Role: role, Content: blocks,
			})
		case "tool":
			sawConversation = true
			block, err := translateChatAnthropicToolResult(
				message,
				index,
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
				messages[len(messages)-1].Content = append(
					messages[len(messages)-1].Content,
					block,
				)
			} else {
				messages = append(messages, translatedAnthropicMessage{
					Role:    "user",
					Content: []map[string]any{block},
				})
			}
		default:
			return nil, nil, unsupportedFeature(
				"messages item %d role %q cannot be represented by Anthropic Messages",
				index,
				role,
			)
		}
		previousSourceRole = role
	}
	if len(messages) == 0 {
		return nil, nil, fmt.Errorf(
			"messages must contain at least one non-system message",
		)
	}
	return system, messages, nil
}

func translateChatAnthropicConversationMessage(
	message map[string]json.RawMessage,
	role string,
	index int,
) ([]map[string]any, error) {
	name := fmt.Sprintf("messages item %d", index)
	allowed := []string{"role", "content"}
	if role == "assistant" {
		allowed = append(allowed, "tool_calls")
	}
	if err := rejectJSONFieldsFor(
		message,
		name,
		"Anthropic Messages",
		allowed...,
	); err != nil {
		return nil, err
	}
	blocks, err := translateChatAnthropicContent(
		message["content"],
		name+" content",
		role,
	)
	if err != nil {
		return nil, err
	}
	if role == "assistant" && rawNonNull(message["tool_calls"]) {
		toolBlocks, err := translateChatAnthropicAssistantToolCalls(
			message["tool_calls"],
			name+" tool_calls",
		)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, toolBlocks...)
	}
	if len(blocks) == 0 {
		return nil, fmt.Errorf("%s content must not be empty", name)
	}
	return blocks, nil
}

func translateChatAnthropicContent(
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
		return []map[string]any{{"type": "text", "text": text}}, nil
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
				"Anthropic Messages",
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
			result = append(result, map[string]any{
				"type": "text", "text": value,
			})
		case "image_url":
			if role != "user" {
				return nil, unsupportedFeature(
					"%s image content is only representable for user messages",
					partName,
				)
			}
			block, err := translateChatAnthropicImagePart(part, partName)
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		default:
			return nil, unsupportedFeature(
				"%s type %q cannot be represented by Anthropic Messages",
				partName,
				partType,
			)
		}
	}
	return result, nil
}

func translateChatAnthropicTextContent(
	raw json.RawMessage,
	name string,
	allowNull bool,
) ([]map[string]any, error) {
	blocks, err := translateChatAnthropicContent(raw, name, "system")
	if allowNull && err != nil && !rawNonNull(raw) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if block["type"] != "text" {
			return nil, unsupportedFeature(
				"%s supports only text when translated to Anthropic system content",
				name,
			)
		}
	}
	return blocks, nil
}

func translateChatAnthropicImagePart(
	part map[string]json.RawMessage,
	name string,
) (map[string]any, error) {
	if err := rejectJSONFieldsFor(
		part,
		name,
		"Anthropic Messages",
		"type", "image_url",
	); err != nil {
		return nil, err
	}
	image, err := strictJSONObjectFor(
		part["image_url"],
		name+".image_url",
		"Anthropic Messages",
		"url", "detail",
	)
	if err != nil {
		return nil, err
	}
	if rawNonNull(image["detail"]) {
		return nil, unsupportedFeature(
			"%s detail cannot be represented by Anthropic Messages",
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
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return nil, unsupportedFeature(
			"%s media type %q is not supported by Anthropic Messages",
			name,
			mediaType,
		)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s image data must not be empty", name)
	}
	return map[string]any{
		"type": "image",
		"source": map[string]any{
			"type":       "base64",
			"media_type": mediaType,
			"data":       base64.StdEncoding.EncodeToString(data),
		},
	}, nil
}

func translateChatAnthropicAssistantToolCalls(
	raw json.RawMessage,
	name string,
) ([]map[string]any, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil ||
		len(calls) == 0 {
		return nil, fmt.Errorf(
			"%s must be a non-empty array of objects",
			name,
		)
	}
	result := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		callName := fmt.Sprintf("%s item %d", name, index)
		if err := rejectJSONFieldsFor(
			call,
			callName,
			"Anthropic Messages",
			"id", "type", "function",
		); err != nil {
			return nil, err
		}
		var id, callType string
		if err := json.Unmarshal(call["id"], &id); err != nil ||
			id == "" {
			return nil, fmt.Errorf(
				"%s id must be a non-empty string",
				callName,
			)
		}
		if err := json.Unmarshal(call["type"], &callType); err != nil ||
			callType != "function" {
			return nil, unsupportedFeature(
				"%s must be a function tool call",
				callName,
			)
		}
		function, err := strictJSONObjectFor(
			call["function"],
			callName+".function",
			"Anthropic Messages",
			"name", "arguments",
		)
		if err != nil {
			return nil, err
		}
		var functionName, arguments string
		if err := json.Unmarshal(
			function["name"],
			&functionName,
		); err != nil || functionName == "" {
			return nil, fmt.Errorf(
				"%s function.name must be a non-empty string",
				callName,
			)
		}
		if err := json.Unmarshal(
			function["arguments"],
			&arguments,
		); err != nil {
			return nil, fmt.Errorf(
				"%s function.arguments must be a JSON string",
				callName,
			)
		}
		var input map[string]json.RawMessage
		if err := json.Unmarshal([]byte(arguments), &input); err != nil ||
			input == nil {
			return nil, fmt.Errorf(
				"%s function.arguments must encode a JSON object",
				callName,
			)
		}
		result = append(result, map[string]any{
			"type": "tool_use",
			"id":   id,
			"name": functionName,
			"input": json.RawMessage(
				append([]byte(nil), []byte(arguments)...),
			),
		})
	}
	return result, nil
}

func translateChatAnthropicToolResult(
	message map[string]json.RawMessage,
	index int,
) (map[string]any, error) {
	name := fmt.Sprintf("messages item %d", index)
	if err := rejectJSONFieldsFor(
		message,
		name,
		"Anthropic Messages",
		"role", "tool_call_id", "content",
	); err != nil {
		return nil, err
	}
	var toolUseID string
	if err := json.Unmarshal(
		message["tool_call_id"],
		&toolUseID,
	); err != nil || toolUseID == "" {
		return nil, fmt.Errorf(
			"%s tool_call_id must be a non-empty string",
			name,
		)
	}
	content, err := translateChatAnthropicTextContent(
		message["content"],
		name+" content",
		false,
	)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"type":        "tool_result",
		"tool_use_id": toolUseID,
		"content":     content,
	}, nil
}

func translateChatAnthropicTools(
	envelope map[string]json.RawMessage,
) ([]map[string]any, map[string]any, error) {
	var translatedTools []map[string]any
	toolNames := make(map[string]struct{})
	if rawNonNull(envelope["tools"]) {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(envelope["tools"], &tools); err != nil ||
			len(tools) == 0 {
			return nil, nil, fmt.Errorf(
				"tools must be a non-empty array of objects",
			)
		}
		translatedTools = make([]map[string]any, 0, len(tools))
		for index, tool := range tools {
			name := fmt.Sprintf("tools item %d", index)
			if err := rejectJSONFieldsFor(
				tool,
				name,
				"Anthropic Messages",
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
				"Anthropic Messages",
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
			spec := map[string]any{"name": functionName}
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
					spec["description"] = description
				}
			}
			var schema any = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
			if rawNonNull(function["parameters"]) {
				var schemaObject map[string]json.RawMessage
				if err := json.Unmarshal(
					function["parameters"],
					&schemaObject,
				); err != nil || schemaObject == nil {
					return nil, nil, fmt.Errorf(
						"%s function.parameters must be a JSON object",
						name,
					)
				}
				schema = json.RawMessage(append(
					[]byte(nil),
					function["parameters"]...,
				))
			}
			spec["input_schema"] = schema
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
			translatedTools = append(translatedTools, spec)
		}
	}

	var choice map[string]any
	forcedToolName := ""
	if rawNonNull(envelope["tool_choice"]) {
		rawChoice := envelope["tool_choice"]
		var namedChoice string
		if json.Unmarshal(rawChoice, &namedChoice) == nil {
			switch namedChoice {
			case "none":
				choice = map[string]any{"type": "none"}
			case "auto":
				choice = map[string]any{"type": "auto"}
			case "required":
				choice = map[string]any{"type": "any"}
			default:
				return nil, nil, fmt.Errorf(
					"tool_choice must be none, auto, required, or a function tool object",
				)
			}
		} else {
			choiceObject, err := strictJSONObjectFor(
				rawChoice,
				"tool_choice",
				"Anthropic Messages",
				"type", "function",
			)
			if err != nil {
				return nil, nil, err
			}
			var choiceType string
			if err := json.Unmarshal(
				choiceObject["type"],
				&choiceType,
			); err != nil || choiceType != "function" {
				return nil, nil, unsupportedFeature(
					"only function tool_choice objects can be represented by Anthropic Messages",
				)
			}
			function, err := strictJSONObjectFor(
				choiceObject["function"],
				"tool_choice.function",
				"Anthropic Messages",
				"name",
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
					"tool_choice.function.name must be a non-empty string",
				)
			}
			choice = map[string]any{
				"type": "tool", "name": functionName,
			}
			forcedToolName = functionName
		}
	}
	if choice != nil && len(translatedTools) == 0 {
		if choice["type"] == "none" {
			choice = nil
		} else {
			return nil, nil, fmt.Errorf("tool_choice requires tools")
		}
	}
	if forcedToolName != "" {
		if _, exists := toolNames[forcedToolName]; !exists {
			return nil, nil, fmt.Errorf(
				"tool_choice references unknown function %q",
				forcedToolName,
			)
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
		if len(translatedTools) == 0 {
			return nil, nil, fmt.Errorf(
				"parallel_tool_calls requires tools",
			)
		}
		if choice == nil {
			choice = map[string]any{"type": "auto"}
		}
		choice["disable_parallel_tool_use"] = !parallel
	}
	return translatedTools, choice, nil
}

func strictJSONObjectFor(
	raw json.RawMessage,
	name string,
	target string,
	allowed ...string,
) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil ||
		object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	if err := rejectJSONFieldsFor(
		object,
		name,
		target,
		allowed...,
	); err != nil {
		return nil, err
	}
	return object, nil
}

func rejectJSONFieldsFor(
	object map[string]json.RawMessage,
	name string,
	target string,
	allowed ...string,
) error {
	set := make(map[string]struct{}, len(allowed))
	for _, field := range allowed {
		set[field] = struct{}{}
	}
	for field, raw := range object {
		if _, exists := set[field]; !exists && rawNonNull(raw) {
			return unsupportedFeature(
				"%s field %q cannot be represented by %s",
				name,
				field,
				target,
			)
		}
	}
	return nil
}

type anthropicChatResponse struct {
	ID         string                       `json:"id"`
	Type       string                       `json:"type"`
	Role       string                       `json:"role"`
	Model      string                       `json:"model"`
	Content    []map[string]json.RawMessage `json:"content"`
	StopReason string                       `json:"stop_reason"`
}

func translateAnthropicResponseToChatCompletions(
	body []byte,
	requestID string,
	model string,
	created int64,
) ([]byte, ledger.TokenUsage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil ||
		envelope == nil {
		return nil, missingUsage(), fmt.Errorf(
			"the Anthropic response must be a JSON object",
		)
	}
	if err := rejectAnthropicResponseFields(
		envelope,
		"Anthropic message",
		"id", "type", "role", "model", "content",
		"stop_reason", "stop_sequence", "usage",
	); err != nil {
		return nil, missingUsage(), err
	}
	var source anthropicChatResponse
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, missingUsage(), fmt.Errorf(
			"the Anthropic response must be a JSON object",
		)
	}
	usage, found := extractAnthropicMessagesUsage(body)
	if !found || usage.Completeness != ledger.UsageComplete {
		return nil, missingUsage(), fmt.Errorf(
			"the Anthropic response must contain complete usage",
		)
	}
	if source.ID == "" || source.Type != "message" ||
		source.Role != "assistant" || source.Model == "" {
		return nil, usage, fmt.Errorf(
			"the Anthropic response must be an assistant message",
		)
	}
	content := strings.Builder{}
	toolCalls := make([]map[string]any, 0)
	for index, block := range source.Content {
		if block == nil {
			return nil, usage, fmt.Errorf(
				"the Anthropic output content item %d must be an object",
				index,
			)
		}
		var blockType string
		if err := json.Unmarshal(block["type"], &blockType); err != nil {
			return nil, usage, fmt.Errorf(
				"the Anthropic output content item %d type is invalid",
				index,
			)
		}
		switch blockType {
		case "text":
			if err := rejectResponseFields(
				block,
				"type", "text", "citations",
			); err != nil {
				return nil, usage, err
			}
			if rawActive(block["citations"]) {
				return nil, usage, fmt.Errorf(
					"the Anthropic citations cannot be represented in Chat Completions",
				)
			}
			var text string
			if err := json.Unmarshal(block["text"], &text); err != nil {
				return nil, usage, fmt.Errorf(
					"the Anthropic output text must be a string",
				)
			}
			content.WriteString(text)
		case "tool_use":
			if err := rejectResponseFields(
				block,
				"type", "id", "name", "input",
			); err != nil {
				return nil, usage, err
			}
			var id, name string
			if json.Unmarshal(block["id"], &id) != nil || id == "" ||
				json.Unmarshal(block["name"], &name) != nil ||
				name == "" {
				return nil, usage, fmt.Errorf(
					"the Anthropic output tool_use identity is invalid",
				)
			}
			var input map[string]json.RawMessage
			if json.Unmarshal(block["input"], &input) != nil ||
				input == nil {
				return nil, usage, fmt.Errorf(
					"the Anthropic output tool_use input must be an object",
				)
			}
			arguments, err := json.Marshal(input)
			if err != nil {
				return nil, usage, err
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": id, "type": "function",
				"function": map[string]any{
					"name": name, "arguments": string(arguments),
				},
			})
		default:
			return nil, usage, fmt.Errorf(
				"the Anthropic output content type %q cannot be represented in Chat Completions",
				blockType,
			)
		}
	}
	finishReason, err := anthropicChatFinishReason(source.StopReason)
	if err != nil {
		return nil, usage, err
	}
	if finishReason == "tool_calls" && len(toolCalls) == 0 ||
		finishReason != "tool_calls" && len(toolCalls) != 0 {
		return nil, usage, fmt.Errorf(
			"the Anthropic stop reason and tool_use content are inconsistent",
		)
	}
	message := map[string]any{"role": "assistant"}
	if content.Len() == 0 {
		message["content"] = nil
	} else {
		message["content"] = content.String()
	}
	if len(toolCalls) != 0 {
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
		"usage": chatUsageFromAnthropicLedger(usage),
	}
	translated, err := json.Marshal(result)
	return translated, usage, err
}

func rejectResponseFields(
	object map[string]json.RawMessage,
	allowed ...string,
) error {
	return rejectAnthropicResponseFields(
		object,
		"Anthropic output content",
		allowed...,
	)
}

func rejectAnthropicResponseFields(
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

func anthropicChatFinishReason(reason string) (string, error) {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop", nil
	case "max_tokens", "model_context_window_exceeded":
		return "length", nil
	case "tool_use":
		return "tool_calls", nil
	case "refusal":
		return "content_filter", nil
	default:
		return "", fmt.Errorf(
			"the Anthropic stop reason %q cannot be represented in Chat Completions",
			reason,
		)
	}
}

func chatUsageFromAnthropicLedger(
	usage ledger.TokenUsage,
) map[string]any {
	promptTokens := tokenValue(usage.InputTokens) +
		tokenValue(usage.CachedInputTokens) +
		tokenValue(usage.CacheCreationTokens)
	result := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": tokenValue(usage.OutputTokens),
		"total_tokens":      tokenValue(usage.TotalTokens),
	}
	if usage.CachedInputTokens != nil {
		result["prompt_tokens_details"] = map[string]any{
			"cached_tokens": *usage.CachedInputTokens,
		}
	}
	if usage.ReasoningTokens != nil {
		result["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": *usage.ReasoningTokens,
		}
	}
	return result
}

func translateAnthropicErrorToOpenAI(
	w http.ResponseWriter,
	response *http.Response,
) {
	body, _ := readBoundedResponse(response.Body, 1<<20)
	var source anthropicErrorEnvelope
	_ = json.Unmarshal(body, &source)
	message := strings.TrimSpace(source.Error.Message)
	if message == "" {
		message = "the upstream Anthropic request failed"
	}
	writeOpenAIError(
		w,
		response.StatusCode,
		"upstream_error",
		message,
	)
}

type anthropicChatStreamState struct {
	requestID    string
	model        string
	created      int64
	includeUsage bool
	started      bool
	finished     bool
	terminal     bool
	failed       bool
	blocks       map[int]string
	stopped      map[int]bool
	toolIndexes  map[int]int
	toolJSON     map[int]string
	nextTool     int
	usage        ledger.TokenUsage
	result       proxyResult
}

func proxyAnthropicChatCompletionsStream(
	destination io.Writer,
	source io.Reader,
	requestID string,
	model string,
	includeUsage bool,
	result proxyResult,
) (proxyResult, error) {
	state := &anthropicChatStreamState{
		requestID: requestID, model: model,
		created: time.Now().Unix(), includeUsage: includeUsage,
		blocks: make(map[int]string), stopped: make(map[int]bool),
		toolIndexes: make(map[int]int), toolJSON: make(map[int]string),
		usage:  missingUsage(),
		result: result,
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
	if !state.terminal {
		return state.result, fmt.Errorf(
			"the Anthropic Messages stream ended before a terminal event",
		)
	}
	if state.failed {
		return state.result, nil
	}
	if !state.started || !state.finished {
		return state.result, fmt.Errorf(
			"the Anthropic Messages stream ended before a final message delta",
		)
	}
	if state.usage.Completeness != ledger.UsageComplete {
		return state.result, fmt.Errorf(
			"the Anthropic Messages stream ended before complete usage",
		)
	}
	state.result.usage = state.usage
	return state.result, nil
}

func (s *anthropicChatStreamState) handle(
	destination io.Writer,
	payload []byte,
) error {
	var event map[string]json.RawMessage
	if err := json.Unmarshal(payload, &event); err != nil ||
		event == nil {
		return fmt.Errorf("the Anthropic stream event must be a JSON object")
	}
	var eventType string
	if err := json.Unmarshal(event["type"], &eventType); err != nil {
		return fmt.Errorf("the Anthropic stream event type must be a string")
	}
	allowedFields := map[string][]string{
		"ping":                {"type"},
		"message_start":       {"type", "message"},
		"content_block_start": {"type", "index", "content_block"},
		"content_block_delta": {"type", "index", "delta"},
		"content_block_stop":  {"type", "index"},
		"message_delta":       {"type", "delta", "usage"},
		"message_stop":        {"type"},
		"error":               {"type", "error", "request_id"},
	}
	allowed, knownEvent := allowedFields[eventType]
	if knownEvent {
		if err := rejectAnthropicResponseFields(
			event,
			"Anthropic stream event",
			allowed...,
		); err != nil {
			return err
		}
	}
	if s.terminal {
		return fmt.Errorf(
			"the Anthropic stream event arrived after a terminal event",
		)
	}
	switch eventType {
	case "ping":
		return nil
	case "message_start":
		return s.handleMessageStart(destination, event, payload)
	case "content_block_start":
		return s.handleBlockStart(destination, event)
	case "content_block_delta":
		return s.handleBlockDelta(destination, event)
	case "content_block_stop":
		return s.handleBlockStop(destination, event)
	case "message_delta":
		return s.handleMessageDelta(destination, event, payload)
	case "message_stop":
		if !s.started || !s.finished {
			return fmt.Errorf(
				"the Anthropic message_stop order is invalid",
			)
		}
		for index := range s.blocks {
			if !s.stopped[index] {
				return fmt.Errorf(
					"the Anthropic content block %d did not stop",
					index,
				)
			}
		}
		if s.usage.Completeness != ledger.UsageComplete {
			return fmt.Errorf(
				"the Anthropic message_stop is missing complete usage",
			)
		}
		s.terminal = true
		s.result.usage = s.usage
		if s.includeUsage {
			if err := s.writeUsageChunk(destination); err != nil {
				return err
			}
		}
		return writeSSEData(destination, []byte("[DONE]"))
	case "error":
		var source anthropicError
		_ = json.Unmarshal(event["error"], &source)
		message := strings.TrimSpace(source.Message)
		if message == "" {
			message = "the upstream Anthropic stream failed"
		}
		errorPayload, _ := json.Marshal(openAIErrorEnvelope{
			Error: openAIError{
				Message: message,
				Type:    "api_error",
				Code:    "upstream_stream_error",
			},
		})
		if err := writeSSEData(destination, errorPayload); err != nil {
			return err
		}
		s.failed = true
		s.terminal = true
		s.result.outcome = ledger.OutcomeUpstreamError
		s.result.failureClass = anthropicStreamErrorClass
		if validFailureClassSuffix(source.Type) {
			s.result.failureClass += "_" + source.Type
		}
		return nil
	default:
		return fmt.Errorf(
			"the Anthropic stream event type %q cannot be represented in Chat Completions",
			eventType,
		)
	}
}

func (s *anthropicChatStreamState) handleMessageStart(
	destination io.Writer,
	event map[string]json.RawMessage,
	payload []byte,
) error {
	if s.started {
		return fmt.Errorf("the Anthropic message_start is duplicated")
	}
	var message struct {
		ID         string            `json:"id"`
		Type       string            `json:"type"`
		Role       string            `json:"role"`
		Model      string            `json:"model"`
		Content    []json.RawMessage `json:"content"`
		StopReason json.RawMessage   `json:"stop_reason"`
	}
	var rawMessage map[string]json.RawMessage
	if json.Unmarshal(event["message"], &rawMessage) != nil ||
		rawMessage == nil {
		return fmt.Errorf("the Anthropic message_start is invalid")
	}
	if err := rejectAnthropicResponseFields(
		rawMessage,
		"Anthropic message_start message",
		"id", "type", "role", "model", "content",
		"stop_reason", "stop_sequence", "usage",
	); err != nil {
		return err
	}
	if json.Unmarshal(event["message"], &message) != nil ||
		message.ID == "" || message.Type != "message" ||
		message.Role != "assistant" || message.Model == "" ||
		len(message.Content) != 0 ||
		rawNonNull(message.StopReason) {
		return fmt.Errorf("the Anthropic message_start is invalid")
	}
	usage, found := extractAnthropicMessagesUsage(payload)
	if !found {
		return fmt.Errorf("the Anthropic message_start usage is invalid")
	}
	s.usage = anthropicOperationMessages.mergeUsage(s.usage, usage)
	s.started = true
	return s.writeChunk(
		destination,
		map[string]any{"role": "assistant"},
		nil,
	)
}

func (s *anthropicChatStreamState) handleBlockStart(
	destination io.Writer,
	event map[string]json.RawMessage,
) error {
	if !s.started || s.finished {
		return fmt.Errorf("the Anthropic content_block_start order is invalid")
	}
	var index int
	if json.Unmarshal(event["index"], &index) != nil || index < 0 {
		return fmt.Errorf("the Anthropic content block index is invalid")
	}
	if _, exists := s.blocks[index]; exists {
		return fmt.Errorf("the Anthropic content block index is duplicated")
	}
	var block map[string]json.RawMessage
	if json.Unmarshal(event["content_block"], &block) != nil ||
		block == nil {
		return fmt.Errorf("the Anthropic content_block_start is invalid")
	}
	var blockType string
	if json.Unmarshal(block["type"], &blockType) != nil {
		return fmt.Errorf("the Anthropic content block type is invalid")
	}
	s.blocks[index] = blockType
	switch blockType {
	case "text":
		if err := rejectResponseFields(
			block,
			"type", "text", "citations",
		); err != nil {
			return err
		}
		if rawActive(block["citations"]) {
			return fmt.Errorf(
				"the Anthropic citations cannot be represented in Chat Completions",
			)
		}
		var text string
		if json.Unmarshal(block["text"], &text) != nil || text != "" {
			return fmt.Errorf(
				"the Anthropic streaming text block start is invalid",
			)
		}
		return nil
	case "tool_use":
		if err := rejectResponseFields(
			block,
			"type", "id", "name", "input",
		); err != nil {
			return err
		}
		var id, name string
		var input map[string]json.RawMessage
		if json.Unmarshal(block["id"], &id) != nil || id == "" ||
			json.Unmarshal(block["name"], &name) != nil || name == "" ||
			json.Unmarshal(block["input"], &input) != nil ||
			input == nil || len(input) != 0 {
			return fmt.Errorf(
				"the Anthropic streaming tool_use block start is invalid",
			)
		}
		toolIndex := s.nextTool
		s.nextTool++
		s.toolIndexes[index] = toolIndex
		s.toolJSON[index] = ""
		return s.writeChunk(destination, map[string]any{
			"tool_calls": []any{map[string]any{
				"index": toolIndex, "id": id, "type": "function",
				"function": map[string]any{
					"name": name, "arguments": "",
				},
			}},
		}, nil)
	default:
		return fmt.Errorf(
			"the Anthropic content block type %q cannot be represented in Chat Completions",
			blockType,
		)
	}
}

func (s *anthropicChatStreamState) handleBlockDelta(
	destination io.Writer,
	event map[string]json.RawMessage,
) error {
	if !s.started || s.finished {
		return fmt.Errorf("the Anthropic content_block_delta order is invalid")
	}
	var index int
	if json.Unmarshal(event["index"], &index) != nil || index < 0 ||
		s.stopped[index] {
		return fmt.Errorf("the Anthropic content block delta index is invalid")
	}
	blockType, exists := s.blocks[index]
	if !exists {
		return fmt.Errorf(
			"the Anthropic content block delta has no matching start",
		)
	}
	var delta map[string]json.RawMessage
	if json.Unmarshal(event["delta"], &delta) != nil || delta == nil {
		return fmt.Errorf("the Anthropic content block delta is invalid")
	}
	var deltaType string
	if json.Unmarshal(delta["type"], &deltaType) != nil {
		return fmt.Errorf("the Anthropic content block delta type is invalid")
	}
	switch {
	case blockType == "text" && deltaType == "text_delta":
		if err := rejectResponseFields(delta, "type", "text"); err != nil {
			return err
		}
		var text string
		if json.Unmarshal(delta["text"], &text) != nil {
			return fmt.Errorf("the Anthropic text delta is invalid")
		}
		return s.writeChunk(
			destination,
			map[string]any{"content": text},
			nil,
		)
	case blockType == "tool_use" && deltaType == "input_json_delta":
		if err := rejectResponseFields(
			delta,
			"type", "partial_json",
		); err != nil {
			return err
		}
		var partial string
		if json.Unmarshal(delta["partial_json"], &partial) != nil {
			return fmt.Errorf("the Anthropic input JSON delta is invalid")
		}
		toolIndex, exists := s.toolIndexes[index]
		if !exists {
			return fmt.Errorf(
				"the Anthropic tool delta has no matching start",
			)
		}
		s.toolJSON[index] += partial
		return s.writeChunk(destination, map[string]any{
			"tool_calls": []any{map[string]any{
				"index": toolIndex,
				"function": map[string]any{
					"arguments": partial,
				},
			}},
		}, nil)
	default:
		return fmt.Errorf(
			"the Anthropic content block delta %q cannot update %q in Chat Completions",
			deltaType,
			blockType,
		)
	}
}

func (s *anthropicChatStreamState) handleBlockStop(
	destination io.Writer,
	event map[string]json.RawMessage,
) error {
	if !s.started || s.finished {
		return fmt.Errorf("the Anthropic content_block_stop order is invalid")
	}
	var index int
	if json.Unmarshal(event["index"], &index) != nil || index < 0 {
		return fmt.Errorf("the Anthropic content block stop index is invalid")
	}
	if _, exists := s.blocks[index]; !exists || s.stopped[index] {
		return fmt.Errorf(
			"the Anthropic content block stop has no active block",
		)
	}
	if s.blocks[index] == "tool_use" {
		arguments := s.toolJSON[index]
		if arguments == "" {
			arguments = "{}"
			toolIndex := s.toolIndexes[index]
			if err := s.writeChunk(destination, map[string]any{
				"tool_calls": []any{map[string]any{
					"index": toolIndex,
					"function": map[string]any{
						"arguments": arguments,
					},
				}},
			}, nil); err != nil {
				return err
			}
		}
		var object map[string]json.RawMessage
		if json.Unmarshal([]byte(arguments), &object) != nil ||
			object == nil {
			return fmt.Errorf(
				"the Anthropic streamed tool input must encode a JSON object",
			)
		}
	}
	s.stopped[index] = true
	return nil
}

func (s *anthropicChatStreamState) handleMessageDelta(
	destination io.Writer,
	event map[string]json.RawMessage,
	payload []byte,
) error {
	if !s.started || s.finished {
		return fmt.Errorf("the Anthropic message_delta order is invalid")
	}
	for index := range s.blocks {
		if !s.stopped[index] {
			return fmt.Errorf(
				"the Anthropic message_delta arrived before content blocks stopped",
			)
		}
	}
	usage, found := extractAnthropicMessagesUsage(payload)
	if !found {
		return fmt.Errorf("the Anthropic message_delta usage is invalid")
	}
	s.usage = anthropicOperationMessages.mergeUsage(s.usage, usage)
	var delta map[string]json.RawMessage
	if json.Unmarshal(event["delta"], &delta) != nil || delta == nil {
		return fmt.Errorf("the Anthropic message_delta is invalid")
	}
	if err := rejectResponseFields(
		delta,
		"stop_reason", "stop_sequence",
	); err != nil {
		return err
	}
	if !rawNonNull(delta["stop_reason"]) {
		return nil
	}
	var stopReason string
	if json.Unmarshal(delta["stop_reason"], &stopReason) != nil ||
		stopReason == "" {
		return fmt.Errorf(
			"the Anthropic final message_delta stop_reason is invalid",
		)
	}
	finishReason, err := anthropicChatFinishReason(stopReason)
	if err != nil {
		return err
	}
	if finishReason == "tool_calls" && s.nextTool == 0 ||
		finishReason != "tool_calls" && s.nextTool != 0 {
		return fmt.Errorf(
			"the Anthropic stop reason and streamed tool_use content are inconsistent",
		)
	}
	s.finished = true
	return s.writeChunk(destination, map[string]any{}, &finishReason)
}

func (s *anthropicChatStreamState) writeChunk(
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

func (s *anthropicChatStreamState) writeUsageChunk(
	destination io.Writer,
) error {
	chunk := map[string]any{
		"id":      "chatcmpl-" + s.requestID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   chatUsageFromAnthropicLedger(s.usage),
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return writeSSEData(destination, payload)
}
