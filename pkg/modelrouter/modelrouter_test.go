// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package modelrouter

import (
	"context"
	"testing"
)

func TestPassthrough(t *testing.T) {
	t.Parallel()

	decision, err := (Passthrough{}).Route(context.Background(), Input{
		RequestedModel:       "default",
		ResolvedVirtualModel: "local-default",
		Candidates: []Candidate{
			{Name: "local-default"},
			{Name: "large"},
		},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if decision.VirtualModel != "local-default" {
		t.Fatalf("VirtualModel = %q, want local-default", decision.VirtualModel)
	}
}

func TestPassthroughRejectsCandidateEscape(t *testing.T) {
	t.Parallel()

	_, err := (Passthrough{}).Route(context.Background(), Input{
		ResolvedVirtualModel: "not-allowed",
		Candidates:           []Candidate{{Name: "allowed"}},
	})
	if err == nil {
		t.Fatal("Route() error = nil, want candidate validation error")
	}
}

func TestPassthroughPreservesProviderStatePin(t *testing.T) {
	t.Parallel()

	decision, err := (Passthrough{}).Route(context.Background(), Input{
		RequestedModel: "auto",
		ProviderStatePin: &ProviderStatePin{
			VirtualModel: "second",
			Kind:         "response",
		},
		Candidates: []Candidate{{Name: "first"}, {Name: "second"}},
	})
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if decision.VirtualModel != "second" ||
		decision.Reason != "provider_state_pin" ||
		decision.Annotations["selector"] != "auto" ||
		decision.Annotations["state_affinity_kind"] != "response" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestNativeRouterProviderStatePinDoesNotAdvanceSelector(t *testing.T) {
	t.Parallel()

	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 11,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{"first", "second"}},
		},
		Models: map[string]ModelMetadata{
			"first":  {Enabled: true},
			"second": {Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("NewRouterManager() error = %v", err)
	}
	candidates := []Candidate{{Name: "first"}, {Name: "second"}}
	for range 2 {
		decision, routeErr := router.Route(context.Background(), Input{
			RequestedModel: "auto",
			ProviderStatePin: &ProviderStatePin{
				VirtualModel: "second",
				Kind:         "file",
			},
			Candidates: candidates,
		})
		if routeErr != nil {
			t.Fatalf("pinned Route() error = %v", routeErr)
		}
		if decision.VirtualModel != "second" ||
			decision.Reason != "provider_state_pin" ||
			decision.Annotations["selector"] != "auto" {
			t.Fatalf("pinned decision = %#v", decision)
		}
	}
	decision, err := router.Route(context.Background(), Input{
		RequestedModel: "auto",
		Candidates:     candidates,
	})
	if err != nil {
		t.Fatalf("unpinned Route() error = %v", err)
	}
	if decision.VirtualModel != "first" {
		t.Fatalf(
			"first adaptive selection after pins = %q, want first",
			decision.VirtualModel,
		)
	}
}

func TestNativeRouterCarriesDirectModelProjectionPolicy(t *testing.T) {
	t.Parallel()

	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 7,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {Strategy: "balanced", Models: []string{"logical"}},
		},
		Models: map[string]ModelMetadata{
			"logical": {
				Enabled: true,
				MMProjection: &MMProjectionPolicy{
					AnalyzerModel: "analyzer", FailureMode: "fail_closed",
					TimeoutMS: 2500,
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NewRouterManager() error = %v", err)
	}
	input := Input{
		RequestedModel: "logical-alias", ResolvedVirtualModel: "logical",
		Candidates: []Candidate{{Name: "logical"}},
	}
	decision, err := router.Route(context.Background(), input)
	if err != nil {
		t.Fatalf("Route() error = %v", err)
	}
	if decision.VirtualModel != "logical" || decision.MMProjection == nil ||
		decision.MMProjection.AnalyzerModel != "analyzer" ||
		decision.MMProjection.FailureMode != "fail_closed" ||
		decision.MMProjection.TimeoutMS != 2500 {
		t.Fatalf("decision = %#v", decision)
	}
	decision.MMProjection.TimeoutMS = 1
	next, err := router.Route(context.Background(), input)
	if err != nil || next.MMProjection == nil || next.MMProjection.TimeoutMS != 2500 {
		t.Fatalf("cloned next decision = %#v, %v", next, err)
	}
}

func TestNativeRouterProjectionCapabilitiesApplyPrecedence(t *testing.T) {
	t.Parallel()

	router, err := NewRouterManager(RoutingPolicy{
		Version: RoutingPolicyVersion, Revision: 1,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]VirtualModel{
			"auto": {
				Strategy: "balanced", Models: []string{"inherits", "disabled"},
				MMProjection: &MMProjectionPolicy{FailureMode: "fallback"},
			},
		},
		Models: map[string]ModelMetadata{
			"inherits": {Enabled: true},
			"disabled": {Enabled: true, MMProjectionDisabled: true},
		},
	})
	if err != nil {
		t.Fatalf("NewRouterManager() error = %v", err)
	}
	capabilities, err := router.ProjectionCapabilities(
		context.Background(),
		Input{
			RequestedModel: "auto",
			Candidates:     []Candidate{{Name: "inherits"}, {Name: "disabled"}},
		},
	)
	if err != nil {
		t.Fatalf("ProjectionCapabilities() error = %v", err)
	}
	if len(capabilities["inherits"]) != 2 || capabilities["disabled"] != nil {
		t.Fatalf("projection capabilities = %#v", capabilities)
	}
}
