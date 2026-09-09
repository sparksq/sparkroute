// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestNormalizeRoutingRequestProjectsContentFreeStageSignals(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
  "messages": [
    {"role":"user","content":"fix it"},
    {"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"go test ./...\"}"}}]},
    {"role":"tool","tool_call_id":"call-1","content":"panic: secret-customer-value"}
  ]
}`)
	normalized, err := NormalizeRoutingRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized.StageHistory.Events) != 1 || normalized.StageHistory.Events[0].Severity != stageHardSeverity {
		t.Fatalf("stage history = %#v", normalized.StageHistory)
	}
	encoded, err := json.Marshal(normalized.StageHistory)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-customer-value") || strings.Contains(string(encoded), "go test") || strings.Contains(string(encoded), "exec_command") {
		t.Fatalf("stage history retained request content: %s", encoded)
	}
}

func TestNormalizeResponsesToolHistory(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
  "input": [
    {"role":"user","content":[{"type":"input_text","text":"implement"}]},
    {"type":"function_call","call_id":"call-1","name":"apply_patch","arguments":"{}"},
    {"type":"function_call_output","call_id":"call-1","output":"all tests passed"}
  ]
}`)
	normalized, err := NormalizeRoutingRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	events := normalized.StageHistory.Events
	if len(events) != 1 || events[0].Category != StageToolEdit || !events[0].TestsPassed {
		t.Fatalf("events = %#v", events)
	}
}

func TestStageSignalsRecognizePythonWritesWithoutTreatingSearchesAsWrites(t *testing.T) {
	for _, example := range []struct {
		command  string
		category StageToolCategory
	}{
		{"python3 - <<'PY'\np.write_text(source)\nPY", StageToolWrite},
		{"/usr/bin/python3 -c 'p.write_bytes(data)'", StageToolWrite},
		{"rg '.write_text(' src", StageToolRead},
		{"grep -R 'python p.write_text(' src", StageToolRead},
		{"python3 -c 'print(42)'", StageToolOther},
	} {
		body, _ := json.Marshal(map[string]any{"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call", "function": map[string]any{"name": "exec_command", "arguments": map[string]any{"cmd": example.command}}}}},
			map[string]any{"role": "tool", "tool_call_id": "call", "content": "ok"},
		}})
		normalized, err := NormalizeRoutingRequest(body)
		if err != nil || len(normalized.StageHistory.Events) != 1 || normalized.StageHistory.Events[0].Category != example.category {
			t.Fatalf("%q = %#v, %v", example.command, normalized.StageHistory, err)
		}
	}
}

