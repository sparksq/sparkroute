// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

// safeUnknownCapabilities are optional request features that can be forwarded
// speculatively to a model server. Capabilities representing a distinct API or
// gateway-owned resource contract remain fail-closed even under "unknown: try".
var safeUnknownCapabilities = map[Capability]struct{}{
	CapabilityTools:              {},
	CapabilityVision:             {},
	CapabilityAudioInput:         {},
	CapabilityAudioOutput:        {},
	CapabilityFileInput:          {},
	CapabilityDeveloperMessages:  {},
	CapabilityStructuredOutputs:  {},
	CapabilityJSONMode:           {},
	CapabilityReasoning:          {},
	CapabilityLogprobs:           {},
	CapabilitySeed:               {},
	CapabilityMultipleChoices:    {},
	CapabilityParallelTools:      {},
	CapabilityPrediction:         {},
	CapabilityServiceTier:        {},
	CapabilityProviderTools:      {},
	CapabilityStreamUsage:        {},
	CapabilityPromptCaching:      {},
	CapabilityCitations:          {},
	CapabilityProviderGuardrails: {},
	CapabilityProviderPrompts:    {},
}

// DeclaresCapability reports explicit support after honoring an explicit
// unsupported declaration. It is used when selecting a distinct wire API such
// as Responses, where permissive unknown support is intentionally insufficient.
func (d Deployment) DeclaresCapability(capability Capability) bool {
	for _, unsupported := range d.CapabilityPolicy.Unsupported {
		if unsupported == capability {
			return false
		}
	}
	for _, supported := range d.Capabilities {
		if supported == capability {
			return true
		}
	}
	return false
}

// MatchCapabilities classifies a deployment against a complete required set.
// Explicit unsupported declarations always reject. A permissive match is
// possible only for the bounded safe-to-attempt vocabulary above; API/resource
// capabilities and extensions still require an explicit declaration.
func (d Deployment) MatchCapabilities(required []Capability) CapabilityMatch {
	if len(required) == 0 {
		return CapabilityMatchDeclared
	}
	supported := make(map[Capability]struct{}, len(d.Capabilities))
	for _, capability := range d.Capabilities {
		supported[capability] = struct{}{}
	}
	unsupported := make(map[Capability]struct{}, len(d.CapabilityPolicy.Unsupported))
	for _, capability := range d.CapabilityPolicy.Unsupported {
		unsupported[capability] = struct{}{}
	}
	result := CapabilityMatchDeclared
	for _, capability := range required {
		if _, denied := unsupported[capability]; denied {
			return CapabilityMatchRejected
		}
		if _, declared := supported[capability]; declared {
			continue
		}
		if d.CapabilityPolicy.Unknown.Effective() != UnknownCapabilityTry {
			return CapabilityMatchRejected
		}
		if _, safe := safeUnknownCapabilities[capability]; !safe {
			return CapabilityMatchRejected
		}
		result = CapabilityMatchUnknown
	}
	return result
}
