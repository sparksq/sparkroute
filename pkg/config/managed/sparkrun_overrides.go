package managed

import "github.com/sparksq/sparkroute/pkg/config"

// ApplySparkrunOverrides filters generated entities without mutating either
// owner fragment. Generated models lose excluded targets and disappear only if
// that removes their final target. Operator references are left for validation
// to report; exclusion never silently rewrites operator routing or policies.
func ApplySparkrunOverrides(operator, generated config.Document) (config.Document, error) {
	if err := operator.SparkrunOverrides.Validate(); err != nil {
		return config.Document{}, err
	}
	if operator.SparkrunOverrides == nil || len(operator.SparkrunOverrides.ExcludedDeployments) == 0 {
		return generated, nil
	}
	excluded := make(map[string]bool)
	for _, name := range operator.SparkrunOverrides.ExcludedDeployments {
		excluded[name] = true
	}
	result := generated
	result.Deployments = make([]config.Deployment, 0, len(generated.Deployments))
	for _, deployment := range generated.Deployments {
		if !excluded[deployment.Name] {
			result.Deployments = append(result.Deployments, deployment)
		}
	}
	result.VirtualModels = make([]config.VirtualModel, 0, len(generated.VirtualModels))
	for _, model := range generated.VirtualModels {
		changed := false
		pools := make([]config.RoutingPool, 0, len(model.Pools))
		for _, pool := range model.Pools {
			targets := make([]config.WeightedTarget, 0, len(pool.Targets))
			for _, target := range pool.Targets {
				if excluded[target.Deployment] {
					changed = true
				} else {
					targets = append(targets, target)
				}
			}
			// Preserve pre-existing invalid empty pools for normal validation.
			if len(targets) > 0 || len(pool.Targets) == 0 {
				pool.Targets = targets
				pools = append(pools, pool)
			}
		}
		if changed && len(pools) == 0 {
			continue
		}
		model.Pools = pools
		result.VirtualModels = append(result.VirtualModels, model)
	}
	return result, nil
}
