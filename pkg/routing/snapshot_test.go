package routing

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

type sequencePicker struct {
	values []int64
}

type eligibilityMap map[string]bool

func (e eligibilityMap) Eligible(deployment string) bool {
	return e[deployment]
}

func (p *sequencePicker) Pick(totalWeight int64) (int64, error) {
	if len(p.values) == 0 {
		return 0, fmt.Errorf("picker exhausted for total %d", totalWeight)
	}
	value := p.values[0]
	p.values = p.values[1:]
	return value, nil
}

func TestSnapshotResolvesAliasAndLowestPriority(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlan("default", &sequencePicker{values: []int64{0, 0}})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	selection := plan.Candidates[0]
	if selection.VirtualModel != "public" {
		t.Fatalf("VirtualModel = %q, want public", selection.VirtualModel)
	}
	if selection.Deployment.Name != "primary" {
		t.Fatalf("Deployment = %q, want primary", selection.Deployment.Name)
	}
	if selection.Deployment.Model != "upstream-primary" {
		t.Fatalf("upstream model = %q", selection.Deployment.Model)
	}
}

func TestSnapshotIsImmutableFromSourceDocument(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Providers[0].ExtraBody = map[string]json.RawMessage{"service_tier": json.RawMessage(`"original"`)}
	document.Deployments[1].ExtraBody = map[string]json.RawMessage{"top_k": json.RawMessage(`40`)}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	document.Providers[0].DefaultHeaders["X-Profile"] = config.HeaderValue{Value: "mutated"}
	document.Providers[0].ExtraBody["service_tier"][1] = 'X'
	document.Deployments[1].ExtraBody["top_k"][0] = '9'
	document.Deployments[0].Model = "mutated"

	plan, err := snapshot.BuildPlan("public", &sequencePicker{values: []int64{0, 0}})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	selection := plan.Candidates[0]
	if selection.Provider.DefaultHeaders["X-Profile"].Value != "original" {
		t.Fatalf("compiled provider was mutated: %#v", selection.Provider.DefaultHeaders)
	}
	if selection.Deployment.Model != "upstream-primary" {
		t.Fatalf("compiled deployment model = %q", selection.Deployment.Model)
	}
	if string(selection.Provider.ExtraBody["service_tier"]) != `"original"` || string(selection.Deployment.ExtraBody["top_k"]) != "40" {
		t.Fatalf("compiled extra-body defaults were mutated: provider=%s deployment=%s", selection.Provider.ExtraBody["service_tier"], selection.Deployment.ExtraBody["top_k"])
	}
}

func TestSnapshotUnknownModel(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(routingDocument())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlan("missing", &sequencePicker{})
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("BuildPlan() error = %v, want ErrModelNotFound", err)
	}
}

func TestSnapshotReportsTargetProviderTypes(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(routingDocument())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	found, err := snapshot.HasTargetProviderType(
		"default",
		[]string{"openai_compatible"},
		false,
	)
	if err != nil || !found {
		t.Fatalf(
			"HasTargetProviderType(openai_compatible) = %v, %v",
			found,
			err,
		)
	}
	found, err = snapshot.HasTargetProviderType(
		"default",
		[]string{"bedrock"},
		false,
	)
	if err != nil || found {
		t.Fatalf(
			"HasTargetProviderType(bedrock) = %v, %v",
			found,
			err,
		)
	}
}

func TestSnapshotReportsNativeProtocols(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments[0].NativeProtocols = []config.Protocol{
		config.ProtocolOpenAI,
		config.ProtocolAnthropic,
	}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	for _, protocol := range []config.Protocol{
		config.ProtocolOpenAI,
		config.ProtocolAnthropic,
	} {
		found, findErr := snapshot.HasTargetNativeProtocol("default", protocol, false)
		if findErr != nil || !found {
			t.Fatalf("HasTargetNativeProtocol(%q) = %v, %v", protocol, found, findErr)
		}
	}
	found, err := snapshot.HasTargetNativeProtocol(
		"default",
		config.ProtocolGemini,
		false,
	)
	if err != nil || found {
		t.Fatalf("HasTargetNativeProtocol(gemini) = %v, %v", found, err)
	}
}

