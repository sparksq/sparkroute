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
