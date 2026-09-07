// Package routing compiles immutable request-path routing snapshots.
package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
)

var (
	ErrModelNotFound           = errors.New("virtual model not found")
	ErrNoTargets               = errors.New("virtual model has no routable targets")
	ErrUnsupportedCapabilities = errors.New("no target supports the required capabilities")
	ErrUnsupportedSemantics    = errors.New("no target can preserve the request semantics")
)

type Snapshot struct {
	aliases     map[string]string
	models      map[string]config.VirtualModel
	deployments map[string]config.Deployment
	providers   map[string]config.Provider
}

// ModelCandidates returns immutable canonical virtual-model descriptors which
// an in-process model router may select. Hidden models remain callable and are
// therefore candidates; internal models are exposed only to trusted internal
// invocations.
func (s *Snapshot) ModelCandidates(allowInternal bool) []config.VirtualModel {
	return s.ModelCandidatesForCapabilities(nil, allowInternal)
}

// ModelCandidatesForCapabilities returns logical models with at least one
// configured target which can admit the required capability set. Runtime
// health and protocol translation are intentionally evaluated later by the
// selected model's ordinary execution plan.
func (s *Snapshot) ModelCandidatesForCapabilities(
	required []config.Capability,
	allowInternal bool,
) []config.VirtualModel {
	models := make([]config.VirtualModel, 0, len(s.models))
	for _, model := range s.models {
		if model.Visibility.Effective() == config.ModelVisibilityInternal && !allowInternal {
			continue
		}
		if !s.modelSupportsCapabilities(model, required) {
			continue
		}
		models = append(models, cloneVirtualModel(model))
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].Name < models[j].Name
	})
	return models
}

// ModelSupportsCapabilities reports whether one canonical logical model has at
// least one configured deployment which can admit the supplied capability set.
// Runtime availability and protocol translation remain later plan concerns.
func (s *Snapshot) ModelSupportsCapabilities(
	name string,
	required []config.Capability,
	allowInternal bool,
) bool {
	model, exists := s.models[name]
	if !exists ||
		(model.Visibility.Effective() == config.ModelVisibilityInternal && !allowInternal) {
		return false
	}
	return s.modelSupportsCapabilities(model, required)
}

func (s *Snapshot) modelSupportsCapabilities(
	model config.VirtualModel,
	required []config.Capability,
) bool {
	modelRequired := mergeCapabilities(model.RequiredCapabilities, required)
	for _, pool := range model.Pools {
		for _, target := range pool.Targets {
			deployment, exists := s.deployments[target.Deployment]
			if exists && deployment.MatchCapabilities(modelRequired) != config.CapabilityMatchRejected {
				return true
			}
		}
	}
	return false
}

type Selection struct {
	RequestedModel   string
	VirtualModel     string
	Provider         config.Provider
	Deployment       config.Deployment
	UpstreamProtocol config.Protocol
	NativeProtocol   bool
	PoolPriority     int
	PreferenceClass  int
	// ProviderPreference is the selector-supplied provider rank applied after
	// eligibility filtering. Unlisted providers share the final rank.
	ProviderPreference int
	Weight             int
	ResponseModel      config.ResponseModelMode
}

type ProtocolRoute struct {
	Protocol             config.Protocol
	Native               bool
	RequiredCapabilities []config.Capability
}

type ProtocolResolver func(
	provider config.Provider,
	deployment config.Deployment,
) (ProtocolRoute, bool)

type Plan struct {
	RequestedModel       string
	VirtualModel         string
	RequiredCapabilities []config.Capability
	MaxAttempts          int
	Limits               config.EffectiveModelLimits
	Retry                config.EffectiveRetryPolicy
	Guardrails           config.GuardrailPolicy
	ProviderPriority     []string
	Candidates           []Selection
}

// PreferDeployment moves a soft-affinity match to the front only when it is in
// the same operator priority, protocol/capability class, and explicit provider
// preference rank as the plan's existing first candidate. Hard pins are
// resolved before this point.
func (p *Plan) PreferDeployment(deployment string) bool {
	if p == nil || deployment == "" || len(p.Candidates) == 0 {
		return false
	}
	first := p.Candidates[0]
	if first.Deployment.Name == deployment {
		return true
	}
	for index := 1; index < len(p.Candidates); index++ {
		candidate := p.Candidates[index]
		if candidate.Deployment.Name != deployment {
			continue
		}
		if candidate.PoolPriority != first.PoolPriority ||
			candidate.PreferenceClass != first.PreferenceClass ||
			candidate.ProviderPreference != first.ProviderPreference {
			return false
		}
		copy(p.Candidates[1:index+1], p.Candidates[0:index])
		p.Candidates[0] = candidate
		return true
	}
	return false
}

