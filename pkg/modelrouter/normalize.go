// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// This file normalizes Chat Completions, Responses, and prompt-shaped payloads
// into the protocol-neutral text snapshot consumed by routing policies.

type NormalizedRequest struct {
	Text         string       `json:"text,omitempty"`
	StageHistory StageHistory `json:"stage_history,omitempty"`
}

// decodeRequestObject decodes exactly one JSON object while retaining the
// lexical representation of every number. Generation requests can contain
// integers larger than JavaScript's safe-integer range in extension fields or
// tool arguments; decoding those values through float64 would silently round
// them before forwarding the request.
func decodeRequestObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("request body must be a JSON object")
	}
	if trailing := bytes.TrimSpace(raw[int(decoder.InputOffset()):]); len(trailing) != 0 {
		return nil, errors.New("request body must contain exactly one JSON object")
	}
	return body, nil
}

func NormalizeRoutingRequest(body []byte) (NormalizedRequest, error) {
	parsed, err := decodeRequestObject(body)
	if err != nil {
		return NormalizedRequest{}, err
	}
	return NormalizeParsedRoutingRequest(parsed), nil
}

// NormalizeParsedRoutingRequest derives routing text from an already decoded
// request. The generation ingress uses this entry point so request JSON is not
// decoded a second time merely to evaluate keyword rules.
func NormalizeParsedRoutingRequest(m map[string]any) NormalizedRequest {
	texts := make([]string, 0)
	// Chat requests are routed on the latest user turn. System prompts and old
	// conversation turns should not unexpectedly activate a keyword rule.
	if messages, ok := m["messages"].([]any); ok {
		for i := len(messages) - 1; i >= 0; i-- {
			msg, _ := messages[i].(map[string]any)
			if role, _ := msg["role"].(string); role == "user" {
				collectUserText(msg["content"], &texts)
				break
			}
		}
	} else if input, ok := m["input"]; ok {
		collectResponsesUserText(input, &texts)
	} else if prompt, ok := m["prompt"]; ok {
		collectStrings(prompt, &texts)
	}
	// Plan lowercases a separate copy solely for case-insensitive keyword rules.
	return NormalizedRequest{
		Text:         strings.Join(texts, "\n"),
		StageHistory: extractStageHistory(m),
	}
}

// collectResponsesUserText extracts only the latest user-authored text from a
// Responses API input. Protocol labels, tool output, function arguments, and
// earlier turns must not accidentally activate a keyword route.
func collectResponsesUserText(v any, out *[]string) {
	switch input := v.(type) {
	case string:
		*out = append(*out, input)
	case map[string]any:
		if role, _ := input["role"].(string); role == "user" {
			collectUserText(input["content"], out)
			return
		}
		if typ, _ := input["type"].(string); typ == "input_text" {
			if text, ok := input["text"].(string); ok {
				*out = append(*out, text)
			}
		}
	case []any:
		for i := len(input) - 1; i >= 0; i-- {
			item, _ := input[i].(map[string]any)
			if role, _ := item["role"].(string); role == "user" {
				collectUserText(item["content"], out)
				return
			}
		}
		// Some compatible clients send a bare list of input content parts.
		// In that shape, collect only explicit input_text blocks.
		for _, raw := range input {
			item, _ := raw.(map[string]any)
			if typ, _ := item["type"].(string); typ == "input_text" {
				if text, ok := item["text"].(string); ok {
					*out = append(*out, text)
				}
			}
		}
	}
}

func collectUserText(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case []any:
		for _, item := range x {
			part, _ := item.(map[string]any)
			typ, _ := part["type"].(string)
			if typ == "text" || typ == "input_text" || typ == "" {
				if text, ok := part["text"].(string); ok {
					*out = append(*out, text)
				}
			}
		}
	}
}

func collectStrings(v any, out *[]string) {
	switch x := v.(type) {
	case string:
		*out = append(*out, x)
	case []any:
		for _, y := range x {
			collectStrings(y, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k != "model" {
				collectStrings(x[k], out)
			}
		}
	}
}