func TestBuildPlanPrefersNativeProtocolBeforeTranslation(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments = []config.Deployment{
		{
			Name:            "native",
			Provider:        "provider",
			Model:           "native-model",
			NativeProtocols: []config.Protocol{config.ProtocolOpenAI},
			CapabilityPolicy: config.CapabilityPolicy{
				Unknown: config.UnknownCapabilityTry,
			},
		},
		{
			Name:            "translated",
			Provider:        "provider",
			Model:           "translated-model",
			NativeProtocols: []config.Protocol{config.ProtocolAnthropic},
			Capabilities:    []config.Capability{config.CapabilityTools},
		},
	}
	document.VirtualModels[0].Pools = []config.RoutingPool{{
		Targets: []config.WeightedTarget{
			{Deployment: "translated", Weight: 100},
			{Deployment: "native", Weight: 1},
		},
	}}
	document.VirtualModels[0].Limits.MaxAttempts = 2
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityTools},
		ProtocolResolver: func(
			_ config.Provider,
			deployment config.Deployment,
		) (ProtocolRoute, bool) {
			protocol := deployment.NativeProtocols[0]
			return ProtocolRoute{
				Protocol: protocol,
				Native:   protocol == config.ProtocolOpenAI,
			}, true
		},
		Picker: &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if got := plan.Candidates[0]; got.Deployment.Name != "native" ||
		got.UpstreamProtocol != config.ProtocolOpenAI || !got.NativeProtocol {
		t.Fatalf("first candidate = %#v", got)
	}
	if got := plan.Candidates[1]; got.Deployment.Name != "translated" ||
		got.UpstreamProtocol != config.ProtocolAnthropic || got.NativeProtocol {
		t.Fatalf("second candidate = %#v", got)
	}
}

