package builtin

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	credentialawsworkload "github.com/sparksq/sparkroute/pkg/credentials/awsworkload"
	credentialfile "github.com/sparksq/sparkroute/pkg/credentials/file"
)

func TestNewRegistryInitializesReferencedFileSource(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod() root error = %v", err)
	}
	path := filepath.Join(root, "key")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	document := config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			Type:    "openai_compatible",
			BaseURL: "https://example.com/v1",
			Auth: config.ProviderAuth{
				Type:       config.AuthBearer,
				Credential: credentials.Ref("file://" + path),
			},
		}},
		Deployments: []config.Deployment{{
			Name:     "deployment",
			Provider: "provider",
			Model:    "upstream",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "model",
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "deployment",
					Weight:     1,
				}},
			}},
		}},
	}
	registry, err := NewRegistry(document, Options{
		File: credentialfile.Options{Roots: []string{root}},
	})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	material, err := registry.Resolve(
		context.Background(),
		credentials.Ref("file://"+path),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if string(material.Value) != "secret" {
		t.Fatalf("Value = %q", material.Value)
	}
}

func TestNewRegistryRejectsUnconfiguredScheme(t *testing.T) {
	t.Parallel()

	document := config.Document{
		Providers: []config.Provider{{
			Auth: config.ProviderAuth{
				Credential: credentials.Ref("vault://provider"),
			},
		}},
	}
	if _, err := NewRegistry(document, Options{}); err == nil {
		t.Fatal("NewRegistry() error = nil")
	}
}

func TestNewRegistryForReferences(t *testing.T) {
	registry, err := NewRegistryForReferences(
		[]credentials.Ref{"env://BUILTIN_REGISTRY_TEST"},
		Options{},
	)
	if err != nil {
		t.Fatalf("NewRegistryForReferences() error = %v", err)
	}
	t.Setenv("BUILTIN_REGISTRY_TEST", "secret")
	material, err := registry.Resolve(
		context.Background(),
		credentials.Ref("env://BUILTIN_REGISTRY_TEST"),
	)
	if err != nil || string(material.Value) != "secret" {
		t.Fatalf("Resolve() = %q, %v", material.Value, err)
	}
}

func TestNewRegistryForDynamicReferencesDoesNotExpandStatus(t *testing.T) {
	t.Setenv("BUILTIN_DYNAMIC_REGISTRY_TEST", "secret")
	registry, err := NewRegistryForDynamicReferences(
		nil,
		[]string{"env"},
		Options{},
	)
	if err != nil {
		t.Fatalf("NewRegistryForDynamicReferences() error = %v", err)
	}
	material, err := registry.Resolve(
		context.Background(),
		credentials.Ref("env://BUILTIN_DYNAMIC_REGISTRY_TEST"),
	)
	if err != nil || string(material.Value) != "secret" {
		t.Fatalf("Resolve() = %q, %v", material.Value, err)
	}
	if status := registry.CredentialStatus(); status.ConfiguredReferences != 0 {
		t.Fatalf("CredentialStatus() = %#v", status)
	}
}

func TestNewRegistryInitializesAWSWorkloadSource(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDEXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	document := config.Document{
		Providers: []config.Provider{{
			Name:   "bedrock",
			Type:   "bedrock",
			Region: "us-east-1",
			Auth: config.ProviderAuth{
				Type:       config.AuthAWSSigV4,
				Credential: credentials.Ref("workload://aws"),
			},
		}},
		Deployments: []config.Deployment{{
			Name:     "bedrock",
			Provider: "bedrock",
			Model:    "model",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "model",
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "bedrock",
					Weight:     1,
				}},
			}},
		}},
	}
	registry, err := NewRegistry(document, Options{})
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	resolved, err := registry.Resolve(
		context.Background(),
		credentials.Ref("workload://aws"),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	material, err := credentialawsworkload.DecodeSigningMaterial(
		resolved.Value,
	)
	if err != nil || material.AccessKeyID != "AKIDEXAMPLE" {
		t.Fatalf("material = %#v, %v", material, err)
	}
}
