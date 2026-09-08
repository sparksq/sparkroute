package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

const (
	maxHeaders           = 64
	maxExtraBodyFields   = 64
	maxExtraBodyName     = 128
	maxExtraBodyValue    = 1 << 20
	maxExtraBodyBytes    = 4 << 20
	maxHeaderName        = 128
	maxHeaderValue       = 8192
	maxAttempts          = 16
	maxIDLength          = 256
	maxCapabilities      = 64
	maxCapabilityLength  = 64
	maxConcurrency       = 1_000_000
	maxCircuitSamples    = 10_000
	maxRetryConcurrency  = 1_000_000
	maxGuardrails        = 8
	maxGuardrailPrompt   = 16 << 10
	minGuardrailWindow   = 1 << 10
	maxGuardrailWindow   = 1 << 20
	maxGuardrailContext  = 4 << 20
	maxClusterCandidates = 64
	maxEndpointOverrides = 64
	maxQueuedWaiters     = 100_000
	maxQueuedBodyBytes   = int64(1 << 40)
	maxPIIFileTextBytes  = int64(32 << 20)
)

const (
	defaultCircuitConsecutiveFailures       = 5
	defaultCircuitMinimumSamples            = 20
	defaultCircuitSampleWindow              = 50
	defaultCircuitFailureRate               = 0.5
	defaultCircuitBaseEjectionTime          = 30 * time.Second
	defaultCircuitMaxEjectionTime           = 10 * time.Minute
	maxConfiguredEjectionTime               = 24 * time.Hour
	defaultActivationTimeout                = 30 * time.Minute
	maxConfiguredActivationTimeout          = 24 * time.Hour
	maxConfiguredIdleTTL                    = 30 * 24 * time.Hour
	defaultMaxQueuedWaiters                 = 100
	defaultMaxQueuedBodyBytes         int64 = 256 << 20
)

const (
	defaultOverallTimeout            = 10 * time.Minute
	defaultStreamIdleTimeout         = 2 * time.Minute
	maxConfiguredTimeout             = 24 * time.Hour
	defaultRetryBaseBackoff          = 100 * time.Millisecond
	defaultRetryMaxBackoff           = 2 * time.Second
	defaultRetryMaxRetryAfter        = 30 * time.Second
	defaultRetryBudgetRatio          = 0.2
	defaultRetryMinConcurrency       = 1
	maxRetryBudgetRatio              = 10
	defaultPromptCacheAffinityTTL    = 5 * time.Minute
	maxPromptCacheAffinityTTL        = 24 * time.Hour
	defaultPromptCacheMinPrefixBytes = 4 << 10
	maxPromptCacheMinPrefixBytes     = 32 << 20
	defaultPromptCacheMaxPrefixes    = 8
	maxPromptCacheMaxPrefixes        = 64
)

var standardCapabilities = map[Capability]struct{}{
	CapabilityTools:                 {},
	CapabilityVision:                {},
	CapabilityAudioInput:            {},
	CapabilityAudioOutput:           {},
	CapabilityFileInput:             {},
	CapabilityFiles:                 {},
	CapabilityDeveloperMessages:     {},
	CapabilityStructuredOutputs:     {},
	CapabilityJSONMode:              {},
	CapabilityReasoning:             {},
	CapabilityLogprobs:              {},
	CapabilitySeed:                  {},
	CapabilityMultipleChoices:       {},
	CapabilityParallelTools:         {},
	CapabilityPrediction:            {},
	CapabilityServiceTier:           {},
	CapabilityProviderTools:         {},
	CapabilityStreamUsage:           {},
	CapabilityStoredCompletion:      {},
	CapabilityResponses:             {},
	CapabilityResponsesCompact:      {},
	CapabilityBackgroundResponses:   {},
	CapabilityConversations:         {},
	CapabilitySingleVectorEmbedding: {},
	CapabilityTokenCounting:         {},
	CapabilityPromptCaching:         {},
	CapabilityCitations:             {},
	CapabilityProviderGuardrails:    {},
	CapabilityProviderPrompts:       {},
}

var standardProtocols = map[Protocol]struct{}{
	ProtocolOpenAI:    {},
	ProtocolAnthropic: {},
	ProtocolGemini:    {},
	ProtocolBedrock:   {},
}

var protectedHeaders = map[string]struct{}{
	"authorization":             {},
	"anthropic-beta":            {},
	"anthropic-user-profile-id": {},
	"anthropic-version":         {},
	"connection":                {},
	"content-length":            {},
	"host":                      {},
	"proxy-authorization":       {},
	"proxy-connection":          {},
	"te":                        {},
	"traceparent":               {},
	"tracestate":                {},
	"trailer":                   {},
	"transfer-encoding":         {},
	"upgrade":                   {},
	"x-api-key":                 {},
	"x-amz-content-sha256":      {},
	"x-amz-date":                {},
	"x-amz-security-token":      {},
	"x-goog-api-key":            {},
	"x-request-id":              {},
	"x-sparkroute-request-id":   {},
}

// Extra-body defaults are intentionally limited to provider tuning knobs.
// Request structure, routing semantics, and transport framing must stay owned
// by the protocol adapter and caller so defaults cannot bypass capability
// detection or make a buffered request stream unexpectedly.
var protectedExtraBodyFields = map[string]struct{}{
	"extra_body":     {},
	"input":          {},
	"messages":       {},
	"model":          {},
	"prompt":         {},
	"stream":         {},
	"stream_options": {},
	"tool_choice":    {},
	"tools":          {},
}