type Eligibility interface {
	Eligible(deployment string) bool
}

type PlanOptions struct {
	RequiredCapabilities []config.Capability
	AllowedProviderTypes []string
	Compatibility        func(providerType string) bool
	ProtocolResolver     ProtocolResolver
	PinnedDeployment     string
	PinnedProvider       string
	SingleAttempt        bool
	Eligibility          Eligibility
	Picker               WeightedPicker
	SelectionKey         string
	// ProviderPriority is a bounded, case-insensitive list of configured
	// provider names to move ahead of the otherwise canonical attempt order.
	// Unknown names are harmless and unlisted candidates remain stable.
	ProviderPriority []string
	AllowInternal    bool
}

type UnsupportedCapabilitiesError struct {
	Required []config.Capability
}

func (e *UnsupportedCapabilitiesError) Error() string {
	return fmt.Sprintf("%s: %v", ErrUnsupportedCapabilities, e.Required)
}

func (e *UnsupportedCapabilitiesError) Unwrap() error {
	return ErrUnsupportedCapabilities
}

func Compile(document config.Document) (*Snapshot, error) {
	document = document.ResolveCapabilityPolicies(config.UnknownCapabilityTry)
	if err := document.Validate(); err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		aliases:     make(map[string]string),
		models:      make(map[string]config.VirtualModel, len(document.VirtualModels)),
		deployments: make(map[string]config.Deployment, len(document.Deployments)),
		providers:   make(map[string]config.Provider, len(document.Providers)),
	}
	for _, provider := range document.Providers {
		snapshot.providers[provider.Name] = cloneProvider(provider)
	}
	for _, deployment := range document.Deployments {
		snapshot.deployments[deployment.Name] = cloneDeployment(deployment)
	}
	for _, model := range document.VirtualModels {
		model = cloneVirtualModel(model)
		snapshot.models[model.Name] = model
		snapshot.aliases[model.Name] = model.Name
		for _, alias := range model.Aliases {
			snapshot.aliases[alias] = model.Name
		}
	}
	return snapshot, nil
}

// BuildPlan resolves a model once and produces a weighted order without
// replacement inside each priority pool. Exhausting a pool advances to the next
// priority. This lets the executor retry a different deployment while naturally
// renormalizing weights over the untried candidates.
func (s *Snapshot) BuildPlan(requested string, picker WeightedPicker) (Plan, error) {
	return s.BuildPlanFor(requested, PlanOptions{Picker: picker})
}

// ResolveModel resolves a canonical name or alias and applies internal-model
// access policy without selecting an upstream target.
func (s *Snapshot) ResolveModel(
	requested string,
	allowInternal bool,
) (config.VirtualModel, error) {
	canonical, exists := s.aliases[requested]
	if !exists {
		return config.VirtualModel{}, fmt.Errorf("%w: %s", ErrModelNotFound, requested)
	}
	model := s.models[canonical]
	if model.Visibility.Effective() == config.ModelVisibilityInternal &&
		!allowInternal {
		return config.VirtualModel{}, fmt.Errorf("%w: %s", ErrModelNotFound, requested)
	}
	return cloneVirtualModel(model), nil
}

// HasTargetProviderType reports whether the visible virtual model contains at
// least one configured target of an allowed provider type. It intentionally
// ignores health, capacity, and request capabilities; callers use it only to
// decide whether provider-specific request preprocessing can still be useful.
func (s *Snapshot) HasTargetProviderType(
	requested string,
	allowed []string,
	allowInternal bool,
) (bool, error) {
	model, err := s.ResolveModel(requested, allowInternal)
	if err != nil {
		return false, err
	}
	for _, pool := range model.Pools {
		for _, target := range pool.Targets {
			deployment, exists := s.deployments[target.Deployment]
			if !exists {
				continue
			}
			provider, exists := s.providers[deployment.Provider]
			if exists && providerTypeAllowed(provider.Type, allowed) {
				return true, nil
			}
		}
	}
	return false, nil
}

