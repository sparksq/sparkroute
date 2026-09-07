// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

// This file owns request planning, built-in model selection, and decision
// enrichment. Policy persistence and runtime metrics stay isolated.

func (r *RouterManager) Select(requested string, req NormalizedRequest, available []string, dryRun bool) (RouteDecision, error) {
	plan, err := r.Plan(requested, req, available)
	if err != nil {
		return RouteDecision{}, err
	}
	return r.ResolveNative(plan, dryRun)
}

// Route adapts the SparkRoute policy engine to the protocol-neutral gateway
// model-router seam. Candidate validation remains authoritative in both
// layers: the policy can narrow the supplied set, never escape it.
func (r *RouterManager) Route(ctx context.Context, input Input) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	available := make([]string, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
		available = append(available, candidate.Name)
	}
	requested := input.RequestedModel
	if input.ProviderStatePin != nil {
		// Provider-owned state is stronger than an adaptive selector. Resolve the
		// pinned logical model as an explicit route so current model-level policy
		// still applies without advancing selector counters or re-running keyword
		// selection. The gateway separately enforces the provider/deployment pin.
		requested = input.ProviderStatePin.VirtualModel
	} else if !r.OwnsVirtualRequest(requested) && input.ResolvedVirtualModel != "" {
		// Gateway aliases are not part of the model-policy namespace. Select the
		// already resolved canonical logical model so direct aliases retain their
		// normal behavior while still receiving per-model projection policy.
		requested = input.ResolvedVirtualModel
	}
	decision, err := r.Select(
		requested,
		NormalizedRequest{Text: input.Features.RoutingText, StageHistory: input.Features.StageHistory},
		available,
		false,
	)
	if err != nil {
		return Decision{}, err
	}
	allowed := false
	for _, candidate := range input.Candidates {
		if candidate.Name == decision.ResolvedModel {
			allowed = true
			break
		}
	}
	if !allowed {
		return Decision{}, fmt.Errorf(
			"routing policy selected virtual model %q outside the candidate set",
			decision.ResolvedModel,
		)
	}
	annotations := map[string]string{
		"strategy": decision.Strategy,
	}
	if decision.VirtualModel != "" {
		annotations["selector"] = decision.VirtualModel
	}
	if decision.MatchedRule != "" {
		annotations["matched_rule"] = decision.MatchedRule
	}
	if decision.RoutingPreset != "" {
		annotations["routing_preset"] = decision.RoutingPreset
	}
	if decision.Stage != nil {
		annotations["decision_source"] = decision.Stage.DecisionSource
		annotations["routing_tier"] = decision.Stage.Tier
		annotations["stage_score"] = fmt.Sprintf("%.6f", decision.Stage.Score)
		annotations["stage_confidence"] = fmt.Sprintf("%.6f", decision.Stage.Confidence)
		annotations["stage_severity"] = fmt.Sprintf("%.6f", decision.Stage.Dimensions.Severity)
		annotations["stage_spinning"] = fmt.Sprintf("%.6f", decision.Stage.Dimensions.Spinning)
		annotations["stage_exploring"] = fmt.Sprintf("%.6f", decision.Stage.Dimensions.Exploring)
		annotations["stage_production_intensity"] = fmt.Sprintf("%.6f", decision.Stage.Dimensions.ProductionIntensity)
	}
	reason := decision.Reason
	if input.ProviderStatePin != nil {
		annotations["selector"] = input.RequestedModel
		annotations["state_affinity_kind"] = input.ProviderStatePin.Kind
		reason = "provider_state_pin"
	}
	return Decision{
		VirtualModel:     decision.ResolvedModel,
		Router:           "fox-sparkroute-native",
		Version:          fmt.Sprintf("policy-v%d-revision-%d", RoutingPolicyVersion, decision.Revision),
		Reason:           reason,
		ProviderPriority: append([]string(nil), decision.ProviderPriority...),
		MMProjection:     cloneMMProjectionPolicy(decision.projectionPolicy),
		Annotations:      annotations,
		Stage:            decision.Stage,
	}, nil
}

// ProjectionCapabilities resolves the same virtual namespace, routing preset,
// keyword rule, and per-model precedence used by Route without advancing any
// selection state. Projection covers inline image/video and audio input; the
// current gateway capability vocabulary represents those as vision and
// audio_input.
func (r *RouterManager) ProjectionCapabilities(
	ctx context.Context,
	input Input,
) (map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	available := make([]string, 0, len(input.Candidates))
	for _, candidate := range input.Candidates {
		available = append(available, candidate.Name)
	}
	requested := input.RequestedModel
	if input.ProviderStatePin != nil {
		requested = input.ProviderStatePin.VirtualModel
	}
	plan, err := r.Plan(
		requested,
		NormalizedRequest{Text: input.Features.RoutingText, StageHistory: input.Features.StageHistory},
		available,
	)
	if err != nil {
		return nil, err
	}
	result := make(map[string][]string)
	for _, candidate := range plan.Eligible {
		if resolvedMMProjectionPolicy(plan, candidate) != nil {
			result[candidate] = []string{"vision", "audio_input"}
		}
	}
	return result, nil
}