// Validate rejects ambiguous or unsafe configuration before publication.
func (d Document) Validate() error {
	if err := d.Observability.Validate(); err != nil {
		return fmt.Errorf("observability: %w", err)
	}
	// Standalone is the base schema profile. The cluster profile resolves the
	// same precedence using its strict default before invoking this validation.
	d = d.ResolveCapabilityPolicies(UnknownCapabilityTry)
	if d.ModelRouting != nil {
		if err := modelrouter.ValidateRoutingPolicy(*d.ModelRouting); err != nil {
			return fmt.Errorf("model_routing: %w", err)
		}
	}
	if err := validateCapabilityDefaults(d.CapabilityDefaults); err != nil {
		return fmt.Errorf("capability_defaults: %w", err)
	}
	providers := make(map[string]struct{}, len(d.Providers))
	for i, provider := range d.Providers {
		path := fmt.Sprintf("providers[%d]", i)
		if err := validateCapabilityDefaults(provider.CapabilityDefaults); err != nil {
			return fmt.Errorf("%s.capability_defaults: %w", path, err)
		}
		if err := validateID(provider.Name); err != nil {
			return fmt.Errorf("%s.name: %w", path, err)
		}
		if _, exists := providers[provider.Name]; exists {
			return fmt.Errorf("%s.name: duplicate provider %q", path, provider.Name)
		}
		providers[provider.Name] = struct{}{}
		if err := validateSubscriptionProvider(provider); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if provider.Type == "sparkrun" && (provider.BaseURL != "" || provider.Auth.Type != "" || provider.Region != "" || provider.SubscriptionProfile != "") {
			return fmt.Errorf("%s: sparkrun obtains endpoints and authentication from its workloads", path)
		}
		if strings.TrimSpace(provider.Type) == "" {
			return fmt.Errorf("%s.type: required", path)
		}
		if provider.Type == "bedrock" {
			if err := validateAWSRegion(provider.Region); err != nil {
				return fmt.Errorf("%s.region: %w", path, err)
			}
			if provider.BaseURL != "" {
				if err := validateBaseURL(provider.BaseURL); err != nil {
					return fmt.Errorf("%s.base_url: %w", path, err)
				}
			}
			if provider.Auth.Type != AuthAWSSigV4 {
				return fmt.Errorf(
					"%s.auth.type: bedrock providers require aws_sigv4",
					path,
				)
			}
		} else {
			if provider.Region != "" {
				return fmt.Errorf("%s.region: only bedrock providers accept a region", path)
			}
			if provider.BaseURL != "" {
				if err := validateBaseURL(provider.BaseURL); err != nil {
					return fmt.Errorf("%s.base_url: %w", path, err)
				}
			}
			if provider.Auth.Type == AuthAWSSigV4 {
				return fmt.Errorf(
					"%s.auth.type: aws_sigv4 is only valid for bedrock providers",
					path,
				)
			}
		}
		if err := validateAuth(provider.Auth); err != nil {
			return fmt.Errorf("%s.auth: %w", path, err)
		}
		if err := validateHeaders(provider.DefaultHeaders); err != nil {
			return fmt.Errorf("%s.default_headers: %w", path, err)
		}
		if err := validateExtraBody(provider.ExtraBody); err != nil {
			return fmt.Errorf("%s.extra_body: %w", path, err)
		}
	}

	deployments := make(map[string]Deployment, len(d.Deployments))
	activationBindings := make(map[string]string)
	for i, deployment := range d.Deployments {
		path := fmt.Sprintf("deployments[%d]", i)
		if err := validateID(deployment.Name); err != nil {
			return fmt.Errorf("%s.name: %w", path, err)
		}
		if len(deployment.Title) > 4096 || strings.IndexFunc(deployment.Title, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s.title: must be at most 4096 bytes without control characters", path)
		}
		if _, exists := deployments[deployment.Name]; exists {
			return fmt.Errorf("%s.name: duplicate deployment %q", path, deployment.Name)
		}
		deployments[deployment.Name] = deployment
		if _, exists := providers[deployment.Provider]; !exists {
			return fmt.Errorf("%s.provider: unknown provider %q", path, deployment.Provider)
		}
		if strings.TrimSpace(deployment.Model) == "" {
			return fmt.Errorf("%s.model: required", path)
		}
		if err := validateNativeProtocols(deployment.NativeProtocols); err != nil {
			return fmt.Errorf("%s.native_protocols: %w", path, err)
		}
		provider := d.provider(deployment.Provider)
		if provider.Type == "openai_responses" && !deployment.SupportsNativeProtocol(provider, ProtocolOpenAI) {
			return fmt.Errorf("%s.native_protocols: OpenAI Responses providers require the openai protocol", path)
		}
		if provider.Type == "openai_subscription" {
			if deployment.Credential != "" || deployment.EndpointSource.Type.Effective() != EndpointSourceStatic ||
				len(deployment.NativeProtocols) != 0 && (len(deployment.NativeProtocols) != 1 || deployment.NativeProtocols[0] != ProtocolOpenAI) {
				return fmt.Errorf("%s: subscription deployments require static OpenAI endpoints and provider-owned authentication", path)
			}
			if !deployment.DeclaresCapability(CapabilityResponses) {
				return fmt.Errorf("%s.capabilities: subscription deployments must declare responses", path)
			}
		}
		if err := deployment.EndpointSource.Validate(); err != nil {
			return fmt.Errorf("%s.endpoint_source: %w", path, err)
		}
		if deployment.EndpointSource.Type.Effective() == EndpointSourceActivatable {
			bindingKey := deployment.EndpointSource.Controller + "\x00" +
				deployment.EndpointSource.Revision
			if previous, exists := activationBindings[bindingKey]; exists {
				return fmt.Errorf(
					"%s.endpoint_source.revision: activation binding is already used by deployment %q",
					path,
					previous,
				)
			}
			activationBindings[bindingKey] = deployment.Name
		}
		if deployment.EndpointSource.Type.Effective() == EndpointSourceStatic &&
			provider.BaseURL == "" && provider.Type != "bedrock" && provider.Type != "openai_subscription" {
			return fmt.Errorf("%s.endpoint_source: static deployments require provider base_url", path)
		}
		if provider.Type == "bedrock" &&
			deployment.EndpointSource.Type.Effective() != EndpointSourceStatic {
			return fmt.Errorf("%s.endpoint_source: bedrock deployments must use static endpoints", path)
		}
		if deployment.Credential != "" {
			if err := deployment.Credential.Validate(); err != nil {
				return fmt.Errorf("%s.credential: %w", path, err)
			}
			provider := d.provider(deployment.Provider)
			if provider.Auth.Type == AuthNone {
				return fmt.Errorf("%s.credential: provider auth is not configured", path)
			}
			if provider.Auth.Type == AuthAWSSigV4 &&
				!isAWSWorkloadReference(string(deployment.Credential)) {
				return fmt.Errorf(
					"%s.credential: aws_sigv4 requires a workload:// reference",
					path,
				)
			}
		}
		if err := validateHeaders(deployment.UpstreamHeaders); err != nil {
			return fmt.Errorf("%s.upstream_headers: %w", path, err)
		}
		if err := validateExtraBody(deployment.ExtraBody); err != nil {
			return fmt.Errorf("%s.extra_body: %w", path, err)
		}
		if err := validateCapabilities(deployment.Capabilities); err != nil {
			return fmt.Errorf("%s.capabilities: %w", path, err)
		}
		if err := validateCapabilityPolicy(
			deployment.CapabilityPolicy,
			deployment.Capabilities,
		); err != nil {
			return fmt.Errorf("%s.capability_policy: %w", path, err)
		}
		if deployment.MaxConcurrency < 0 || deployment.MaxConcurrency > maxConcurrency {
			return fmt.Errorf(
				"%s.max_concurrency: must be between 1 and %d, or 0 for unlimited",
				path,
				maxConcurrency,
			)
		}
		if err := deployment.Circuit.Validate(); err != nil {
			return fmt.Errorf("%s.circuit: %w", path, err)
		}
	}

	modelNames := make(map[string]string, len(d.VirtualModels))
	for i, model := range d.VirtualModels {
		path := fmt.Sprintf("virtual_models[%d]", i)
		if err := validateID(model.Name); err != nil {
			return fmt.Errorf("%s.name: %w", path, err)
		}
		if previous, exists := modelNames[model.Name]; exists {
			return fmt.Errorf("%s.name: %q already belongs to %s", path, model.Name, previous)
		}
		modelNames[model.Name] = model.Name
		switch model.Visibility {
		case "", ModelVisibilityPublic, ModelVisibilityHidden, ModelVisibilityInternal:
		default:
			return fmt.Errorf(
				"%s.visibility: must be public, hidden, internal, or empty",
				path,
			)
		}
		if err := ValidateRequestOverrides(model.RequestOverrides); err != nil {
			return fmt.Errorf("%s.request_overrides: %w", path, err)
		}
		if err := validateCapabilities(model.RequiredCapabilities); err != nil {
			return fmt.Errorf("%s.required_capabilities: %w", path, err)
		}
		for aliasIndex, alias := range model.Aliases {
			if err := validateID(alias); err != nil {
				return fmt.Errorf("%s.aliases[%d]: %w", path, aliasIndex, err)
			}
			if previous, exists := modelNames[alias]; exists {
				return fmt.Errorf("%s.aliases[%d]: %q already belongs to %s", path, aliasIndex, alias, previous)
			}
			modelNames[alias] = model.Name
		}
		switch model.ResponseModel {
		case "", ResponseModelVirtual, ResponseModelRequested, ResponseModelUpstream:
		default:
			return fmt.Errorf(
				"%s.response_model: must be virtual, requested, upstream, or empty",
				path,
			)
		}
		if err := model.Selection.Validate(); err != nil {
			return fmt.Errorf("%s.selection: %w", path, err)
		}
		if err := model.Limits.Validate(); err != nil {
			return fmt.Errorf("%s.limits: %w", path, err)
		}
		if err := model.Retry.Validate(); err != nil {
			return fmt.Errorf("%s.retry: %w", path, err)
		}
		if len(model.Pools) == 0 {
			return fmt.Errorf("%s.pools: at least one routing pool is required", path)
		}
		priorities := make(map[int]struct{}, len(model.Pools))
		targets := make(map[string]struct{})
		for poolIndex, pool := range model.Pools {
			if _, exists := priorities[pool.Priority]; exists {
				return fmt.Errorf(
					"%s.pools[%d].priority: duplicate priority %d",
					path,
					poolIndex,
					pool.Priority,
				)
			}
			priorities[pool.Priority] = struct{}{}
			if len(pool.Targets) == 0 {
				return fmt.Errorf("%s.pools[%d].targets: at least one target is required", path, poolIndex)
			}
			for targetIndex, target := range pool.Targets {
				if _, exists := deployments[target.Deployment]; !exists {
					return fmt.Errorf(
						"%s.pools[%d].targets[%d].deployment: unknown deployment %q",
						path,
						poolIndex,
						targetIndex,
						target.Deployment,
					)
				}
				if _, exists := targets[target.Deployment]; exists {
					return fmt.Errorf(
						"%s.pools[%d].targets[%d].deployment: duplicate deployment %q",
						path,
						poolIndex,
						targetIndex,
						target.Deployment,
					)
				}
				targets[target.Deployment] = struct{}{}
				if target.Weight <= 0 {
					return fmt.Errorf(
						"%s.pools[%d].targets[%d].weight: must be positive",
						path,
						poolIndex,
						targetIndex,
					)
				}
			}
		}
		if len(model.RequiredCapabilities) > 0 &&
			!hasCapableTarget(model, deployments) {
			return fmt.Errorf(
				"%s.required_capabilities: no routing target supports the complete required set",
				path,
			)
		}
	}
	if d.ModelRouting != nil {
		canonicalModels := make(map[string]struct{}, len(d.VirtualModels))
		for _, model := range d.VirtualModels {
			canonicalModels[model.Name] = struct{}{}
		}
		for name := range d.ModelRouting.Models {
			if _, exists := canonicalModels[name]; !exists {
				return fmt.Errorf(
					"model_routing.models[%q]: unknown canonical virtual model",
					name,
				)
			}
		}
	}
	for i, model := range d.VirtualModels {
		if err := validatePrivacy(
			fmt.Sprintf("virtual_models[%d].privacy", i),
			model.Privacy,
		); err != nil {
			return err
		}
		if err := validateGuardrails(
			fmt.Sprintf("virtual_models[%d].guardrails", i),
			model.Guardrails,
			modelNames,
		); err != nil {
			return err
		}
	}
	return nil
}

