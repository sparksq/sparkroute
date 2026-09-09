// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestDocumentValidate(t *testing.T) {
	t.Parallel()

	document := validDocument()
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	model, ok := document.CanonicalModel("default")
	if !ok {
		t.Fatal("CanonicalModel(default) not found")
	}
	if model.Name != "local-default" {
		t.Fatalf("CanonicalModel(default).Name = %q, want local-default", model.Name)
	}
}

func TestPIIPolicyDefaultsAndValidation(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].Privacy = &PrivacyPolicy{PII: &PIIPolicy{}}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	effective := document.VirtualModels[0].Privacy.PII.Effective()
	if effective.Mode != PIIModeSubstitute ||
		effective.Scope != PIIScopeRequest ||
		effective.Response != PIIResponseRestore ||
		effective.TraceContent != PIITraceMasked ||
		effective.FailureMode != PIIFailClosed ||
		effective.MediaText != PIIMediaTextBestEffort ||
		effective.Files.Text != PIIFileTextDisabled ||
		effective.Files.MaxTextBytes != defaultPIIFileMaxTextBytes ||
		!reflect.DeepEqual(effective.Entities, defaultPIIEntities) {
		t.Fatalf("Effective() = %#v", effective)
	}

	tests := map[string]PIIPolicy{
		"unknown mode":             {Mode: "unknown"},
		"unknown entity":           {Entities: []PIIEntity{"unknown"}},
		"duplicate entity":         {Entities: []PIIEntity{PIIEntityEmail, PIIEntityEmail}},
		"unknown scope":            {Scope: "global"},
		"unknown response":         {Response: "passthrough"},
		"unknown trace":            {TraceContent: "raw"},
		"unknown failure":          {FailureMode: "ignore"},
		"disabled options":         {Mode: PIIModeDisabled, Entities: []PIIEntity{PIIEntityEmail}},
		"unknown media inspection": {MediaText: "sometimes"},
		"file request scope": {
			Scope: PIIScopeRequest,
			Files: &PIIFilePolicy{Text: PIIFileTextRequired},
		},
		"file size limit": {
			Scope: PIIScopeConversation,
			Files: &PIIFilePolicy{Text: PIIFileTextBestEffort, MaxTextBytes: maxPIIFileTextBytes + 1},
		},
	}
	for name, policy := range tests {
		name, policy := name, policy
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			invalid := validDocument()
			invalid.VirtualModels[0].Privacy = &PrivacyPolicy{PII: &policy}
			if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "privacy.pii") {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestDocumentValidateRejectsAliasCollision(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels = append(document.VirtualModels, VirtualModel{
		Name:    "other",
		Aliases: []string{"default"},
		Pools: []RoutingPool{{
			Targets: []WeightedTarget{{Deployment: "local-default", Weight: 1}},
		}},
	})
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "already belongs") {
		t.Fatalf("Validate() error = %v, want alias collision", err)
	}
}

func TestDocumentValidateRejectsProtectedHeader(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"Authorization",
		"Anthropic-Version",
		"anthropic-beta",
		"Anthropic-User-Profile-Id",
		"X-Amz-Date",
		"X-Amz-Security-Token",
		"X-Amz-Content-Sha256",
	} {
		document := validDocument()
		document.Deployments[0].UpstreamHeaders[name] = HeaderValue{
			ValueFrom: credentials.Ref("env://TOKEN"),
		}
		err := document.Validate()
		if err == nil ||
			!strings.Contains(err.Error(), "controlled by the gateway") {
			t.Errorf(
				"%s Validate() error = %v, want protected-header error",
				name,
				err,
			)
		}
	}
}

func TestDocumentValidateRequiresSecretRefForAuthLikeHeader(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].UpstreamHeaders["X-Deployment-Token"] = HeaderValue{
		Value: "plaintext-secret",
	}
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "must use value_from") {
		t.Fatalf("Validate() error = %v, want value_from error", err)
	}
}