func TestBuildPlanAppliesProtocolRouteCapabilitiesPerTarget(t *testing.T) {
	t.Parallel()
	document := routingDocument()
	document.Deployments = []config.Deployment{
		config.Deployment{
			Name:         "responses-native",
			Provider:     document.Providers[0].Name,
			Model:        "responses-model",
			Capabilities: []config.Capability{config.CapabilityResponses},
		},
		config.Deployment{
			Name:     "chat-fallback",
			Provider: document.Providers[0].Name,
			Model:    "chat-model",
		},
	}
	document.VirtualModels[0].Pools = []config.RoutingPool{
		{Targets: []config.WeightedTarget{
			{Deployment: "chat-fallback", Weight: 1},
			{Deployment: "responses-native", Weight: 1},
		}},
	}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := snapshot.BuildPlanFor(document.VirtualModels[0].Name, PlanOptions{
		ProtocolResolver: func(_ config.Provider, deployment config.Deployment) (ProtocolRoute, bool) {
			if deployment.Name == "responses-native" {
				return ProtocolRoute{
					Protocol:             config.ProtocolOpenAI,
					Native:               true,
					RequiredCapabilities: []config.Capability{config.CapabilityResponses},
				}, true
			}
			return ProtocolRoute{Protocol: config.ProtocolOpenAI}, true
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 2 {
		t.Fatalf("candidate count = %d, want 2", len(plan.Candidates))
	}
	if plan.Candidates[0].Deployment.Name != "responses-native" ||
		!plan.Candidates[0].NativeProtocol {
		t.Fatalf("first candidate = %#v, want native Responses target", plan.Candidates[0])
	}
}

func TestSnapshotEnforcesInternalModelAccess(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.VirtualModels[0].Visibility = config.ModelVisibilityInternal
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if _, err := snapshot.BuildPlan("default", &sequencePicker{}); !errors.Is(
		err,
		ErrModelNotFound,
	) {
		t.Fatalf("external BuildPlan() error = %v, want ErrModelNotFound", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		AllowInternal: true,
		Picker:        &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("internal BuildPlanFor() error = %v", err)
	}
	if plan.VirtualModel != "public" {
		t.Fatalf("internal plan = %#v", plan)
	}
}

func TestSnapshotAllowsHiddenModelAccess(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.VirtualModels[0].Visibility = config.ModelVisibilityHidden
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	if _, err := snapshot.BuildPlan(
		"default",
		&sequencePicker{values: []int64{0, 0}},
	); err != nil {
		t.Fatalf("BuildPlan() hidden model error = %v", err)
	}
}

func TestBuildPlanUsesWeightedOrderWithoutReplacement(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments = append(document.Deployments,
		config.Deployment{Name: "primary-b", Provider: "provider", Model: "upstream-b"},
		config.Deployment{Name: "primary-c", Provider: "provider", Model: "upstream-c"},
	)
	document.VirtualModels[0].Pools[1].Targets = []config.WeightedTarget{
		{Deployment: "primary", Weight: 10},
		{Deployment: "primary-b", Weight: 20},
		{Deployment: "primary-c", Weight: 30},
	}
	document.VirtualModels[0].Limits.MaxAttempts = 4
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlan("default", &sequencePicker{
		// 15/60 selects B, then 35/40 selects C, then A. The priority-10
		// fallback is ordered only after all priority-0 targets.
		values: []int64{15, 35, 0, 0},
	})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	got := make([]string, 0, len(plan.Candidates))
	for _, candidate := range plan.Candidates {
		got = append(got, candidate.Deployment.Name)
	}
	want := []string{"primary-b", "primary-c", "primary", "fallback"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("deployment order = %v, want %v", got, want)
	}
	if plan.RequestedModel != "default" || plan.VirtualModel != "public" {
		t.Fatalf("plan identity = %#v", plan)
	}
	if plan.MaxAttempts != 4 {
		t.Fatalf("MaxAttempts = %d, want 4", plan.MaxAttempts)
	}
}

func TestBuildPlanCapsDefaultAttemptsAtCandidateCount(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(routingDocument())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlan("public", &sequencePicker{values: []int64{0, 0}})
	if err != nil {
		t.Fatalf("BuildPlan() error = %v", err)
	}
	if plan.MaxAttempts != 2 {
		t.Fatalf("MaxAttempts = %d, want 2", plan.MaxAttempts)
	}
	if plan.Limits.MaxAttempts != 2 ||
		plan.Limits.OverallTimeout != 10*time.Minute ||
		plan.Limits.PerTryTimeout != 10*time.Minute {
		t.Fatalf("effective limits = %#v", plan.Limits)
	}
	if plan.Retry.BaseBackoff != 100*time.Millisecond ||
		plan.Retry.Budget.Ratio != 0.2 ||
		plan.Retry.Budget.MinConcurrency != 1 {
		t.Fatalf("effective retry policy = %#v", plan.Retry)
	}
}

func TestBuildPlanRejectsInvalidPickerValue(t *testing.T) {
	t.Parallel()

	snapshot, err := Compile(routingDocument())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlan("public", &sequencePicker{values: []int64{1}})
	if err == nil || !strings.Contains(err.Error(), "outside") {
		// The exact picker contract error is intentionally not exported, but it
		// must be detected rather than indexing the wrong target.
		t.Fatalf("BuildPlan() error = %v, want picker range error", err)
	}
}

func TestBuildPlanForFiltersCapabilitiesBeforeWeightedSelection(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments[0].Capabilities = []config.Capability{config.CapabilityTools}
	document.Deployments[1].Capabilities = nil
	document.Deployments[1].CapabilityPolicy.Unknown = config.UnknownCapabilityReject
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityTools},
		AllowedProviderTypes: []string{"openai_compatible"},
		Picker:               &sequencePicker{values: []int64{0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if len(plan.Candidates) != 1 || plan.Candidates[0].Deployment.Name != "fallback" {
		t.Fatalf("capability-filtered candidates = %#v", plan.Candidates)
	}
	if len(plan.RequiredCapabilities) != 1 ||
		plan.RequiredCapabilities[0] != config.CapabilityTools {
		t.Fatalf("required capabilities = %v", plan.RequiredCapabilities)
	}
}

func TestBuildPlanForRejectsUnsupportedCapabilities(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
		AllowedProviderTypes: []string{"openai_compatible"},
		Picker:               &sequencePicker{},
	})
	if !errors.Is(err, ErrUnsupportedCapabilities) {
		t.Fatalf("BuildPlanFor() error = %v, want ErrUnsupportedCapabilities", err)
	}
	var capabilityError *UnsupportedCapabilitiesError
	if !errors.As(err, &capabilityError) ||
		len(capabilityError.Required) != 1 ||
		capabilityError.Required[0] != config.CapabilityVision {
		t.Fatalf("unsupported capability detail = %#v", capabilityError)
	}
}

func TestBuildPlanForDefaultsUnknownCapabilityToTry(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityTools},
		Picker:               &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if len(plan.Candidates) != 2 ||
		plan.Candidates[0].Deployment.Name != "primary" ||
		plan.Candidates[1].Deployment.Name != "fallback" {
		t.Fatalf("permissive candidates = %#v", plan.Candidates)
	}
}

func TestBuildPlanForPrefersDeclaredOverPermissiveWithinPool(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.VirtualModels[0].Pools = []config.RoutingPool{{
		Priority: 0,
		Targets: []config.WeightedTarget{
			{Deployment: "primary", Weight: 100},
			{Deployment: "fallback", Weight: 1},
		},
	}}
	document.Deployments[0].Capabilities = []config.Capability{config.CapabilityTools}
	document.Deployments[1].CapabilityPolicy.Unknown = config.UnknownCapabilityTry
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityTools},
		Picker:               &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if len(plan.Candidates) != 2 ||
		plan.Candidates[0].Deployment.Name != "fallback" ||
		plan.Candidates[1].Deployment.Name != "primary" {
		t.Fatalf("capability confidence order = %#v", plan.Candidates)
	}
}

func TestBuildPlanForPermissivePolicyRemainsFailClosedForHardCapabilities(
	t *testing.T,
) {
	t.Parallel()

	document := routingDocument()
	document.Deployments[1].CapabilityPolicy.Unknown = config.UnknownCapabilityTry
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{
			config.CapabilitySingleVectorEmbedding,
		},
		Picker: &sequencePicker{},
	})
	if !errors.Is(err, ErrUnsupportedCapabilities) {
		t.Fatalf("BuildPlanFor() error = %v, want ErrUnsupportedCapabilities", err)
	}
}

