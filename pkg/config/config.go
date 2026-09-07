// Package config defines the storage-neutral gateway configuration model.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

// Version identifies the immutable bytes loaded by a Source.
type Version string

// Source loads one authoritative configuration document.
type Source interface {
	Load(ctx context.Context) (Document, Version, error)
}

// WatchSource optionally emits active-version change hints. Notifications may
// be coalesced or missed; consumers must periodically reconcile with Load.
type WatchSource interface {
	Source
	Watch(ctx context.Context) (<-chan Version, error)
}

// Document is the first public configuration schema. It intentionally covers
// only the fields exercised by the foundation data plane.
type Document struct {
	CapabilityDefaults CapabilityDefaults `json:"capability_defaults,omitempty,omitzero"`
	// ModelRouting optionally publishes a selector namespace (for example
	// "auto") whose choices are ordinary virtual models in this document.
	ModelRouting  *modelrouter.RoutingPolicy `json:"model_routing,omitempty"`
	Providers     []Provider                 `json:"providers"`
	Deployments   []Deployment               `json:"deployments"`
	VirtualModels []VirtualModel             `json:"virtual_models"`
}

type Provider struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url,omitempty"`
	// SubscriptionProfile names a private, renewable credential stored outside configuration.
	SubscriptionProfile string                 `json:"subscription_profile,omitempty"`
	Region              string                 `json:"region,omitempty"`
	Auth                ProviderAuth           `json:"auth,omitempty"`
	DefaultHeaders      map[string]HeaderValue `json:"default_headers,omitempty"`
	// ExtraBody supplies bounded top-level inference request defaults. Caller
	// fields always win, and deployment defaults take precedence over provider
	// defaults when both define a missing field.
	ExtraBody          map[string]json.RawMessage `json:"extra_body,omitempty"`
	CapabilityDefaults CapabilityDefaults         `json:"capability_defaults,omitempty,omitzero"`
}

type Deployment struct {
	Name             string                     `json:"name"`
	Provider         string                     `json:"provider"`
	Model            string                     `json:"model"`
	NativeProtocols  []Protocol                 `json:"native_protocols,omitempty"`
	EndpointSource   EndpointSource             `json:"endpoint_source,omitempty,omitzero"`
	Credential       credentials.Ref            `json:"credential,omitempty"`
	UpstreamHeaders  map[string]HeaderValue     `json:"upstream_headers,omitempty"`
	ExtraBody        map[string]json.RawMessage `json:"extra_body,omitempty"`
	Capabilities     []Capability               `json:"capabilities,omitempty"`
	CapabilityPolicy CapabilityPolicy           `json:"capability_policy,omitempty,omitzero"`
	MaxConcurrency   int                        `json:"max_concurrency,omitempty"`
	Circuit          CircuitPolicy              `json:"circuit,omitempty"`
}

// Protocol identifies an upstream inference HTTP dialect. Protocol support is
// deployment-specific because one runtime endpoint can expose more than one
// dialect (for example, both OpenAI and Anthropic compatibility APIs).
type Protocol string

const (
	ProtocolOpenAI    Protocol = "openai"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolGemini    Protocol = "gemini"
	ProtocolBedrock   Protocol = "bedrock"
)

// ProtocolForProviderType returns the backward-compatible native protocol for
// a provider type. Explicit Deployment.NativeProtocols override this default.
func ProtocolForProviderType(providerType string) Protocol {
	switch providerType {
	case "openai", "openai_compatible", "openai_responses", "openai_subscription":
		return ProtocolOpenAI
	case "anthropic":
		return ProtocolAnthropic
	case "gemini":
		return ProtocolGemini
	case "bedrock":
		return ProtocolBedrock
	default:
		return ""
	}
}

// EffectiveNativeProtocols returns the configured native protocols, or the
// provider-type-derived default when native_protocols is omitted.
func (d Deployment) EffectiveNativeProtocols(provider Provider) []Protocol {
	if len(d.NativeProtocols) != 0 {
		return append([]Protocol(nil), d.NativeProtocols...)
	}
	protocol := ProtocolForProviderType(provider.Type)
	if protocol == "" {
		return nil
	}
	return []Protocol{protocol}
}