func TestDocumentValidateRejectsHeaderInjection(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].UpstreamHeaders["X-Profile"] = HeaderValue{
		Value: "valid\r\nX-Injected: true",
	}
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("Validate() error = %v, want control-character error", err)
	}
}

func TestDocumentValidateExtraBody(t *testing.T) {
	t.Parallel()
	document := validDocument()
	document.Providers[0].ExtraBody = map[string]json.RawMessage{
		"service_tier": json.RawMessage(`"priority"`),
	}
	document.Deployments[0].ExtraBody = map[string]json.RawMessage{
		"chat_template_kwargs": json.RawMessage(`{"enable_thinking":false}`),
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for _, protected := range []string{"model", "messages", "input", "stream", "tools"} {
		document := validDocument()
		document.Deployments[0].ExtraBody = map[string]json.RawMessage{protected: json.RawMessage(`true`)}
		if err := document.Validate(); err == nil || !strings.Contains(err.Error(), "protocol-owned") {
			t.Errorf("%s Validate() error = %v", protected, err)
		}
	}
}

func TestDocumentValidateProviderAuthentication(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Providers[0].Auth = ProviderAuth{
		Type:       AuthHeader,
		Header:     "X-API-Key",
		Prefix:     "key ",
		Credential: credentials.Ref("env://PROVIDER_KEY"),
	}
	document.Deployments[0].Credential = credentials.Ref("env://DEPLOYMENT_KEY")
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestDocumentValidateBedrockProvider(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Providers[0] = Provider{
		Name:   "local",
		Type:   "bedrock",
		Region: "us-east-1",
		Auth: ProviderAuth{
			Type:       AuthAWSSigV4,
			Credential: credentials.Ref("workload://aws"),
		},
	}
	document.Deployments[0].Model =
		"us.anthropic.claude-sonnet-4-20250514-v1:0"
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Document)
	}{
		{
			name: "missing region",
			mutate: func(value *Document) {
				value.Providers[0].Region = ""
			},
		},
		{
			name: "wrong auth",
			mutate: func(value *Document) {
				value.Providers[0].Auth = ProviderAuth{}
			},
		},
		{
			name: "non-workload credential",
			mutate: func(value *Document) {
				value.Providers[0].Auth.Credential =
					credentials.Ref("env://AWS_SECRET")
			},
		},
		{
			name: "AWS auth on non-Bedrock provider",
			mutate: func(value *Document) {
				value.Providers[0].Type = "openai_compatible"
				value.Providers[0].BaseURL = "https://example.com/v1"
				value.Providers[0].Region = ""
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			invalid := document
			invalid.Providers = append(
				[]Provider(nil),
				document.Providers...,
			)
			test.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestDocumentValidateRejectsCredentialWithoutAuth(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].Credential = credentials.Ref("env://DEPLOYMENT_KEY")
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "provider auth is not configured") {
		t.Fatalf("Validate() error = %v, want provider-auth error", err)
	}
}

func TestDocumentValidateRoutingPolicy(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].Selection.Mode = SelectionWeightedRandom
	document.VirtualModels[0].Limits = ModelLimits{
		MaxAttempts:       3,
		OverallTimeout:    Duration(5 * time.Minute),
		PerTryTimeout:     Duration(time.Minute),
		StreamIdleTimeout: Duration(30 * time.Second),
	}
	document.VirtualModels[0].Retry = RetryPolicy{
		BaseBackoff:   Duration(50 * time.Millisecond),
		MaxBackoff:    Duration(time.Second),
		MaxRetryAfter: Duration(10 * time.Second),
		Budget: RetryBudgetPolicy{
			Ratio:          0.25,
			MinConcurrency: 2,
		},
	}
	document.VirtualModels[0].ResponseModel = ResponseModelRequested
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	limits := document.VirtualModels[0].Limits.Effective()
	if limits.OverallTimeout != 5*time.Minute ||
		limits.PerTryTimeout != time.Minute ||
		limits.StreamIdleTimeout != 30*time.Second {
		t.Fatalf("effective limits = %#v", limits)
	}
	retry := document.VirtualModels[0].Retry.Effective()
	if retry.Budget.Ratio != 0.25 || retry.Budget.MinConcurrency != 2 {
		t.Fatalf("effective retry policy = %#v", retry)
	}
}

func TestDocumentValidateWeightedHashAndPromptCacheAffinity(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].Selection = SelectionPolicy{
		Mode:    SelectionWeightedHash,
		HashKey: SelectionHashThreadID,
		PromptCacheAffinity: PromptCacheAffinityPolicy{
			Enabled: true,
			TTL:     Duration(7 * time.Minute),
			Scope:   PromptCacheAffinityCaller,
		},
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	effective := document.VirtualModels[0].Selection.PromptCacheAffinity.Effective()
	if !effective.Enabled || effective.TTL != 7*time.Minute ||
		effective.MinPrefixBytes != 4<<10 ||
		effective.MaxPrefixesPerRequest != 8 ||
		effective.Scope != PromptCacheAffinityCaller {
		t.Fatalf("effective prompt-cache affinity = %#v", effective)
	}
}

func TestPromptCacheAffinityDefaultsDisabled(t *testing.T) {
	t.Parallel()

	effective := (PromptCacheAffinityPolicy{}).Effective()
	if effective.Enabled {
		t.Fatal("zero-value prompt-cache affinity is enabled")
	}
	if effective.TTL != 5*time.Minute || effective.MinPrefixBytes != 4<<10 ||
		effective.MaxPrefixesPerRequest != 8 ||
		effective.Scope != PromptCacheAffinityCaller {
		t.Fatalf("zero-value effective prompt-cache affinity = %#v", effective)
	}
}

func TestDocumentValidateRoutingPolicyRejectsInvalidSelection(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		selection SelectionPolicy
		contains  string
	}{
		{
			name: "hash key without weighted hash",
			selection: SelectionPolicy{
				Mode: SelectionWeightedRandom, HashKey: SelectionHashThreadID,
			},
			contains: "hash_key requires",
		},
		{
			name:      "weighted hash without key",
			selection: SelectionPolicy{Mode: SelectionWeightedHash},
			contains:  "hash_key is required",
		},
		{
			name: "invalid affinity scope",
			selection: SelectionPolicy{PromptCacheAffinity: PromptCacheAffinityPolicy{
				Enabled: true, Scope: "global",
			}},
			contains: "scope must be",
		},
		{
			name: "invalid affinity ttl",
			selection: SelectionPolicy{PromptCacheAffinity: PromptCacheAffinityPolicy{
				Enabled: true, TTL: Duration(25 * time.Hour),
			}},
			contains: "ttl must be",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			document.VirtualModels[0].Selection = test.selection
			err := document.Validate()
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("Validate() error = %v, want %q", err, test.contains)
			}
		})
	}
}

