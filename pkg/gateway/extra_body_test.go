// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"encoding/json"
	"testing"
)

func TestApplyExtraBodyDefaultsHonorsCallerAndDeploymentPrecedence(t *testing.T) {
	t.Parallel()
	body, err := applyExtraBodyDefaults(
		[]byte(`{"model":"upstream","temperature":0}`),
		map[string]json.RawMessage{
			"temperature":  json.RawMessage(`1`),
			"service_tier": json.RawMessage(`"auto"`),
		},
		map[string]json.RawMessage{
			"service_tier": json.RawMessage(`"priority"`),
			"top_k":        json.RawMessage(`40`),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["temperature"] != float64(0) || decoded["service_tier"] != "priority" || decoded["top_k"] != float64(40) {
		t.Fatalf("merged body = %#v", decoded)
	}
}

func TestApplyExtraBodyDefaultsRequiresObject(t *testing.T) {
	t.Parallel()
	if _, err := applyExtraBodyDefaults(
		[]byte(`[]`),
		map[string]json.RawMessage{"seed": json.RawMessage(`1`)},
		nil,
	); err == nil {
		t.Fatal("applyExtraBodyDefaults() error = nil")
	}
}