func (d Deployment) SupportsNativeProtocol(provider Provider, protocol Protocol) bool {
	for _, candidate := range d.EffectiveNativeProtocols(provider) {
		if candidate == protocol {
			return true
		}
	}
	return false
}

// EndpointSource describes how a deployment obtains its serving endpoint.
// The zero value is a static provider URL for backward compatibility.
type EndpointSource struct {
	Type               EndpointSourceType `json:"type,omitempty"`
	Controller         string             `json:"controller,omitempty"`
	Revision           string             `json:"revision,omitempty"`
	Recipe             string             `json:"recipe,omitempty"`
	RecipeRevision     string             `json:"recipe_revision,omitempty"`
	ClusterCandidates  []string           `json:"cluster_candidates,omitempty"`
	Overrides          map[string]string  `json:"overrides,omitempty"`
	ActivationTimeout  Duration           `json:"activation_timeout,omitempty"`
	IdleTTL            Duration           `json:"idle_ttl,omitempty"`
	MaxQueuedWaiters   int                `json:"max_queued_waiters,omitempty"`
	MaxQueuedBodyBytes int64              `json:"max_queued_body_bytes,omitempty"`
	ColdStart          ColdStartPolicy    `json:"cold_start,omitempty"`
}

func (s EndpointSource) IsZero() bool {
	return s.Type == "" && s.Controller == "" && s.Revision == "" &&
		s.Recipe == "" && s.RecipeRevision == "" &&
		len(s.ClusterCandidates) == 0 && len(s.Overrides) == 0 &&
		s.ActivationTimeout == 0 && s.IdleTTL == 0 &&
		s.MaxQueuedWaiters == 0 && s.MaxQueuedBodyBytes == 0 && s.ColdStart == ""
}

type EndpointSourceType string

const (
	EndpointSourceStatic      EndpointSourceType = "static"
	EndpointSourceDiscovered  EndpointSourceType = "discovered"
	EndpointSourceActivatable EndpointSourceType = "activatable"
)

func (t EndpointSourceType) Effective() EndpointSourceType {
	if t == "" {
		return EndpointSourceStatic
	}
	return t
}

type ColdStartPolicy string

const (
	ColdStartWait   ColdStartPolicy = "wait"
	ColdStartReject ColdStartPolicy = "reject"
)

func (p ColdStartPolicy) Effective() ColdStartPolicy {
	if p == "" {
		return ColdStartWait
	}
	return p
}

// Capability is one request semantic that a deployment explicitly supports.
// Baseline text Chat Completions require no flag; endpoint and optional request
// semantics are gated before a billable upstream request.
type Capability string

// CapabilityPolicy controls how a deployment handles safe-to-attempt request
// capabilities that it has not explicitly declared. It is intended for local
// integrations such as Sparkrun where support can depend on a model/runtime
// combination that is not yet fully classified.
type CapabilityPolicy struct {
	Unknown     UnknownCapabilityPolicy `json:"unknown,omitempty"`
	Unsupported []Capability            `json:"unsupported,omitempty"`
}

// CapabilityDefaults supplies the unknown-capability behavior inherited by
// deployments that do not set capability_policy.unknown explicitly. Document
// defaults apply to every provider; provider defaults take precedence.
type CapabilityDefaults struct {
	Unknown UnknownCapabilityPolicy `json:"unknown,omitempty"`
}

func (d CapabilityDefaults) IsZero() bool {
	return d.Unknown == ""
}

func (p CapabilityPolicy) IsZero() bool {
	return p.Unknown == "" && len(p.Unsupported) == 0
}

type UnknownCapabilityPolicy string

const (
	UnknownCapabilityReject UnknownCapabilityPolicy = "reject"
	UnknownCapabilityTry    UnknownCapabilityPolicy = "try"
)

func (p UnknownCapabilityPolicy) Effective() UnknownCapabilityPolicy {
	if p == "" {
		return UnknownCapabilityTry
	}
	return p
}