func (r *RouterManager) ProjectionAnalyzerModels() []string {
	compiled := r.runtime.Load()
	if compiled == nil {
		return nil
	}
	seen := map[string]struct{}{}
	for _, metadata := range compiled.policy.Models {
		if metadata.MMProjection != nil {
			model := strings.TrimSpace(metadata.MMProjection.AnalyzerModel)
			if model != "" {
				seen[model] = struct{}{}
			}
		}
	}
	for _, virtual := range compiled.policy.VirtualModels {
		if virtual.MMProjection != nil {
			model := strings.TrimSpace(virtual.MMProjection.AnalyzerModel)
			if model != "" {
				seen[model] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for model := range seen {
		result = append(result, model)
	}
	sort.Strings(result)
	return result
}

// Plan resolves explicit/virtual semantics, ordered rules, and candidate
// eligibility without advancing counters or recording a decision.
func (r *RouterManager) Plan(requested string, req NormalizedRequest, available []string) (RoutePlan, error) {
	compiled := r.runtime.Load()
	if compiled == nil {
		return RoutePlan{}, errors.New("routing policy is unavailable")
	}
	p := compiled.policy
	requested = strings.TrimSpace(requested)
	if requested == "" {
		requested = p.DefaultVirtualModel
	}
	if requested == "" {
		requested = "auto"
	}
	avail := make(map[string]bool, len(available))
	for _, m := range available {
		avail[m] = true
	}
	virtualMatch, virtual, virtualErr := compiled.resolveVirtualRequest(requested)
	if virtualErr != nil {
		return RoutePlan{}, virtualErr
	}
	// Membership in the virtual namespace is authoritative at request time. If
	// a physical/main route shares this name, catalog filtering may hide that
	// underlying card; calls to the shared name intentionally enter the selector.
	if !virtual && avail[requested] {
		meta, known := p.Models[requested]
		if known && !meta.Enabled {
			return RoutePlan{}, fmt.Errorf("explicit model %q is disabled", requested)
		}
		return RoutePlan{RequestedModel: requested, Strategy: "explicit", Revision: p.Revision, Candidates: []CandidateTrace{{Model: requested, Eligible: true}}, Eligible: []string{requested}, Models: p.Models, Explicit: true, At: time.Now().UTC()}, nil
	}
	if !virtual {
		return RoutePlan{}, fmt.Errorf("unknown or unavailable explicit model %q", requested)
	}
	vm := virtualMatch.Virtual
	strategy, names := vm.Strategy, virtualModelsWithOverrides(vm, virtualMatch.Kwargs)
	stage := cloneStageRouterPolicy(vm.StageRouter)
	projection := cloneMMProjectionPolicy(vm.MMProjection)
	kwargs := cloneRouterKwargs(virtualMatch.Kwargs)
	matched := ""
	text := ""
	if len(compiled.rules) > 0 {
		text = strings.ToLower(req.Text)
	}
	for i, compiledRule := range compiled.rules {
		rule := compiledRule.rule
		if compiledRule.matches(text) {
			matched = rule.Name
			if matched == "" {
				matched = fmt.Sprintf("rule[%d]", i)
			}
			if rule.VirtualModel != "" {
				x := p.VirtualModels[rule.VirtualModel]
				strategy, names = x.Strategy, x.Models
				stage = cloneStageRouterPolicy(x.StageRouter)
				projection = cloneMMProjectionPolicy(x.MMProjection)
			} else {
				strategy, names = rule.Strategy, rule.Models
				stage = nil
				projection = nil
			}
			break
		}
	}
	if override, ok := kwargs["strategy"].(string); ok && strategies[override] {
		strategy = override
	}
	providerPriority := routingProviderPriority(kwargs)
	if len(names) == 0 {
		for n := range avail {
			// Virtual names, aliases, and preset endpoints are selectors, not
			// physical wildcard candidates. Excluding them prevents a virtual
			// alias that collides with discovery from recursively selecting itself.
			if !compiled.ownsVirtualRequest(n) || compiled.isIdentityVirtualModel(n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
	}
	traces := make([]CandidateTrace, 0, len(names))
	eligible := make([]string, 0, len(names))
	for _, n := range names {
		m, configured := p.Models[n]
		reason := ""
		if configured && !m.Enabled {
			reason = "disabled"
		} else if !avail[n] {
			reason = "unavailable"
		}
		t := CandidateTrace{Model: n, Eligible: reason == "", Reason: reason}
		traces = append(traces, t)
		if reason == "" {
			eligible = append(eligible, n)
		}
	}
	if len(eligible) == 0 {
		return RoutePlan{}, fmt.Errorf("no eligible models for %q", requested)
	}
	return RoutePlan{RequestedModel: requested, VirtualModel: virtualMatch.Name, VirtualAlias: virtualMatch.Alias, RoutingPreset: virtualMatch.Preset, RouterKwargs: kwargs, ProviderPriority: providerPriority, Strategy: strategy, MatchedRule: matched, Revision: p.Revision, Candidates: traces, Eligible: eligible, Models: p.Models, StageRouter: stage, StageHistory: req.StageHistory, MMProjection: projection, At: time.Now().UTC()}, nil
}

func virtualModelsWithOverrides(virtual VirtualModel, kwargs map[string]any) []string {
	models := append([]string(nil), virtual.Models...)
	if override, ok := routingStringList(kwargs["models"], false); ok && len(override) > 0 {
		models = override
	}
	return models
}

type virtualRequestMatch struct {
	Name    string
	Alias   string
	Preset  string
	Virtual VirtualModel
	Kwargs  map[string]any
}

func resolveVirtualRequest(policy RoutingPolicy, requested string) (virtualRequestMatch, bool, error) {
	return compileRoutingPolicy(policy).resolveVirtualRequest(requested)
}

func policyOwnsVirtualRequest(policy RoutingPolicy, requested string) bool {
	return compileRoutingPolicy(policy).ownsVirtualRequest(requested)
}

func (r *RouterManager) OwnsVirtualRequest(requested string) bool {
	compiled := r.runtime.Load()
	return compiled != nil && compiled.ownsVirtualRequest(requested)
}

func cloneRouterKwargs(kwargs map[string]any) map[string]any {
	return cloneJSONObject(kwargs)
}

func routingProviderPriority(kwargs map[string]any) []string {
	raw, exists := kwargs["provider_priority"]
	if !exists {
		return nil
	}
	providers, ok := routingStringList(raw, true)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(providers))
	seen := map[string]bool{}
	for _, provider := range providers {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if validBackendID(provider) && !seen[provider] {
			seen[provider] = true
			out = append(out, provider)
		}
	}
	return out
}

// ResolveNative completes a plan using the built-in deterministic strategies.
func (r *RouterManager) ResolveNative(plan RoutePlan, dryRun bool) (RouteDecision, error) {
	return r.resolveNative(plan, dryRun)
}

func (r *RouterManager) resolveNative(plan RoutePlan, dryRun bool) (RouteDecision, error) {
	if len(plan.Eligible) == 0 {
		return RouteDecision{}, errors.New("route plan has no eligible models")
	}
	if plan.Explicit {
		chosen := plan.Eligible[0]
		d := RouteDecision{RequestedModel: plan.RequestedModel, ResolvedModel: chosen, Strategy: "explicit", Reason: "explicit physical model", Revision: plan.Revision, At: plan.At, Candidates: cloneTraces(plan.Candidates), projectionPolicy: resolvedMMProjectionPolicy(plan, chosen)}
		d = enrichRouteDecision(plan, d)
		return d, nil
	}
	key := plan.VirtualModel + "|" + plan.RoutingPreset + "|" + plan.MatchedRule + "|" + plan.Strategy
	var chosen string
	var scores map[string]float64
	var stageTrace *StageRouteTrace
	if plan.Strategy == "stage_router" {
		chosen, stageTrace = chooseStageRoute(plan)
		if chosen == "" {
			return RouteDecision{}, errors.New("stage_router has no eligible tier target")
		}
		scores = map[string]float64{chosen: 1}
	} else {
		chosen, scores = r.choose(plan.Strategy, plan.Eligible, plan.Models, key, dryRun)
	}
	traces := cloneTraces(plan.Candidates)
	for i := range traces {
		if s, ok := scores[traces[i].Model]; ok {
			traces[i].Score = s
			traces[i].Scores = map[string]float64{"selection": s}
		}
	}
	reason := "selected by " + plan.Strategy
	if plan.MatchedRule != "" {
		reason = "matched " + plan.MatchedRule + "; " + reason
	}
	if stageTrace != nil {
		reason = fmt.Sprintf("stage_router selected %s tier via %s (confidence %.3f)", stageTrace.Tier, stageTrace.DecisionSource, stageTrace.Confidence)
	}
	d := RouteDecision{RequestedModel: plan.RequestedModel, ResolvedModel: chosen, Strategy: plan.Strategy, MatchedRule: plan.MatchedRule, Reason: reason, Revision: plan.Revision, Candidates: traces, Stage: stageTrace, At: plan.At, projectionPolicy: resolvedMMProjectionPolicy(plan, chosen)}
	d = enrichRouteDecision(plan, d)
	return d, nil
}

func enrichRouteDecision(plan RoutePlan, decision RouteDecision) RouteDecision {
	decision.VirtualModel = plan.VirtualModel
	decision.VirtualAlias = plan.VirtualAlias
	decision.RoutingPreset = plan.RoutingPreset
	decision.RouterKwargs = cloneRouterKwargs(plan.RouterKwargs)
	decision.ProviderPriority = append([]string(nil), plan.ProviderPriority...)
	return decision
}

func cloneMMProjectionPolicy(policy *MMProjectionPolicy) *MMProjectionPolicy {
	if policy == nil {
		return nil
	}
	clone := *policy
	return &clone
}

// resolvedMMProjectionPolicy applies the selected model's setting first and
// retains a virtual model's MMProjection policy as a backwards-compatible
// default. This makes direct and virtual-selected calls to the same logical
// model behave identically while allowing each model to override failure and
// timeout behavior independently.
func resolvedMMProjectionPolicy(plan RoutePlan, model string) *MMProjectionPolicy {
	if metadata, configured := plan.Models[model]; configured {
		if metadata.MMProjectionDisabled {
			return nil
		}
		if metadata.MMProjection != nil {
			return cloneMMProjectionPolicy(metadata.MMProjection)
		}
	}
	return cloneMMProjectionPolicy(plan.MMProjection)
}
func cloneTraces(in []CandidateTrace) []CandidateTrace {
	out := append([]CandidateTrace(nil), in...)
	for i := range out {
		if in[i].Scores != nil {
			out[i].Scores = make(map[string]float64, len(in[i].Scores))
			for k, v := range in[i].Scores {
				out[i].Scores[k] = v
			}
		}
	}
	return out
}
func containsString(in []string, value string) bool {
	for _, s := range in {
		if s == value {
			return true
		}
	}
	return false
}

func (r *RouterManager) choose(strategy string, names []string, models map[string]ModelMetadata, key string, dry bool) (string, map[string]float64) {
	scores := map[string]float64{}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if strategy == "random" {
		total := 0
		for _, n := range sorted {
			w := models[n].Weight
			if w <= 0 {
				w = 1
			}
			total += w
		}
		pick := 0
		if n, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(total))); err == nil {
			pick = int(n.Int64())
		} else {
			pick = int(time.Now().UnixNano() % int64(total))
		}
		for _, n := range sorted {
			w := models[n].Weight
			if w <= 0 {
				w = 1
			}
			if pick < w {
				return n, scores
			}
			pick -= w
		}
	}
	if strategy == "round_robin" || strategy == "weighted_round_robin" {
		total := uint64(0)
		for _, n := range sorted {
			w := 1
			if strategy == "weighted_round_robin" && models[n].Weight > 0 {
				w = models[n].Weight
			}
			total += uint64(w)
		}
		r.counterMu.Lock()
		idx := r.counters[key] % total
		if !dry {
			r.counters[key]++
		}
		r.counterMu.Unlock()
		cumulative := uint64(0)
		for _, n := range sorted {
			w := 1
			if strategy == "weighted_round_robin" && models[n].Weight > 0 {
				w = models[n].Weight
			}
			cumulative += uint64(w)
			if idx < cumulative {
				return n, scores
			}
		}
	}
	best := ""
	bestScore := math.Inf(1)
	observations := map[string]ModelObservation(nil)
	if strategy == "fastest" || strategy == "balanced" {
		observations = make(map[string]ModelObservation, len(sorted))
		r.observationMu.RLock()
		for _, name := range sorted {
			observations[name] = r.observations[name]
		}
		r.observationMu.RUnlock()
	}
	maxSize, maxCost, maxLatency := 0.0, 0.0, 0.0
	for _, n := range sorted {
		m := models[n]
		maxSize = math.Max(maxSize, m.SizeB)
		maxCost = math.Max(maxCost, m.InputPrice+m.OutputPrice)
		maxLatency = math.Max(maxLatency, observations[n].EWMALatencyMS)
	}
	for _, n := range sorted {
		m := models[n]
		var s float64
		switch strategy {
		case "smallest":
			s = m.SizeB
		case "largest":
			s = -m.SizeB
		case "lowest_cost":
			s = m.InputPrice + m.OutputPrice
		case "fastest":
			s = observations[n].EWMALatencyMS
			if s == 0 && maxLatency > 0 {
				s = maxLatency
			}
		case "balanced":
			size, cost, lat := m.SizeB/math.Max(maxSize, 1), (m.InputPrice+m.OutputPrice)/math.Max(maxCost, 1), observations[n].EWMALatencyMS/math.Max(maxLatency, 1)
			s = cost + lat - size*0.25 - float64(m.Priority)*0.01
		}
		scores[n] = s
		if best == "" || s < bestScore || (s == bestScore && n < best) {
			best, bestScore = n, s
		}
	}
	return best, scores
}