func TestBuildPlanForExplicitUnsupportedOverridesPermissivePolicy(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	document.Deployments[1].CapabilityPolicy = config.CapabilityPolicy{
		Unknown:     config.UnknownCapabilityTry,
		Unsupported: []config.Capability{config.CapabilityVision},
	}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
		Picker:               &sequencePicker{},
	})
	if !errors.Is(err, ErrUnsupportedCapabilities) {
		t.Fatalf("BuildPlanFor() error = %v, want ErrUnsupportedCapabilities", err)
	}
}

func TestBuildPlanForRejectsTargetsThatCannotPreserveRequestSemantics(
	t *testing.T,
) {
	t.Parallel()

	snapshot, err := Compile(routingDocument())
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		AllowedProviderTypes: []string{"openai_compatible"},
		Compatibility: func(string) bool {
			return false
		},
		Picker: &sequencePicker{},
	})
	if !errors.Is(err, ErrUnsupportedSemantics) {
		t.Fatalf(
			"BuildPlanFor() error = %v, want ErrUnsupportedSemantics",
			err,
		)
	}
}

func TestBuildPlanForRenormalizesAfterTargetEjection(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments = append(document.Deployments,
		config.Deployment{Name: "primary-b", Provider: "provider", Model: "upstream-b"},
	)
	document.VirtualModels[0].Pools = []config.RoutingPool{{
		Priority: 0,
		Targets: []config.WeightedTarget{
			{Deployment: "primary", Weight: 1},
			{Deployment: "primary-b", Weight: 1},
			{Deployment: "fallback", Weight: 1},
		},
	}}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	eligibility := eligibilityMap{
		"primary":   true,
		"primary-b": false,
		"fallback":  true,
	}
	counts := map[string]int{}
	for iteration := range 100 {
		firstPick := int64(iteration % 2)
		plan, err := snapshot.BuildPlanFor("default", PlanOptions{
			Eligibility: eligibility,
			Picker:      &sequencePicker{values: []int64{firstPick, 0}},
		})
		if err != nil {
			t.Fatalf("BuildPlanFor() error = %v", err)
		}
		counts[plan.Candidates[0].Deployment.Name]++
		for _, candidate := range plan.Candidates {
			if candidate.Deployment.Name == "primary-b" {
				t.Fatal("ineligible target remained in plan")
			}
		}
	}
	if counts["primary"] != 50 || counts["fallback"] != 50 {
		t.Fatalf("renormalized first selections = %v, want 50/50", counts)
	}
}

