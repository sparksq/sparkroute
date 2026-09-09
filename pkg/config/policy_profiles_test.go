// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func policyRef(s string) *string { return &s }
func policyDocument() Document {
	d := validDocument()
	parent := d.VirtualModels[0]
	variant := parent
	variant.Name = parent.Name + ":low"
	variant.Aliases = nil
	variant.RequestOverrides = map[string]map[string]json.RawMessage{"chat_completions": {"temperature": json.RawMessage(`0.2`)}}
	d.VirtualModels = append(d.VirtualModels, variant)
	d.PIIProfiles = map[string]PIIPolicy{"private": {Entities: []PIIEntity{PIIEntityEmail}, Files: &PIIFilePolicy{}}}
	d.GuardrailProfiles = map[string]GuardrailPolicy{"safe": {Pre: []Guardrail{{Name: "check", Model: parent.Name}}, Post: []Guardrail{{Name: "post", Model: parent.Name}}, Stream: &GuardrailStreamPolicy{}}}
	d.ModelPolicies = map[string]ModelPolicyAssignment{parent.Name: {PIIProfile: policyRef("private"), GuardrailProfile: policyRef("safe")}}
	return d
}

func TestPolicyProfilesResolveInheritanceAndIsolateAuthoredConfiguration(t *testing.T) {
	d := policyDocument()
	before, _ := json.Marshal(d)
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	resolved, err := d.ResolveModelPolicies()
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range resolved.VirtualModels {
		if model.Privacy == nil || model.Privacy.PII.Entities[0] != PIIEntityEmail || len(model.Guardrails.Pre) != 1 {
			t.Fatalf("unresolved model: %#v", model)
		}
	}
	resolved.VirtualModels[0].Privacy.PII.Entities[0] = PIIEntityPhone
	resolved.VirtualModels[0].Privacy.PII.Files.Metadata = true
	resolved.VirtualModels[0].Guardrails.Pre[0].Prompt = "changed"
	resolved.VirtualModels[0].Guardrails.Stream.WindowBytes = 4096
	if resolved.VirtualModels[1].Privacy.PII.Entities[0] != PIIEntityEmail || resolved.VirtualModels[1].Guardrails.Pre[0].Prompt != "" {
		t.Fatal("assigned models share mutable policies")
	}
	after, _ := json.Marshal(d)
	if string(before) != string(after) {
		t.Fatal("resolution mutated authored configuration")
	}
	encoded, _, err := EncodeCanonical(d)
	if err != nil {
		t.Fatal(err)
	}
	var roundtrip Document
	if err := json.Unmarshal(encoded, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.VirtualModels[0].Privacy != nil || len(roundtrip.VirtualModels[1].Guardrails.Pre) != 0 || !reflect.DeepEqual(roundtrip.ModelPolicies, d.ModelPolicies) {
		t.Fatal("encoding materialized policy assignments")
	}
}

func TestPolicyAssignmentsPerFieldOverridesAndDormantModels(t *testing.T) {
	d := policyDocument()
	parent, child := d.VirtualModels[0].Name, d.VirtualModels[1].Name
	d.ModelPolicies[child] = ModelPolicyAssignment{PIIProfile: policyRef("")}
	d.ModelPolicies["temporarily-absent"] = ModelPolicyAssignment{PIIProfile: policyRef("private")}
	resolved, err := d.ResolveModelPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.VirtualModels[1].Privacy.PII.Mode != PIIModeDisabled || len(resolved.VirtualModels[1].Guardrails.Pre) != 1 {
		t.Fatal("per-field inheritance/disable failed")
	}
	delete(d.ModelPolicies, parent)
	d.VirtualModels[0].Privacy = &PrivacyPolicy{PII: &PIIPolicy{Response: PIIResponseMasked}}
	resolved, err = d.ResolveModelPolicies()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.VirtualModels[0].Privacy.PII.Response != PIIResponseMasked {
		t.Fatal("omitted assignment discarded inline settings")
	}
	d.VirtualModels = nil
	d.Deployments = nil
	d.Providers = nil
	if err = d.Validate(); err != nil {
		t.Fatalf("dormant assignments should survive inventory removal: %v", err)
	}
}

func TestPolicyProfilesValidateDefinitionsAndReferences(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Document)
		want   string
	}{
		{"unknown dormant profile", func(d *Document) { d.ModelPolicies["gone"] = ModelPolicyAssignment{PIIProfile: policyRef("missing")} }, "unknown profile"},
		{"profile deletion", func(d *Document) { delete(d.PIIProfiles, "private") }, "unknown profile"},
		{"alias assignment", func(d *Document) {
			d.ModelPolicies["default"] = ModelPolicyAssignment{PIIProfile: policyRef("private")}
		}, "canonical virtual model"},
		{"unused invalid PII", func(d *Document) { d.PIIProfiles["bad"] = PIIPolicy{Scope: "invalid"} }, "scope"},
		{"unused invalid guardrail", func(d *Document) {
			d.GuardrailProfiles["bad"] = GuardrailPolicy{Pre: []Guardrail{{Name: "check", Model: "none", FailureMode: "invalid"}}}
		}, "failure_mode"},
		{"invalid name", func(d *Document) { d.PIIProfiles["has space"] = PIIPolicy{} }, "pii_profiles"},
		{"active missing guard target", func(d *Document) {
			d.GuardrailProfiles["safe"] = GuardrailPolicy{Pre: []Guardrail{{Name: "check", Model: "missing"}}}
		}, "unknown virtual model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := policyDocument()
			tt.change(&d)
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v want %s", err, tt.want)
			}
		})
	}
	d := policyDocument()
	d.GuardrailProfiles["unused"] = GuardrailPolicy{Pre: []Guardrail{{Name: "check", Model: "temporarily-absent"}}}
	if err := d.Validate(); err != nil {
		t.Fatalf("unused guard profile: %v", err)
	}
}
