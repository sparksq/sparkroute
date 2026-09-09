// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ModelPolicyAssignment is an operator overlay keyed by canonical virtual-model
// name. Nil retains model settings (or a parent request profile's assignment);
// an empty string explicitly disables that policy. Absent models stay dormant.
type ModelPolicyAssignment struct {
	PIIProfile       *string `json:"pii_profile,omitempty"`
	GuardrailProfile *string `json:"guardrail_profile,omitempty"`
}

// ResolveModelPolicies validates reusable policy definitions and resolves their
// assignments into private model copies. Authored configuration is not changed.
// Full Document.Validate additionally checks references in effective guardrails.
func (d Document) ResolveModelPolicies() (Document, error) {
	if len(d.PIIProfiles) > 256 || len(d.GuardrailProfiles) > 256 || len(d.ModelPolicies) > 4096 {
		return Document{}, fmt.Errorf("policy profiles: at most 256 profiles of each type and 4096 model assignments are supported")
	}
	for _, name := range sortedPolicyNames(d.PIIProfiles) {
		policy := d.PIIProfiles[name]
		if err := validateID(name); err != nil {
			return Document{}, fmt.Errorf("pii_profiles[%q]: %w", name, err)
		}
		if err := validatePrivacy(fmt.Sprintf("pii_profiles[%q]", name), &PrivacyPolicy{PII: &policy}); err != nil {
			return Document{}, err
		}
	}
	for _, name := range sortedPolicyNames(d.GuardrailProfiles) {
		policy := d.GuardrailProfiles[name]
		if err := validateID(name); err != nil {
			return Document{}, fmt.Errorf("guardrail_profiles[%q]: %w", name, err)
		}
		// An unused profile may outlive generated inventory. Active model policies
		// are checked against the real model namespace by Document.Validate.
		names := map[string]string{}
		for _, entry := range append(slices.Clone(policy.Pre), policy.Post...) {
			names[entry.Model] = entry.Model
		}
		if err := validateGuardrails(fmt.Sprintf("guardrail_profiles[%q]", name), policy, names); err != nil {
			return Document{}, err
		}
	}
	canonical := make(map[string]VirtualModel, len(d.VirtualModels))
	aliases := map[string]string{}
	for _, model := range d.VirtualModels {
		canonical[model.Name] = model
		for _, alias := range model.Aliases {
			aliases[alias] = model.Name
		}
	}
	for _, name := range sortedPolicyNames(d.ModelPolicies) {
		assignment := d.ModelPolicies[name]
		if err := validateID(name); err != nil {
			return Document{}, fmt.Errorf("model_policies[%q]: %w", name, err)
		}
		if parent, alias := aliases[name]; alias && parent != name {
			return Document{}, fmt.Errorf("model_policies[%q]: use canonical virtual model %q instead of its alias", name, parent)
		}
		if ref := assignment.PIIProfile; ref != nil && *ref != "" {
			if _, exists := d.PIIProfiles[*ref]; !exists {
				return Document{}, fmt.Errorf("model_policies[%q].pii_profile: unknown profile %q", name, *ref)
			}
		}
		if ref := assignment.GuardrailProfile; ref != nil && *ref != "" {
			if _, exists := d.GuardrailProfiles[*ref]; !exists {
				return Document{}, fmt.Errorf("model_policies[%q].guardrail_profile: unknown profile %q", name, *ref)
			}
		}
	}
	resolved := d
	resolved.VirtualModels = slices.Clone(d.VirtualModels)
	for index, model := range resolved.VirtualModels {
		assignment := d.ModelPolicies[model.Name]
		// The UI groups explicit request variants beneath their immediate parent.
		// Walk the same hierarchy, with the nearest field assignment taking priority.
		child := model
		for len(child.RequestOverrides) > 0 {
			separator := strings.LastIndex(child.Name, ":")
			if separator < 0 {
				break
			}
			parent, exists := canonical[child.Name[:separator]]
			if !exists {
				break
			}
			inherited := d.ModelPolicies[parent.Name]
			if assignment.PIIProfile == nil {
				assignment.PIIProfile = inherited.PIIProfile
			}
			if assignment.GuardrailProfile == nil {
				assignment.GuardrailProfile = inherited.GuardrailProfile
			}
			child = parent
		}
		if ref := assignment.PIIProfile; ref != nil {
			policy := PIIPolicy{Mode: PIIModeDisabled}
			if *ref != "" {
				policy = d.PIIProfiles[*ref]
			}
			policy.Entities = slices.Clone(policy.Entities)
			if policy.Files != nil {
				files := *policy.Files
				policy.Files = &files
			}
			privacy := PrivacyPolicy{}
			if model.Privacy != nil {
				privacy = *model.Privacy
			}
			privacy.PII = &policy
			model.Privacy = &privacy
		}
		if ref := assignment.GuardrailProfile; ref != nil {
			policy := GuardrailPolicy{}
			if *ref != "" {
				policy = d.GuardrailProfiles[*ref]
			}
			policy.Pre = slices.Clone(policy.Pre)
			policy.Post = slices.Clone(policy.Post)
			if policy.Stream != nil {
				stream := *policy.Stream
				policy.Stream = &stream
			}
			model.Guardrails = policy
		}
		resolved.VirtualModels[index] = model
	}
	return resolved, nil
}

func sortedPolicyNames[T any](values map[string]T) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
