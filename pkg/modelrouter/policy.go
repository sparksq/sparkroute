// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MMProjectionTrace records the separate post-selection request-transform
// stage; it is not a completion-provider attempt.
type MMProjectionTrace struct {
	ProviderID    string `json:"provider_id"`
	State         string `json:"state"`
	FailureMode   string `json:"failure_mode"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	RequestID     string `json:"request_id,omitempty"`
	MediaCount    int    `json:"media_count,omitempty"`
	CacheHit      bool   `json:"cache_hit,omitempty"`
	AnalyzerModel string `json:"analyzer_model,omitempty"`
	// TextInspectionState and InspectedMedia are content-free MMBridge
	// attestations used by fail-closed PII media policies.
	TextInspectionState string `json:"text_inspection_state,omitempty"`
	InspectedMedia      int    `json:"inspected_media,omitempty"`
	Error               string `json:"error,omitempty"`
}

// This file owns the routing policy contract, validation, cloning, and
// revision-safe persistence. Request normalization, runtime selection, and
// observations are isolated in their respective routing files.

const (
	RoutingPolicyVersion      = 2
	maxRoutingVirtualModels   = 256
	maxRoutingModels          = 4096
	maxRoutingKeywordRules    = 256
	maxRoutingCandidateModels = 4096
	maxRoutingKeywordsPerRule = 128
	maxRoutingTagsPerModel    = 128
	maxRoutingPublicEndpoints = 4096
)

var (
	ErrRevisionConflict   = errors.New("routing policy revision conflict")
	ErrLegacyScorerPolicy = errors.New("routing policy uses the removed scorer/router-LLM subsystem")
)

type RoutingPolicy struct {
	Version             int                      `json:"version"`
	Revision            uint64                   `json:"revision"`
	DefaultVirtualModel string                   `json:"default_virtual_model,omitempty"`
	VirtualModels       map[string]VirtualModel  `json:"virtual_models"`
	Models              map[string]ModelMetadata `json:"models"`
	KeywordRules        []KeywordRule            `json:"keyword_rules,omitempty"`
}

type VirtualModel struct {
	Strategy     string                    `json:"strategy"`
	Models       []string                  `json:"models,omitempty"`
	Aliases      []string                  `json:"aliases,omitempty"`
	Kwargs       map[string]map[string]any `json:"kwargs,omitempty"`
	StageRouter  *StageRouterPolicy        `json:"stage_router,omitempty"`
	MMProjection *MMProjectionPolicy       `json:"mm_projection,omitempty"`
}

type StagePicker string

const (
	StagePickerEfficientFirst StagePicker = "efficient_first"
	StagePickerCapableFirst   StagePicker = "capable_first"
)

// StageRouterPolicy chooses between exactly two configured logical models
// using a content-free summary of recent tool execution. A pointer threshold
// preserves the meaningful explicit value 0; nil defaults to 0.5.
type StageRouterPolicy struct {
	CapableModel        string      `json:"capable_model"`
	EfficientModel      string      `json:"efficient_model"`
	Picker              StagePicker `json:"picker,omitempty"`
	ConfidenceThreshold *float64    `json:"confidence_threshold,omitempty"`
	RecentTurnWindow    int         `json:"recent_turn_window,omitempty"`
}

func (p StageRouterPolicy) effectivePicker() StagePicker {
	if p.Picker == "" {
		return StagePickerEfficientFirst
	}
	return p.Picker
}

func (p StageRouterPolicy) effectiveThreshold() float64 {
	if p.ConfidenceThreshold == nil {
		return 0.5
	}
	return *p.ConfidenceThreshold
}

func (p StageRouterPolicy) effectiveWindow() int {
	if p.RecentTurnWindow <= 0 {
		return 3
	}
	return p.RecentTurnWindow
}

// Kwargs contains named routing presets. A request for "auto:high" applies
// the "high" object; an unsuffixed request applies "default" when present.
// Aliases inherit the same presets. Reserved keys are interpreted by the Go
// router. Unknown keys remain part of the policy contract for forward-compatible
// Go routing extensions, but do not affect selection in this release.

// MMProjectionPolicy opts a physical/logical model or a virtual-model default
// into the configured multimedia projection service after model selection and
// before provider delivery. The selected logical model and its ordinary
// provider fallback chain are retained.
type MMProjectionPolicy struct {
	AnalyzerModel string `json:"analyzer_model,omitempty"`
	FailureMode   string `json:"failure_mode,omitempty"`
	TimeoutMS     int    `json:"timeout_ms,omitempty"`
}

type ModelMetadata struct {
	Enabled              bool                `json:"enabled"`
	Weight               int                 `json:"weight,omitempty"`
	Priority             int                 `json:"priority,omitempty"`
	SizeB                float64             `json:"size_b,omitempty"`
	InputPrice           float64             `json:"input_price,omitempty"`
	OutputPrice          float64             `json:"output_price,omitempty"`
	Context              int                 `json:"context,omitempty"`
	Tags                 []string            `json:"tags,omitempty"`
	MMProjection         *MMProjectionPolicy `json:"mm_projection,omitempty"`
	MMProjectionDisabled bool                `json:"mm_projection_disabled,omitempty"`
	DiscoveryDisabled    bool                `json:"discovery_disabled,omitempty"`
}

type KeywordRule struct {
	Name         string   `json:"name,omitempty"`
	Keywords     []string `json:"keywords"`
	MatchAll     bool     `json:"match_all,omitempty"`
	Strategy     string   `json:"strategy,omitempty"`
	Models       []string `json:"models,omitempty"`
	VirtualModel string   `json:"virtual_model,omitempty"`
}

type CandidateTrace struct {
	Model    string             `json:"model"`
	Eligible bool               `json:"eligible"`
	Reason   string             `json:"reason,omitempty"`
	Score    float64            `json:"score,omitempty"`
	Scores   map[string]float64 `json:"scores,omitempty"`
}

type RouteDecision struct {
	RequestedModel   string             `json:"requested_model"`
	ResolvedModel    string             `json:"resolved_model"`
	VirtualModel     string             `json:"virtual_model,omitempty"`
	VirtualAlias     string             `json:"virtual_alias,omitempty"`
	RoutingPreset    string             `json:"routing_preset,omitempty"`
	RouterKwargs     map[string]any     `json:"router_kwargs,omitempty"`
	ProviderPriority []string           `json:"provider_priority,omitempty"`
	Strategy         string             `json:"strategy"`
	MatchedRule      string             `json:"matched_rule,omitempty"`
	Reason           string             `json:"reason"`
	Revision         uint64             `json:"revision"`
	Candidates       []CandidateTrace   `json:"candidates"`
	Stage            *StageRouteTrace   `json:"stage,omitempty"`
	At               time.Time          `json:"at"`
	MMProjection     *MMProjectionTrace `json:"mm_projection,omitempty"`
	projectionPolicy *MMProjectionPolicy
}

// RoutePlan is an immutable snapshot of routing policy and eligibility.
type RoutePlan struct {
	RequestedModel   string
	VirtualModel     string
	VirtualAlias     string
	RoutingPreset    string
	RouterKwargs     map[string]any
	ProviderPriority []string
	Strategy         string
	MatchedRule      string
	Revision         uint64
	Candidates       []CandidateTrace
	Eligible         []string
	Models           map[string]ModelMetadata
	StageRouter      *StageRouterPolicy
	StageHistory     StageHistory
	MMProjection     *MMProjectionPolicy
	Explicit         bool
	At               time.Time
}

type ModelObservation struct {
	Inflight      int64   `json:"inflight"`
	Requests      uint64  `json:"requests"`
	Errors        uint64  `json:"errors"`
	EWMALatencyMS float64 `json:"ewma_latency_ms"`
	LastStatus    int     `json:"last_status"`
}

type RouterState struct {
	Policy RoutingPolicy               `json:"policy"`
	Models map[string]ModelObservation `json:"models"`
}

type RouteHandle struct {
	Model   string
	started time.Time
	once    sync.Once
}

type RouterManager struct {
	// mu protects the durable/admin policy representation. The request path reads
	// runtime instead, so publishing and serializing policy never blocks routing.
	mu                 sync.RWMutex
	publishMu          sync.Mutex
	policy             RoutingPolicy
	runtime            atomic.Pointer[compiledRoutingPolicy]
	metadata           map[string]DiscoveredMetadataSnapshot
	metadataGeneration uint64

	// Selection counters and observations have independent contention domains.
	// A slow admin policy read must not serialize request metrics or round robin.
	counterMu     sync.Mutex
	counters      map[string]uint64
	observationMu sync.RWMutex
	observations  map[string]ModelObservation
}

func DefaultRoutingPolicy() RoutingPolicy {
	return RoutingPolicy{Version: RoutingPolicyVersion, Revision: 1, DefaultVirtualModel: "auto", VirtualModels: map[string]VirtualModel{"auto": {Strategy: "balanced"}}, Models: map[string]ModelMetadata{}}
}

func NewRouterManager(policy RoutingPolicy) (*RouterManager, error) {
	if policy.Version == 0 {
		policy.Version = RoutingPolicyVersion
	}
	if policy.Revision == 0 {
		policy.Revision = 1
	}
	if err := ValidateRoutingPolicy(policy); err != nil {
		return nil, err
	}
	router := &RouterManager{
		counters: map[string]uint64{}, observations: map[string]ModelObservation{},
		metadata: map[string]DiscoveredMetadataSnapshot{},
	}
	router.installPolicyLocked(policy)
	return router, nil
}

func NewDefaultRouterManager() *RouterManager {
	r, _ := NewRouterManager(DefaultRoutingPolicy())
	return r
}

var strategies = map[string]bool{"random": true, "round_robin": true, "weighted_round_robin": true, "smallest": true, "largest": true, "lowest_cost": true, "fastest": true, "balanced": true, "stage_router": true}

func ValidateRoutingPolicy(p RoutingPolicy) error {
	if p.Version != RoutingPolicyVersion {
		return fmt.Errorf("unsupported routing policy version %d", p.Version)
	}
	if len(p.VirtualModels) == 0 {
		return errors.New("at least one virtual model is required")
	}
	if len(p.VirtualModels) > maxRoutingVirtualModels {
		return fmt.Errorf("routing policy has %d virtual models; maximum is %d", len(p.VirtualModels), maxRoutingVirtualModels)
	}
	if len(p.Models) > maxRoutingModels {
		return fmt.Errorf("routing policy has %d model metadata entries; maximum is %d", len(p.Models), maxRoutingModels)
	}
	if len(p.KeywordRules) > maxRoutingKeywordRules {
		return fmt.Errorf("routing policy has %d keyword rules; maximum is %d", len(p.KeywordRules), maxRoutingKeywordRules)
	}
	if _, ok := p.VirtualModels["auto"]; !ok {
		return errors.New("virtual model auto is required")
	}
	if err := validateVirtualNamespace(p); err != nil {
		return err
	}
	for name, vm := range p.VirtualModels {
		if !strategies[vm.Strategy] {
			return fmt.Errorf("virtual model %q: unknown strategy %q", name, vm.Strategy)
		}
		if len(vm.Models) > maxRoutingCandidateModels {
			return fmt.Errorf("virtual model %q has more than %d candidates", name, maxRoutingCandidateModels)
		}
		if err := validateModels(vm.Models, p.Models); err != nil {
			return fmt.Errorf("virtual model %q: %w", name, err)
		}
		if err := validateStageRouterPolicy(vm); err != nil {
			return fmt.Errorf("virtual model %q: %w", name, err)
		}
		for preset, kwargs := range vm.Kwargs {
			if err := validateRoutingKwargs(kwargs, p.Models); err != nil {
				return fmt.Errorf("virtual model %q kwargs %q: %w", name, preset, err)
			}
			if vm.Strategy == "stage_router" {
				if _, changesStrategy := kwargs["strategy"]; changesStrategy {
					return fmt.Errorf("virtual model %q kwargs %q: stage_router presets cannot override strategy", name, preset)
				}
				if _, changesModels := kwargs["models"]; changesModels {
					return fmt.Errorf("virtual model %q kwargs %q: stage_router presets cannot override models", name, preset)
				}
			}
		}
		if err := validateMMProjectionPolicy(vm.MMProjection); err != nil {
			return fmt.Errorf("virtual model %q: %w", name, err)
		}
	}
	if p.DefaultVirtualModel != "" {
		if _, ok := p.VirtualModels[p.DefaultVirtualModel]; !ok {
			return fmt.Errorf("unknown default virtual model %q", p.DefaultVirtualModel)
		}
	}
	for name, m := range p.Models {
		if strings.TrimSpace(name) == "" || len(name) > 256 || hasControlCharacter(name) {
			return fmt.Errorf("model metadata has invalid model name %q", name)
		}
		if m.Weight < 0 || m.Weight > 10000 {
			return fmt.Errorf("model %q weight must be between 0 and 10000", name)
		}
		if m.InputPrice < 0 || m.OutputPrice < 0 || m.SizeB < 0 || m.Context < 0 ||
			math.IsNaN(m.InputPrice) || math.IsNaN(m.OutputPrice) || math.IsNaN(m.SizeB) ||
			math.IsInf(m.InputPrice, 0) || math.IsInf(m.OutputPrice, 0) || math.IsInf(m.SizeB, 0) {
			return fmt.Errorf("model %q has invalid numeric metadata", name)
		}
		if len(m.Tags) > maxRoutingTagsPerModel {
			return fmt.Errorf("model %q has more than %d tags", name, maxRoutingTagsPerModel)
		}
		for _, tag := range m.Tags {
			if strings.TrimSpace(tag) == "" || len(tag) > 128 || hasControlCharacter(tag) {
				return fmt.Errorf("model %q has invalid tag %q", name, tag)
			}
		}
		if err := validateMMProjectionPolicy(m.MMProjection); err != nil {
			return fmt.Errorf("model %q: %w", name, err)
		}
		if m.MMProjectionDisabled && m.MMProjection != nil {
			return fmt.Errorf("model %q: mm_projection and mm_projection_disabled cannot both be set", name)
		}
	}
	for i, rule := range p.KeywordRules {
		if len(rule.Keywords) == 0 {
			return fmt.Errorf("rule %d has no keywords", i)
		}
		if len(rule.Keywords) > maxRoutingKeywordsPerRule {
			return fmt.Errorf("rule %d has more than %d keywords", i, maxRoutingKeywordsPerRule)
		}
		if len(rule.Models) > maxRoutingCandidateModels {
			return fmt.Errorf("rule %d has more than %d candidates", i, maxRoutingCandidateModels)
		}
		for _, k := range rule.Keywords {
			if strings.TrimSpace(k) == "" || len(k) > 256 || hasControlCharacter(k) {
				return fmt.Errorf("rule %d has empty keyword", i)
			}
		}
		if rule.VirtualModel != "" {
			if _, ok := p.VirtualModels[rule.VirtualModel]; !ok {
				return fmt.Errorf("rule %d references unknown virtual model %q", i, rule.VirtualModel)
			}
			if rule.Strategy != "" || len(rule.Models) > 0 {
				return fmt.Errorf("rule %d mixes virtual_model with strategy/models", i)
			}
		} else {
			if rule.Strategy == "stage_router" {
				return fmt.Errorf("rule %d: stage_router must be selected through virtual_model so its two-tier policy is retained", i)
			}
			if !strategies[rule.Strategy] {
				return fmt.Errorf("rule %d has unknown strategy %q", i, rule.Strategy)
			}
			if err := validateModels(rule.Models, p.Models); err != nil {
				return fmt.Errorf("rule %d: %w", i, err)
			}
		}
	}
	return nil
}

func validateStageRouterPolicy(virtual VirtualModel) error {
	if virtual.Strategy != "stage_router" {
		if virtual.StageRouter != nil {
			return errors.New("stage_router settings require strategy stage_router")
		}
		return nil
	}
	if virtual.StageRouter == nil {
		return errors.New("strategy stage_router requires stage_router settings")
	}
	policy := *virtual.StageRouter
	if policy.CapableModel == "" || policy.EfficientModel == "" || policy.CapableModel == policy.EfficientModel {
		return errors.New("stage_router requires distinct capable_model and efficient_model values")
	}
	if len(virtual.Models) != 2 || !containsString(virtual.Models, policy.CapableModel) || !containsString(virtual.Models, policy.EfficientModel) {
		return errors.New("stage_router models must contain exactly capable_model and efficient_model")
	}
	switch policy.effectivePicker() {
	case StagePickerEfficientFirst, StagePickerCapableFirst:
	default:
		return errors.New("stage_router picker must be efficient_first or capable_first")
	}
	threshold := policy.effectiveThreshold()
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold < 0 || threshold > 1 {
		return errors.New("stage_router confidence_threshold must be between 0 and 1")
	}
	if policy.RecentTurnWindow < 0 || policy.RecentTurnWindow > 32 {
		return errors.New("stage_router recent_turn_window must be between 1 and 32, or 0 for the default")
	}
	return nil
}

func validateVirtualNamespace(policy RoutingPolicy) error {
	owners := make(map[string]string, len(policy.VirtualModels))
	totalEndpoints := 0
	for name := range policy.VirtualModels {
		if strings.TrimSpace(name) == "" || len(name) > 256 || strings.Contains(name, ":") || hasControlCharacter(name) {
			return fmt.Errorf("virtual model %q has an invalid name", name)
		}
		owners[name] = name
	}
	for name, virtual := range policy.VirtualModels {
		if len(virtual.Aliases) > 64 {
			return fmt.Errorf("virtual model %q has more than 64 aliases", name)
		}
		for _, rawAlias := range virtual.Aliases {
			alias := strings.TrimSpace(rawAlias)
			if alias == "" || alias != rawAlias || len(alias) > 256 || strings.Contains(alias, ":") || hasControlCharacter(alias) {
				return fmt.Errorf("virtual model %q has invalid alias %q", name, rawAlias)
			}
			if owner, exists := owners[alias]; exists {
				return fmt.Errorf("virtual alias %q conflicts with virtual model or alias owned by %q", alias, owner)
			}
			owners[alias] = name
		}
		if len(virtual.Kwargs) > 64 {
			return fmt.Errorf("virtual model %q has more than 64 kwargs presets", name)
		}
		totalEndpoints += (1 + len(virtual.Aliases)) * (1 + len(virtual.Kwargs))
		if totalEndpoints > maxRoutingPublicEndpoints {
			return fmt.Errorf("virtual namespace publishes more than %d endpoints", maxRoutingPublicEndpoints)
		}
		for preset, kwargs := range virtual.Kwargs {
			if !validRoutingPresetName(preset) || kwargs == nil {
				return fmt.Errorf("virtual model %q has invalid kwargs preset %q", name, preset)
			}
			endpoints := append([]string{name}, virtual.Aliases...)
			for _, endpoint := range endpoints {
				variant := endpoint + ":" + preset
				if _, exists := policy.VirtualModels[variant]; exists {
					return fmt.Errorf("virtual kwargs endpoint %q conflicts with a virtual model", variant)
				}
			}
		}
	}
	return nil
}

func validRoutingPresetName(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for i, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || (i > 0 && (character == '-' || character == '_' || character == '.')) {
			continue
		}
		return false
	}
	return true
}

func hasControlCharacter(value string) bool {
	for _, character := range value {
		if character < 32 || character == 127 {
			return true
		}
	}
	return false
}

func validBackendID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for i, character := range id {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			(i > 0 && (character == '-' || character == '_')) {
			continue
		}
		return false
	}
	return true
}

// PublicModels returns the stable selector namespace published by this policy.
func (r *RouterManager) PublicModels() []string {
	policy := r.Policy()
	models := make([]string, 0, len(policy.VirtualModels))
	for name, virtual := range policy.VirtualModels {
		endpoints := append([]string{name}, virtual.Aliases...)
		for _, endpoint := range endpoints {
			models = append(models, endpoint)
			for preset := range virtual.Kwargs {
				models = append(models, endpoint+":"+preset)
			}
		}
	}
	sort.Strings(models)
	return models
}

func validateRoutingKwargs(kwargs map[string]any, models map[string]ModelMetadata) error {
	if len(kwargs) > 64 {
		return errors.New("kwargs object has more than 64 keys")
	}
	encoded, err := json.Marshal(kwargs)
	if err != nil || len(encoded) > 64<<10 {
		return errors.New("kwargs object must be JSON and at most 64 KiB")
	}
	for key := range kwargs {
		if strings.TrimSpace(key) == "" || len(key) > 128 || hasControlCharacter(key) {
			return fmt.Errorf("invalid kwargs key %q", key)
		}
	}
	if raw, exists := kwargs["strategy"]; exists {
		strategy, ok := raw.(string)
		if !ok || !strategies[strategy] {
			return errors.New("strategy kwarg must name a supported Go fallback strategy")
		}
	}
	if raw, exists := kwargs["models"]; exists {
		names, ok := routingStringList(raw, false)
		if !ok || len(names) == 0 {
			return errors.New("models kwarg must be a non-empty list of model IDs")
		}
		if err := validateModels(names, models); err != nil {
			return err
		}
	}
	if raw, exists := kwargs["provider_priority"]; exists {
		providers, ok := routingStringList(raw, true)
		if !ok || len(providers) == 0 || len(providers) > 64 {
			return errors.New("provider_priority kwarg must be a provider ID or bounded list of provider IDs")
		}
		for _, provider := range providers {
			if !validBackendID(strings.ToLower(strings.TrimSpace(provider))) {
				return fmt.Errorf("provider_priority contains invalid provider %q", provider)
			}
		}
	}
	return nil
}

func routingStringList(value any, allowCSV bool) ([]string, bool) {
	var input []string
	switch typed := value.(type) {
	case string:
		if !allowCSV {
			return nil, false
		}
		input = strings.Split(typed, ",")
	case []string:
		input = typed
	case []any:
		input = make([]string, 0, len(typed))
		for _, raw := range typed {
			item, ok := raw.(string)
			if !ok {
				return nil, false
			}
			input = append(input, item)
		}
	default:
		return nil, false
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(input))
	for _, raw := range input {
		item := strings.TrimSpace(raw)
		if item == "" || len(item) > 256 || hasControlCharacter(item) {
			return nil, false
		}
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out, true
}

func validateMMProjectionPolicy(policy *MMProjectionPolicy) error {
	if policy == nil {
		return nil
	}
	mode := strings.TrimSpace(policy.FailureMode)
	if mode == "" {
		mode = "fallback"
	}
	if mode != "fallback" && mode != "fail_closed" {
		return errors.New("mm_projection failure_mode must be fallback or fail_closed")
	}
	if policy.TimeoutMS != 0 && (policy.TimeoutMS < 1000 || policy.TimeoutMS > 900000) {
		return errors.New("mm_projection timeout_ms must be between 1000 and 900000")
	}
	analyzer := strings.TrimSpace(policy.AnalyzerModel)
	invalidAnalyzer := len(analyzer) > 256
	for _, character := range analyzer {
		if character < 32 || character == 127 {
			invalidAnalyzer = true
			break
		}
	}
	if invalidAnalyzer {
		return errors.New("mm_projection analyzer_model must be at most 256 characters without control characters")
	}
	return nil
}

func validateModels(names []string, models map[string]ModelMetadata) error {
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if seen[n] {
			return fmt.Errorf("duplicate model %q", n)
		}
		seen[n] = true
		if _, ok := models[n]; !ok {
			return fmt.Errorf("unknown model %q", n)
		}
	}
	return nil
}

func (r *RouterManager) Policy() RoutingPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return clonePolicy(r.policy)
}

func (r *RouterManager) Revision() uint64 {
	if runtime := r.runtime.Load(); runtime != nil {
		return runtime.policy.Revision
	}
	return 0
}

func (r *RouterManager) SetPolicy(p RoutingPolicy, expectedRevision uint64) error {
	if err := ValidateRoutingPolicy(p); err != nil {
		return err
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if expectedRevision != r.policy.Revision {
		return fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expectedRevision, r.policy.Revision)
	}
	p.Revision = r.policy.Revision + 1
	r.installPolicyLocked(p)
	return nil
}

func (r *RouterManager) Save(path string, expectedRevision uint64) error {
	r.mu.RLock()
	if expectedRevision != r.policy.Revision {
		cur := r.policy.Revision
		r.mu.RUnlock()
		return fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expectedRevision, cur)
	}
	p := clonePolicy(r.policy)
	r.mu.RUnlock()
	return writePolicyAtomic(path, p)
}

// Publish durably writes a validated next revision before making it visible to
// concurrent routing calls. An empty path is useful for ephemeral operation.
func (r *RouterManager) Publish(p RoutingPolicy, expectedRevision uint64, path string) error {
	if err := ValidateRoutingPolicy(p); err != nil {
		return err
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	r.mu.RLock()
	if expectedRevision != r.policy.Revision {
		current := r.policy.Revision
		r.mu.RUnlock()
		return fmt.Errorf("%w: expected %d, current %d", ErrRevisionConflict, expectedRevision, current)
	}
	p.Revision = r.policy.Revision + 1
	r.mu.RUnlock()
	if path != "" {
		if err := writePolicyAtomic(path, p); err != nil {
			return err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.installPolicyLocked(p)
	return nil
}

// installPolicyLocked replaces the immutable runtime snapshot together with the
// admin/persistence representation. Callers that are already serving requests
// hold r.mu; construction calls it before the manager becomes visible.
func (r *RouterManager) installPolicyLocked(policy RoutingPolicy) {
	stored := clonePolicy(policy)
	r.policy = stored
	r.runtime.Store(compileRoutingPolicy(mergeDiscoveredMetadata(stored, r.metadata)))
	r.counterMu.Lock()
	r.counters = map[string]uint64{}
	r.counterMu.Unlock()
}

func writePolicyAtomic(path string, p RoutingPolicy) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".routing-policy-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err == nil {
		// The rename is already the commit point. Directory sync is best effort:
		// reporting a post-rename failure would leave disk and memory on different
		// revisions even though rollback is no longer safe.
		if d, e := os.Open(dir); e == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return err
}

func LoadRouterManager(path string) (*RouterManager, error) {
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	legacy, e := containsLegacyScorerPolicyJSON(b)
	if e != nil {
		return nil, e
	}
	if legacy {
		return nil, fmt.Errorf("%w; remove each virtual_models.*.scorer block and choose a Go strategy before restarting", ErrLegacyScorerPolicy)
	}
	var p RoutingPolicy
	if e = json.Unmarshal(b, &p); e != nil {
		return nil, e
	}
	migrateRoutingPolicy(&p)
	return NewRouterManager(p)
}

// migrateRoutingPolicy upgrades scorer-free persisted shapes in memory. Older
// loaders also accepted an omitted version, so zero is part of this migration.
// Callers must inspect raw JSON for a retired scorer block before invoking it.
func migrateRoutingPolicy(policy *RoutingPolicy) {
	if policy != nil && (policy.Version == 0 || policy.Version == 1) {
		policy.Version = RoutingPolicyVersion
	}
}

// containsLegacyScorerPolicyJSON inspects exactly one routing-policy object
// before normal decoding can discard a removed field. Callers that own wrapper
// documents must pass the raw routing_policy/previous_router/next_router value,
// not the whole wrapper. Keeping this non-recursive prevents opaque kwargs from
// being mistaken for nested policies.
func containsLegacyScorerPolicyJSON(data []byte) (bool, error) {
	var policy struct {
		VirtualModels map[string]json.RawMessage `json:"virtual_models"`
	}
	if err := json.Unmarshal(data, &policy); err != nil {
		return false, err
	}
	for _, rawVirtual := range policy.VirtualModels {
		var virtual map[string]json.RawMessage
		if err := json.Unmarshal(rawVirtual, &virtual); err != nil {
			return false, err
		}
		for key, rawScorer := range virtual {
			// encoding/json matched struct fields case-insensitively in the
			// retired schema, so hand-authored Scorer/SCORER keys also carry
			// legacy semantics and must fail closed.
			if !strings.EqualFold(key, "scorer") {
				continue
			}
			scorer := strings.TrimSpace(string(rawScorer))
			if scorer != "" && scorer != "null" {
				return true, nil
			}
		}
	}
	return false, nil
}

func clonePolicy(p RoutingPolicy) RoutingPolicy {
	out := p
	out.VirtualModels = make(map[string]VirtualModel, len(p.VirtualModels))
	for name, virtual := range p.VirtualModels {
		cloned := virtual
		cloned.Models = append([]string(nil), virtual.Models...)
		cloned.Aliases = append([]string(nil), virtual.Aliases...)
		cloned.MMProjection = cloneMMProjectionPolicy(virtual.MMProjection)
		if virtual.StageRouter != nil {
			stage := *virtual.StageRouter
			if virtual.StageRouter.ConfidenceThreshold != nil {
				threshold := *virtual.StageRouter.ConfidenceThreshold
				stage.ConfidenceThreshold = &threshold
			}
			cloned.StageRouter = &stage
		}
		if virtual.Kwargs != nil {
			cloned.Kwargs = make(map[string]map[string]any, len(virtual.Kwargs))
			for preset, kwargs := range virtual.Kwargs {
				cloned.Kwargs[preset] = cloneJSONObject(kwargs)
			}
		}
		out.VirtualModels[name] = cloned
	}
	out.Models = make(map[string]ModelMetadata, len(p.Models))
	for name, metadata := range p.Models {
		cloned := metadata
		cloned.Tags = append([]string(nil), metadata.Tags...)
		cloned.MMProjection = cloneMMProjectionPolicy(metadata.MMProjection)
		out.Models[name] = cloned
	}
	out.KeywordRules = make([]KeywordRule, len(p.KeywordRules))
	for i, rule := range p.KeywordRules {
		out.KeywordRules[i] = rule
		out.KeywordRules[i].Keywords = append([]string(nil), rule.Keywords...)
		out.KeywordRules[i].Models = append([]string(nil), rule.Models...)
	}
	return out
}

// CloneRoutingPolicy returns a deep copy safe for configuration composition.
func CloneRoutingPolicy(policy RoutingPolicy) RoutingPolicy {
	return clonePolicy(policy)
}

func cloneJSONObject(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	out := make(map[string]any, len(value))
	for key, item := range value {
		out[key] = cloneJSONValue(item)
	}
	return out
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneJSONObject(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneJSONValue(item)
		}
		return out
	case []string:
		return append([]string(nil), typed...)
	default:
		// Validated kwargs contain only immutable JSON scalar values here.
		return typed
	}
}