// ResolveCapabilityPolicies materializes routing precedence: deployment
// override, provider default, document default, then the supplied edition
// default. Native Responses providers also imply the Responses capability.
// The deployment slice is copied; the source document is not mutated.
func (d Document) ResolveCapabilityPolicies(
	editionDefault UnknownCapabilityPolicy,
) Document {
	if editionDefault == "" {
		editionDefault = UnknownCapabilityTry
	}
	globalDefault := d.CapabilityDefaults.Unknown
	if globalDefault == "" {
		globalDefault = editionDefault
	}
	providerDefaults := make(map[string]UnknownCapabilityPolicy, len(d.Providers))
	providerResponses := make(map[string]bool, len(d.Providers))
	for _, provider := range d.Providers {
		value := provider.CapabilityDefaults.Unknown
		if value == "" {
			value = globalDefault
		}
		providerDefaults[provider.Name] = value
		providerResponses[provider.Name] = provider.Type == "openai_responses"
	}
	d.Deployments = append([]Deployment(nil), d.Deployments...)
	for index := range d.Deployments {
		deployment := &d.Deployments[index]
		if providerResponses[deployment.Provider] && !slices.Contains(deployment.Capabilities, CapabilityResponses) {
			deployment.Capabilities = append(slices.Clone(deployment.Capabilities), CapabilityResponses)
		}
		if d.Deployments[index].CapabilityPolicy.Unknown != "" {
			continue
		}
		value := providerDefaults[d.Deployments[index].Provider]
		if value == "" {
			value = globalDefault
		}
		d.Deployments[index].CapabilityPolicy.Unknown = value
	}
	return d
}

// CapabilityMatch describes why a deployment is eligible for a required set.
// Declared matches are preferred over permissive unknown matches by routing.
type CapabilityMatch uint8

const (
	CapabilityMatchRejected CapabilityMatch = iota
	CapabilityMatchUnknown
	CapabilityMatchDeclared
)

const (
	CapabilityTools                 Capability = "tools"
	CapabilityVision                Capability = "vision"
	CapabilityAudioInput            Capability = "audio_input"
	CapabilityAudioOutput           Capability = "audio_output"
	CapabilityFileInput             Capability = "file_input"
	CapabilityFiles                 Capability = "files"
	CapabilityDeveloperMessages     Capability = "developer_messages"
	CapabilityStructuredOutputs     Capability = "structured_outputs"
	CapabilityJSONMode              Capability = "json_mode"
	CapabilityReasoning             Capability = "reasoning"
	CapabilityLogprobs              Capability = "logprobs"
	CapabilitySeed                  Capability = "seed"
	CapabilityMultipleChoices       Capability = "multiple_choices"
	CapabilityParallelTools         Capability = "parallel_tool_calls"
	CapabilityPrediction            Capability = "prediction"
	CapabilityServiceTier           Capability = "service_tier"
	CapabilityProviderTools         Capability = "provider_hosted_tools"
	CapabilityStreamUsage           Capability = "stream_usage"
	CapabilityStoredCompletion      Capability = "stored_completions"
	CapabilityResponses             Capability = "responses"
	CapabilityResponsesCompact      Capability = "responses_compact"
	CapabilityBackgroundResponses   Capability = "background_responses"
	CapabilityConversations         Capability = "conversations"
	CapabilitySingleVectorEmbedding Capability = "single_vector_embedding"
	CapabilityTokenCounting         Capability = "token_counting"
	CapabilityPromptCaching         Capability = "prompt_caching"
	CapabilityCitations             Capability = "citations"
	CapabilityProviderGuardrails    Capability = "provider_guardrails"
	CapabilityProviderPrompts       Capability = "provider_prompts"
)

// CircuitPolicy configures replica-local passive health for one deployment.
// Zero values select safe defaults. Disabled bypasses circuit health but does
// not bypass MaxConcurrency.
type CircuitPolicy struct {
	Disabled            bool     `json:"disabled,omitempty"`
	ConsecutiveFailures int      `json:"consecutive_failures,omitempty"`
	MinimumSamples      int      `json:"minimum_samples,omitempty"`
	SampleWindow        int      `json:"sample_window,omitempty"`
	FailureRate         float64  `json:"failure_rate,omitempty"`
	BaseEjectionTime    Duration `json:"base_ejection_time,omitempty"`
	MaxEjectionTime     Duration `json:"max_ejection_time,omitempty"`
}

