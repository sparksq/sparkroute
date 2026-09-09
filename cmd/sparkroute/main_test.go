// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/version"
)

func TestBuildInfoDoesNotStartGateway(t *testing.T) {
	var output bytes.Buffer
	if err := run(context.Background(), []string{"--build-info"}, &output, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result["version"] != version.Version || result["commit"] != version.Commit || result["license"] != "AGPL-3.0-only" {
		t.Fatalf("unexpected build information: %v", result)
	}
}

func TestEnvBool(t *testing.T) {
	t.Setenv("TEST_SPARKROUTE_BOOL", "true")
	value, err := envBool("TEST_SPARKROUTE_BOOL", false)
	if err != nil || !value {
		t.Fatalf("envBool() = %v, %v", value, err)
	}
}

func TestEnvBoolRejectsInvalidValue(t *testing.T) {
	t.Setenv("TEST_SPARKROUTE_BOOL", "invalid")
	if _, err := envBool("TEST_SPARKROUTE_BOOL", false); err == nil {
		t.Fatal("envBool() error = nil")
	}
}

func TestSparkRouteEnvironmentNameOverridesLegacyStandaloneName(t *testing.T) {
	t.Setenv("SPARKROUTE_TRACE_QUEUE_CAPACITY", "10")
	t.Setenv("SPARKROUTE_TRACE_QUEUE_CAPACITY", "20")
	value, err := envInt("SPARKROUTE_TRACE_QUEUE_CAPACITY", 1)
	if err != nil || value != 20 {
		t.Fatalf("envInt() = %d, %v; want SparkRoute value 20", value, err)
	}
}

func TestCredentialListParsing(t *testing.T) {
	t.Parallel()

	pathList := " /first " + string(os.PathListSeparator) + "/second"
	paths := splitPathList(pathList)
	if len(paths) != 2 ||
		paths[0] != filepath.Clean("/first") ||
		paths[1] != filepath.Clean("/second") {
		t.Fatalf("splitPathList() = %#v", paths)
	}
	namespaces := splitCommaList(" first,second ,, third ")
	if len(namespaces) != 3 ||
		namespaces[0] != "first" ||
		namespaces[2] != "third" {
		t.Fatalf("splitCommaList() = %#v", namespaces)
	}
}

func TestConfigCheckValidatesReferencedCredentialSources(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod() root error = %v", err)
	}
	secretPath := filepath.Join(root, "provider-key")
	if err := os.WriteFile(secretPath, []byte("secret"), 0o600); err != nil {
		t.Fatalf("WriteFile() secret error = %v", err)
	}
	document := config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			Type:    "openai_compatible",
			BaseURL: "https://example.com/v1",
			Auth: config.ProviderAuth{
				Type:       config.AuthBearer,
				Credential: credentials.Ref("file://" + secretPath),
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
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatalf("WriteFile() config error = %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(
		context.Background(),
		[]string{"-config", configPath, "-config-check"},
		io.Discard,
		logger,
	); err == nil || !strings.Contains(err.Error(), "credential file root") {
		t.Fatalf("run() missing root error = %v", err)
	}
	var output bytes.Buffer
	if err := run(
		context.Background(),
		[]string{
			"-config", configPath,
			"-config-check",
			"-credential-file-roots", root,
		},
		&output,
		logger,
	); err != nil {
		t.Fatalf("run() configured source error = %v", err)
	}
	if !strings.HasPrefix(output.String(), "config ok ") {
		t.Fatalf("run() output = %q", output.String())
	}
}

func TestValidateStandaloneLifecycleTargets(t *testing.T) {
	t.Parallel()

	target := lifecycle.Target{
		Deployment: "deployment", Source: lifecycle.EndpointActivatable,
		Controller: "sparkrun",
		Binding: lifecycle.Binding{
			Controller: "sparkrun", Deployment: "deployment", Revision: "binding-v1",
			Recipe: "@local/model", RecipeRevision: "abc123abc123",
		},
	}
	if err := validateStandaloneLifecycleTargets([]lifecycle.Target{target}); err != nil {
		t.Fatal(err)
	}
	target.Binding.RecipeRevision = ""
	if err := validateStandaloneLifecycleTargets([]lifecycle.Target{target}); err == nil ||
		!strings.Contains(err.Error(), "recipe_revision") {
		t.Fatalf("missing recipe revision error = %v", err)
	}
	target.Binding.RecipeRevision = "abc123abc123"
	target.Controller = "external"
	if err := validateStandaloneLifecycleTargets([]lifecycle.Target{target}); err == nil ||
		!strings.Contains(err.Error(), "external") {
		t.Fatalf("unsupported controller error = %v", err)
	}
}

func TestConfigCheckAcceptsPinnedSparkrunWithoutStartingBridge(t *testing.T) {
	t.Parallel()

	document := config.Document{
		Providers: []config.Provider{{Name: "local", Type: "openai_compatible"}},
		Deployments: []config.Deployment{{
			Name: "qwen", Provider: "local", Model: "Qwen/Qwen3-32B",
			EndpointSource: config.EndpointSource{
				Type: config.EndpointSourceActivatable, Controller: "sparkrun",
				Revision: "binding-v1", Recipe: "@local/qwen",
				RecipeRevision: "abc123abc123", ClusterCandidates: []string{"spark-a"},
			},
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "qwen",
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{Deployment: "qwen", Weight: 1}},
			}},
		}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(context.Background(), []string{
		"-config", configPath, "-config-check",
		"-sparkrun", "-sparkrun-command", "/does/not/exist",
	}, &output, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "config ok ") {
		t.Fatalf("run() output = %q", output.String())
	}
}