func TestDocumentValidateEndpointSources(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Providers[0].BaseURL = ""
	document.Deployments[0].EndpointSource = EndpointSource{
		Type:               EndpointSourceActivatable,
		Controller:         "sparkrun",
		Revision:           "binding-sha256",
		Recipe:             "recipes/qwen.yaml",
		RecipeRevision:     "recipe-sha256",
		ClusterCandidates:  []string{"spark-a", "spark-b"},
		Overrides:          map[string]string{"tensor_parallel": "2"},
		ActivationTimeout:  Duration(5 * time.Minute),
		IdleTTL:            Duration(30 * time.Minute),
		MaxQueuedWaiters:   12,
		MaxQueuedBodyBytes: 8 << 20,
		ColdStart:          ColdStartWait,
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	source := document.Deployments[0].EndpointSource
	if source.EffectiveActivationTimeout() != 5*time.Minute ||
		source.EffectiveMaxQueuedWaiters() != 12 ||
		source.EffectiveMaxQueuedBodyBytes() != 8<<20 {
		t.Fatalf("effective endpoint source = %#v", source)
	}

	discovered := validDocument()
	discovered.Deployments[0].EndpointSource = EndpointSource{
		Type: EndpointSourceDiscovered, Controller: "discovery",
	}
	if err := discovered.Validate(); err != nil {
		t.Fatalf("discovered Validate() error = %v", err)
	}
}

func TestDocumentValidateRejectsUnsafeEndpointSources(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Document)
		want   string
	}{
		{
			name: "static without provider URL",
			mutate: func(document *Document) {
				document.Providers[0].BaseURL = ""
			},
			want: "static deployments require provider base_url",
		},
		{
			name: "activatable missing immutable revision",
			mutate: func(document *Document) {
				document.Deployments[0].EndpointSource = EndpointSource{
					Type: EndpointSourceActivatable, Controller: "controller", Recipe: "recipe",
				}
			},
			want: "revision: required",
		},
		{
			name: "duplicate cluster",
			mutate: func(document *Document) {
				document.Deployments[0].EndpointSource = EndpointSource{
					Type: EndpointSourceActivatable, Controller: "controller",
					Revision: "revision", Recipe: "recipe",
					ClusterCandidates: []string{"cluster", "cluster"},
				}
			},
			want: "duplicate",
		},
		{
			name: "unbounded queue",
			mutate: func(document *Document) {
				document.Deployments[0].EndpointSource = EndpointSource{
					Type: EndpointSourceActivatable, Controller: "controller",
					Revision: "revision", Recipe: "recipe", MaxQueuedWaiters: maxQueuedWaiters + 1,
				}
			},
			want: "max_queued_waiters",
		},
		{
			name: "discovered activation fields",
			mutate: func(document *Document) {
				document.Deployments[0].EndpointSource = EndpointSource{
					Type: EndpointSourceDiscovered, Controller: "controller", Recipe: "recipe",
				}
			},
			want: "must not contain activation fields",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			test.mutate(&document)
			err := document.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestDocumentValidateModelVisibilityAndGuardrails(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels = append(document.VirtualModels, VirtualModel{
		Name:       "safety",
		Aliases:    []string{"safety-alias"},
		Visibility: ModelVisibilityInternal,
		Pools: []RoutingPool{{
			Targets: []WeightedTarget{{Deployment: "local-default", Weight: 1}},
		}},
	})
	document.VirtualModels[0].Visibility = ModelVisibilityHidden
	document.VirtualModels[0].Guardrails = GuardrailPolicy{
		Pre: []Guardrail{{
			Name:        "request-safety",
			Model:       "safety-alias",
			Prompt:      "Block unsafe requests.",
			FailureMode: GuardrailFailClosed,
		}},
		Post: []Guardrail{{
			Name:             "response-pii",
			Model:            "safety",
			FailureMode:      GuardrailFailOpen,
			AllowReplacement: true,
		}},
		Stream: &GuardrailStreamPolicy{
			WindowBytes:  4 << 10,
			ContextBytes: 32 << 10,
		},
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestDocumentValidateRejectsInvalidVisibilityAndGuardrails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*Document)
	}{
		{
			name: "visibility",
			mutate: func(document *Document) {
				document.VirtualModels[0].Visibility = "private"
			},
		},
		{
			name: "unknown model",
			mutate: func(document *Document) {
				document.VirtualModels[0].Guardrails.Pre = []Guardrail{{
					Name:  "safety",
					Model: "missing",
				}}
			},
		},
		{
			name: "duplicate name across phases",
			mutate: func(document *Document) {
				guardrail := Guardrail{Name: "safety", Model: "default"}
				document.VirtualModels[0].Guardrails.Pre = []Guardrail{guardrail}
				document.VirtualModels[0].Guardrails.Post = []Guardrail{guardrail}
			},
		},
		{
			name: "failure mode",
			mutate: func(document *Document) {
				document.VirtualModels[0].Guardrails.Pre = []Guardrail{{
					Name:        "safety",
					Model:       "default",
					FailureMode: "sometimes",
				}}
			},
		},
		{
			name: "stream without post guardrail",
			mutate: func(document *Document) {
				document.VirtualModels[0].Guardrails.Stream = &GuardrailStreamPolicy{}
			},
		},
		{
			name: "stream window too small",
			mutate: func(document *Document) {
				document.VirtualModels[0].Guardrails.Post = []Guardrail{{
					Name:  "safety",
					Model: "default",
				}}
				document.VirtualModels[0].Guardrails.Stream = &GuardrailStreamPolicy{
					WindowBytes: 512,
				}
			},
		},
		{
			name: "stream context too large",
			mutate: func(document *Document) {
				document.VirtualModels[0].Guardrails.Post = []Guardrail{{
					Name:  "safety",
					Model: "default",
				}}
				document.VirtualModels[0].Guardrails.Stream = &GuardrailStreamPolicy{
					ContextBytes: (4 << 20) + 1,
				}
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := validDocument()
			test.mutate(&document)
			if err := document.Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestDocumentValidateRejectsDuplicateRoutingTarget(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].Pools = append(document.VirtualModels[0].Pools, RoutingPool{
		Priority: 1,
		Targets: []WeightedTarget{{
			Deployment: "local-default",
			Weight:     1,
		}},
	})
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate deployment") {
		t.Fatalf("Validate() error = %v, want duplicate-deployment error", err)
	}
}

func TestDocumentValidateRejectsExcessiveAttempts(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].Limits.MaxAttempts = 17
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "max_attempts") {
		t.Fatalf("Validate() error = %v, want max-attempts error", err)
	}
}

func TestDocumentValidateRejectsInvalidTimeoutAndRetryPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits ModelLimits
		retry  RetryPolicy
	}{
		{
			name: "per try exceeds maximum",
			limits: ModelLimits{
				OverallTimeout: Duration(time.Second),
				PerTryTimeout:  Duration(25 * time.Hour),
			},
		},
		{
			name: "negative overall timeout",
			limits: ModelLimits{
				OverallTimeout: Duration(-time.Second),
			},
		},
		{
			name: "stream idle exceeds maximum",
			limits: ModelLimits{
				StreamIdleTimeout: Duration(25 * time.Hour),
			},
		},
		{
			name: "backoff order",
			retry: RetryPolicy{
				BaseBackoff: Duration(time.Second),
				MaxBackoff:  Duration(time.Millisecond),
			},
		},
		{
			name: "budget ratio",
			retry: RetryPolicy{
				Budget: RetryBudgetPolicy{Ratio: 11},
			},
		},
		{
			name: "budget minimum",
			retry: RetryPolicy{
				Budget: RetryBudgetPolicy{MinConcurrency: -1},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			document.VirtualModels[0].Limits = test.limits
			document.VirtualModels[0].Retry = test.retry
			if err := document.Validate(); err == nil {
				t.Fatalf(
					"Validate() accepted limits %#v and retry %#v",
					test.limits,
					test.retry,
				)
			}
		})
	}
}

