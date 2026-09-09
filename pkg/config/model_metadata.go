// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import "github.com/sparksq/sparkroute/pkg/modelrouter"

// DeploymentMetadata projects deployment attributes onto every virtual model and
// alias using it. All fallback pools participate; weights do not change limits.
func (d Document) DeploymentMetadata() map[string]modelrouter.DiscoveredModelMetadata {
	deployments := make(map[string]*modelrouter.DiscoveredModelMetadata, len(d.Deployments))
	for _, deployment := range d.Deployments {
		deployments[deployment.Name] = deployment.ModelMetadata
	}
	result := make(map[string]modelrouter.DiscoveredModelMetadata)
	for _, model := range d.VirtualModels {
		var merged modelrouter.DiscoveredModelMetadata
		found := false
		for _, pool := range model.Pools {
			for _, target := range pool.Targets {
				if fields := deployments[target.Deployment]; fields != nil {
					merged = modelrouter.ReduceModelMetadata(merged, *fields)
					found = true
				}
			}
		}
		if found {
			result[model.Name] = merged
			for _, alias := range model.Aliases {
				result[alias] = merged
			}
		}
	}
	return result
}
