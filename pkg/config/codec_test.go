package config

import (
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

func TestEncodeCanonicalRoundTripIsStable(t *testing.T) {
	t.Parallel()

	document := validDocument()
	firstRaw, firstVersion, err := EncodeCanonical(document)
	if err != nil {
		t.Fatalf("EncodeCanonical() error = %v", err)
	}
	decoded, err := Decode(firstRaw)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	secondRaw, secondVersion, err := EncodeCanonical(decoded)
	if err != nil {
		t.Fatalf("second EncodeCanonical() error = %v", err)
	}
	if string(firstRaw) != string(secondRaw) || firstVersion != secondVersion {
		t.Fatalf(
			"canonical round trip changed: first=%s/%s second=%s/%s",
			firstRaw,
			firstVersion,
			secondRaw,
			secondVersion,
		)
	}
}

func TestEncodeCanonicalPreservesModelRouting(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.ModelRouting = &modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 3,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "fastest", Models: []string{"local-default"}},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"local-default": {Enabled: true},
		},
	}
	raw, _, err := EncodeCanonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"model_routing":{"version":2`) {
		t.Fatalf("canonical model routing missing: %s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.ModelRouting == nil ||
		decoded.ModelRouting.VirtualModels["auto"].Strategy != "fastest" {
		t.Fatalf("decoded model routing = %#v", decoded.ModelRouting)
	}
}

func TestModelRoutingRejectsUnknownLogicalModel(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.ModelRouting = &modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 1,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{"missing"}},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"missing": {Enabled: true},
		},
	}
	if _, _, err := EncodeCanonical(document); err == nil ||
		!strings.Contains(err.Error(), "unknown canonical virtual model") {
		t.Fatalf("EncodeCanonical() error = %v", err)
	}
}

func TestEncodeCanonicalOmitsDefaultStaticEndpointSource(t *testing.T) {
	t.Parallel()

	raw, _, err := EncodeCanonical(validDocument())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"endpoint_source"`) {
		t.Fatalf("canonical default config unexpectedly contains endpoint_source: %s", raw)
	}
}

func TestEncodeCanonicalPreservesCapabilityPolicy(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.Deployments[0].CapabilityPolicy = CapabilityPolicy{
		Unknown:     UnknownCapabilityTry,
		Unsupported: []Capability{CapabilityVision},
	}
	raw, _, err := EncodeCanonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(
		string(raw),
		`"capability_policy":{"unknown":"try","unsupported":["vision"]}`,
	) {
		t.Fatalf("canonical capability policy missing: %s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Deployments[0].CapabilityPolicy.Unknown != UnknownCapabilityTry ||
		len(decoded.Deployments[0].CapabilityPolicy.Unsupported) != 1 {
		t.Fatalf("decoded capability policy = %#v", decoded.Deployments[0].CapabilityPolicy)
	}
}

func TestEncodeCanonicalPreservesCapabilityDefaults(t *testing.T) {
	t.Parallel()

	document := validDocument()
	document.CapabilityDefaults.Unknown = UnknownCapabilityReject
	document.Providers[0].CapabilityDefaults.Unknown = UnknownCapabilityTry
	raw, _, err := EncodeCanonical(document)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)
	if !strings.Contains(encoded, `"capability_defaults":{"unknown":"reject"}`) ||
		!strings.Contains(encoded, `"capability_defaults":{"unknown":"try"}`) {
		t.Fatalf("canonical capability defaults missing: %s", raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.CapabilityDefaults.Unknown != UnknownCapabilityReject ||
		decoded.Providers[0].CapabilityDefaults.Unknown != UnknownCapabilityTry {
		t.Fatalf("decoded capability defaults = %#v / %#v", decoded.CapabilityDefaults, decoded.Providers[0].CapabilityDefaults)
	}
}

func TestDecodeRejectsTrailingAndUnknownData(t *testing.T) {
	t.Parallel()

	for name, raw := range map[string]string{
		"unknown field": `{"providers":[],"deployments":[],"virtual_models":[],"extra":true}`,
		"trailing data": `{}` + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := Decode([]byte(raw)); err == nil ||
				name == "unknown field" && !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("Decode() error = %v", err)
			}
		})
	}
}