func TestDocumentValidateCapabilitiesAndCircuitPolicy(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].Capabilities = []Capability{
		CapabilityTools,
		CapabilityVision,
		CapabilitySingleVectorEmbedding,
		"x-provider-extension",
	}
	document.Deployments[0].MaxConcurrency = 32
	document.Deployments[0].Circuit = CircuitPolicy{
		ConsecutiveFailures: 3,
		MinimumSamples:      10,
		SampleWindow:        20,
		FailureRate:         0.4,
		BaseEjectionTime:    Duration(5 * time.Second),
		MaxEjectionTime:     Duration(time.Minute),
	}
	document.VirtualModels[0].RequiredCapabilities = []Capability{
		CapabilityTools,
		CapabilityVision,
		CapabilitySingleVectorEmbedding,
	}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestDocumentValidateDefaultsSafeUnknownCapabilitiesToTry(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.VirtualModels[0].RequiredCapabilities = []Capability{CapabilityTools}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	document.Deployments[0].CapabilityPolicy.Unknown = UnknownCapabilityReject
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "no routing target supports") {
		t.Fatalf("strict Validate() error = %v, want unsupported required capabilities", err)
	}
}

func TestCapabilityDefaultPrecedence(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.CapabilityDefaults.Unknown = UnknownCapabilityReject
	document.Providers[0].CapabilityDefaults.Unknown = UnknownCapabilityTry
	resolved := document.ResolveCapabilityPolicies(UnknownCapabilityReject)
	if got := resolved.Deployments[0].CapabilityPolicy.Unknown; got != UnknownCapabilityTry {
		t.Fatalf("provider default = %q, want try", got)
	}

	document.Deployments[0].CapabilityPolicy.Unknown = UnknownCapabilityReject
	resolved = document.ResolveCapabilityPolicies(UnknownCapabilityTry)
	if got := resolved.Deployments[0].CapabilityPolicy.Unknown; got != UnknownCapabilityReject {
		t.Fatalf("deployment override = %q, want reject", got)
	}

	document.Deployments[0].CapabilityPolicy.Unknown = ""
	document.Providers[0].CapabilityDefaults.Unknown = ""
	resolved = document.ResolveCapabilityPolicies(UnknownCapabilityTry)
	if got := resolved.Deployments[0].CapabilityPolicy.Unknown; got != UnknownCapabilityReject {
		t.Fatalf("document default = %q, want reject", got)
	}

	document.CapabilityDefaults.Unknown = ""
	resolved = document.ResolveCapabilityPolicies(UnknownCapabilityTry)
	if got := resolved.Deployments[0].CapabilityPolicy.Unknown; got != UnknownCapabilityTry {
		t.Fatalf("edition default = %q, want try", got)
	}
}