func validatePrivacy(path string, policy *PrivacyPolicy) error {
	if policy == nil || policy.PII == nil {
		return nil
	}
	pii := policy.PII
	switch pii.Mode {
	case "", PIIModeDisabled, PIIModeSubstitute:
	default:
		return fmt.Errorf("%s.pii.mode: must be disabled, substitute, or empty", path)
	}
	if pii.Mode == PIIModeDisabled {
		if len(pii.Entities) != 0 || pii.Scope != "" || pii.Response != "" ||
			pii.TraceContent != "" || pii.FailureMode != "" || pii.MediaText != "" ||
			pii.Files != nil {
			return fmt.Errorf("%s.pii: disabled policy must not contain other fields", path)
		}
		return nil
	}
	if len(pii.Entities) > 32 {
		return fmt.Errorf("%s.pii.entities: must not contain more than 32 entries", path)
	}
	seen := make(map[PIIEntity]struct{}, len(pii.Entities))
	for index, entity := range pii.Entities {
		switch entity {
		case PIIEntityEmail, PIIEntityPhone, PIIEntitySSN, PIIEntityCreditCard,
			PIIEntityIPv4, PIIEntityPerson, PIIEntityAddress:
		default:
			return fmt.Errorf("%s.pii.entities[%d]: unknown entity %q", path, index, entity)
		}
		if _, exists := seen[entity]; exists {
			return fmt.Errorf("%s.pii.entities[%d]: duplicate entity %q", path, index, entity)
		}
		seen[entity] = struct{}{}
	}
	if pii.Scope != "" && pii.Scope != PIIScopeRequest && pii.Scope != PIIScopeConversation {
		return fmt.Errorf("%s.pii.scope: must be request, conversation, or empty", path)
	}
	if pii.Response != "" && pii.Response != PIIResponseRestore && pii.Response != PIIResponseMasked {
		return fmt.Errorf("%s.pii.response: must be restore, masked, or empty", path)
	}
	if pii.TraceContent != "" && pii.TraceContent != PIITraceMasked &&
		pii.TraceContent != PIITraceCallerVisible && pii.TraceContent != PIITraceDisabled {
		return fmt.Errorf("%s.pii.trace_content: must be masked, caller_visible, disabled, or empty", path)
	}
	if pii.FailureMode != "" && pii.FailureMode != PIIFailOpen && pii.FailureMode != PIIFailClosed {
		return fmt.Errorf("%s.pii.failure_mode: must be fail_open, fail_closed, or empty", path)
	}
	if pii.MediaText != "" && pii.MediaText != PIIMediaTextBestEffort &&
		pii.MediaText != PIIMediaTextRequired {
		return fmt.Errorf("%s.pii.media_text: must be best_effort, required, or empty", path)
	}
	if pii.Files != nil {
		switch pii.Files.Text {
		case "", PIIFileTextDisabled, PIIFileTextBestEffort, PIIFileTextRequired:
		default:
			return fmt.Errorf("%s.pii.files.text: must be disabled, best_effort, required, or empty", path)
		}
		if pii.Files.MaxTextBytes < 0 || pii.Files.MaxTextBytes > maxPIIFileTextBytes {
			return fmt.Errorf("%s.pii.files.max_text_bytes: must be at most %d bytes", path, maxPIIFileTextBytes)
		}
		if (pii.Files.Text == PIIFileTextBestEffort || pii.Files.Text == PIIFileTextRequired ||
			pii.Files.Metadata) && pii.Scope != PIIScopeConversation {
			return fmt.Errorf("%s.pii.files: inspection requires scope conversation", path)
		}
	}
	return nil
}