func TestBuildPlanForFiltersUnsupportedProviderDialect(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Providers[0].Type = "anthropic"
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		AllowedProviderTypes: []string{"openai", "openai_compatible"},
		Picker:               &sequencePicker{},
	})
	if !errors.Is(err, ErrNoTargets) {
		t.Fatalf("BuildPlanFor() error = %v, want ErrNoTargets", err)
	}
}

func TestBuildPlanForPinsDeploymentWithoutBypassingPolicy(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments[0].Capabilities = []config.Capability{config.CapabilityTools}
	document.Deployments[0].CapabilityPolicy.Unknown = config.UnknownCapabilityReject
	document.Deployments[1].Capabilities = []config.Capability{config.CapabilityTools}
	document.VirtualModels[0].Limits.MaxAttempts = 2
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		PinnedDeployment:     "fallback",
		PinnedProvider:       "provider",
		SingleAttempt:        true,
		RequiredCapabilities: []config.Capability{config.CapabilityTools},
		Eligibility:          eligibilityMap{"fallback": true},
		Picker:               &sequencePicker{},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if len(plan.Candidates) != 1 ||
		plan.Candidates[0].Deployment.Name != "fallback" ||
		plan.MaxAttempts != 1 ||
		plan.Limits.MaxAttempts != 1 {
		t.Fatalf("pinned plan = %#v", plan)
	}

	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		PinnedDeployment:     "fallback",
		RequiredCapabilities: []config.Capability{config.CapabilityVision},
		Eligibility:          eligibilityMap{"fallback": true},
	})
	if !errors.Is(err, ErrUnsupportedCapabilities) {
		t.Fatalf("capability-bypassing pin error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		PinnedDeployment: "fallback",
		Eligibility:      eligibilityMap{"fallback": false},
	})
	if !errors.Is(err, ErrNoTargets) {
		t.Fatalf("ineligible pin error = %v", err)
	}
	document.Deployments[0].Provider = "other-provider"
	document.Providers = append(document.Providers, config.Provider{
		Name: "other-provider", Type: "openai_compatible", BaseURL: "https://other.example/v1",
	})
	snapshot, err = Compile(document)
	if err != nil {
		t.Fatalf("Compile(repointed) error = %v", err)
	}
	_, err = snapshot.BuildPlanFor("default", PlanOptions{
		PinnedDeployment: "fallback",
		PinnedProvider:   "provider",
	})
	if !errors.Is(err, ErrNoTargets) {
		t.Fatalf("repointed pin error = %v, want ErrNoTargets", err)
	}
}

