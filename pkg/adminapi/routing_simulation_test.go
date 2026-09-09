// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package adminapi

import (
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func TestSimulateRoutingUsesCapabilityCandidatesAndKeywordRules(t *testing.T) {
	document := simulationDocument()
	result, err := SimulateRouting(RoutingSimulationInput{
		Document:             document,
		RequestedModel:       "auto",
		RoutingText:          "Please use vision for this image",
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
	}, config.UnknownCapabilityReject)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.ResolvedModel != "vision-model" ||
		result.Decision.MatchedRule != "vision" {
		t.Fatalf("decision = %#v", result.Decision)
	}
	if len(result.AvailableModels) != 1 || result.AvailableModels[0] != "vision-model" {
		t.Fatalf("available models = %v", result.AvailableModels)
	}
	if result.StateMode != "stateless" {
		t.Fatalf("state mode = %q", result.StateMode)
	}
}

func TestSimulateRoutingExercisesStageScenariosThroughNativeScorer(t *testing.T) {
	document := simulationDocument()
	document.ModelRouting.KeywordRules = nil
	selector := document.ModelRouting.VirtualModels["auto"]
	selector.Strategy = "stage_router"
	selector.StageRouter = &modelrouter.StageRouterPolicy{CapableModel: "vision-model", EfficientModel: "text-model", Picker: modelrouter.StagePickerEfficientFirst}
	document.ModelRouting.VirtualModels["auto"] = selector
	for _, example := range []struct{ scenario, model, source string }{
		{"no_tools", "text-model", "fall_open"}, {"exploring", "text-model", "fall_open"},
		{"error_recovery", "vision-model", "dimensions"}, {"productive", "text-model", "fall_open"},
		{"tests_passed", "text-model", "tests_passed"}, {"critical_error", "vision-model", "override"},
		{"compacted", "vision-model", "override"},
	} {
		result, err := SimulateRouting(RoutingSimulationInput{Document: document, RequestedModel: "auto", StageScenario: example.scenario}, config.UnknownCapabilityReject)
		if err != nil || result.Decision.ResolvedModel != example.model || result.Decision.Stage.DecisionSource != example.source {
			t.Fatalf("%s = %#v, %v", example.scenario, result, err)
		}
	}
	if _, err := SimulateRouting(RoutingSimulationInput{Document: document, StageScenario: "unknown"}, config.UnknownCapabilityReject); err == nil {
		t.Fatal("unknown scenario accepted")
	}
	result, err := SimulateRouting(RoutingSimulationInput{Document: document, RequestedModel: "auto", StageScenario: "tests_passed", RequiredCapabilities: []config.Capability{config.CapabilityVision}}, config.UnknownCapabilityReject)
	if err != nil || result.Decision.ResolvedModel != "vision-model" || result.Decision.Stage.DecisionSource != "tier_unavailable" {
		t.Fatalf("capability fallback = %#v, %v", result, err)
	}
	// With only the latest tool result, the earlier edit is outside the window.
	selector.StageRouter.Picker = modelrouter.StagePickerCapableFirst
	selector.StageRouter.RecentTurnWindow = 1
	result, err = SimulateRouting(RoutingSimulationInput{Document: document, RequestedModel: "auto", StageScenario: "tests_passed"}, config.UnknownCapabilityReject)
	if err != nil || result.Decision.ResolvedModel != "vision-model" || result.Decision.Stage.DecisionSource != "fall_open" {
		t.Fatalf("tool-result window = %#v, %v", result, err)
	}
}

func TestSimulateRoutingUsesProfileUnknownCapabilityDefault(t *testing.T) {
	document := simulationDocument()
	document.Deployments[0].Capabilities = nil
	document.Deployments[1].Capabilities = nil

	if _, err := SimulateRouting(RoutingSimulationInput{
		Document:             document,
		RequestedModel:       "auto",
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
	}, config.UnknownCapabilityReject); err == nil {
		t.Fatal("strict simulation unexpectedly admitted unknown vision support")
	}
	if _, err := SimulateRouting(RoutingSimulationInput{
		Document:             document,
		RequestedModel:       "auto",
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
	}, config.UnknownCapabilityTry); err != nil {
		t.Fatalf("permissive simulation: %v", err)
	}
}

func TestSimulateRoutingIncludesProjectionCapableTextModel(t *testing.T) {
	document := simulationDocument()
	selector := document.ModelRouting.VirtualModels["auto"]
	selector.Models = []string{"text-model"}
	selector.MMProjection = &modelrouter.MMProjectionPolicy{
		AnalyzerModel: "vision-analyzer", FailureMode: "fail_closed",
	}
	document.ModelRouting.VirtualModels["auto"] = selector

	result, err := SimulateRouting(RoutingSimulationInput{
		Document:             document,
		RequestedModel:       "auto",
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
	}, config.UnknownCapabilityReject)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.ResolvedModel != "text-model" ||
		len(result.AvailableModels) != 2 ||
		result.AvailableModels[0] != "text-model" ||
		result.AvailableModels[1] != "vision-model" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSimulateRoutingReportsPresetProviderPriority(t *testing.T) {
	document := simulationDocument()
	selector := document.ModelRouting.VirtualModels["auto"]
	selector.Kwargs = map[string]map[string]any{
		"private": {"provider_priority": []string{"private", "fallback"}},
	}
	document.ModelRouting.VirtualModels["auto"] = selector
	result, err := SimulateRouting(RoutingSimulationInput{
		Document: document, RequestedModel: "auto:private",
	}, config.UnknownCapabilityReject)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Decision.ProviderPriority) != 2 ||
		result.Decision.ProviderPriority[0] != "private" ||
		result.Decision.ProviderPriority[1] != "fallback" {
		t.Fatalf("provider priority = %v", result.Decision.ProviderPriority)
	}
}

func TestSimulateRoutingAppliesDiscoveredMetadata(t *testing.T) {
	document := simulationDocument()
	selector := document.ModelRouting.VirtualModels["auto"]
	selector.Strategy = "smallest"
	document.ModelRouting.VirtualModels["auto"] = selector
	large, small := 70.0, 7.0
	result, err := SimulateRoutingWithMetadata(RoutingSimulationInput{
		Document: document, RequestedModel: "auto",
	}, config.UnknownCapabilityReject, modelrouter.DiscoveredMetadataState{
		Sources: []modelrouter.DiscoveredMetadataSnapshot{{
			Source: "sparkrun:test", Models: map[string]modelrouter.DiscoveredModelMetadata{
				"text-model": {SizeB: &large}, "vision-model": {SizeB: &small},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.ResolvedModel != "vision-model" ||
		result.StateMode != "stateless_with_discovered_metadata" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSimulateRoutingRejectsMissingPolicyAndOversizedText(t *testing.T) {
	document := simulationDocument()
	document.ModelRouting = nil
	if _, err := SimulateRouting(RoutingSimulationInput{Document: document}, config.UnknownCapabilityTry); err == nil {
		t.Fatal("missing policy was accepted")
	}
	document = simulationDocument()
	text := make([]byte, maxRoutingSimulationTextBytes+1)
	if _, err := SimulateRouting(RoutingSimulationInput{
		Document: document, RoutingText: string(text),
	}, config.UnknownCapabilityTry); err == nil {
		t.Fatal("oversized routing text was accepted")
	}
}

func simulationDocument() config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name: "local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1",
		}},
		Deployments: []config.Deployment{
			{Name: "text", Provider: "local", Model: "text"},
			{Name: "vision", Provider: "local", Model: "vision", Capabilities: []config.Capability{config.CapabilityVision}},
		},
		VirtualModels: []config.VirtualModel{
			{Name: "text-model", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "text", Weight: 1}}}}},
			{Name: "vision-model", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "vision", Weight: 1}}}}},
		},
		ModelRouting: &modelrouter.RoutingPolicy{
			Version: modelrouter.RoutingPolicyVersion, Revision: 3,
			DefaultVirtualModel: "auto",
			VirtualModels: map[string]modelrouter.VirtualModel{
				"auto": {Strategy: "round_robin", Models: []string{"text-model", "vision-model"}},
			},
			Models: map[string]modelrouter.ModelMetadata{
				"text-model": {Enabled: true}, "vision-model": {Enabled: true},
			},
			KeywordRules: []modelrouter.KeywordRule{{
				Name: "vision", Keywords: []string{"vision", "image"}, VirtualModel: "auto",
			}},
		},
	}
}

func TestSimulationInheritsDeploymentSize(t *testing.T) {
	document := simulationDocument()
	a, b := 8.0, 70.0
	document.Deployments[0].ModelMetadata = &modelrouter.DiscoveredModelMetadata{SizeB: &a}
	document.Deployments[1].ModelMetadata = &modelrouter.DiscoveredModelMetadata{SizeB: &b}
	document.ModelRouting.VirtualModels["auto"] = modelrouter.VirtualModel{Strategy: "smallest"}
	result, err := SimulateRouting(RoutingSimulationInput{Document: document, RequestedModel: "auto"}, config.UnknownCapabilityTry)
	if err != nil {
		t.Fatal(err)
	}
	if result.Decision.ResolvedModel != "text-model" {
		t.Fatalf("decision: %+v", result.Decision)
	}
	document.Deployments[0].ModelMetadata.SizeB = &b
	document.Deployments[1].ModelMetadata.SizeB = &a
	result, err = SimulateRouting(RoutingSimulationInput{Document: document, RequestedModel: "auto"}, config.UnknownCapabilityTry)
	if err != nil || result.Decision.ResolvedModel != "vision-model" {
		t.Fatalf("updated decision: %+v, %v", result, err)
	}
}