func (s EndpointSource) Validate() error {
	sourceType := s.Type.Effective()
	switch sourceType {
	case EndpointSourceStatic:
		if s.Controller != "" || s.Revision != "" || s.Recipe != "" ||
			s.RecipeRevision != "" || len(s.ClusterCandidates) != 0 ||
			len(s.Overrides) != 0 || s.ActivationTimeout != 0 || s.IdleTTL != 0 || s.IdleAction != "" ||
			s.MaxQueuedWaiters != 0 || s.MaxQueuedBodyBytes != 0 || s.ColdStart != "" {
			return fmt.Errorf("static source must not contain lifecycle fields")
		}
		return nil
	case EndpointSourceDiscovered:
		if err := validateID(s.Controller); err != nil {
			return fmt.Errorf("controller: %w", err)
		}
		if s.Revision != "" || s.Recipe != "" || s.RecipeRevision != "" ||
			len(s.ClusterCandidates) != 0 || len(s.Overrides) != 0 ||
			s.ActivationTimeout != 0 || s.IdleTTL != 0 || s.IdleAction != "" || s.MaxQueuedWaiters != 0 ||
			s.MaxQueuedBodyBytes != 0 || s.ColdStart != "" {
			return fmt.Errorf("discovered source must not contain activation fields")
		}
		return nil
	case EndpointSourceActivatable:
		if err := validateID(s.Controller); err != nil {
			return fmt.Errorf("controller: %w", err)
		}
		if err := validateID(s.Revision); err != nil {
			return fmt.Errorf("revision: %w", err)
		}
		if err := validateID(s.Recipe); err != nil {
			return fmt.Errorf("recipe: %w", err)
		}
		if s.RecipeRevision != "" {
			if err := validateID(s.RecipeRevision); err != nil {
				return fmt.Errorf("recipe_revision: %w", err)
			}
		}
		if len(s.ClusterCandidates) > maxClusterCandidates {
			return fmt.Errorf("cluster_candidates: too many entries")
		}
		seenClusters := make(map[string]struct{}, len(s.ClusterCandidates))
		for index, candidate := range s.ClusterCandidates {
			if err := validateID(candidate); err != nil {
				return fmt.Errorf("cluster_candidates[%d]: %w", index, err)
			}
			if _, exists := seenClusters[candidate]; exists {
				return fmt.Errorf("cluster_candidates[%d]: duplicate %q", index, candidate)
			}
			seenClusters[candidate] = struct{}{}
		}
		if len(s.Overrides) > maxEndpointOverrides {
			return fmt.Errorf("overrides: too many entries")
		}
		overrideBytes := 0
		for key, value := range s.Overrides {
			if err := validateID(key); err != nil {
				return fmt.Errorf("overrides[%q]: invalid key: %w", key, err)
			}
			if len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
				return fmt.Errorf("overrides[%q]: value is invalid", key)
			}
			overrideBytes += len(key) + len(value)
			if overrideBytes > 64<<10 {
				return fmt.Errorf("overrides: values exceed 65536 bytes")
			}
		}
		if s.IdleAction != "" && s.IdleAction != "stop" && s.IdleAction != "sleep" {
			return fmt.Errorf("idle_action: must be stop or sleep")
		}
		if s.IdleAction == "sleep" && s.Controller != "sparkrun" {
			return fmt.Errorf("idle sleep requires the sparkrun controller")
		}
		activationTimeout := s.ActivationTimeout.Value()
		if activationTimeout < 0 || activationTimeout > maxConfiguredActivationTimeout {
			return fmt.Errorf("activation_timeout: must be between zero and %s", maxConfiguredActivationTimeout)
		}
		idleTTL := s.IdleTTL.Value()
		if idleTTL < 0 || idleTTL > maxConfiguredIdleTTL {
			return fmt.Errorf("idle_ttl: must be between zero and %s", maxConfiguredIdleTTL)
		}
		if s.MaxQueuedWaiters < 0 || s.MaxQueuedWaiters > maxQueuedWaiters {
			return fmt.Errorf("max_queued_waiters: must be between zero and %d", maxQueuedWaiters)
		}
		if s.MaxQueuedBodyBytes < 0 || s.MaxQueuedBodyBytes > maxQueuedBodyBytes {
			return fmt.Errorf("max_queued_body_bytes: must be between zero and %d", maxQueuedBodyBytes)
		}
		switch s.ColdStart.Effective() {
		case ColdStartWait, ColdStartReject:
		default:
			return fmt.Errorf("cold_start: must be wait, reject, or empty")
		}
		return nil
	default:
		return fmt.Errorf("type: must be static, discovered, activatable, or empty")
	}
}

