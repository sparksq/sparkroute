// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"encoding/json"

	llmclient "github.com/scitrera/go-llm/client"
)

// applyExtraBodyDefaults shallow-merges operator-owned provider/deployment
// defaults into one already translated upstream request. The caller's request
// always wins. Deployment defaults override provider defaults only when the
// caller omitted that field entirely.
func applyExtraBodyDefaults(
	body []byte,
	provider map[string]json.RawMessage,
	deployment map[string]json.RawMessage,
) ([]byte, error) {
	if len(provider) == 0 && len(deployment) == 0 {
		return body, nil
	}
	defaults := make(map[string]json.RawMessage, len(provider)+len(deployment))
	for name, value := range provider {
		defaults[name] = value
	}
	for name, value := range deployment {
		defaults[name] = value
	}
	return llmclient.MergeBodyDefaults(body, defaults)
}

// applyRequestOverrides recursively merges operator profile values over caller
// parameters, preserving sibling object fields. Protected request structure is
// excluded by config validation, and the result is revalidated before routing.
func applyRequestOverrides(body []byte, overrides map[string]json.RawMessage) ([]byte, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	for key, value := range overrides {
		var nested, original map[string]json.RawMessage
		if json.Unmarshal(value, &nested) == nil && nested != nil && json.Unmarshal(envelope[key], &original) == nil && original != nil {
			merged, err := applyRequestOverrides(envelope[key], nested)
			if err != nil {
				return nil, err
			}
			envelope[key] = merged
		} else {
			envelope[key] = value
		}
	}
	return json.Marshal(envelope)
}
