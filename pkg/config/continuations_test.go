// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderContinuationsValidationAndRoundTrip(t *testing.T) {
	for _, policy := range []ContinuationPolicy{ContinuationsDefault, ContinuationsNone, ContinuationsSameOrigin303, "all_redirects"} {
		t.Run(string(policy), func(t *testing.T) {
			document := validDocument()
			document.Providers[0].Continuations = policy
			err := document.Validate()
			if policy == "all_redirects" {
				if err == nil || !strings.Contains(err.Error(), "providers[0].continuations") {
					t.Fatalf("invalid policy error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(document)
			if err != nil {
				t.Fatal(err)
			}
			var restored Document
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if restored.Providers[0].Continuations != policy {
				t.Fatal("continuation policy lost in serialization")
			}
			if policy == ContinuationsDefault && strings.Contains(string(raw), "continuations") {
				t.Fatal("omitted policy changed existing configuration serialization")
			}
		})
	}
}