func (s EndpointSource) EffectiveActivationTimeout() time.Duration {
	if s.ActivationTimeout == 0 {
		return defaultActivationTimeout
	}
	return s.ActivationTimeout.Value()
}

func (s EndpointSource) EffectiveMaxQueuedWaiters() int {
	if s.MaxQueuedWaiters == 0 {
		return defaultMaxQueuedWaiters
	}
	return s.MaxQueuedWaiters
}

func (s EndpointSource) EffectiveMaxQueuedBodyBytes() int64 {
	if s.MaxQueuedBodyBytes == 0 {
		return defaultMaxQueuedBodyBytes
	}
	return s.MaxQueuedBodyBytes
}

func validateGuardrails(
	path string,
	policy GuardrailPolicy,
	modelNames map[string]string,
) error {
	if policy.Stream != nil {
		if len(policy.Post) == 0 {
			return fmt.Errorf(
				"%s.stream: requires at least one post guardrail",
				path,
			)
		}
		if policy.Stream.WindowBytes != 0 &&
			(policy.Stream.WindowBytes < minGuardrailWindow ||
				policy.Stream.WindowBytes > maxGuardrailWindow) {
			return fmt.Errorf(
				"%s.stream.window_bytes: must be zero or between %d and %d",
				path,
				minGuardrailWindow,
				maxGuardrailWindow,
			)
		}
		if policy.Stream.ContextBytes < 0 ||
			policy.Stream.ContextBytes > maxGuardrailContext {
			return fmt.Errorf(
				"%s.stream.context_bytes: must be between zero and %d",
				path,
				maxGuardrailContext,
			)
		}
	}
	names := make(map[string]struct{}, len(policy.Pre)+len(policy.Post))
	phases := []struct {
		name       string
		guardrails []Guardrail
	}{
		{name: "pre", guardrails: policy.Pre},
		{name: "post", guardrails: policy.Post},
	}
	for _, phase := range phases {
		phaseName := phase.name
		guardrails := phase.guardrails
		if len(guardrails) > maxGuardrails {
			return fmt.Errorf(
				"%s.%s: must not contain more than %d entries",
				path,
				phaseName,
				maxGuardrails,
			)
		}
		for index, guardrail := range guardrails {
			entryPath := fmt.Sprintf("%s.%s[%d]", path, phaseName, index)
			if err := validateID(guardrail.Name); err != nil {
				return fmt.Errorf("%s.name: %w", entryPath, err)
			}
			if _, exists := names[guardrail.Name]; exists {
				return fmt.Errorf(
					"%s.name: duplicate guardrail name %q",
					entryPath,
					guardrail.Name,
				)
			}
			names[guardrail.Name] = struct{}{}
			if err := validateID(guardrail.Model); err != nil {
				return fmt.Errorf("%s.model: %w", entryPath, err)
			}
			if _, exists := modelNames[guardrail.Model]; !exists {
				return fmt.Errorf(
					"%s.model: unknown virtual model or alias %q",
					entryPath,
					guardrail.Model,
				)
			}
			if len(guardrail.Prompt) > maxGuardrailPrompt {
				return fmt.Errorf(
					"%s.prompt: must not exceed %d bytes",
					entryPath,
					maxGuardrailPrompt,
				)
			}
			switch guardrail.FailureMode {
			case "", GuardrailFailOpen, GuardrailFailClosed:
			default:
				return fmt.Errorf(
					"%s.failure_mode: must be fail_open, fail_closed, or empty",
					entryPath,
				)
			}
		}
	}
	return nil
}

// Validate checks one virtual-model limit policy independently of a document.
func (p ModelLimits) Validate() error {
	if p.MaxAttempts < 0 || p.MaxAttempts > maxAttempts {
		return fmt.Errorf(
			"max_attempts must be between 1 and %d, or 0 for the default",
			maxAttempts,
		)
	}
	effective := p.Effective()
	if effective.OverallTimeout <= 0 ||
		effective.OverallTimeout > maxConfiguredTimeout {
		return fmt.Errorf(
			"overall_timeout must be positive and at most %s",
			maxConfiguredTimeout,
		)
	}
	if effective.PerTryTimeout <= 0 ||
		effective.PerTryTimeout > maxConfiguredTimeout {
		return fmt.Errorf(
			"per_try_timeout must be positive and at most %s",
			maxConfiguredTimeout,
		)
	}
	if effective.StreamIdleTimeout <= 0 ||
		effective.StreamIdleTimeout > maxConfiguredTimeout {
		return fmt.Errorf(
			"stream_idle_timeout must be positive and at most %s",
			maxConfiguredTimeout,
		)
	}
	return nil
}

func (p ModelLimits) Effective() EffectiveModelLimits {
	attempts := p.MaxAttempts
	if attempts == 0 {
		attempts = 3
	}
	overall := p.OverallTimeout.Value()
	if overall == 0 {
		overall = defaultOverallTimeout
	}
	perTry := p.PerTryTimeout.Value()
	if perTry == 0 {
		perTry = overall
	}
	streamIdle := p.StreamIdleTimeout.Value()
	if streamIdle == 0 {
		streamIdle = defaultStreamIdleTimeout
	}
	return EffectiveModelLimits{
		MaxAttempts:       attempts,
		OverallTimeout:    overall,
		PerTryTimeout:     perTry,
		StreamIdleTimeout: streamIdle,
	}
}