// HasTargetNativeProtocol reports whether a visible virtual model contains at
// least one target that natively serves protocol. It intentionally ignores
// health, capacity, and request capabilities.
func (s *Snapshot) HasTargetNativeProtocol(
	requested string,
	protocol config.Protocol,
	allowInternal bool,
) (bool, error) {
	model, err := s.ResolveModel(requested, allowInternal)
	if err != nil {
		return false, err
	}
	for _, pool := range model.Pools {
		for _, target := range pool.Targets {
			deployment, exists := s.deployments[target.Deployment]
			if !exists {
				continue
			}
			provider, exists := s.providers[deployment.Provider]
			if exists && deployment.SupportsNativeProtocol(provider, protocol) {
				return true, nil
			}
		}
	}
	return false, nil
}

// BuildPlanFor resolves the model once, filters unsupported or unavailable
// targets before weighted selection, and produces a weighted order without
// replacement inside each priority pool.
func (s *Snapshot) BuildPlanFor(requested string, options PlanOptions) (Plan, error) {
	model, err := s.ResolveModel(requested, options.AllowInternal)
	if err != nil {
		return Plan{}, err
	}
	canonical := model.Name
	picker := options.Picker
	if picker == nil {
		picker = CryptoPicker{}
	}
	required := mergeCapabilities(
		model.RequiredCapabilities,
		options.RequiredCapabilities,
	)
	pools := append([]config.RoutingPool(nil), model.Pools...)
	sort.SliceStable(pools, func(i, j int) bool {
		return pools[i].Priority < pools[j].Priority
	})

	candidates := make([]Selection, 0)
	protocolTargets := 0
	compatibleTargets := 0
	capableTargets := 0
	preferenceClass := 0
	for _, pool := range pools {
		type eligibleTarget struct {
			target config.WeightedTarget
			route  ProtocolRoute
		}
		nativeDeclared := make([]eligibleTarget, 0, len(pool.Targets))
		nativeUnknown := make([]eligibleTarget, 0, len(pool.Targets))
		translatedDeclared := make([]eligibleTarget, 0, len(pool.Targets))
		translatedUnknown := make([]eligibleTarget, 0, len(pool.Targets))
		for _, target := range pool.Targets {
			if options.PinnedDeployment != "" &&
				target.Deployment != options.PinnedDeployment {
				continue
			}
			deployment, exists := s.deployments[target.Deployment]
			if !exists {
				continue
			}
			if options.PinnedProvider != "" &&
				deployment.Provider != options.PinnedProvider {
				continue
			}
			provider, exists := s.providers[deployment.Provider]
			if !exists || !providerTypeAllowed(provider.Type, options.AllowedProviderTypes) {
				continue
			}
			protocolTargets++
			route := ProtocolRoute{Native: true}
			if protocols := deployment.EffectiveNativeProtocols(provider); len(protocols) != 0 {
				route.Protocol = protocols[0]
			}
			if options.ProtocolResolver != nil {
				var compatible bool
				route, compatible = options.ProtocolResolver(provider, deployment)
				if !compatible || route.Protocol == "" {
					continue
				}
			} else if options.Compatibility != nil &&
				!options.Compatibility(provider.Type) {
				continue
			}
			compatibleTargets++
			routeRequired := required
			if len(route.RequiredCapabilities) != 0 {
				routeRequired = append(
					append([]config.Capability(nil), required...),
					route.RequiredCapabilities...,
				)
			}
			capabilityMatch := deployment.MatchCapabilities(routeRequired)
			if capabilityMatch == config.CapabilityMatchRejected {
				continue
			}
			capableTargets++
			if options.Eligibility != nil &&
				!options.Eligibility.Eligible(deployment.Name) {
				continue
			}
			eligible := eligibleTarget{target: target, route: route}
			switch {
			case route.Native && capabilityMatch == config.CapabilityMatchDeclared:
				nativeDeclared = append(nativeDeclared, eligible)
			case route.Native:
				nativeUnknown = append(nativeUnknown, eligible)
			case capabilityMatch == config.CapabilityMatchDeclared:
				translatedDeclared = append(translatedDeclared, eligible)
			default:
				translatedUnknown = append(translatedUnknown, eligible)
			}
		}
		// Within one operator-defined priority pool, native protocol matches are
		// attempted before translations. Declared capability support is then
		// preferred over permissive unknown support within each protocol class.
		for _, group := range [][]eligibleTarget{
			nativeDeclared,
			nativeUnknown,
			translatedDeclared,
			translatedUnknown,
		} {
			class := preferenceClass
			preferenceClass++
			remaining := group
			if model.Selection.Mode == config.SelectionWeightedHash &&
				options.SelectionKey != "" && len(remaining) > 1 {
				targets := make([]config.WeightedTarget, len(remaining))
				for index := range remaining {
					targets[index] = remaining[index].target
				}
				order := weightedHashOrder(options.SelectionKey, targets)
				ordered := make([]eligibleTarget, len(remaining))
				for index, original := range order {
					ordered[index] = remaining[original]
				}
				remaining = ordered
			}
			for len(remaining) > 0 {
				var totalWeight int64
				for _, eligible := range remaining {
					weight := int64(eligible.target.Weight)
					if weight > int64(^uint64(0)>>1)-totalWeight {
						return Plan{}, fmt.Errorf("routing pool %d weight overflow", pool.Priority)
					}
					totalWeight += weight
				}
				value := int64(0)
				if options.PinnedDeployment == "" &&
					(model.Selection.Mode != config.SelectionWeightedHash ||
						options.SelectionKey == "") {
					value, err = picker.Pick(totalWeight)
					if err != nil {
						return Plan{}, err
					}
					if value < 0 || value >= totalWeight {
						return Plan{}, fmt.Errorf(
							"weighted picker returned %d outside [0,%d)",
							value,
							totalWeight,
						)
					}
				}
				index := 0
				var boundary int64
				for candidateIndex, candidate := range remaining {
					boundary += int64(candidate.target.Weight)
					if value < boundary {
						index = candidateIndex
						break
					}
				}
				eligible := remaining[index]
				remaining = append(remaining[:index], remaining[index+1:]...)
				target := eligible.target

				deployment, exists := s.deployments[target.Deployment]
				if !exists {
					continue
				}
				provider, exists := s.providers[deployment.Provider]
				if !exists {
					continue
				}
				candidates = append(candidates, Selection{
					RequestedModel:   requested,
					VirtualModel:     canonical,
					Provider:         cloneProvider(provider),
					Deployment:       cloneDeployment(deployment),
					UpstreamProtocol: eligible.route.Protocol,
					NativeProtocol:   eligible.route.Native,
					PoolPriority:     pool.Priority,
					PreferenceClass:  class,
					Weight:           target.Weight,
					ResponseModel:    effectiveResponseModel(model.ResponseModel),
				})
			}
		}
	}
	if len(candidates) == 0 {
		if protocolTargets > 0 && compatibleTargets == 0 {
			return Plan{}, ErrUnsupportedSemantics
		}
		if protocolTargets > 0 && capableTargets == 0 && len(required) > 0 {
			return Plan{}, &UnsupportedCapabilitiesError{
				Required: append([]config.Capability(nil), required...),
			}
		}
		return Plan{}, fmt.Errorf("%w: %s", ErrNoTargets, canonical)
	}
	providerPriority := applyProviderPriority(candidates, options.ProviderPriority)
	if options.PinnedDeployment != "" && len(candidates) > 1 {
		candidates = candidates[:1]
	}
	limits := model.Limits.Effective()
	maxAttempts := limits.MaxAttempts
	if maxAttempts > len(candidates) {
		maxAttempts = len(candidates)
	}
	if options.SingleAttempt && maxAttempts > 1 {
		maxAttempts = 1
	}
	limits.MaxAttempts = maxAttempts
	return Plan{
		RequestedModel:       requested,
		VirtualModel:         canonical,
		RequiredCapabilities: append([]config.Capability(nil), required...),
		MaxAttempts:          maxAttempts,
		Limits:               limits,
		Retry:                model.Retry.Effective(),
		Guardrails:           cloneGuardrailPolicy(model.Guardrails),
		ProviderPriority:     providerPriority,
		Candidates:           candidates,
	}, nil
}