func TestDocumentValidateCapabilityDefaults(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.CapabilityDefaults.Unknown = "allow"
	if err := document.Validate(); err == nil || !strings.Contains(err.Error(), "capability_defaults") {
		t.Fatalf("global defaults error = %v", err)
	}

	document = validDocument()
	document.Providers[0].CapabilityDefaults.Unknown = "allow"
	if err := document.Validate(); err == nil || !strings.Contains(err.Error(), "capability_defaults") {
		t.Fatalf("provider defaults error = %v", err)
	}
}

func TestDocumentValidatePermissiveUnknownCapabilities(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].CapabilityPolicy.Unknown = UnknownCapabilityTry
	document.VirtualModels[0].RequiredCapabilities = []Capability{CapabilityTools}
	if err := document.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	document.VirtualModels[0].RequiredCapabilities = []Capability{
		CapabilitySingleVectorEmbedding,
	}
	err := document.Validate()
	if err == nil || !strings.Contains(err.Error(), "no routing target supports") {
		t.Fatalf("Validate() hard-capability error = %v", err)
	}
}

func TestDocumentValidateCapabilityPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy CapabilityPolicy
	}{
		{
			name:   "invalid unknown behavior",
			policy: CapabilityPolicy{Unknown: "allow"},
		},
		{
			name: "duplicate unsupported capability",
			policy: CapabilityPolicy{Unsupported: []Capability{
				CapabilityVision,
				CapabilityVision,
			}},
		},
		{
			name: "supported and unsupported overlap",
			policy: CapabilityPolicy{Unsupported: []Capability{
				CapabilityTools,
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			document.Deployments[0].Capabilities = []Capability{CapabilityTools}
			document.Deployments[0].CapabilityPolicy = test.policy
			if err := document.Validate(); err == nil {
				t.Fatalf("Validate() accepted policy %#v", test.policy)
			}
		})
	}
}

