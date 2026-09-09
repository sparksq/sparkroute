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
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
)

const chatBedrockStreamFailureClass = "bedrock_chat_translation_stream"

type translatedBedrockMessage struct {
	Role    string           `json:"role"`
	Content []map[string]any `json:"content"`
}

func translateChatCompletionsRequestToBedrockLegacy(
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
				"Chat Completions field %q cannot be represented by Bedrock Converse",
				field,
			)
		}
	}
	if err := validateChatBedrockNeutralFields(envelope); err != nil {
		return nil, err
	}
	system, messages, err := translateChatBedrockMessages(
		envelope["messages"],
	)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	result["messages"] = messages
	if len(system) != 0 {
		result["system"] = system
	}
	inferenceConfig, err := translateChatBedrockInferenceConfig(
		envelope,
	)
	if err != nil {
		return nil, err
	}
	if len(inferenceConfig) != 0 {
		result["inferenceConfig"] = inferenceConfig
	}
	toolConfig, err := translateChatBedrockToolConfig(envelope)
	if err != nil {
		return nil, err
	}
	if toolConfig != nil {
		result["toolConfig"] = toolConfig
	}
	return json.Marshal(result)
}

func validateChatBedrockNeutralFields(
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
		if err := json.Unmarshal(
			envelope[field],
			&value,
		); err != nil {
			return fmt.Errorf("%s must be a number", field)
		}
		if value != 0 {
			return unsupportedFeature(
				"Chat Completions %s cannot be represented by Bedrock Converse unless it is zero",
				field,
			)
		}
	}
	for _, field := range []string{"logprobs", "store"} {
		if !rawNonNull(envelope[field]) {
			continue
		}
		var value bool
		if err := json.Unmarshal(
			envelope[field],
			&value,
		); err != nil {
			return fmt.Errorf("%s must be a boolean", field)
		}
		if value {
			return unsupportedFeature(
				"Chat Completions %s=true cannot be represented by Bedrock Converse",
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
				"Chat Completions n values other than 1 cannot be represented by Bedrock Converse",
			)
		}
	}
	if rawNonNull(envelope["response_format"]) {
		format, err := strictJSONObject(
			envelope["response_format"],
			"response_format",
			"type",
		)
		if err != nil {
			return err
		}
		var formatType string
		if err := json.Unmarshal(format["type"], &formatType); err != nil ||
			formatType != "text" {
			return unsupportedFeature(
				"Chat Completions response_format cannot be represented by Bedrock Converse unless its type is text",
			)
		}
	}
	if rawNonNull(envelope["modalities"]) {
		var modalities []string
		if err := json.Unmarshal(
			envelope["modalities"],
			&modalities,
		); err != nil || len(modalities) != 1 ||
			modalities[0] != "text" {
			return unsupportedFeature(
				"Chat Completions modalities cannot be represented by Bedrock Converse unless they contain only text",
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
		options, err := strictJSONObject(
			envelope["stream_options"],
			"stream_options",
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

func translateChatBedrockInferenceConfig(
	envelope map[string]json.RawMessage,
) (map[string]any, error) {
	result := make(map[string]any)
	maxCompletion := envelope["max_completion_tokens"]
	maxTokens := envelope["max_tokens"]
	if rawNonNull(maxCompletion) && rawNonNull(maxTokens) {
		return nil, fmt.Errorf(
			"max_tokens and max_completion_tokens cannot both be set",
		)
	}
	rawMax := maxCompletion
	if !rawNonNull(rawMax) {
		rawMax = maxTokens
	}
	if rawNonNull(rawMax) {
		var value int
		if err := json.Unmarshal(rawMax, &value); err != nil ||
			value < 1 {
			return nil, fmt.Errorf(
				"maximum completion tokens must be a positive integer",
			)
		}
		result["maxTokens"] = value
	}
	for _, field := range []struct {
		chat    string
		bedrock string
		min     float64
		max     float64
	}{
		{
			chat: "temperature", bedrock: "temperature",
			min: 0, max: 1,
		},
		{chat: "top_p", bedrock: "topP", min: 0, max: 1},
	} {
		raw := envelope[field.chat]
		if !rawNonNull(raw) {
			continue
		}
		var value float64
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, fmt.Errorf("%s must be a number", field.chat)
		}
		if value < field.min || value > field.max {
			return nil, unsupportedFeature(
				"Chat Completions %s=%v is outside the Bedrock Converse range [%v,%v]",
				field.chat,
				value,
				field.min,
				field.max,
			)
		}
		result[field.bedrock] = value
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

func chatStopSequences(raw json.RawMessage) ([]string, error) {
	var single string
	if json.Unmarshal(raw, &single) == nil {
		if single == "" {
			return nil, fmt.Errorf("stop must not contain an empty string")
		}
		return []string{single}, nil
	}
	var result []string
	if err := json.Unmarshal(raw, &result); err != nil ||
		len(result) == 0 || len(result) > 4 {
		return nil, fmt.Errorf(
			"stop must be a string or an array of one to four strings",
		)
	}
	for _, stop := range result {
		if stop == "" {
			return nil, fmt.Errorf(
				"stop must not contain an empty string",
			)
		}
	}
	return result, nil
}

func translateChatBedrockMessages(
	raw json.RawMessage,
) ([]map[string]any, []translatedBedrockMessage, error) {
	var source []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &source); err != nil ||
		len(source) == 0 {
		return nil, nil, fmt.Errorf(
			"messages must be a non-empty array of objects",
		)
	}
	system := make([]map[string]any, 0)
	messages := make([]translatedBedrockMessage, 0, len(source))
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
					"system messages after conversation messages cannot be represented by Bedrock Converse",
				)
			}
			if err := rejectJSONFields(
				message,
				fmt.Sprintf("messages item %d", index),
				"role", "content",
			); err != nil {
				return nil, nil, err
			}
			blocks, err := translateChatTextContent(
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
				"developer messages cannot be represented distinctly by Bedrock Converse",
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
			blocks, err := translateChatConversationMessage(
				message,
				role,
				index,
			)
			if err != nil {
				return nil, nil, err
			}
			messages = append(messages, translatedBedrockMessage{
				Role: role, Content: blocks,
			})
		case "tool":
			sawConversation = true
			block, err := translateChatToolResult(message, index)
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
				messages = append(messages, translatedBedrockMessage{
					Role:    "user",
					Content: []map[string]any{block},
				})
			}
		default:
			return nil, nil, unsupportedFeature(
				"messages item %d role %q cannot be represented by Bedrock Converse",
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

func translateChatConversationMessage(
	message map[string]json.RawMessage,
	role string,
	index int,
) ([]map[string]any, error) {
	name := fmt.Sprintf("messages item %d", index)
	allowed := []string{"role", "content"}
	if role == "assistant" {
		allowed = append(allowed, "tool_calls")
	}
	if err := rejectJSONFields(message, name, allowed...); err != nil {
		return nil, err
	}
	blocks, err := translateChatContent(
		message["content"],
		name+" content",
		role,
	)
	if err != nil {
		return nil, err
	}
	if role == "assistant" && rawNonNull(message["tool_calls"]) {
		toolBlocks, err := translateChatAssistantToolCalls(
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

func translateChatContent(
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
			if err := rejectJSONFields(
				part,
				partName,
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
			block, err := translateChatImagePart(part, partName)
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		case "file":
			if role != "user" {
				return nil, unsupportedFeature(
					"%s document content is only representable for user messages",
					partName,
				)
			}
			block, err := translateChatFilePart(part, partName)
			if err != nil {
				return nil, err
			}
			result = append(result, block)
		default:
			return nil, unsupportedFeature(
				"%s type %q cannot be represented by Bedrock Converse",
				partName,
				partType,
			)
		}
	}
	return result, nil
}

func translateChatTextContent(
	raw json.RawMessage,
	name string,
	allowNull bool,
) ([]map[string]any, error) {
	blocks, err := translateChatContent(raw, name, "system")
	if allowNull && err != nil && !rawNonNull(raw) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, block := range blocks {
		if _, text := block["text"]; !text {
			return nil, unsupportedFeature(
				"%s supports only text when translated to Bedrock system content",
				name,
			)
		}
	}
	return blocks, nil
}

func translateChatImagePart(
	part map[string]json.RawMessage,
	name string,
) (map[string]any, error) {
	if err := rejectJSONFields(
		part,
		name,
		"type", "image_url",
	); err != nil {
		return nil, err
	}
	image, err := strictJSONObject(
		part["image_url"],
		name+".image_url",
		"url", "detail",
	)
	if err != nil {
		return nil, err
	}
	if rawNonNull(image["detail"]) {
		return nil, unsupportedFeature(
			"%s detail cannot be represented by Bedrock Converse",
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
	if len(data) == 0 {
		return nil, fmt.Errorf("%s image data must not be empty", name)
	}
	format, exists := map[string]string{
		"image/png":  "png",
		"image/jpeg": "jpeg",
		"image/jpg":  "jpeg",
		"image/gif":  "gif",
		"image/webp": "webp",
	}[strings.ToLower(mediaType)]
	if !exists {
		return nil, unsupportedFeature(
			"%s media type %q is not supported by Bedrock Converse",
			name,
			mediaType,
		)
	}
	return map[string]any{
		"image": map[string]any{
			"format": format,
			"source": map[string]any{"bytes": data},
		},
	}, nil
}

func translateChatFilePart(
	part map[string]json.RawMessage,
	name string,
) (map[string]any, error) {
	if err := rejectJSONFields(
		part,
		name,
		"type", "file",
	); err != nil {
		return nil, err
	}
	file, err := strictJSONObject(
		part["file"],
		name+".file",
		"filename", "file_data", "file_id",
	)
	if err != nil {
		return nil, err
	}
	if rawNonNull(file["file_id"]) {
		return nil, unsupportedFeature(
			"%s provider-owned file_id cannot be represented by Bedrock Converse",
			name,
		)
	}
	var filename, encoded string
	if err := json.Unmarshal(file["filename"], &filename); err != nil ||
		filename == "" {
		return nil, fmt.Errorf(
			"%s file.filename must be a non-empty string",
			name,
		)
	}
	if err := json.Unmarshal(file["file_data"], &encoded); err != nil ||
		encoded == "" {
		return nil, fmt.Errorf(
			"%s file.file_data must be a non-empty base64 string",
			name,
		)
	}
	mediaType := ""
	var data []byte
	if strings.HasPrefix(encoded, "data:") {
		mediaType, data, err = decodeBase64DataURL(encoded)
	} else {
		data, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil {
		return nil, fmt.Errorf(
			"%s file.file_data must contain valid base64 data",
			name,
		)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf(
			"%s file.file_data must not be empty",
			name,
		)
	}
	format, documentName, err := bedrockDocumentIdentity(
		filename,
		mediaType,
	)
	if err != nil {
		return nil, unsupportedFeature("%s: %v", name, err)
	}
	return map[string]any{
		"document": map[string]any{
			"format": format,
			"name":   documentName,
			"source": map[string]any{"bytes": data},
		},
	}, nil
}

func decodeBase64DataURL(value string) (string, []byte, error) {
	metadata, payload, exists := strings.Cut(value, ",")
	if !exists || !strings.HasPrefix(metadata, "data:") ||
		!strings.HasSuffix(
			strings.ToLower(metadata),
			";base64",
		) {
		return "", nil, fmt.Errorf("not a base64 data URL")
	}
	rawMediaType := metadata[len("data:") : len(metadata)-len(";base64")]
	mediaType, parameters, err := mime.ParseMediaType(rawMediaType)
	if err != nil || mediaType == "" || len(parameters) != 0 {
		return "", nil, fmt.Errorf("invalid data URL media type")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", nil, fmt.Errorf("invalid base64 payload")
	}
	return strings.ToLower(mediaType), data, nil
}

func bedrockDocumentIdentity(
	filename string,
	mediaType string,
) (string, string, error) {
	base := path.Base(strings.ReplaceAll(filename, "\\", "/"))
	extension := strings.ToLower(strings.TrimPrefix(
		path.Ext(base),
		".",
	))
	formatByExtension := map[string]string{
		"pdf": "pdf", "csv": "csv", "doc": "doc", "docx": "docx",
		"xls": "xls", "xlsx": "xlsx", "html": "html", "htm": "html",
		"txt": "txt", "md": "md",
	}
	format := formatByExtension[extension]
	formatByMediaType := map[string]string{
		"application/pdf":    "pdf",
		"text/csv":           "csv",
		"application/msword": "doc",
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document": "docx",
		"application/vnd.ms-excel": "xls",
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet": "xlsx",
		"text/html":     "html",
		"text/plain":    "txt",
		"text/markdown": "md",
	}
	if mediaType != "" {
		mediaFormat := formatByMediaType[strings.ToLower(mediaType)]
		if mediaFormat == "" {
			return "", "", fmt.Errorf(
				"document media type %q is not supported by Bedrock Converse",
				mediaType,
			)
		}
		if format != "" && format != mediaFormat {
			return "", "", fmt.Errorf(
				"document filename extension and media type disagree",
			)
		}
		format = mediaFormat
	}
	if format == "" {
		return "", "", fmt.Errorf(
			"document filename must have a supported extension",
		)
	}
	documentName := strings.TrimSuffix(base, path.Ext(base))
	if !validBedrockDocumentName(documentName) {
		return "", "", fmt.Errorf(
			"document filename cannot be represented as a Bedrock document name",
		)
	}
	return format, documentName, nil
}

func validBedrockDocumentName(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' ||
			char == ' ' || char == '-' || char == '_' ||
			char == '(' || char == ')' ||
			char == '[' || char == ']' {
			continue
		}
		return false
	}
	return true
}

func translateChatAssistantToolCalls(
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
		if err := rejectJSONFields(
			call,
			callName,
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
		function, err := strictJSONObject(
			call["function"],
			callName+".function",
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
			"toolUse": map[string]any{
				"toolUseId": id,
				"name":      functionName,
				"input":     input,
			},
		})
	}
	return result, nil
}

func translateChatToolResult(
	message map[string]json.RawMessage,
	index int,
) (map[string]any, error) {
	name := fmt.Sprintf("messages item %d", index)
	if err := rejectJSONFields(
		message,
		name,
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
	content, err := translateChatTextContent(
		message["content"],
		name+" content",
		false,
	)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"toolResult": map[string]any{
			"toolUseId": toolUseID,
			"content":   content,
		},
	}, nil
}

func translateChatBedrockToolConfig(
	envelope map[string]json.RawMessage,
) (map[string]any, error) {
	var translatedTools []map[string]any
	toolNames := make(map[string]struct{})
	if rawNonNull(envelope["tools"]) {
		var tools []map[string]json.RawMessage
		if err := json.Unmarshal(envelope["tools"], &tools); err != nil ||
			len(tools) == 0 {
			return nil, fmt.Errorf(
				"tools must be a non-empty array of objects",
			)
		}
		translatedTools = make([]map[string]any, 0, len(tools))
		for index, tool := range tools {
			name := fmt.Sprintf("tools item %d", index)
			if err := rejectJSONFields(
				tool,
				name,
				"type", "function",
			); err != nil {
				return nil, err
			}
			var toolType string
			if err := json.Unmarshal(
				tool["type"],
				&toolType,
			); err != nil || toolType != "function" {
				return nil, unsupportedFeature(
					"%s must be a function tool",
					name,
				)
			}
			function, err := strictJSONObject(
				tool["function"],
				name+".function",
				"name", "description", "parameters", "strict",
			)
			if err != nil {
				return nil, err
			}
			if rawNonNull(function["strict"]) {
				var strict bool
				if err := json.Unmarshal(
					function["strict"],
					&strict,
				); err != nil {
					return nil, fmt.Errorf(
						"%s function.strict must be a boolean",
						name,
					)
				}
				if strict {
					return nil, unsupportedFeature(
						"%s function.strict=true cannot be represented by Bedrock Converse",
						name,
					)
				}
			}
			var functionName string
			if err := json.Unmarshal(
				function["name"],
				&functionName,
			); err != nil || functionName == "" {
				return nil, fmt.Errorf(
					"%s function.name must be a non-empty string",
					name,
				)
			}
			if _, exists := toolNames[functionName]; exists {
				return nil, fmt.Errorf(
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
					return nil, fmt.Errorf(
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
					return nil, fmt.Errorf(
						"%s function.parameters must be a JSON object",
						name,
					)
				}
				schema = json.RawMessage(append(
					[]byte(nil),
					function["parameters"]...,
				))
			}
			spec["inputSchema"] = map[string]any{"json": schema}
			translatedTools = append(
				translatedTools,
				map[string]any{"toolSpec": spec},
			)
		}
	}
	if rawNonNull(envelope["parallel_tool_calls"]) {
		var parallel bool
		if err := json.Unmarshal(
			envelope["parallel_tool_calls"],
			&parallel,
		); err != nil {
			return nil, fmt.Errorf(
				"parallel_tool_calls must be a boolean",
			)
		}
		if !parallel {
			return nil, unsupportedFeature(
				"parallel_tool_calls=false cannot be represented by Bedrock Converse",
			)
		}
		if len(translatedTools) == 0 {
			return nil, fmt.Errorf(
				"parallel_tool_calls requires tools",
			)
		}
	}
	var toolChoice map[string]any
	forcedToolName := ""
	if rawNonNull(envelope["tool_choice"]) {
		rawChoice := envelope["tool_choice"]
		var choice string
		if json.Unmarshal(rawChoice, &choice) == nil {
			switch choice {
			case "none":
				if len(translatedTools) != 0 {
					return nil, unsupportedFeature(
						"tool_choice=none with tools cannot be represented by Bedrock Converse",
					)
				}
			case "auto":
				toolChoice = map[string]any{"auto": map[string]any{}}
			case "required":
				toolChoice = map[string]any{"any": map[string]any{}}
			default:
				return nil, fmt.Errorf(
					"tool_choice must be none, auto, required, or a function tool object",
				)
			}
		} else {
			choiceObject, err := strictJSONObject(
				rawChoice,
				"tool_choice",
				"type", "function",
			)
			if err != nil {
				return nil, err
			}
			var choiceType string
			if err := json.Unmarshal(
				choiceObject["type"],
				&choiceType,
			); err != nil || choiceType != "function" {
				return nil, unsupportedFeature(
					"only function tool_choice objects can be represented by Bedrock Converse",
				)
			}
			function, err := strictJSONObject(
				choiceObject["function"],
				"tool_choice.function",
				"name",
			)
			if err != nil {
				return nil, err
			}
			var functionName string
			if err := json.Unmarshal(
				function["name"],
				&functionName,
			); err != nil || functionName == "" {
				return nil, fmt.Errorf(
					"tool_choice.function.name must be a non-empty string",
				)
			}
			toolChoice = map[string]any{
				"tool": map[string]any{"name": functionName},
			}
			forcedToolName = functionName
		}
	}
	if toolChoice != nil && len(translatedTools) == 0 {
		return nil, fmt.Errorf("tool_choice requires tools")
	}
	if len(translatedTools) == 0 {
		return nil, nil
	}
	if forcedToolName != "" {
		if _, exists := toolNames[forcedToolName]; !exists {
			return nil, fmt.Errorf(
				"tool_choice references unknown function %q",
				forcedToolName,
			)
		}
	}
	result := map[string]any{"tools": translatedTools}
	if toolChoice != nil {
		result["toolChoice"] = toolChoice
	}
	return result, nil
}

func strictJSONObject(
	raw json.RawMessage,
	name string,
	allowed ...string,
) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil ||
		object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	if err := rejectJSONFields(object, name, allowed...); err != nil {
		return nil, err
	}
	return object, nil
}

func rejectJSONFields(
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
			return unsupportedFeature(
				"%s field %q cannot be represented by Bedrock Converse",
				name,
				field,
			)
		}
	}
	return nil
}

type bedrockChatResponse struct {
	Output struct {
		Message struct {
			Role    string                       `json:"role"`
			Content []map[string]json.RawMessage `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string          `json:"stopReason"`
	Usage      json.RawMessage `json:"usage"`
}

func translateBedrockResponseToChatCompletionsLegacy(
	body []byte,
	requestID string,
	model string,
	created int64,
) ([]byte, ledger.TokenUsage, error) {
	var source bedrockChatResponse
	if err := json.Unmarshal(body, &source); err != nil {
		return nil, missingUsage(), fmt.Errorf(
			"the Bedrock response must be a JSON object",
		)
	}
	usage, found := extractBedrockUsage(body)
	if !found {
		return nil, missingUsage(), fmt.Errorf(
			"the Bedrock response must contain valid usage",
		)
	}
	if source.Output.Message.Role != "assistant" {
		return nil, usage, fmt.Errorf(
			"the Bedrock output message role must be assistant",
		)
	}
	content := strings.Builder{}
	toolCalls := make([]map[string]any, 0)
	for index, block := range source.Output.Message.Content {
		if block == nil {
			return nil, usage, fmt.Errorf(
				"the Bedrock output content item %d must be an object",
				index,
			)
		}
		active := 0
		for field, raw := range block {
			if !rawNonNull(raw) {
				continue
			}
			active++
			switch field {
			case "text":
				var text string
				if err := json.Unmarshal(raw, &text); err != nil {
					return nil, usage, fmt.Errorf(
						"the Bedrock output text must be a string",
					)
				}
				content.WriteString(text)
			case "toolUse":
				var toolUse struct {
					ToolUseID string          `json:"toolUseId"`
					Name      string          `json:"name"`
					Input     json.RawMessage `json:"input"`
				}
				if err := json.Unmarshal(raw, &toolUse); err != nil ||
					toolUse.ToolUseID == "" ||
					toolUse.Name == "" ||
					!json.Valid(toolUse.Input) {
					return nil, usage, fmt.Errorf(
						"the Bedrock output toolUse is invalid",
					)
				}
				var input map[string]json.RawMessage
				if err := json.Unmarshal(
					toolUse.Input,
					&input,
				); err != nil || input == nil {
					return nil, usage, fmt.Errorf(
						"the Bedrock output toolUse input must be an object",
					)
				}
				toolCalls = append(toolCalls, map[string]any{
					"id":   toolUse.ToolUseID,
					"type": "function",
					"function": map[string]any{
						"name":      toolUse.Name,
						"arguments": string(toolUse.Input),
					},
				})
			default:
				return nil, usage, fmt.Errorf(
					"the Bedrock output content field %q cannot be represented in Chat Completions",
					field,
				)
			}
		}
		if active != 1 {
			return nil, usage, fmt.Errorf(
				"the Bedrock output content must contain one union member",
			)
		}
	}
	finishReason, err := bedrockChatFinishReason(source.StopReason)
	if err != nil {
		return nil, usage, err
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
		"usage": chatUsageFromLedger(usage),
	}
	translated, err := json.Marshal(result)
	return translated, usage, err
}

func bedrockChatFinishReason(reason string) (string, error) {
	switch reason {
	case "end_turn", "stop_sequence":
		return "stop", nil
	case "max_tokens", "model_context_window_exceeded":
		return "length", nil
	case "tool_use":
		return "tool_calls", nil
	case "content_filtered", "guardrail_intervened":
		return "content_filter", nil
	default:
		return "", fmt.Errorf(
			"the Bedrock stop reason %q cannot be represented in Chat Completions",
			reason,
		)
	}
}

func chatUsageFromLedger(usage ledger.TokenUsage) map[string]any {
	result := map[string]any{
		"prompt_tokens":     tokenValue(usage.InputTokens),
		"completion_tokens": tokenValue(usage.OutputTokens),
		"total_tokens":      tokenValue(usage.TotalTokens),
	}
	if usage.CachedInputTokens != nil {
		result["prompt_tokens_details"] = map[string]any{
			"cached_tokens": *usage.CachedInputTokens,
		}
	}
	return result
}

func tokenValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func translateBedrockErrorToOpenAI(
	w http.ResponseWriter,
	response *http.Response,
) {
	body, _ := readBoundedResponse(response.Body, 1<<20)
	var source struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &source)
	if strings.TrimSpace(source.Message) == "" {
		source.Message = "the upstream Bedrock request failed"
	}
	writeOpenAIError(
		w,
		response.StatusCode,
		"upstream_error",
		source.Message,
	)
}

type bedrockChatStreamState struct {
	requestID    string
	model        string
	created      int64
	includeUsage bool
	started      bool
	stopped      bool
	sawUsage     bool
	toolIndexes  map[int]int
	nextTool     int
}

func proxyBedrockChatCompletionsStream(
	destination io.Writer,
	source io.Reader,
	requestID string,
	model string,
	includeUsage bool,
	result proxyResult,
) (proxyResult, error) {
	state := bedrockChatStreamState{
		requestID:    requestID,
		model:        model,
		created:      time.Now().Unix(),
		includeUsage: includeUsage,
		toolIndexes:  make(map[int]int),
	}
	for {
		message, err := readAWSEventMessage(source)
		if err == io.EOF {
			if !state.stopped {
				return result, fmt.Errorf(
					"the Bedrock ConverseStream ended before messageStop",
				)
			}
			if state.includeUsage && !state.sawUsage {
				return result, fmt.Errorf(
					"the Bedrock ConverseStream ended before required usage metadata",
				)
			}
			if err := writeSSEData(destination, []byte("[DONE]")); err != nil {
				return result, err
			}
			return result, nil
		}
		if err != nil {
			return result, err
		}
		if message.header(":message-type") == "exception" {
			var sourceError struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(message.payload, &sourceError)
			if sourceError.Message == "" {
				sourceError.Message = "the upstream Bedrock stream failed"
			}
			payload, _ := json.Marshal(openAIErrorEnvelope{
				Error: openAIError{
					Message: sourceError.Message,
					Type:    "api_error",
					Code:    "upstream_stream_error",
				},
			})
			if err := writeSSEData(destination, payload); err != nil {
				return result, err
			}
			_, outcome, failureClass :=
				inspectBedrockStreamMessage(message)
			result.outcome = outcome
			result.failureClass = failureClass
			return result, nil
		}
		if messageType := message.header(":message-type"); messageType != "" && messageType != "event" {
			return result, fmt.Errorf(
				"the Bedrock event-stream message type is invalid",
			)
		}
		if !json.Valid(message.payload) {
			return result, fmt.Errorf(
				"the Bedrock event-stream payload is invalid",
			)
		}
		eventType := message.header(":event-type")
		switch eventType {
		case "messageStart":
			if state.started || state.stopped {
				return result, fmt.Errorf(
					"the Bedrock messageStart order is invalid",
				)
			}
			var event struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(message.payload, &event) != nil ||
				event.Role != "assistant" {
				return result, fmt.Errorf(
					"the Bedrock messageStart role is invalid",
				)
			}
			state.started = true
			if err := state.writeChunk(destination, map[string]any{
				"role": "assistant",
			}, nil); err != nil {
				return result, err
			}
		case "contentBlockStart":
			if !state.started || state.stopped {
				return result, fmt.Errorf(
					"the Bedrock contentBlockStart order is invalid",
				)
			}
			var event struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Start             struct {
					ToolUse *struct {
						ToolUseID string `json:"toolUseId"`
						Name      string `json:"name"`
					} `json:"toolUse"`
				} `json:"start"`
			}
			if json.Unmarshal(message.payload, &event) != nil ||
				event.ContentBlockIndex < 0 ||
				event.Start.ToolUse == nil ||
				event.Start.ToolUse.ToolUseID == "" ||
				event.Start.ToolUse.Name == "" {
				return result, fmt.Errorf(
					"the Bedrock contentBlockStart is invalid or unrepresentable",
				)
			}
			if _, exists := state.toolIndexes[event.ContentBlockIndex]; exists {
				return result, fmt.Errorf(
					"the Bedrock tool content block index is duplicated",
				)
			}
			toolIndex := state.nextTool
			state.nextTool++
			state.toolIndexes[event.ContentBlockIndex] = toolIndex
			if err := state.writeChunk(destination, map[string]any{
				"tool_calls": []any{map[string]any{
					"index": toolIndex,
					"id":    event.Start.ToolUse.ToolUseID,
					"type":  "function",
					"function": map[string]any{
						"name":      event.Start.ToolUse.Name,
						"arguments": "",
					},
				}},
			}, nil); err != nil {
				return result, err
			}
		case "contentBlockDelta":
			if !state.started || state.stopped {
				return result, fmt.Errorf(
					"the Bedrock contentBlockDelta order is invalid",
				)
			}
			var event struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
				Delta             struct {
					Text    *string `json:"text"`
					ToolUse *struct {
						Input string `json:"input"`
					} `json:"toolUse"`
				} `json:"delta"`
			}
			if json.Unmarshal(message.payload, &event) != nil ||
				event.ContentBlockIndex < 0 {
				return result, fmt.Errorf(
					"the Bedrock contentBlockDelta is invalid",
				)
			}
			if event.Delta.Text != nil &&
				event.Delta.ToolUse == nil {
				if err := state.writeChunk(
					destination,
					map[string]any{"content": *event.Delta.Text},
					nil,
				); err != nil {
					return result, err
				}
				break
			}
			if event.Delta.ToolUse != nil &&
				event.Delta.Text == nil {
				toolIndex, exists :=
					state.toolIndexes[event.ContentBlockIndex]
				if !exists {
					return result, fmt.Errorf(
						"the Bedrock tool delta has no matching start",
					)
				}
				if err := state.writeChunk(destination, map[string]any{
					"tool_calls": []any{map[string]any{
						"index": toolIndex,
						"function": map[string]any{
							"arguments": event.Delta.ToolUse.Input,
						},
					}},
				}, nil); err != nil {
					return result, err
				}
				break
			}
			return result, fmt.Errorf(
				"the Bedrock contentBlockDelta is unrepresentable",
			)
		case "contentBlockStop":
			if !state.started || state.stopped {
				return result, fmt.Errorf(
					"the Bedrock contentBlockStop order is invalid",
				)
			}
			var event struct {
				ContentBlockIndex int `json:"contentBlockIndex"`
			}
			if json.Unmarshal(message.payload, &event) != nil ||
				event.ContentBlockIndex < 0 {
				return result, fmt.Errorf(
					"the Bedrock contentBlockStop is invalid",
				)
			}
		case "messageStop":
			if !state.started || state.stopped {
				return result, fmt.Errorf(
					"the Bedrock messageStop order is invalid",
				)
			}
			var event struct {
				StopReason string `json:"stopReason"`
			}
			if json.Unmarshal(message.payload, &event) != nil {
				return result, fmt.Errorf(
					"the Bedrock messageStop is invalid",
				)
			}
			finishReason, err := bedrockChatFinishReason(
				event.StopReason,
			)
			if err != nil {
				return result, err
			}
			state.stopped = true
			if err := state.writeChunk(
				destination,
				map[string]any{},
				&finishReason,
			); err != nil {
				return result, err
			}
		case "metadata":
			if !state.stopped {
				return result, fmt.Errorf(
					"the Bedrock metadata arrived before messageStop",
				)
			}
			if state.sawUsage {
				return result, fmt.Errorf(
					"the Bedrock metadata event is duplicated",
				)
			}
			usage, found := extractBedrockUsage(message.payload)
			if !found {
				return result, fmt.Errorf(
					"the Bedrock metadata usage is invalid",
				)
			}
			state.sawUsage = true
			result.usage = usage
			if state.includeUsage {
				if err := state.writeUsageChunk(
					destination,
					usage,
				); err != nil {
					return result, err
				}
			}
		default:
			return result, fmt.Errorf(
				"the Bedrock event type %q cannot be represented in Chat Completions",
				eventType,
			)
		}
	}
}

func (s *bedrockChatStreamState) writeChunk(
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

func (s *bedrockChatStreamState) writeUsageChunk(
	destination io.Writer,
	usage ledger.TokenUsage,
) error {
	chunk := map[string]any{
		"id":      "chatcmpl-" + s.requestID,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{},
		"usage":   chatUsageFromLedger(usage),
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return writeSSEData(destination, payload)
}

func writeSSEData(destination io.Writer, payload []byte) error {
	event := make([]byte, 0, len(payload)+8)
	event = append(event, "data: "...)
	event = append(event, payload...)
	event = append(event, '\n', '\n')
	if _, err := destination.Write(event); err != nil {
		return err
	}
	if flusher, ok := destination.(interface{ Flush() }); ok {
		flusher.Flush()
	}
	return nil
}

func translatedOpenAIResponseModel(
	selection routing.Selection,
) string {
	model, rewrite := presentedModelName(selection)
	if rewrite {
		return model
	}
	return selection.Deployment.Model
}

func translatedChatIncludesUsage(
	requestBody []byte,
) bool {
	var envelope map[string]json.RawMessage
	if json.Unmarshal(requestBody, &envelope) != nil {
		return false
	}
	return streamIncludesUsage(envelope["stream_options"])
}

func configureTranslatedOpenAIHeaders(
	header http.Header,
	streaming bool,
) {
	removeRepresentationValidators(header)
	header.Del("Content-Length")
	if streaming {
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("X-Accel-Buffering", "no")
	} else {
		header.Set("Content-Type", "application/json")
	}
}

func translatedChatCreated() int64 {
	return time.Now().Unix()
}
