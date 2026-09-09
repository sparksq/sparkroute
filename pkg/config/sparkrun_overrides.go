package config

import "fmt"

// SparkrunOverrides records operator intent separately from generated input.
// Exclusions affect only deployments in the sparkrun managed set. They remain
// valid while the source entry is absent, so discovery/restarts cannot undo them.
type SparkrunOverrides struct {
	ExcludedDeployments []string `json:"excluded_deployments,omitempty"`
}

func (o *SparkrunOverrides) Validate() error {
	if o == nil {
		return nil
	}
	if len(o.ExcludedDeployments) > 4096 {
		return fmt.Errorf("sparkrun_overrides.excluded_deployments: at most 4096 entries are supported")
	}
	seen := make(map[string]bool, len(o.ExcludedDeployments))
	for _, name := range o.ExcludedDeployments {
		if err := validateID(name); err != nil {
			return fmt.Errorf("sparkrun_overrides.excluded_deployments[%q]: %w", name, err)
		}
		if seen[name] {
			return fmt.Errorf("sparkrun_overrides.excluded_deployments: duplicate deployment %q", name)
		}
		seen[name] = true
	}
	return nil
}