const (
	maxProviderPriorityEntries   = 64
	maxProviderPriorityNameBytes = 256
)

// applyProviderPriority performs a stable override on already filtered
// candidates. An explicit selector preference may cross ordinary pool and
// protocol/capability preference classes, matching SparkRoute's provider-chain
// semantics, but it can never reintroduce an ineligible or pinned-out target.
func applyProviderPriority(candidates []Selection, configured []string) []string {
	limit := len(configured)
	if limit > maxProviderPriorityEntries {
		limit = maxProviderPriorityEntries
	}
	priority := make([]string, 0, limit)
	ranks := make(map[string]int, limit)
	for _, raw := range configured[:limit] {
		name := strings.TrimSpace(raw)
		if name == "" || len(name) > maxProviderPriorityNameBytes {
			continue
		}
		name = strings.ToLower(name)
		if _, exists := ranks[name]; exists {
			continue
		}
		ranks[name] = len(priority)
		priority = append(priority, name)
	}
	if len(priority) == 0 {
		return nil
	}
	unlisted := len(priority)
	for index := range candidates {
		rank, listed := ranks[strings.ToLower(candidates[index].Provider.Name)]
		if !listed {
			rank = unlisted
		}
		candidates[index].ProviderPreference = rank
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ProviderPreference < candidates[j].ProviderPreference
	})
	return priority
}