// Validate checks target selection policy independently of a document.
func (p SelectionPolicy) Validate() error {
	switch p.Mode {
	case "", SelectionWeightedRandom:
		if p.HashKey != "" {
			return fmt.Errorf("hash_key requires mode weighted_hash")
		}
	case SelectionWeightedHash:
		if p.HashKey == "" {
			return fmt.Errorf("hash_key is required for mode weighted_hash")
		}
	default:
		return fmt.Errorf("mode must be weighted_random, weighted_hash, or empty")
	}
	switch p.HashKey {
	case "", SelectionHashTenant, SelectionHashPrincipal, SelectionHashUser,
		SelectionHashWorkspace, SelectionHashThreadID, SelectionHashTaskID,
		SelectionHashExperiment, SelectionHashSession:
	default:
		return fmt.Errorf(
			"hash_key must be tenant, principal, user, workspace, thread_id, task_id, experiment, or session",
		)
	}
	if err := p.PromptCacheAffinity.Validate(); err != nil {
		return fmt.Errorf("prompt_cache_affinity: %w", err)
	}
	return nil
}

func (p PromptCacheAffinityPolicy) Validate() error {
	effective := p.Effective()
	if effective.TTL <= 0 || effective.TTL > maxPromptCacheAffinityTTL {
		return fmt.Errorf(
			"ttl must be positive and at most %s",
			maxPromptCacheAffinityTTL,
		)
	}
	if effective.MinPrefixBytes < 1 ||
		effective.MinPrefixBytes > maxPromptCacheMinPrefixBytes {
		return fmt.Errorf(
			"min_prefix_bytes must be between 1 and %d, or 0 for the default",
			maxPromptCacheMinPrefixBytes,
		)
	}
	if effective.MaxPrefixesPerRequest < 1 ||
		effective.MaxPrefixesPerRequest > maxPromptCacheMaxPrefixes {
		return fmt.Errorf(
			"max_prefixes_per_request must be between 1 and %d, or 0 for the default",
			maxPromptCacheMaxPrefixes,
		)
	}
	switch effective.Scope {
	case PromptCacheAffinityCaller, PromptCacheAffinityTenant:
	default:
		return fmt.Errorf("scope must be caller, tenant, or empty")
	}
	return nil
}

func (p PromptCacheAffinityPolicy) Effective() EffectivePromptCacheAffinityPolicy {
	ttl := p.TTL.Value()
	if ttl == 0 {
		ttl = defaultPromptCacheAffinityTTL
	}
	minimum := p.MinPrefixBytes
	if minimum == 0 {
		minimum = defaultPromptCacheMinPrefixBytes
	}
	maximum := p.MaxPrefixesPerRequest
	if maximum == 0 {
		maximum = defaultPromptCacheMaxPrefixes
	}
	scope := p.Scope
	if scope == "" {
		scope = PromptCacheAffinityCaller
	}
	return EffectivePromptCacheAffinityPolicy{
		Enabled:               p.Enabled,
		TTL:                   ttl,
		MinPrefixBytes:        minimum,
		MaxPrefixesPerRequest: maximum,
		Scope:                 scope,
	}
}

// Validate checks one virtual-model retry policy independently of a document.
func (p RetryPolicy) Validate() error {
	effective := p.Effective()
	if effective.BaseBackoff <= 0 ||
		effective.BaseBackoff > maxConfiguredTimeout {
		return fmt.Errorf(
			"base_backoff must be positive and at most %s",
			maxConfiguredTimeout,
		)
	}
	if effective.MaxBackoff < effective.BaseBackoff ||
		effective.MaxBackoff > maxConfiguredTimeout {
		return fmt.Errorf(
			"max_backoff must be at least base_backoff and at most %s",
			maxConfiguredTimeout,
		)
	}
	if effective.MaxRetryAfter <= 0 ||
		effective.MaxRetryAfter > maxConfiguredTimeout {
		return fmt.Errorf(
			"max_retry_after must be positive and at most %s",
			maxConfiguredTimeout,
		)
	}
	if effective.Budget.Ratio <= 0 ||
		effective.Budget.Ratio > maxRetryBudgetRatio {
		return fmt.Errorf(
			"budget.ratio must be greater than 0 and at most %g, or 0 for the default",
			float64(maxRetryBudgetRatio),
		)
	}
	if effective.Budget.MinConcurrency < 1 ||
		effective.Budget.MinConcurrency > maxRetryConcurrency {
		return fmt.Errorf(
			"budget.min_concurrency must be between 1 and %d, or 0 for the default",
			maxRetryConcurrency,
		)
	}
	return nil
}

func (p RetryPolicy) Effective() EffectiveRetryPolicy {
	base := p.BaseBackoff.Value()
	if base == 0 {
		base = defaultRetryBaseBackoff
	}
	maximum := p.MaxBackoff.Value()
	if maximum == 0 {
		maximum = defaultRetryMaxBackoff
		if maximum < base {
			maximum = base
		}
	}
	maxRetryAfter := p.MaxRetryAfter.Value()
	if maxRetryAfter == 0 {
		maxRetryAfter = defaultRetryMaxRetryAfter
	}
	ratio := p.Budget.Ratio
	if ratio == 0 {
		ratio = defaultRetryBudgetRatio
	}
	minimum := p.Budget.MinConcurrency
	if minimum == 0 {
		minimum = defaultRetryMinConcurrency
	}
	return EffectiveRetryPolicy{
		Disabled:      p.Disabled,
		BaseBackoff:   base,
		MaxBackoff:    maximum,
		MaxRetryAfter: maxRetryAfter,
		Budget: EffectiveRetryBudgetPolicy{
			Disabled:       p.Budget.Disabled,
			Ratio:          ratio,
			MinConcurrency: minimum,
		},
	}
}

// Validate checks one circuit policy independently of a complete document.
func (p CircuitPolicy) Validate() error {
	effective := p.Effective()
	consecutive := effective.ConsecutiveFailures
	if consecutive < 1 || consecutive > maxCircuitSamples {
		return fmt.Errorf("consecutive_failures must be between 1 and %d, or 0 for default", maxCircuitSamples)
	}
	minimum := effective.MinimumSamples
	if minimum < 1 || minimum > maxCircuitSamples {
		return fmt.Errorf("minimum_samples must be between 1 and %d, or 0 for default", maxCircuitSamples)
	}
	window := effective.SampleWindow
	if window < minimum || window > maxCircuitSamples {
		return fmt.Errorf(
			"sample_window must be between effective minimum_samples (%d) and %d",
			minimum,
			maxCircuitSamples,
		)
	}
	rate := effective.FailureRate
	if rate <= 0 || rate > 1 {
		return fmt.Errorf("failure_rate must be greater than 0 and at most 1, or 0 for default")
	}
	base := effective.BaseEjectionTime
	maximum := effective.MaxEjectionTime
	if base <= 0 || base > maxConfiguredEjectionTime {
		return fmt.Errorf("base_ejection_time must be positive and at most %s", maxConfiguredEjectionTime)
	}
	if maximum < base || maximum > maxConfiguredEjectionTime {
		return fmt.Errorf(
			"max_ejection_time must be at least base_ejection_time and at most %s",
			maxConfiguredEjectionTime,
		)
	}
	return nil
}

