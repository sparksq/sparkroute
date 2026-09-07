// Package headers resolves and merges configured custom upstream headers.
package headers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
)

const maxResolvedValueBytes = 8192

type Purpose string

const (
	PurposeInference Purpose = "inference"
	PurposeHealth    Purpose = "health"
)

// Resolve merges provider defaults and deployment overrides for one request.
// Adapter authentication and transport-controlled headers are intentionally
// applied after this function returns.
func Resolve(
	ctx context.Context,
	provider map[string]config.HeaderValue,
	deployment map[string]config.HeaderValue,
	purpose Purpose,
	secrets credentials.Source,
) (http.Header, error) {
	result := make(http.Header)
	if err := apply(ctx, result, provider, purpose, secrets); err != nil {
		return nil, fmt.Errorf("provider headers: %w", err)
	}
	if err := apply(ctx, result, deployment, purpose, secrets); err != nil {
		return nil, fmt.Errorf("deployment headers: %w", err)
	}
	return result, nil
}

// ApplyAuthentication resolves and injects adapter-owned provider
// authentication after all custom headers have been merged.
func ApplyAuthentication(
	ctx context.Context,
	target http.Header,
	auth config.ProviderAuth,
	deploymentCredential credentials.Ref,
	secrets credentials.Source,
) error {
	if auth.Type == config.AuthNone {
		return nil
	}
	ref := deploymentCredential
	if ref == "" {
		ref = auth.Credential
	}
	if secrets == nil {
		return fmt.Errorf("credential source is required")
	}
	material, err := secrets.Resolve(ctx, ref)
	if err != nil {
		return fmt.Errorf("resolve provider credential: %w", err)
	}
	value := string(material.Value)
	if err := validateResolvedValue(value); err != nil {
		return fmt.Errorf("provider credential: %w", err)
	}
	switch auth.Type {
	case config.AuthBearer:
		target.Set("Authorization", "Bearer "+value)
	case config.AuthHeader:
		target.Set(auth.Header, auth.Prefix+value)
	default:
		return fmt.Errorf("unsupported provider auth type %q", auth.Type)
	}
	return nil
}

func apply(
	ctx context.Context,
	target http.Header,
	values map[string]config.HeaderValue,
	purpose Purpose,
	secrets credentials.Source,
) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})

	for _, name := range names {
		value := values[name]
		if !applies(value.Scope, purpose) {
			continue
		}
		resolved := value.Value
		if value.ValueFrom != "" {
			if secrets == nil {
				return fmt.Errorf("%s: credential source is required", name)
			}
			material, err := secrets.Resolve(ctx, value.ValueFrom)
			if err != nil {
				return fmt.Errorf("%s: resolve value_from: %w", name, err)
			}
			resolved = string(material.Value)
		}
		if err := validateResolvedValue(resolved); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		target.Set(name, resolved)
	}
	return nil
}

func applies(scope config.HeaderScope, purpose Purpose) bool {
	switch scope {
	case "", config.HeaderScopeBoth:
		return true
	case config.HeaderScopeInference:
		return purpose == PurposeInference
	case config.HeaderScopeHealth:
		return purpose == PurposeHealth
	default:
		return false
	}
}

func validateResolvedValue(value string) error {
	if len(value) > maxResolvedValueBytes {
		return fmt.Errorf("resolved value exceeds %d bytes", maxResolvedValueBytes)
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("resolved value contains a newline")
	}
	return nil
}
