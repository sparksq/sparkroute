package modelrouter

// ValidateModelMetadata validates the shared deployment/discovery field contract.
func ValidateModelMetadata(metadata DiscoveredModelMetadata) error {
	return validateDiscoveredModelMetadata("deployment", metadata)
}

// ReduceModelMetadata combines multiple targets conservatively: minimum context,
// maximum size and prices, and the union of tags. Absent fields stay absent.
func ReduceModelMetadata(current, next DiscoveredModelMetadata) DiscoveredModelMetadata {
	return reduceDiscoveredModelMetadata(cloneDiscoveredModelMetadata(current), next)
}

// ReplaceDeploymentMetadata installs a complete configuration-derived snapshot.
// Deployment fields override legacy logical-model fields and live discovery,
// including explicit zero prices. Selection policy and observations are retained.
func (r *RouterManager) ReplaceDeploymentMetadata(models map[string]DiscoveredModelMetadata) error {
	normalized, err := normalizeDiscoveredMetadataSnapshot(DiscoveredMetadataSnapshot{Source: "deployments", Models: models})
	if err != nil {
		return err
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deploymentMetadata = normalized.Models
	r.runtime.Store(compileRoutingPolicy(r.effectivePolicy(r.policy)))
	return nil
}

func (r *RouterManager) effectivePolicy(policy RoutingPolicy) RoutingPolicy {
	effective := mergeDiscoveredMetadata(policy, r.metadata)
	for name, fields := range r.deploymentMetadata {
		model, exists := effective.Models[name]
		if !exists {
			model.Enabled = true
		}
		if fields.SizeB != nil {
			model.SizeB = *fields.SizeB
		}
		if fields.Context != nil {
			model.Context = *fields.Context
		}
		if fields.InputPrice != nil {
			model.InputPrice = *fields.InputPrice
		}
		if fields.OutputPrice != nil {
			model.OutputPrice = *fields.OutputPrice
		}
		if fields.Tags != nil {
			model.Tags = append([]string(nil), fields.Tags...)
		}
		effective.Models[name] = model
	}
	return effective
}