func TestDocumentValidateRejectsInvalidCapabilities(t *testing.T) {
	t.Parallel()

	for _, capability := range []Capability{
		"Tools",
		"unknown",
		"x-invalid space",
	} {
		t.Run(string(capability), func(t *testing.T) {
			document := validDocument()
			document.Deployments[0].Capabilities = []Capability{capability}
			if err := document.Validate(); err == nil {
				t.Fatalf("Validate() accepted capability %q", capability)
			}
		})
	}
}

func TestDocumentValidateRejectsInvalidCircuitPolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		policy CircuitPolicy
	}{
		{name: "failure rate", policy: CircuitPolicy{FailureRate: 1.1}},
		{
			name: "sample window",
			policy: CircuitPolicy{
				MinimumSamples: 10,
				SampleWindow:   5,
			},
		},
		{
			name: "ejection order",
			policy: CircuitPolicy{
				BaseEjectionTime: Duration(time.Minute),
				MaxEjectionTime:  Duration(time.Second),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			document := validDocument()
			document.Deployments[0].Circuit = test.policy
			if err := document.Validate(); err == nil {
				t.Fatalf("Validate() accepted policy %#v", test.policy)
			}
		})
	}
}

func TestDurationJSONRequiresUnits(t *testing.T) {
	t.Parallel()

	var document struct {
		Duration Duration `json:"duration"`
	}
	if err := json.Unmarshal([]byte(`{"duration":"1500ms"}`), &document); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got := document.Duration.Value(); got != 1500*time.Millisecond {
		t.Fatalf("duration = %s, want 1.5s", got)
	}
	if err := json.Unmarshal([]byte(`{"duration":1500}`), &document); err == nil {
		t.Fatal("numeric duration unexpectedly accepted")
	}
}