func (p CircuitPolicy) Effective() EffectiveCircuitPolicy {
	consecutive := p.ConsecutiveFailures
	if consecutive == 0 {
		consecutive = defaultCircuitConsecutiveFailures
	}
	minimum := p.MinimumSamples
	if minimum == 0 {
		minimum = defaultCircuitMinimumSamples
	}
	window := p.SampleWindow
	if window == 0 {
		window = defaultCircuitSampleWindow
		if window < minimum {
			window = minimum
		}
	}
	rate := p.FailureRate
	if rate == 0 {
		rate = defaultCircuitFailureRate
	}
	base := p.BaseEjectionTime.Value()
	if base == 0 {
		base = defaultCircuitBaseEjectionTime
	}
	maximum := p.MaxEjectionTime.Value()
	if maximum == 0 {
		maximum = defaultCircuitMaxEjectionTime
		if maximum < base {
			maximum = base
		}
	}
	return EffectiveCircuitPolicy{
		Disabled:            p.Disabled,
		ConsecutiveFailures: consecutive,
		MinimumSamples:      minimum,
		SampleWindow:        window,
		FailureRate:         rate,
		BaseEjectionTime:    base,
		MaxEjectionTime:     maximum,
	}
}

func validateCapabilities(capabilities []Capability) error {
	if len(capabilities) > maxCapabilities {
		return fmt.Errorf("too many capabilities: %d > %d", len(capabilities), maxCapabilities)
	}
	seen := make(map[Capability]struct{}, len(capabilities))
	for index, capability := range capabilities {
		if err := validateCapability(capability); err != nil {
			return fmt.Errorf("[%d]: %w", index, err)
		}
		if _, exists := seen[capability]; exists {
			return fmt.Errorf("[%d]: duplicate capability %q", index, capability)
		}
		seen[capability] = struct{}{}
	}
	return nil
}

// ValidateCapabilitySet validates a request-time capability set using the same
// vocabulary, extension, duplicate, and size rules as configured deployments
// and virtual models. It is useful for administrative planning tools which do
// not mutate a configuration document merely to validate simulation input.
func ValidateCapabilitySet(capabilities []Capability) error {
	return validateCapabilities(capabilities)
}

func validateNativeProtocols(protocols []Protocol) error {
	seen := make(map[Protocol]struct{}, len(protocols))
	for index, protocol := range protocols {
		if _, exists := standardProtocols[protocol]; !exists {
			return fmt.Errorf(
				"[%d]: unsupported protocol %q; must be openai, anthropic, gemini, or bedrock",
				index,
				protocol,
			)
		}
		if _, exists := seen[protocol]; exists {
			return fmt.Errorf("[%d]: duplicate protocol %q", index, protocol)
		}
		seen[protocol] = struct{}{}
	}
	return nil
}

func validateCapabilityPolicy(
	policy CapabilityPolicy,
	supported []Capability,
) error {
	switch policy.Unknown {
	case "", UnknownCapabilityReject, UnknownCapabilityTry:
	default:
		return fmt.Errorf("unknown must be reject, try, or empty")
	}
	if err := validateCapabilities(policy.Unsupported); err != nil {
		return fmt.Errorf("unsupported: %w", err)
	}
	declared := make(map[Capability]struct{}, len(supported))
	for _, capability := range supported {
		declared[capability] = struct{}{}
	}
	for index, capability := range policy.Unsupported {
		if _, exists := declared[capability]; exists {
			return fmt.Errorf(
				"unsupported[%d]: capability %q is also declared supported",
				index,
				capability,
			)
		}
	}
	return nil
}

func validateCapabilityDefaults(defaults CapabilityDefaults) error {
	switch defaults.Unknown {
	case "", UnknownCapabilityReject, UnknownCapabilityTry:
		return nil
	default:
		return fmt.Errorf("unknown must be reject, try, or empty")
	}
}

func validateCapability(capability Capability) error {
	value := string(capability)
	if value == "" || len(value) > maxCapabilityLength {
		return fmt.Errorf("capability length must be between 1 and %d", maxCapabilityLength)
	}
	if value != strings.ToLower(value) {
		return fmt.Errorf("capability %q must be lowercase", value)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= '0' && char <= '9' ||
			char == '_' || char == '-' || char == '.' {
			continue
		}
		return fmt.Errorf("capability %q contains invalid character %q", value, char)
	}
	if _, standard := standardCapabilities[capability]; standard {
		return nil
	}
	if strings.HasPrefix(value, "x-") && len(value) > 2 {
		return nil
	}
	return fmt.Errorf("unknown capability %q; extensions must use the x- prefix", value)
}

func hasCapableTarget(
	model VirtualModel,
	deployments map[string]Deployment,
) bool {
	for _, pool := range model.Pools {
		for _, target := range pool.Targets {
			deployment, exists := deployments[target.Deployment]
			if exists && deployment.MatchCapabilities(model.RequiredCapabilities) != CapabilityMatchRejected {
				return true
			}
		}
	}
	return false
}

func (d Document) provider(name string) Provider {
	for _, provider := range d.Providers {
		if provider.Name == name {
			return provider
		}
	}
	return Provider{}
}