func TestStageRouterSelectsCapableForCriticalError(t *testing.T) {
	t.Parallel()
	router := stageTestRouter(t, StagePickerEfficientFirst, 0.5)
	decision, err := router.Select("auto", NormalizedRequest{StageHistory: StageHistory{
		TurnDepth: 4,
		Events:    []StageEvent{{Category: StageToolOther, Severity: 1}},
	}}, []string{"strong", "weak"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ResolvedModel != "strong" || decision.Stage == nil || decision.Stage.DecisionSource != "override" || decision.Stage.Tier != "capable" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestStageRouterUsesCorroborativeSignalsAndFallback(t *testing.T) {
	t.Parallel()
	router := stageTestRouter(t, StagePickerEfficientFirst, 0.4)
	decision, err := router.Select("auto", NormalizedRequest{StageHistory: StageHistory{
		TurnDepth: 10,
		Events:    []StageEvent{{Category: StageToolRead}},
	}}, []string{"strong", "weak"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ResolvedModel != "strong" || decision.Stage.DecisionSource != "dimensions" || decision.Stage.Dimensions.Exploring != 1 {
		t.Fatalf("exploration decision = %#v", decision)
	}

	decision, err = router.Select("auto", NormalizedRequest{StageHistory: StageHistory{
		TurnDepth: 10,
		Events:    []StageEvent{{Category: StageToolRead}},
	}}, []string{"weak"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ResolvedModel != "weak" || decision.Stage.DecisionSource != "tier_unavailable" {
		t.Fatalf("fallback decision = %#v", decision)
	}
}

func TestStageRouterSettledProductionUsesEfficientTier(t *testing.T) {
	t.Parallel()
	router := stageTestRouter(t, StagePickerCapableFirst, 0.5)
	decision, err := router.Select("auto", NormalizedRequest{StageHistory: StageHistory{
		TurnDepth: 10,
		Events:    []StageEvent{{Category: StageToolEdit, TestsPassed: true}},
	}}, []string{"strong", "weak"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if decision.ResolvedModel != "weak" || decision.Stage.DecisionSource != "tests_passed" {
		t.Fatalf("decision = %#v", decision)
	}
}

func TestValidateStageRouterPolicy(t *testing.T) {
	t.Parallel()
	policy := stageTestPolicy(StagePickerEfficientFirst, 0.5)
	policy.VirtualModels["auto"] = VirtualModel{Strategy: "stage_router", Models: []string{"strong", "weak"}}
	if err := ValidateRoutingPolicy(policy); err == nil || !strings.Contains(err.Error(), "requires stage_router settings") {
		t.Fatalf("ValidateRoutingPolicy() error = %v", err)
	}
}

func TestStageRouterCompactionOverridesMissingToolHistory(t *testing.T) {
	router := stageTestRouter(t, StagePickerEfficientFirst, 1)
	decision, err := router.Select("auto", NormalizedRequest{StageHistory: StageHistory{Compacted: true}}, []string{"strong", "weak"}, false)
	if err != nil || decision.ResolvedModel != "strong" || decision.Stage.DecisionSource != "override" {
		t.Fatalf("compacted context without tool events = %#v, %v", decision, err)
	}
}

func TestStageRouterClosedThresholdKeepsDefaultAtBoundary(t *testing.T) {
	for _, picker := range []StagePicker{StagePickerEfficientFirst, StagePickerCapableFirst} {
		for _, threshold := range []float64{0, math.Tanh(0.5)} {
			history := StageHistory{Events: []StageEvent{{Category: StageToolOther}}}
			if threshold > 0 {
				history.TurnDepth = 10
				if picker == StagePickerEfficientFirst {
					history.Events[0].Category = StageToolRead
				} else {
					history.Events[0].Category = StageToolEdit
				}
			}
			router := stageTestRouter(t, picker, threshold)
			decision, err := router.Select("auto", NormalizedRequest{StageHistory: history}, []string{"strong", "weak"}, false)
			if err != nil || decision.Stage.Tier != stageDefaultTier(picker) || decision.Stage.DecisionSource != "fall_open" {
				t.Fatalf("picker=%s threshold=%g boundary = %#v, %v", picker, threshold, decision, err)
			}
			if threshold > 0 {
				router = stageTestRouter(t, picker, math.Nextafter(threshold, 0))
				decision, err = router.Select("auto", NormalizedRequest{StageHistory: history}, []string{"strong", "weak"}, false)
				if err != nil || decision.Stage.Tier == stageDefaultTier(picker) || decision.Stage.DecisionSource != "dimensions" {
					t.Fatalf("above threshold did not switch: %#v, %v", decision, err)
				}
			}
		}
	}
}

func stageTestRouter(t *testing.T, picker StagePicker, threshold float64) *RouterManager {
	t.Helper()
	router, err := NewRouterManager(stageTestPolicy(picker, threshold))
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func stageTestPolicy(picker StagePicker, threshold float64) RoutingPolicy {
	return RoutingPolicy{
		Version:             RoutingPolicyVersion,
		Revision:            1,
		DefaultVirtualModel: "auto",
		Models: map[string]ModelMetadata{
			"strong": {Enabled: true},
			"weak":   {Enabled: true},
		},
		VirtualModels: map[string]VirtualModel{
			"auto": {
				Strategy: "stage_router",
				Models:   []string{"strong", "weak"},
				StageRouter: &StageRouterPolicy{
					CapableModel: "strong", EfficientModel: "weak", Picker: picker,
					ConfidenceThreshold: &threshold,
				},
			},
		},
	}
}