func TestDeploymentNativeProtocols(t *testing.T) {
	t.Parallel()

	provider := Provider{Type: "openai_compatible"}
	deployment := Deployment{}
	if got := deployment.EffectiveNativeProtocols(provider); !reflect.DeepEqual(
		got,
		[]Protocol{ProtocolOpenAI},
	) {
		t.Fatalf("default native protocols = %v", got)
	}
	deployment.NativeProtocols = []Protocol{
		ProtocolOpenAI,
		ProtocolAnthropic,
	}
	if !deployment.SupportsNativeProtocol(provider, ProtocolAnthropic) {
		t.Fatal("explicit Anthropic protocol was not supported")
	}

	for _, protocols := range [][]Protocol{
		{ProtocolOpenAI, ProtocolOpenAI},
		{"openai-compatible"},
	} {
		document := validDocument()
		document.Deployments[0].NativeProtocols = protocols
		if err := document.Validate(); err == nil {
			t.Fatalf("Validate() accepted native protocols %v", protocols)
		}
	}
}

func TestDeploymentDeclaresCapabilityHonorsExplicitUnsupported(t *testing.T) {
	t.Parallel()
	deployment := Deployment{Capabilities: []Capability{CapabilityResponses}}
	if !deployment.DeclaresCapability(CapabilityResponses) {
		t.Fatal("declared Responses capability was not reported")
	}
	deployment.CapabilityPolicy.Unsupported = []Capability{CapabilityResponses}
	if deployment.DeclaresCapability(CapabilityResponses) {
		t.Fatal("explicitly unsupported Responses capability was reported")
	}
}

func validDocument() Document {
	return Document{
		Providers: []Provider{{
			Name:    "local",
			Type:    "openai_compatible",
			BaseURL: "http://127.0.0.1:8000/v1",
			DefaultHeaders: map[string]HeaderValue{
				"X-API-Version": {Value: "2026-01-01"},
			},
		}},
		Deployments: []Deployment{{
			Name:     "local-default",
			Provider: "local",
			Model:    "upstream-model",
			UpstreamHeaders: map[string]HeaderValue{
				"X-Profile": {Value: "default"},
			},
		}},
		VirtualModels: []VirtualModel{{
			Name:    "local-default",
			Aliases: []string{"default"},
			Pools: []RoutingPool{{
				Priority: 0,
				Targets: []WeightedTarget{{
					Deployment: "local-default",
					Weight:     100,
				}},
			}},
		}},
	}
}
