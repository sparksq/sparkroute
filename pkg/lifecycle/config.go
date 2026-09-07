package lifecycle

import (
	"fmt"
	"slices"
	"sort"

	"github.com/sparksq/sparkroute/pkg/config"
)

// TargetsFromDocument compiles published dynamic endpoint configuration into
// lifecycle targets. Static deployments are intentionally omitted.
func TargetsFromDocument(document config.Document) ([]Target, error) {
	if err := document.Validate(); err != nil {
		return nil, fmt.Errorf("validate lifecycle configuration: %w", err)
	}
	modelsByDeployment := make(map[string][]string)
	for _, model := range document.VirtualModels {
		for _, pool := range model.Pools {
			for _, weighted := range pool.Targets {
				modelsByDeployment[weighted.Deployment] = append(
					modelsByDeployment[weighted.Deployment],
					model.Name,
				)
			}
		}
	}
	targets := make([]Target, 0)
	for _, deployment := range document.Deployments {
		source := deployment.EndpointSource
		sourceType := source.Type.Effective()
		if sourceType == config.EndpointSourceStatic {
			continue
		}
		target := Target{
			Deployment: deployment.Name, UpstreamModel: deployment.Model,
			LogicalModels: append([]string(nil), modelsByDeployment[deployment.Name]...),
			Controller:    source.Controller, Protocol: providerProtocol(document, deployment.Provider),
		}
		sort.Strings(target.LogicalModels)
		target.LogicalModels = slices.Compact(target.LogicalModels)
		switch sourceType {
		case config.EndpointSourceDiscovered:
			target.Source = EndpointDiscovered
		case config.EndpointSourceActivatable:
			target.Source = EndpointActivatable
			virtualModels := append([]string(nil), modelsByDeployment[deployment.Name]...)
			sort.Strings(virtualModels)
			virtualModel := ""
			if len(virtualModels) == 1 {
				virtualModel = virtualModels[0]
			}
			target.Binding = Binding{
				Controller: source.Controller, Revision: source.Revision,
				VirtualModel: virtualModel, Deployment: deployment.Name,
				Recipe: source.Recipe, RecipeRevision: source.RecipeRevision,
				ClusterCandidates:  append([]string(nil), source.ClusterCandidates...),
				Overrides:          cloneOverrides(source.Overrides),
				ActivationTimeout:  source.EffectiveActivationTimeout(),
				IdleTTL:            source.IdleTTL.Value(),
				MaxQueuedWaiters:   source.EffectiveMaxQueuedWaiters(),
				MaxQueuedBodyBytes: source.EffectiveMaxQueuedBodyBytes(),
				ColdStart:          ColdStartBehavior(source.ColdStart.Effective()),
			}
		default:
			return nil, fmt.Errorf("deployment %q has unsupported endpoint source %q", deployment.Name, sourceType)
		}
		targets = append(targets, target)
	}
	sort.Slice(targets, func(i, j int) bool {
		return targets[i].Deployment < targets[j].Deployment
	})
	return targets, nil
}

func providerProtocol(document config.Document, providerName string) string {
	for _, provider := range document.Providers {
		if provider.Name != providerName {
			continue
		}
		if provider.Type == "openai" || provider.Type == "openai_compatible" {
			return "openai"
		}
		return provider.Type
	}
	return ""
}

func cloneOverrides(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, value := range source {
		result[name] = value
	}
	return result
}
