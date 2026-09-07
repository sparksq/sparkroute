package headers

import (
	"context"
	"net/http"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
)

type fakeSecrets map[credentials.Ref]string

func (s fakeSecrets) Resolve(_ context.Context, ref credentials.Ref) (credentials.Material, error) {
	return credentials.Material{Value: []byte(s[ref])}, nil
}

func TestResolvePrecedenceScopeAndSecret(t *testing.T) {
	t.Parallel()

	provider := map[string]config.HeaderValue{
		"X-Profile": {Value: "provider"},
		"X-Probe":   {Value: "enabled", Scope: config.HeaderScopeHealth},
	}
	deployment := map[string]config.HeaderValue{
		"x-profile": {Value: "deployment"},
		"X-Token": {
			ValueFrom: credentials.Ref("env://MODEL_TOKEN"),
			Scope:     config.HeaderScopeInference,
		},
	}
	resolved, err := Resolve(
		context.Background(),
		provider,
		deployment,
		PurposeInference,
		fakeSecrets{"env://MODEL_TOKEN": "secret"},
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if got := resolved.Get("X-Profile"); got != "deployment" {
		t.Fatalf("X-Profile = %q, want deployment", got)
	}
	if got := resolved.Get("X-Token"); got != "secret" {
		t.Fatalf("X-Token = %q, want secret", got)
	}
	if got := resolved.Get("X-Probe"); got != "" {
		t.Fatalf("X-Probe = %q, want empty for inference", got)
	}
}

func TestResolveRejectsSecretHeaderInjection(t *testing.T) {
	t.Parallel()

	_, err := Resolve(
		context.Background(),
		nil,
		map[string]config.HeaderValue{
			"X-Token": {ValueFrom: credentials.Ref("env://MODEL_TOKEN")},
		},
		PurposeInference,
		fakeSecrets{"env://MODEL_TOKEN": "secret\r\nX-Injected: true"},
	)
	if err == nil {
		t.Fatal("Resolve() error = nil, want newline rejection")
	}
}

func TestApplyAuthenticationUsesDeploymentCredential(t *testing.T) {
	t.Parallel()

	target := make(http.Header)
	err := ApplyAuthentication(
		context.Background(),
		target,
		config.ProviderAuth{
			Type:       config.AuthBearer,
			Credential: credentials.Ref("env://PROVIDER"),
		},
		credentials.Ref("env://DEPLOYMENT"),
		fakeSecrets{
			"env://PROVIDER":   "provider-secret",
			"env://DEPLOYMENT": "deployment-secret",
		},
	)
	if err != nil {
		t.Fatalf("ApplyAuthentication() error = %v", err)
	}
	if got := target.Get("Authorization"); got != "Bearer deployment-secret" {
		t.Fatalf("Authorization = %q", got)
	}
}