func TestConfigCheckValidatesRoutingPolicy(t *testing.T) {
	t.Parallel()

	document := config.Document{
		Providers:   []config.Provider{{Name: "local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1"}},
		Deployments: []config.Deployment{{Name: "deployment", Provider: "local", Model: "upstream"}},
		VirtualModels: []config.VirtualModel{{
			Name:  "logical",
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "deployment", Weight: 1}}}},
		}},
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(directory, "routing-policy.json")
	policy := `{
		"version":2,
		"revision":1,
		"default_virtual_model":"auto",
		"virtual_models":{"auto":{"strategy":"round_robin","models":["logical"]}},
		"models":{"logical":{"enabled":true}}
	}`
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := run(context.Background(), []string{
		"-config", configPath,
		"-routing-policy", policyPath,
		"-config-check",
	}, &output, logger); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "config ok ") {
		t.Fatalf("run() output = %q", output.String())
	}
}

func TestSQLiteConfigSourceInitializesForManagedStandalone(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-config-source", "sqlite",
		"-config-sqlite", filepath.Join(directory, "config.sqlite"),
		"-client-credentials-sqlite", filepath.Join(directory, "credentials.sqlite"),
		"-admin-auth-mode", "managed",
		"-data-address", "0.0.0.0:0",
		"-config-check",
	}, &output, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "config ok ") {
		t.Fatalf("run() output = %q", output.String())
	}
	if _, err := os.Stat(filepath.Join(directory, "config.sqlite")); err != nil {
		t.Fatalf("SQLite configuration was not initialized: %v", err)
	}
}

func TestStandaloneRejectsUnauthenticatedAdminOnNonLoopbackDataListener(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-data-address", "0.0.0.0:8080",
		"-admin-address=",
	}, &output, logger)
	if err == nil || !strings.Contains(err.Error(), "requires a loopback -data-address") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestSQLiteConfigSourceAllowsExplicitDisabledAdminOnLoopback(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-config-source", "sqlite",
		"-config-sqlite", filepath.Join(directory, "config.sqlite"),
		"-admin-auth-mode", "disabled",
		"-data-address", "127.0.0.1:0",
		"-config-check",
	}, &output, logger)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDisabledAdminRejectsNonLoopbackListener(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-admin-auth-mode", "disabled",
		"-data-address", "0.0.0.0:8080",
		"-admin-address", "0.0.0.0:8081",
	}, &output, logger)
	if err == nil || !strings.Contains(err.Error(), "requires a loopback admin listener") {
		t.Fatalf("run() error = %v", err)
	}
}

func TestDisabledAdminAllowsExplicitNonLoopbackOverride(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-config-source", "sqlite",
		"-config-sqlite", filepath.Join(directory, "config.sqlite"),
		"-admin-auth-mode", "disabled",
		"-allow-insecure-admin-nonloopback",
		"-data-address", "0.0.0.0:0",
		"-config-check",
	}, &output, logger)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTokenFileModesRequireTheirTokenPaths(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "admin",
			args: []string{"-admin-auth-mode", "token-file"},
			want: "-admin-auth-mode token-file requires -admin-token-file",
		},
		{
			name: "caller",
			args: []string{"-caller-auth-mode", "token-file"},
			want: "-caller-auth-mode token-file requires -caller-token-file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := run(context.Background(), test.args, io.Discard, logger)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("run() error = %v; want substring %q", err, test.want)
			}
		})
	}
}

func TestTokenFileAdminAllowsExplicitSharedNonLoopbackListener(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-config-source", "sqlite",
		"-config-sqlite", filepath.Join(directory, "config.sqlite"),
		"-admin-auth-mode", "token-file",
		"-admin-token-file", filepath.Join(directory, "admin-token.secret"),
		"-allow-insecure-admin-nonloopback",
		"-data-address", "0.0.0.0:0",
		"-config-check",
	}, &output, logger)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(output.String(), "config ok ") {
		t.Fatalf("run() output = %q", output.String())
	}
}

func TestNonLoopbackOverrideRejectedUnlessAdminAuthIsDisabled(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(context.Background(), []string{
		"-allow-insecure-admin-nonloopback",
		"-data-address", "127.0.0.1:8080",
	}, &output, logger)
	if err == nil || !strings.Contains(err.Error(), "requires -admin-auth-mode disabled") {
		t.Fatalf("run() error = %v", err)
	}
}