type EffectiveCircuitPolicy struct {
	Disabled            bool
	ConsecutiveFailures int
	MinimumSamples      int
	SampleWindow        int
	FailureRate         float64
	BaseEjectionTime    time.Duration
	MaxEjectionTime     time.Duration
}

// Duration is a strict string-encoded time.Duration for configuration. Requiring
// units avoids accidentally interpreting a JSON number as nanoseconds.
type Duration time.Duration

func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

func (d Duration) String() string {
	return time.Duration(d).String()
}

func (d *Duration) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*d = 0
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("duration must be a string with units: %w", err)
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", value, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	if d == 0 {
		return json.Marshal("0s")
	}
	return json.Marshal(d.String())
}

type AuthType string

const (
	AuthNone     AuthType = ""
	AuthBearer   AuthType = "bearer"
	AuthHeader   AuthType = "header"
	AuthAWSSigV4 AuthType = "aws_sigv4"
)

// ProviderAuth describes adapter-owned authentication injection. Deployment
// Credential overrides Auth.Credential so one provider can expose independently
// attributable targets with different keys.
type ProviderAuth struct {
	Type       AuthType        `json:"type,omitempty"`
	Header     string          `json:"header,omitempty"`
	Prefix     string          `json:"prefix,omitempty"`
	Credential credentials.Ref `json:"credential,omitempty"`
}

type HeaderScope string

const (
	HeaderScopeBoth      HeaderScope = "both"
	HeaderScopeInference HeaderScope = "inference"
	HeaderScopeHealth    HeaderScope = "health"
)

// HeaderValue contains either a non-secret literal or a secret reference.
// Exactly one of Value and ValueFrom must be set.
type HeaderValue struct {
	Value     string          `json:"value,omitempty"`
	ValueFrom credentials.Ref `json:"value_from,omitempty"`
	Scope     HeaderScope     `json:"scope,omitempty"`
}