func TestBuildPlanForWeightedHashIsStableAndWeighted(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Deployments = []config.Deployment{
		{Name: "light", Provider: "provider", Model: "light"},
		{Name: "heavy", Provider: "provider", Model: "heavy"},
	}
	document.VirtualModels[0].Selection = config.SelectionPolicy{
		Mode: config.SelectionWeightedHash, HashKey: config.SelectionHashThreadID,
	}
	document.VirtualModels[0].Pools = []config.RoutingPool{{
		Targets: []config.WeightedTarget{
			{Deployment: "light", Weight: 1},
			{Deployment: "heavy", Weight: 9},
		},
	}}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	first, err := snapshot.BuildPlanFor("default", PlanOptions{SelectionKey: "revision\x00thread-a"})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	second, err := snapshot.BuildPlanFor("default", PlanOptions{SelectionKey: "revision\x00thread-a"})
	if err != nil {
		t.Fatalf("BuildPlanFor(repeat) error = %v", err)
	}
	if first.Candidates[0].Deployment.Name != second.Candidates[0].Deployment.Name {
		t.Fatalf("stable assignments differ: %#v vs %#v", first.Candidates, second.Candidates)
	}

	counts := map[string]int{}
	for index := 0; index < 5000; index++ {
		plan, buildErr := snapshot.BuildPlanFor(
			"default",
			PlanOptions{SelectionKey: fmt.Sprintf("revision\x00thread-%d", index)},
		)
		if buildErr != nil {
			t.Fatalf("BuildPlanFor(%d) error = %v", index, buildErr)
		}
		counts[plan.Candidates[0].Deployment.Name]++
	}
	if counts["heavy"] < 7*counts["light"] {
		t.Fatalf("weighted assignments = %#v, want heavy target strongly preferred", counts)
	}
}

func TestBuildPlanForWeightedHashFallsBackToPickerWithoutTrustedKey(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.VirtualModels[0].Selection = config.SelectionPolicy{
		Mode: config.SelectionWeightedHash, HashKey: config.SelectionHashThreadID,
	}
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		Picker: &sequencePicker{values: []int64{0, 0}},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if plan.Candidates[0].Deployment.Name != "primary" {
		t.Fatalf("fallback first candidate = %#v", plan.Candidates[0])
	}
}

func TestPlanPreferDeploymentDoesNotCrossPreferenceBoundary(t *testing.T) {
	t.Parallel()

	plan := Plan{Candidates: []Selection{
		{Deployment: config.Deployment{Name: "a"}, PoolPriority: 0, PreferenceClass: 0},
		{Deployment: config.Deployment{Name: "b"}, PoolPriority: 0, PreferenceClass: 0},
		{Deployment: config.Deployment{Name: "c"}, PoolPriority: 10, PreferenceClass: 4},
	}}
	if !plan.PreferDeployment("b") || plan.Candidates[0].Deployment.Name != "b" {
		t.Fatalf("same-class affinity was not preferred: %#v", plan.Candidates)
	}
	if plan.PreferDeployment("c") || plan.Candidates[0].Deployment.Name != "b" {
		t.Fatalf("lower-priority affinity crossed boundary: %#v", plan.Candidates)
	}
}