func validateAuth(auth ProviderAuth) error {
	switch auth.Type {
	case AuthNone:
		if auth.Header != "" || auth.Prefix != "" || auth.Credential != "" {
			return fmt.Errorf("type is required when auth fields are set")
		}
		return nil
	case AuthBearer:
		if auth.Header != "" || auth.Prefix != "" {
			return fmt.Errorf("bearer auth does not accept header or prefix overrides")
		}
	case AuthHeader:
		if err := validateHeaderName(auth.Header); err != nil {
			return fmt.Errorf("header: %w", err)
		}
		lowerName := strings.ToLower(auth.Header)
		switch lowerName {
		case "connection", "content-length", "host", "proxy-connection", "te",
			"trailer", "transfer-encoding", "upgrade":
			return fmt.Errorf("header %q is transport-controlled", auth.Header)
		}
		if err := validateHeaderValue(auth.Prefix); err != nil {
			return fmt.Errorf("prefix: %w", err)
		}
	case AuthAWSSigV4:
		if auth.Header != "" || auth.Prefix != "" {
			return fmt.Errorf(
				"aws_sigv4 auth does not accept header or prefix overrides",
			)
		}
	default:
		return fmt.Errorf("type must be bearer, header, aws_sigv4, or empty")
	}
	if auth.Credential == "" {
		return fmt.Errorf("credential is required")
	}
	if err := auth.Credential.Validate(); err != nil {
		return fmt.Errorf("credential: %w", err)
	}
	if auth.Type == AuthAWSSigV4 &&
		!isAWSWorkloadReference(string(auth.Credential)) {
		return fmt.Errorf("aws_sigv4 requires a workload:// credential")
	}
	return nil
}

func isAWSWorkloadReference(ref string) bool {
	value := strings.ToLower(ref)
	return value == "workload://aws" ||
		value == "workload://aws/default"
}

func validateAWSRegion(value string) error {
	if value == "" {
		return fmt.Errorf("required")
	}
	if len(value) > 64 || value != strings.ToLower(value) {
		return fmt.Errorf("must be a lowercase AWS region identifier")
	}
	for index, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return fmt.Errorf("must be a lowercase AWS region identifier")
	}
	return nil
}

func validateID(value string) error {
	if value == "" {
		return fmt.Errorf("required")
	}
	if len(value) > maxIDLength {
		return fmt.Errorf("must not exceed %d bytes", maxIDLength)
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("must not have surrounding whitespace")
	}
	for _, ch := range value {
		if unicode.IsSpace(ch) || unicode.IsControl(ch) {
			return fmt.Errorf("must not contain whitespace or control characters")
		}
	}
	return nil
}

func validateBaseURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if parsed.Host == "" {
		return fmt.Errorf("host is required")
	}
	if parsed.User != nil {
		return fmt.Errorf("userinfo is not allowed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("query and fragment are not allowed")
	}
	return nil
}

func validateExtraBody(defaults map[string]json.RawMessage) error {
	if len(defaults) > maxExtraBodyFields {
		return fmt.Errorf("too many fields: %d > %d", len(defaults), maxExtraBodyFields)
	}
	total := 0
	for name, value := range defaults {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" || trimmed != name || len(name) > maxExtraBodyName || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid field name %q", name)
		}
		if _, protected := protectedExtraBodyFields[strings.ToLower(name)]; protected {
			return fmt.Errorf("field %q is protocol-owned", name)
		}
		if len(value) > maxExtraBodyValue || !json.Valid(value) {
			return fmt.Errorf("field %q is invalid JSON or exceeds %d bytes", name, maxExtraBodyValue)
		}
		total += len(name) + len(value)
		if total > maxExtraBodyBytes {
			return fmt.Errorf("fields exceed %d bytes", maxExtraBodyBytes)
		}
	}
	return nil
}

func validateHeaders(headers map[string]HeaderValue) error {
	if len(headers) > maxHeaders {
		return fmt.Errorf("too many headers: %d > %d", len(headers), maxHeaders)
	}
	seen := make(map[string]string, len(headers))
	for name, value := range headers {
		lowerName := strings.ToLower(name)
		if previous, exists := seen[lowerName]; exists {
			return fmt.Errorf("duplicate case-insensitive names %q and %q", previous, name)
		}
		seen[lowerName] = name
		if err := validateHeaderName(name); err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
		if _, protected := protectedHeaders[lowerName]; protected {
			return fmt.Errorf("%q is controlled by the gateway or provider adapter", name)
		}
		hasValue := value.Value != ""
		hasRef := value.ValueFrom != ""
		if hasValue == hasRef {
			return fmt.Errorf("%q must set exactly one of value or value_from", name)
		}
		if hasValue {
			if isAuthenticationLike(lowerName) {
				return fmt.Errorf("%q must use value_from", name)
			}
			if err := validateHeaderValue(value.Value); err != nil {
				return fmt.Errorf("%q.value: %w", name, err)
			}
		} else if err := value.ValueFrom.Validate(); err != nil {
			return fmt.Errorf("%q.value_from: %w", name, err)
		}
		switch value.Scope {
		case "", HeaderScopeBoth, HeaderScopeInference, HeaderScopeHealth:
		default:
			return fmt.Errorf("%q.scope: must be inference, health, or empty for both", name)
		}
	}
	return nil
}

func validateHeaderName(name string) error {
	if len(name) == 0 || len(name) > maxHeaderName {
		return fmt.Errorf("length must be between 1 and %d", maxHeaderName)
	}
	for _, ch := range name {
		if !isTokenChar(ch) {
			return fmt.Errorf("contains invalid HTTP token character %q", ch)
		}
	}
	return nil
}

func isTokenChar(ch rune) bool {
	if ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", ch)
}

func validateHeaderValue(value string) error {
	if len(value) > maxHeaderValue {
		return fmt.Errorf("exceeds %d bytes", maxHeaderValue)
	}
	for _, ch := range value {
		if ch == '\r' || ch == '\n' || unicode.IsControl(ch) && ch != '\t' {
			return fmt.Errorf("contains a control character")
		}
	}
	return nil
}

func isAuthenticationLike(lowerName string) bool {
	return strings.HasSuffix(lowerName, "-key") ||
		strings.HasSuffix(lowerName, "-secret") ||
		strings.HasSuffix(lowerName, "-token")
}

// ValidateRequestOverrides applies the same rules to virtual models and recipe defaults.
func ValidateRequestOverrides(values map[string]map[string]json.RawMessage) error {
	for operation, overrides := range values {
		for _, key := range []string{"contents", "system", "system_instruction", "systemInstruction", "instructions", "history", "conversation", "previous_response_id"} {
			if _, exists := overrides[key]; exists {
				return fmt.Errorf("%s: request structure field %s cannot be overridden", operation, key)
			}
		}

		switch operation {
		case "chat_completions", "responses", "responses_compact", "messages", "messages_count_tokens", "generate_content", "stream_generate_content", "count_tokens", "converse", "converse_stream", "embeddings", "embed_content", "batch_embed_contents":
		default:
			return fmt.Errorf("unsupported operation %q", operation)
		}
		if err := validateExtraBody(overrides); err != nil {
			return fmt.Errorf("%s: %w", operation, err)
		}
	}
	return nil
}
