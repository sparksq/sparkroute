package adminapi

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/routing"
)

const maxRoutingSimulationTextBytes = 16 << 10

// RoutingSimulationInput describes a content-bounded, stateless policy
// preview. RoutingText is used only for keyword rules and is never persisted
// or forwarded to a provider by the simulation path.
type RoutingSimulationInput struct {
	Document             config.Document     `json:"document"`
	RequestedModel       string              `json:"requested_model"`
	RoutingText          string              `json:"routing_text,omitempty"`
	RequiredCapabilities []config.Capability `json:"required_capabilities,omitempty"`
}

// RoutingSimulationResult reports the exact native policy decision together
// with the capability-compatible logical-model universe supplied to it.
// StateMode is explicit because adaptive observations and live round-robin
// counters are replica-local runtime state and are not applied to draft policy.
type RoutingSimulationResult struct {
	Decision             modelrouter.RouteDecision `json:"decision"`
	AvailableModels      []string                  `json:"available_models"`
	RequiredCapabilities []config.Capability       `json:"required_capabilities,omitempty"`
	StateMode            string                    `json:"state_mode"`
}

// SimulateRouting validates and evaluates a draft policy through the same
// compiler, capability filter, planner, and native strategy implementation as
// inference. The newly constructed manager makes this side-effect free with
// respect to the serving router.
func SimulateRouting(
	input RoutingSimulationInput,
	unknownDefault config.UnknownCapabilityPolicy,
) (RoutingSimulationResult, error) {
	return SimulateRoutingWithMetadata(input, unknownDefault, modelrouter.DiscoveredMetadataState{})
}

// SimulateRoutingWithMetadata applies the same validated discovery snapshots
// as the serving router while retaining stateless counters and observations.
func SimulateRoutingWithMetadata(
	input RoutingSimulationInput,
	unknownDefault config.UnknownCapabilityPolicy,
	metadata modelrouter.DiscoveredMetadataState,
) (RoutingSimulationResult, error) {
	requested := strings.TrimSpace(input.RequestedModel)
	if len(requested) > 256 || strings.ContainsAny(requested, "\r\n") {
		return RoutingSimulationResult{}, fmt.Errorf("requested_model is invalid")
	}
	if len(input.RoutingText) > maxRoutingSimulationTextBytes {
		return RoutingSimulationResult{}, fmt.Errorf(
			"routing_text exceeds %d bytes",
			maxRoutingSimulationTextBytes,
		)
	}
	if err := config.ValidateCapabilitySet(input.RequiredCapabilities); err != nil {
		return RoutingSimulationResult{}, fmt.Errorf("required_capabilities: %w", err)
	}
	if input.Document.ModelRouting == nil {
		return RoutingSimulationResult{}, fmt.Errorf("document has no model_routing policy")
	}

	document := input.Document.ResolveCapabilityPolicies(unknownDefault)
	snapshot, err := routing.Compile(document)
	if err != nil {
		return RoutingSimulationResult{}, fmt.Errorf("compile configuration: %w", err)
	}
	manager, err := modelrouter.NewRouterManager(*document.ModelRouting)
	if err != nil {
		return RoutingSimulationResult{}, fmt.Errorf("compile model routing: %w", err)
	}
	if err := manager.ReplaceDeploymentMetadata(document.DeploymentMetadata()); err != nil {
		return RoutingSimulationResult{}, err
	}
	for _, source := range metadata.Sources {
		if err := manager.ReplaceDiscoveredMetadata(source); err != nil {
			return RoutingSimulationResult{}, fmt.Errorf("apply discovered model metadata: %w", err)
		}
	}
	candidates := snapshot.ModelCandidates(false)
	routerCandidates := make([]modelrouter.Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		routerCandidates = append(routerCandidates, modelrouter.Candidate{Name: candidate.Name})
	}
	projectable, err := manager.ProjectionCapabilities(
		context.Background(),
		modelrouter.Input{
			RequestedModel: requested,
			Candidates:     routerCandidates,
			Features: modelrouter.RequestFeatures{
				RoutingText: input.RoutingText,
			},
		},
	)
	if err != nil {
		return RoutingSimulationResult{}, fmt.Errorf("resolve projection capabilities: %w", err)
	}
	available := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		required := input.RequiredCapabilities
		if projected := projectable[candidate.Name]; len(projected) != 0 {
			required = simulationCapabilitiesWithout(required, projected)
		}
		if snapshot.ModelSupportsCapabilities(candidate.Name, required, false) {
			available = append(available, candidate.Name)
		}
	}
	sort.Strings(available)
	decision, err := manager.Select(
		requested,
		modelrouter.NormalizedRequest{Text: input.RoutingText},
		available,
		true,
	)
	if err != nil {
		return RoutingSimulationResult{}, fmt.Errorf("simulate model routing: %w", err)
	}
	return RoutingSimulationResult{
		Decision:             decision,
		AvailableModels:      available,
		RequiredCapabilities: append([]config.Capability(nil), input.RequiredCapabilities...),
		StateMode:            simulationStateMode(metadata),
	}, nil
}

func simulationStateMode(metadata modelrouter.DiscoveredMetadataState) string {
	if len(metadata.Sources) > 0 {
		return "stateless_with_discovered_metadata"
	}
	return "stateless"
}

func simulationCapabilitiesWithout(
	required []config.Capability,
	projected []string,
) []config.Capability {
	removed := make(map[string]struct{}, len(projected))
	for _, capability := range projected {
		removed[capability] = struct{}{}
	}
	result := make([]config.Capability, 0, len(required))
	for _, capability := range required {
		if _, exists := removed[string(capability)]; !exists {
			result = append(result, capability)
		}
	}
	return result
}