// CredentialReferences returns the unique secret references used by provider
// authentication and custom headers, sorted for deterministic validation.
func (d Document) CredentialReferences() []credentials.Ref {
	unique := make(map[credentials.Ref]struct{})
	add := func(ref credentials.Ref) {
		if ref != "" {
			unique[ref] = struct{}{}
		}
	}
	for _, provider := range d.Providers {
		add(provider.Auth.Credential)
		for _, header := range provider.DefaultHeaders {
			add(header.ValueFrom)
		}
	}
	for _, deployment := range d.Deployments {
		add(deployment.Credential)
		for _, header := range deployment.UpstreamHeaders {
			add(header.ValueFrom)
		}
	}
	result := make([]credentials.Ref, 0, len(unique))
	for ref := range unique {
		result = append(result, ref)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return result
}

type VirtualModel struct {
	Name                 string            `json:"name"`
	Aliases              []string          `json:"aliases,omitempty"`
	Visibility           ModelVisibility   `json:"visibility,omitempty"`
	RequiredCapabilities []Capability      `json:"required_capabilities,omitempty"`
	ResponseModel        ResponseModelMode `json:"response_model,omitempty"`
	Selection            SelectionPolicy   `json:"selection,omitempty"`
	Limits               ModelLimits       `json:"limits,omitempty"`
	Retry                RetryPolicy       `json:"retry,omitempty"`
	Guardrails           GuardrailPolicy   `json:"guardrails,omitempty"`
	Privacy              *PrivacyPolicy    `json:"privacy,omitempty"`
	Pools                []RoutingPool     `json:"pools"`
}

type ModelVisibility string

const (
	ModelVisibilityPublic   ModelVisibility = "public"
	ModelVisibilityHidden   ModelVisibility = "hidden"
	ModelVisibilityInternal ModelVisibility = "internal"
)

func (v ModelVisibility) Effective() ModelVisibility {
	if v == "" {
		return ModelVisibilityPublic
	}
	return v
}

// GuardrailPolicy runs ordered, non-streaming Chat Completions subrequests
// through other virtual models before or after a primary protocol call,
// including OpenAI and Anthropic ingress operations. Stream enables bounded,
// incremental screening of streaming post-response output.
type GuardrailPolicy struct {
	Pre    []Guardrail            `json:"pre,omitempty"`
	Post   []Guardrail            `json:"post,omitempty"`
	Stream *GuardrailStreamPolicy `json:"stream,omitempty"`
}

type GuardrailStreamPolicy struct {
	WindowBytes  int `json:"window_bytes,omitempty"`
	ContextBytes int `json:"context_bytes,omitempty"`
}

type EffectiveGuardrailStreamPolicy struct {
	WindowBytes  int
	ContextBytes int
}

func (p GuardrailStreamPolicy) Effective() EffectiveGuardrailStreamPolicy {
	windowBytes := p.WindowBytes
	if windowBytes == 0 {
		windowBytes = 16 << 10
	}
	contextBytes := p.ContextBytes
	if contextBytes == 0 {
		contextBytes = 64 << 10
	}
	return EffectiveGuardrailStreamPolicy{
		WindowBytes:  windowBytes,
		ContextBytes: contextBytes,
	}
}

type GuardrailFailureMode string

const (
	GuardrailFailOpen   GuardrailFailureMode = "fail_open"
	GuardrailFailClosed GuardrailFailureMode = "fail_closed"
)

type Guardrail struct {
	Name             string               `json:"name"`
	Model            string               `json:"model"`
	Prompt           string               `json:"prompt,omitempty"`
	FailureMode      GuardrailFailureMode `json:"failure_mode,omitempty"`
	AllowReplacement bool                 `json:"allow_replacement,omitempty"`
}

func (m GuardrailFailureMode) Effective() GuardrailFailureMode {
	if m == "" {
		return GuardrailFailClosed
	}
	return m
}

// PrivacyPolicy contains deterministic content transformations that run inside
// the gateway trust boundary. It is intentionally separate from model-backed
// guardrails, which may receive content at another provider.
type PrivacyPolicy struct {
	PII *PIIPolicy `json:"pii,omitempty"`
}

type PIIMode string
type PIIEntity string
type PIIScope string
type PIIResponseMode string
type PIITraceContent string
type PIIFailureMode string
type PIIMediaTextMode string
type PIIFileTextMode string

const (
	PIIModeDisabled   PIIMode = "disabled"
	PIIModeSubstitute PIIMode = "substitute"

	PIIEntityEmail      PIIEntity = "email"
	PIIEntityPhone      PIIEntity = "phone"
	PIIEntitySSN        PIIEntity = "ssn"
	PIIEntityCreditCard PIIEntity = "credit_card"
	PIIEntityIPv4       PIIEntity = "ipv4"
	PIIEntityPerson     PIIEntity = "person"
	PIIEntityAddress    PIIEntity = "address"

	PIIScopeRequest      PIIScope = "request"
	PIIScopeConversation PIIScope = "conversation"

	PIIResponseRestore PIIResponseMode = "restore"
	PIIResponseMasked  PIIResponseMode = "masked"

	PIITraceMasked        PIITraceContent = "masked"
	PIITraceCallerVisible PIITraceContent = "caller_visible"
	PIITraceDisabled      PIITraceContent = "disabled"

	PIIFailOpen   PIIFailureMode = "fail_open"
	PIIFailClosed PIIFailureMode = "fail_closed"

	PIIMediaTextBestEffort PIIMediaTextMode = "best_effort"
	PIIMediaTextRequired   PIIMediaTextMode = "required"

	PIIFileTextDisabled   PIIFileTextMode = "disabled"
	PIIFileTextBestEffort PIIFileTextMode = "best_effort"
	PIIFileTextRequired   PIIFileTextMode = "required"
)

const defaultPIIFileMaxTextBytes int64 = 4 << 20

var defaultPIIEntities = []PIIEntity{
	PIIEntityEmail,
	PIIEntityPhone,
	PIIEntitySSN,
	PIIEntityCreditCard,
	PIIEntityIPv4,
}

type PIIPolicy struct {
	Mode         PIIMode          `json:"mode,omitempty"`
	Entities     []PIIEntity      `json:"entities,omitempty"`
	Scope        PIIScope         `json:"scope,omitempty"`
	Response     PIIResponseMode  `json:"response,omitempty"`
	TraceContent PIITraceContent  `json:"trace_content,omitempty"`
	FailureMode  PIIFailureMode   `json:"failure_mode,omitempty"`
	MediaText    PIIMediaTextMode `json:"media_text,omitempty"`
	Files        *PIIFilePolicy   `json:"files,omitempty"`
}

type PIIFilePolicy struct {
	Text         PIIFileTextMode `json:"text,omitempty"`
	Metadata     bool            `json:"metadata,omitempty"`
	MaxTextBytes int64           `json:"max_text_bytes,omitempty"`
}

type EffectivePIIFilePolicy struct {
	Text         PIIFileTextMode
	Metadata     bool
	MaxTextBytes int64
}

type EffectivePIIPolicy struct {
	Mode         PIIMode
	Entities     []PIIEntity
	Scope        PIIScope
	Response     PIIResponseMode
	TraceContent PIITraceContent
	FailureMode  PIIFailureMode
	MediaText    PIIMediaTextMode
	Files        EffectivePIIFilePolicy
}

func (p *PIIPolicy) Effective() EffectivePIIPolicy {
	if p == nil || p.Mode == PIIModeDisabled {
		return EffectivePIIPolicy{Mode: PIIModeDisabled}
	}
	mode := p.Mode
	if mode == "" {
		mode = PIIModeSubstitute
	}
	entities := append([]PIIEntity(nil), p.Entities...)
	if len(entities) == 0 {
		entities = append([]PIIEntity(nil), defaultPIIEntities...)
	}
	scope := p.Scope
	if scope == "" {
		scope = PIIScopeRequest
	}
	response := p.Response
	if response == "" {
		response = PIIResponseRestore
	}
	traceContent := p.TraceContent
	if traceContent == "" {
		traceContent = PIITraceMasked
	}
	failureMode := p.FailureMode
	if failureMode == "" {
		failureMode = PIIFailClosed
	}
	mediaText := p.MediaText
	if mediaText == "" {
		mediaText = PIIMediaTextBestEffort
	}
	files := EffectivePIIFilePolicy{
		Text: PIIFileTextDisabled, MaxTextBytes: defaultPIIFileMaxTextBytes,
	}
	if p.Files != nil {
		files.Text = p.Files.Text
		if files.Text == "" {
			files.Text = PIIFileTextDisabled
		}
		files.Metadata = p.Files.Metadata
		if p.Files.MaxTextBytes > 0 {
			files.MaxTextBytes = p.Files.MaxTextBytes
		}
	}
	return EffectivePIIPolicy{
		Mode: mode, Entities: entities, Scope: scope, Response: response,
		TraceContent: traceContent, FailureMode: failureMode,
		MediaText: mediaText, Files: files,
	}
}

type ResponseModelMode string

const (
	ResponseModelVirtual   ResponseModelMode = "virtual"
	ResponseModelRequested ResponseModelMode = "requested"
	ResponseModelUpstream  ResponseModelMode = "upstream"
)

type SelectionMode string

const (
	SelectionWeightedRandom SelectionMode = "weighted_random"
	SelectionWeightedHash   SelectionMode = "weighted_hash"
)

type SelectionPolicy struct {
	Mode                SelectionMode             `json:"mode,omitempty"`
	HashKey             SelectionHashKey          `json:"hash_key,omitempty"`
	PromptCacheAffinity PromptCacheAffinityPolicy `json:"prompt_cache_affinity,omitempty,omitzero"`
}

// SelectionHashKey names trusted identity or attribution used for stable
// weighted routing. Request payload fields are deliberately not accepted as
// routing identities.
type SelectionHashKey string

const (
	SelectionHashTenant     SelectionHashKey = "tenant"
	SelectionHashPrincipal  SelectionHashKey = "principal"
	SelectionHashUser       SelectionHashKey = "user"
	SelectionHashWorkspace  SelectionHashKey = "workspace"
	SelectionHashThreadID   SelectionHashKey = "thread_id"
	SelectionHashTaskID     SelectionHashKey = "task_id"
	SelectionHashExperiment SelectionHashKey = "experiment"
	SelectionHashSession    SelectionHashKey = "session"
)

type PromptCacheAffinityScope string

const (
	PromptCacheAffinityCaller PromptCacheAffinityScope = "caller"
	PromptCacheAffinityTenant PromptCacheAffinityScope = "tenant"
)

// PromptCacheAffinityPolicy enables best-effort routing back to a deployment
// that recently completed a request with a matching prompt prefix. Its zero
// value is disabled. Affinity never bypasses target eligibility, protocol or
// capability checks, operator priority pools, or normal failover.
type PromptCacheAffinityPolicy struct {
	Enabled               bool                     `json:"enabled,omitempty"`
	TTL                   Duration                 `json:"ttl,omitempty"`
	MinPrefixBytes        int                      `json:"min_prefix_bytes,omitempty"`
	MaxPrefixesPerRequest int                      `json:"max_prefixes_per_request,omitempty"`
	Scope                 PromptCacheAffinityScope `json:"scope,omitempty"`
}

type EffectivePromptCacheAffinityPolicy struct {
	Enabled               bool
	TTL                   time.Duration
	MinPrefixBytes        int
	MaxPrefixesPerRequest int
	Scope                 PromptCacheAffinityScope
}

func (p PromptCacheAffinityPolicy) IsZero() bool {
	return !p.Enabled && p.TTL == 0 && p.MinPrefixBytes == 0 &&
		p.MaxPrefixesPerRequest == 0 && p.Scope == ""
}

type ModelLimits struct {
	MaxAttempts       int      `json:"max_attempts,omitempty"`
	OverallTimeout    Duration `json:"overall_timeout,omitempty"`
	PerTryTimeout     Duration `json:"per_try_timeout,omitempty"`
	StreamIdleTimeout Duration `json:"stream_idle_timeout,omitempty"`
}

type EffectiveModelLimits struct {
	MaxAttempts       int
	OverallTimeout    time.Duration
	PerTryTimeout     time.Duration
	StreamIdleTimeout time.Duration
}

// RetryPolicy controls delays and the shared retry-concurrency budget for one
// virtual model. Disabled prevents fallback attempts even when MaxAttempts is
// greater than one. Zero values otherwise select safe defaults.
type RetryPolicy struct {
	Disabled      bool              `json:"disabled,omitempty"`
	BaseBackoff   Duration          `json:"base_backoff,omitempty"`
	MaxBackoff    Duration          `json:"max_backoff,omitempty"`
	MaxRetryAfter Duration          `json:"max_retry_after,omitempty"`
	Budget        RetryBudgetPolicy `json:"budget,omitempty"`
}

type RetryBudgetPolicy struct {
	Disabled       bool    `json:"disabled,omitempty"`
	Ratio          float64 `json:"ratio,omitempty"`
	MinConcurrency int     `json:"min_concurrency,omitempty"`
}

type EffectiveRetryPolicy struct {
	Disabled      bool
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
	MaxRetryAfter time.Duration
	Budget        EffectiveRetryBudgetPolicy
}

type EffectiveRetryBudgetPolicy struct {
	Disabled       bool
	Ratio          float64
	MinConcurrency int
}

type RoutingPool struct {
	Priority int              `json:"priority"`
	Targets  []WeightedTarget `json:"targets"`
}

type WeightedTarget struct {
	Deployment string `json:"deployment"`
	Weight     int    `json:"weight"`
}

// CanonicalModel resolves a canonical name or alias.
func (d Document) CanonicalModel(requested string) (VirtualModel, bool) {
	for _, model := range d.VirtualModels {
		if model.Name == requested {
			return model, true
		}
		for _, alias := range model.Aliases {
			if alias == requested {
				return model, true
			}
		}
	}
	return VirtualModel{}, false
}
