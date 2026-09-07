// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"fmt"
	"strings"
)

// compiledRoutingPolicy is immutable after publication. It retains a private
// policy clone for metadata while indexing every request-time namespace lookup.
type compiledRoutingPolicy struct {
	policy       RoutingPolicy
	virtualBases map[string]compiledVirtualBase
	rules        []compiledKeywordRule
}

type compiledVirtualBase struct {
	name    string
	alias   string
	virtual VirtualModel
}

type compiledKeywordRule struct {
	rule     KeywordRule
	keywords []string
}

func compileRoutingPolicy(policy RoutingPolicy) *compiledRoutingPolicy {
	policy = clonePolicy(policy)
	compiled := &compiledRoutingPolicy{
		policy:       policy,
		virtualBases: make(map[string]compiledVirtualBase, len(policy.VirtualModels)),
		rules:        make([]compiledKeywordRule, 0, len(policy.KeywordRules)),
	}
	for name, virtual := range policy.VirtualModels {
		compiled.virtualBases[name] = compiledVirtualBase{name: name, virtual: virtual}
		for _, alias := range virtual.Aliases {
			compiled.virtualBases[alias] = compiledVirtualBase{name: name, alias: alias, virtual: virtual}
		}
	}
	for _, rule := range policy.KeywordRules {
		keywords := make([]string, len(rule.Keywords))
		for i, keyword := range rule.Keywords {
			keywords[i] = strings.ToLower(strings.TrimSpace(keyword))
		}
		compiled.rules = append(compiled.rules, compiledKeywordRule{rule: rule, keywords: keywords})
	}
	return compiled
}

func (compiled *compiledRoutingPolicy) resolveVirtualRequest(requested string) (virtualRequestMatch, bool, error) {
	if base, exists := compiled.virtualBases[requested]; exists {
		preset := ""
		var kwargs map[string]any
		if configured, hasDefault := base.virtual.Kwargs["default"]; hasDefault {
			preset = "default"
			kwargs = configured
		}
		return virtualRequestMatch{Name: base.name, Alias: base.alias, Preset: preset, Virtual: base.virtual, Kwargs: kwargs}, true, nil
	}
	separator := strings.LastIndex(requested, ":")
	if separator <= 0 || separator == len(requested)-1 {
		return virtualRequestMatch{}, false, nil
	}
	base, exists := compiled.virtualBases[requested[:separator]]
	if !exists {
		return virtualRequestMatch{}, false, nil
	}
	preset := requested[separator+1:]
	kwargs, exists := base.virtual.Kwargs[preset]
	if !exists {
		return virtualRequestMatch{}, false, fmt.Errorf("unknown kwargs preset %q for virtual model %q", preset, base.name)
	}
	return virtualRequestMatch{Name: base.name, Alias: base.alias, Preset: preset, Virtual: base.virtual, Kwargs: kwargs}, true, nil
}

func (compiled *compiledRoutingPolicy) ownsVirtualRequest(requested string) bool {
	requested = strings.TrimSpace(requested)
	if _, exists := compiled.virtualBases[requested]; exists {
		return true
	}
	separator := strings.LastIndex(requested, ":")
	return separator > 0 && separator < len(requested)-1 && compiled.virtualBases[requested[:separator]].name != ""
}

// isIdentityVirtualModel identifies a base selector whose unqualified request
// still resolves to the same logical model. Such a wrapper may publish aliases
// and :n variants without removing the underlying logical model from wildcard
// selectors such as auto.
func (compiled *compiledRoutingPolicy) isIdentityVirtualModel(name string) bool {
	base, exists := compiled.virtualBases[name]
	if !exists || base.alias != "" || base.name != name {
		return false
	}
	models := virtualModelsWithOverrides(base.virtual, base.virtual.Kwargs["default"])
	return len(models) == 1 && models[0] == name
}

func (compiled compiledKeywordRule) matches(text string) bool {
	matched := 0
	for _, keyword := range compiled.keywords {
		if strings.Contains(text, keyword) {
			matched++
		}
	}
	if compiled.rule.MatchAll {
		return matched == len(compiled.keywords)
	}
	return matched > 0
}
