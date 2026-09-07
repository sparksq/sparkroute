package config

import (
	"slices"
	"testing"
)

func TestNativeResponsesProviderDefaults(t *testing.T) {
	document := validDocument()
	document.Providers[0].Type = "openai_responses"
	document.Deployments[0].Capabilities = []Capability{CapabilityTools}
	document.VirtualModels[0].RequiredCapabilities = []Capability{CapabilityResponses}
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
	resolved := document.ResolveCapabilityPolicies(UnknownCapabilityReject)
	deployment := resolved.Deployments[0]
	if !deployment.DeclaresCapability(CapabilityResponses) || !deployment.SupportsNativeProtocol(resolved.Providers[0], ProtocolOpenAI) {
		t.Fatal("native Responses provider did not imply protocol and capability")
	}
	if len(document.Deployments[0].Capabilities) != 1 {
		t.Fatal("source document was mutated")
	}
	twice := resolved.ResolveCapabilityPolicies(UnknownCapabilityTry)
	if !slices.Equal(twice.Deployments[0].Capabilities, deployment.Capabilities) {
		t.Fatal("defaults are not idempotent")
	}
	document.Deployments[0].CapabilityPolicy.Unsupported = []Capability{CapabilityResponses}
	if err := document.Validate(); err == nil {
		t.Fatal("accepted a provider that rejects its native API")
	}
	document.Deployments[0].CapabilityPolicy.Unsupported = nil
	document.Deployments[0].NativeProtocols = []Protocol{ProtocolAnthropic}
	if err := document.Validate(); err == nil {
		t.Fatal("accepted Responses provider without OpenAI protocol")
	}
}

func TestChatAndAnthropicProvidersKeepExistingCapabilitySemantics(t *testing.T) {
	for _, providerType := range []string{"openai", "openai_compatible", "anthropic"} {
		document := validDocument()
		document.Providers[0].Type = providerType
		document.Deployments[0].Capabilities = nil
		resolved := document.ResolveCapabilityPolicies(UnknownCapabilityTry)
		if resolved.Deployments[0].DeclaresCapability(CapabilityResponses) {
			t.Fatalf("%s acquired an unrelated API", providerType)
		}
		if providerType == "anthropic" && ProtocolForProviderType(providerType) != ProtocolAnthropic {
			t.Fatal("Anthropic protocol lost")
		}
	}
}
