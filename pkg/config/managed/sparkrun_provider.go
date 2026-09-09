// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package managed

import (
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/sparksq/sparkroute/pkg/config"
)

const SharedSparkrunProvider = "sparkrun"

// NormalizeSparkrunProvider gives the shared lifecycle provider a stable owner,
// including when a recipe is saved before the plugin has supplied its set.
// Call only while preparing a write: stored fragments and their merged revision
// must continue to pass integrity checks until they are replaced atomically.
// Other providers retain the usual ownership and collision rules.
func NormalizeSparkrunProvider(sets map[Owner]config.Document) (map[Owner]config.Document, error) {
	sets = maps.Clone(sets)
	shared := config.Provider{Name: SharedSparkrunProvider, Type: "sparkrun"}
	needed, migrateLegacy := false, false
	legacyCount := 0
	for owner, document := range sets {
		seen := map[string]bool{}
		for _, provider := range document.Providers {
			if seen[provider.Name] {
				return nil, fmt.Errorf("duplicate provider %q owned by %q", provider.Name, owner)
			}
			seen[provider.Name] = true
			if reflect.DeepEqual(provider, shared) {
				needed = true
			}
			if provider.Name == "sparkrun:operator" {
				legacyCount++
				// Never discard custom provider settings during migration.
				migrateLegacy = (provider.Type == "sparkrun" || provider.Type == "openai" || provider.Type == "openai_compatible") &&
					reflect.DeepEqual(provider, config.Provider{Name: provider.Name, Type: provider.Type})
			}
		}
	}
	migrateLegacy = migrateLegacy && legacyCount == 1
	for _, document := range sets {
		for _, deployment := range document.Deployments {
			if deployment.Provider == SharedSparkrunProvider && deployment.EndpointSource.Controller == "sparkrun" {
				needed = true
			}
			if deployment.Provider == "sparkrun:operator" && deployment.EndpointSource.Controller != "sparkrun" {
				migrateLegacy = false
			}
		}
	}
	needed = needed || migrateLegacy
	if !needed {
		return sets, nil
	}
	for owner, document := range sets {
		for _, provider := range document.Providers {
			if provider.Name == SharedSparkrunProvider && !reflect.DeepEqual(provider, shared) {
				return nil, fmt.Errorf("shared sparkrun provider owned by %q has conflicting settings; use a different name for a customized provider", owner)
			}
		}
		document.Providers = slices.DeleteFunc(slices.Clone(document.Providers), func(p config.Provider) bool {
			return p.Name == SharedSparkrunProvider || (migrateLegacy && p.Name == "sparkrun:operator")
		})
		if migrateLegacy {
			document.Deployments = slices.Clone(document.Deployments)
			for i := range document.Deployments {
				if document.Deployments[i].Provider == "sparkrun:operator" {
					document.Deployments[i].Provider = SharedSparkrunProvider
				}
			}
		}
		sets[owner] = document
	}
	generated := sets[OwnerSparkrun]
	generated.Providers = append(generated.Providers, shared)
	sets[OwnerSparkrun] = generated
	return sets, nil
}
