// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package builtin composes the credential sources shipped with the standalone
// gateway. Sources are initialized only when the active document references
// their scheme.
package builtin

import (
	"fmt"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	credentialawsworkload "github.com/sparksq/sparkroute/pkg/credentials/awsworkload"
	credentialenv "github.com/sparksq/sparkroute/pkg/credentials/env"
	credentialfile "github.com/sparksq/sparkroute/pkg/credentials/file"
	credentialkubernetes "github.com/sparksq/sparkroute/pkg/credentials/kubernetes"
)

type Options struct {
	File       credentialfile.Options
	Kubernetes credentialkubernetes.Options
	Additional map[string]credentials.Source
}

func NewRegistry(
	document config.Document,
	options Options,
) (*credentials.Registry, error) {
	return NewRegistryForReferences(document.CredentialReferences(), options)
}

// NewRegistryForReferences composes the built-in sources required by an
// arbitrary trusted subsystem, such as a separate caller-auth configuration.
func NewRegistryForReferences(
	references []credentials.Ref,
	options Options,
) (*credentials.Registry, error) {
	return newRegistry(references, nil, options)
}

// NewRegistryForDynamicReferences composes sources for a trusted finite set
// of credential schemes even when the concrete references are supplied later.
// Only references passed in references are projected in credential status;
// dynamic resolutions cannot grow that projection.
func NewRegistryForDynamicReferences(
	references []credentials.Ref,
	schemes []string,
	options Options,
) (*credentials.Registry, error) {
	return newRegistry(references, schemes, options)
}

func newRegistry(
	references []credentials.Ref,
	dynamicSchemes []string,
	options Options,
) (*credentials.Registry, error) {
	registry := credentials.NewRegistry()
	configured := make(map[string]struct{})
	register := func(scheme string, source credentials.Source) error {
		if err := registry.Register(scheme, source); err != nil {
			return err
		}
		configured[strings.ToLower(scheme)] = struct{}{}
		return nil
	}
	if err := register("env", credentialenv.Source{}); err != nil {
		return nil, fmt.Errorf("register environment credential source: %w", err)
	}
	for scheme, source := range options.Additional {
		if err := register(scheme, source); err != nil {
			return nil, fmt.Errorf("register credential source %q: %w", scheme, err)
		}
	}

	referenced := make(map[string]struct{})
	for _, ref := range references {
		if err := ref.Validate(); err != nil {
			return nil, fmt.Errorf("credential reference %q: %w", ref, err)
		}
		scheme, _, _ := strings.Cut(string(ref), "://")
		referenced[strings.ToLower(scheme)] = struct{}{}
	}
	for _, scheme := range dynamicSchemes {
		scheme = strings.ToLower(strings.TrimSpace(scheme))
		if scheme == "" {
			continue
		}
		if err := credentials.Ref(scheme + "://dynamic").Validate(); err != nil {
			return nil, fmt.Errorf("dynamic credential scheme %q: %w", scheme, err)
		}
		referenced[scheme] = struct{}{}
	}
	if _, needed := referenced["file"]; needed {
		if _, exists := configured["file"]; !exists {
			source, err := credentialfile.New(options.File)
			if err != nil {
				return nil, fmt.Errorf("configure file credential source: %w", err)
			}
			if err := register("file", source); err != nil {
				return nil, err
			}
		}
	}
	if _, needed := referenced["k8s"]; needed {
		if _, exists := configured["k8s"]; !exists {
			source, err := credentialkubernetes.New(options.Kubernetes)
			if err != nil {
				return nil, fmt.Errorf("configure Kubernetes Secret credential source: %w", err)
			}
			if err := register("k8s", source); err != nil {
				return nil, err
			}
		}
	}
	if _, needed := referenced["workload"]; needed {
		if _, exists := configured["workload"]; !exists {
			if err := register(
				"workload",
				credentialawsworkload.New(
					credentialawsworkload.Options{},
				),
			); err != nil {
				return nil, err
			}
		}
	}
	for scheme := range referenced {
		if _, exists := configured[scheme]; !exists {
			return nil, fmt.Errorf(
				"credential scheme %q is referenced but not configured",
				scheme,
			)
		}
	}
	if err := registry.TrackReferences(references); err != nil {
		return nil, err
	}
	return registry, nil
}
