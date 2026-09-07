package gateway

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type requestPromptAffinity struct {
	directory      *promptcache.Directory
	scopeDigest    string
	virtualModel   string
	operation      string
	configRevision string
	prefixes       []promptcache.Prefix
	ttl            time.Duration
	match          *promptcache.Match
}

func (h *chatCompletionsHandler) preparePromptCacheAffinity(
	ctx context.Context,
	model config.VirtualModel,
	caller identity.Identity,
	envelope map[string]json.RawMessage,
	plan routing.Plan,
	disabled bool,
) requestPromptAffinity {
	policy := model.Selection.PromptCacheAffinity.Effective()
	sequenceField := h.operation.promptCacheSequenceField()
	if disabled || !policy.Enabled || sequenceField == "" ||
		h.promptCache == nil || h.promptFingerprinter == nil {
		return requestPromptAffinity{}
	}
	scope, ok := promptCacheScope(policy.Scope, caller)
	if !ok {
		return requestPromptAffinity{}
	}
	scopeDigest := h.promptFingerprinter.ScopeDigest(scope)
	prefixes, err := h.promptFingerprinter.Prefixes(
		envelope,
		promptcache.FingerprintOptions{
			ScopeDigest: scopeDigest, VirtualModel: model.Name,
			Operation: h.operation.String(), ConfigRevision: h.configRevision,
			SequenceField: sequenceField, MinPrefixBytes: policy.MinPrefixBytes,
			MaxPrefixes: policy.MaxPrefixesPerRequest,
		},
	)
	if err != nil || len(prefixes) == 0 {
		return requestPromptAffinity{}
	}
	candidates := promptCacheCandidates(plan)
	if len(candidates) == 0 {
		return requestPromptAffinity{}
	}
	state := requestPromptAffinity{
		directory: h.promptCache, scopeDigest: scopeDigest,
		virtualModel: model.Name, operation: h.operation.String(),
		configRevision: h.configRevision, prefixes: prefixes, ttl: policy.TTL,
	}
	if match, found := h.promptCache.Lookup(ctx, promptcache.Query{
		ScopeDigest: scopeDigest, VirtualModel: model.Name,
		Operation: h.operation.String(), ConfigRevision: h.configRevision,
		Prefixes: prefixes, Candidates: candidates, Now: time.Now(),
	}); found {
		state.match = &match
	}
	return state
}

// promptCacheCandidates limits lookup itself to the soft-preference boundary,
// rather than looking up every route and rejecting an impermissible result
// afterward. This lets an admissible shorter prefix win even when a longer
// prefix exists only in a lower-priority or less-preferred class.
func promptCacheCandidates(plan routing.Plan) []promptcache.Route {
	if len(plan.Candidates) == 0 {
		return nil
	}
	first := plan.Candidates[0]
	candidates := make([]promptcache.Route, 0, len(plan.Candidates))
	for _, selection := range plan.Candidates {
		if selection.PoolPriority != first.PoolPriority ||
			selection.PreferenceClass != first.PreferenceClass {
			continue
		}
		candidates = append(candidates, promptCacheRoute(selection))
	}
	return candidates
}

func (a requestPromptAffinity) record(selection routing.Selection, now time.Time) {
	if a.directory == nil || len(a.prefixes) == 0 {
		return
	}
	a.directory.Record(promptcache.Observation{
		ScopeDigest: a.scopeDigest, VirtualModel: a.virtualModel,
		Operation: a.operation, ConfigRevision: a.configRevision,
		Prefixes: a.prefixes, Route: promptCacheRoute(selection),
		LastSuccess: now, ExpiresAt: now.Add(a.ttl),
	})
}

func promptCacheRoute(selection routing.Selection) promptcache.Route {
	return promptcache.Route{
		Provider: selection.Provider.Name, Deployment: selection.Deployment.Name,
		UpstreamModel:    selection.Deployment.Model,
		UpstreamProtocol: string(selectionUpstreamProtocol(selection)),
	}
}

func promptCacheScope(
	scope config.PromptCacheAffinityScope,
	caller identity.Identity,
) (string, bool) {
	tenant := caller.Principal.Tenant
	if tenant == "" {
		tenant = caller.Attribution[identity.AttributeTenant]
	}
	switch scope {
	case config.PromptCacheAffinityTenant:
		if tenant == "" {
			tenant = "default"
		}
		return "tenant\x00" + tenant, true
	default:
		principal := caller.Principal.ID
		if principal == "" {
			// The standalone/Sparkrun profile may intentionally run with
			// authentication disabled. It is a single-tenant deployment, so all
			// such requests belong to one explicit implicit-caller scope.
			principal = "anonymous"
		}
		return "caller\x00" + tenant + "\x00" + principal, true
	}
}

func (o openAIOperation) promptCacheSequenceField() string {
	switch o {
	case openAIOperationChatCompletions, anthropicOperationMessages,
		bedrockOperationConverse, bedrockOperationConverseStream:
		return "messages"
	case openAIOperationResponses:
		return "input"
	case geminiOperationGenerateContent, geminiOperationStreamContent:
		return "contents"
	default:
		return ""
	}
}