func TestBuildPlanForAppliesStableProviderPriorityAfterEligibility(t *testing.T) {
	t.Parallel()

	document := routingDocument()
	document.Providers = append(document.Providers, config.Provider{
		Name: "Preferred", Type: "openai_compatible", BaseURL: "https://preferred.example/v1",
	})
	document.Deployments[0].Provider = "Preferred"
	snapshot, err := Compile(document)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	plan, err := snapshot.BuildPlanFor("default", PlanOptions{
		Picker:           &sequencePicker{values: []int64{0, 0}},
		ProviderPriority: []string{strings.Repeat("x", maxProviderPriorityNameBytes+1), " preferred ", "missing", "PREFERRED"},
	})
	if err != nil {
		t.Fatalf("BuildPlanFor() error = %v", err)
	}
	if len(plan.Candidates) != 2 ||
		plan.Candidates[0].Deployment.Name != "fallback" ||
		plan.Candidates[1].Deployment.Name != "primary" {
		t.Fatalf("provider-priority candidates = %#v", plan.Candidates)
	}
	if strings.Join(plan.ProviderPriority, ",") != "preferred,missing" ||
		plan.Candidates[0].ProviderPreference != 0 ||
		plan.Candidates[1].ProviderPreference != 2 {
		t.Fatalf("provider-priority plan = %#v", plan)
	}

	filtered, err := snapshot.BuildPlanFor("default", PlanOptions{
		Eligibility:      eligibilityMap{"primary": true, "fallback": false},
		Picker:           &sequencePicker{values: []int64{0}},
		ProviderPriority: []string{"preferred"},
	})
	if err != nil {
		t.Fatalf("filtered BuildPlanFor() error = %v", err)
	}
	if len(filtered.Candidates) != 1 || filtered.Candidates[0].Deployment.Name != "primary" {
		t.Fatalf("provider priority reintroduced ineligible target: %#v", filtered.Candidates)
	}
	pinned, err := snapshot.BuildPlanFor("default", PlanOptions{
		PinnedDeployment: "primary", ProviderPriority: []string{"preferred"},
	})
	if err != nil {
		t.Fatalf("pinned BuildPlanFor() error = %v", err)
	}
	if len(pinned.Candidates) != 1 || pinned.Candidates[0].Deployment.Name != "primary" {
		t.Fatalf("provider priority bypassed deployment pin: %#v", pinned.Candidates)
	}
}

func TestPlanAffinityCannotOverrideProviderPriorityRank(t *testing.T) {
	t.Parallel()

	plan := Plan{Candidates: []Selection{
		{Deployment: config.Deployment{Name: "preferred-a"}, PoolPriority: 0, PreferenceClass: 0, ProviderPreference: 0},
		{Deployment: config.Deployment{Name: "ordinary"}, PoolPriority: 0, PreferenceClass: 0, ProviderPreference: 1},
		{Deployment: config.Deployment{Name: "preferred-b"}, PoolPriority: 0, PreferenceClass: 0, ProviderPreference: 0},
	}}
	if plan.PreferDeployment("ordinary") || plan.Candidates[0].Deployment.Name != "preferred-a" {
		t.Fatalf("affinity crossed provider-priority boundary: %#v", plan.Candidates)
	}
	if !plan.PreferDeployment("preferred-b") || plan.Candidates[0].Deployment.Name != "preferred-b" {
		t.Fatalf("same-provider-rank affinity was not applied: %#v", plan.Candidates)
	}
}

func routingDocument() config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			Type:    "openai_compatible",
			BaseURL: "https://example.com/v1",
			DefaultHeaders: map[string]config.HeaderValue{
				"X-Profile": {Value: "original"},
			},
		}},
		Deployments: []config.Deployment{
			{Name: "fallback", Provider: "provider", Model: "upstream-fallback"},
			{Name: "primary", Provider: "provider", Model: "upstream-primary"},
		},
		VirtualModels: []config.VirtualModel{{
			Name:    "public",
			Aliases: []string{"default"},
			Pools: []config.RoutingPool{
				{
					Priority: 10,
					Targets: []config.WeightedTarget{{
						Deployment: "fallback",
						Weight:     1,
					}},
				},
				{
					Priority: 0,
					Targets: []config.WeightedTarget{{
						Deployment: "primary",
						Weight:     1,
					}},
				},
			},
		}},
	}
}
