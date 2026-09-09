// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package managed

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestNormalizeSharedSparkrunProvider(t *testing.T) {
	shared := config.Provider{Name: SharedSparkrunProvider, Type: "sparkrun"}
	for _, legacyType := range []string{"sparkrun", "openai", "openai_compatible"} {
		t.Run(legacyType, func(t *testing.T) {
			deployment := config.Deployment{Name: "stable-id", Provider: "sparkrun:operator", Model: "model",
				EndpointSource: config.EndpointSource{Controller: "sparkrun", Revision: "stable-revision"}}
			sets := map[Owner]config.Document{
				OwnerOperator: {Providers: []config.Provider{{Name: "sparkrun:operator", Type: legacyType}}, Deployments: []config.Deployment{deployment}},
				OwnerSparkrun: {Providers: []config.Provider{shared}},
			}
			before, _ := json.Marshal(sets)
			normalized, err := NormalizeSparkrunProvider(sets)
			if err != nil {
				t.Fatal(err)
			}
			deployment.Provider = SharedSparkrunProvider
			if len(normalized[OwnerOperator].Providers) != 0 || !reflect.DeepEqual(normalized[OwnerOperator].Deployments, []config.Deployment{deployment}) ||
				!reflect.DeepEqual(normalized[OwnerSparkrun].Providers, []config.Provider{shared}) {
				t.Fatal(normalized)
			}
			after, _ := json.Marshal(sets)
			if string(before) != string(after) {
				t.Fatal("normalization mutated the original fragments")
			}
			again, err := NormalizeSparkrunProvider(normalized)
			if err != nil || !reflect.DeepEqual(again, normalized) {
				t.Fatal("normalization is not idempotent", err)
			}
		})
	}
}

func TestSharedSparkrunProviderPreservesCustomLegacySettings(t *testing.T) {
	for _, nonLifecycle := range []bool{false, true} {
		provider := config.Provider{Name: "sparkrun:operator", Type: "sparkrun"}
		deployment := config.Deployment{Name: "custom", Provider: provider.Name, EndpointSource: config.EndpointSource{Controller: "sparkrun"}}
		if nonLifecycle {
			deployment.EndpointSource.Controller = ""
		} else {
			provider.ExtraBody = map[string]json.RawMessage{"temperature": json.RawMessage("0.5")}
		}
		sets := map[Owner]config.Document{OwnerOperator: {Providers: []config.Provider{provider}, Deployments: []config.Deployment{deployment}}}
		normalized, err := NormalizeSparkrunProvider(sets)
		if err != nil || !reflect.DeepEqual(normalized, sets) {
			t.Fatal("changed custom configuration", normalized, err)
		}
	}
}

func TestSharedSparkrunProviderRejectsConflicts(t *testing.T) {
	shared := config.Provider{Name: SharedSparkrunProvider, Type: "sparkrun"}
	for _, providers := range [][]config.Provider{
		{shared, shared},
		{{Name: SharedSparkrunProvider, Type: "openai", BaseURL: "http://localhost:8000/v1"}},
		{{Name: SharedSparkrunProvider, Type: "sparkrun", ExtraBody: map[string]json.RawMessage{"temperature": json.RawMessage("0.5")}}},
	} {
		_, err := NormalizeSparkrunProvider(map[Owner]config.Document{
			OwnerOperator: {Providers: providers}, OwnerSparkrun: {Providers: []config.Provider{shared}},
		})
		if err == nil || (!strings.Contains(err.Error(), "conflicting") && !strings.Contains(err.Error(), "duplicate")) {
			t.Fatal("accepted conflicting shared provider", err)
		}
	}
}