func mergeCapabilities(groups ...[]config.Capability) []config.Capability {
	set := make(map[config.Capability]struct{})
	for _, group := range groups {
		for _, capability := range group {
			set[capability] = struct{}{}
		}
	}
	result := make([]config.Capability, 0, len(set))
	for capability := range set {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return result
}

func providerTypeAllowed(providerType string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if providerType == candidate {
			return true
		}
	}
	return false
}

func effectiveResponseModel(mode config.ResponseModelMode) config.ResponseModelMode {
	if mode == "" {
		return config.ResponseModelVirtual
	}
	return mode
}

func cloneProvider(provider config.Provider) config.Provider {
	provider.DefaultHeaders = cloneHeaders(provider.DefaultHeaders)
	provider.ExtraBody = cloneRawMessages(provider.ExtraBody)
	return provider
}

func cloneDeployment(deployment config.Deployment) config.Deployment {
	deployment.UpstreamHeaders = cloneHeaders(deployment.UpstreamHeaders)
	deployment.ExtraBody = cloneRawMessages(deployment.ExtraBody)
	deployment.NativeProtocols = append([]config.Protocol(nil), deployment.NativeProtocols...)
	deployment.Capabilities = append([]config.Capability(nil), deployment.Capabilities...)
	deployment.CapabilityPolicy.Unsupported = append(
		[]config.Capability(nil),
		deployment.CapabilityPolicy.Unsupported...,
	)
	deployment.EndpointSource.ClusterCandidates = append(
		[]string(nil),
		deployment.EndpointSource.ClusterCandidates...,
	)
	if deployment.EndpointSource.Overrides != nil {
		overrides := make(map[string]string, len(deployment.EndpointSource.Overrides))
		for name, value := range deployment.EndpointSource.Overrides {
			overrides[name] = value
		}
		deployment.EndpointSource.Overrides = overrides
	}
	return deployment
}

func cloneRawMessages(input map[string]json.RawMessage) map[string]json.RawMessage {
	if input == nil {
		return nil
	}
	result := make(map[string]json.RawMessage, len(input))
	for name, value := range input {
		result[name] = append(json.RawMessage(nil), value...)
	}
	return result
}

func cloneVirtualModel(model config.VirtualModel) config.VirtualModel {
	model.Aliases = append([]string(nil), model.Aliases...)
	model.RequiredCapabilities = append(
		[]config.Capability(nil),
		model.RequiredCapabilities...,
	)
	pools := make([]config.RoutingPool, len(model.Pools))
	for index, pool := range model.Pools {
		pool.Targets = append([]config.WeightedTarget(nil), pool.Targets...)
		pools[index] = pool
	}
	model.Pools = pools
	model.Guardrails = cloneGuardrailPolicy(model.Guardrails)
	model.Privacy = clonePrivacyPolicy(model.Privacy)
	return model
}

func cloneGuardrailPolicy(policy config.GuardrailPolicy) config.GuardrailPolicy {
	policy.Pre = append([]config.Guardrail(nil), policy.Pre...)
	policy.Post = append([]config.Guardrail(nil), policy.Post...)
	return policy
}

func clonePrivacyPolicy(policy *config.PrivacyPolicy) *config.PrivacyPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	if policy.PII != nil {
		pii := *policy.PII
		pii.Entities = append([]config.PIIEntity(nil), pii.Entities...)
		clone.PII = &pii
	}
	return &clone
}

func cloneHeaders(source map[string]config.HeaderValue) map[string]config.HeaderValue {
	if source == nil {
		return nil
	}
	cloned := make(map[string]config.HeaderValue, len(source))
	for name, value := range source {
		cloned[name] = value
	}
	return cloned
}
