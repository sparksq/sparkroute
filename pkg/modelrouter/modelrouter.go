// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

// Package modelrouter selects a configured logical virtual model before the
// gateway builds its provider/deployment execution plan.
package modelrouter

import (
	"context"
	"fmt"
	"time"
)

type RequestFeatures struct {
	IngressProtocol  string
	Operation        string
	Streaming        bool
	HasTools         bool
	StructuredOutput bool
	Modalities       []string
	EstimatedTokens  int64
	// RoutingText is the normalized latest user-authored text. It is populated
	// only for an in-process router and must not be forwarded to an external
	// resolver without a separate content-disclosure policy.
	RoutingText string
	// StageHistory is a bounded, content-free projection of completed tool
	// operations. It is safe to pass to an external router; raw tool names,
	// arguments, outputs, and resource IDs are absent.
	StageHistory StageHistory
}

type Candidate struct {
	Name         string
	Capabilities []string
}

type Input struct {
	RequestedModel       string
	ResolvedVirtualModel string
	// ProviderStatePin makes a logical-model selection sticky for a request
	// which consumes provider-owned state. Routers must either return exactly
	// this virtual model or fail; they must never adaptively reselect another
	// candidate. Kind is bounded provenance such as response, conversation,
	// item, file, or mixed and must never contain a provider resource ID.
	ProviderStatePin *ProviderStatePin
	Candidates       []Candidate
	Features         RequestFeatures
	RoutingIdentity  map[string]string
}

type ProviderStatePin struct {
	VirtualModel string
	Kind         string
}

type Decision struct {
	VirtualModel     string
	Router           string
	Version          string
	Reason           string
	ProviderPriority []string
	MMProjection     *MMProjectionPolicy
	Annotations      map[string]string
	Stage            *StageRouteTrace
}

type Router interface {
	Route(ctx context.Context, input Input) (Decision, error)
}

// ModelPublisher is implemented by routers which add selector names to the
// public model namespace (for example "auto" and "auto:high").
type ModelPublisher interface {
	PublicModels() []string
}

// NamespaceInspector distinguishes an owned selector which is temporarily
// unavailable from a genuinely unknown model name.
type NamespaceInspector interface {
	OwnsVirtualRequest(string) bool
}

// RouteObserver lets adaptive selectors consume the outcome of the gateway's
// complete execution (including retries and protocol translation).
type RouteObserver interface {
	ObserveRoute(virtualModel string, latency time.Duration, status int)
}

// ProjectionCapabilityProvider identifies selector candidates whose effective
// policy can transform media before deployment capability filtering. The
// gateway still re-detects capabilities from the transformed body before it
// builds the provider plan.
type ProjectionCapabilityProvider interface {
	ProjectionCapabilities(
		ctx context.Context,
		input Input,
	) (map[string][]string, error)
}

// ProjectionPolicyInspector exposes only analyzer model names from published
// policy so a protected callback cannot be redirected through a selector or to
// an unrelated logical model.
type ProjectionPolicyInspector interface {
	ProjectionAnalyzerModels() []string
}

// DiscoveredMetadataPublisher accepts complete, source-scoped model metadata
// snapshots. Publishing replaces only that source's prior snapshot; it never
// mutates the operator-authored routing policy or advances its revision.
type DiscoveredMetadataPublisher interface {
	ReplaceDiscoveredMetadata(DiscoveredMetadataSnapshot) error
	RemoveDiscoveredMetadata(source string) error
}

// DiscoveredMetadataInspector exposes the bounded, content-free discovery
// inputs used to enrich routing. It contains model names and public sizing,
// pricing, context, and tag metadata, but no endpoint or credential material.
type DiscoveredMetadataInspector interface {
	DiscoveredMetadata() DiscoveredMetadataState
}

// Passthrough preserves normal direct/alias resolution. It exists so command
// composition can depend on Router without enabling content-aware routing.
type Passthrough struct{}

func (Passthrough) Route(ctx context.Context, input Input) (Decision, error) {
	if err := ctx.Err(); err != nil {
		return Decision{}, err
	}
	if input.ProviderStatePin != nil {
		pinned := input.ProviderStatePin.VirtualModel
		for _, candidate := range input.Candidates {
			if candidate.Name == pinned {
				return Decision{
					VirtualModel: pinned,
					Router:       "passthrough",
					Version:      "v1",
					Reason:       "provider_state_pin",
					Annotations: map[string]string{
						"selector":            input.RequestedModel,
						"state_affinity_kind": input.ProviderStatePin.Kind,
					},
				}, nil
			}
		}
		return Decision{}, fmt.Errorf(
			"provider-state virtual model %q is not an allowed candidate",
			pinned,
		)
	}
	if input.ResolvedVirtualModel == "" {
		return Decision{}, fmt.Errorf("resolved virtual model is required")
	}
	for _, candidate := range input.Candidates {
		if candidate.Name == input.ResolvedVirtualModel {
			return Decision{
				VirtualModel: input.ResolvedVirtualModel,
				Router:       "passthrough",
				Version:      "v1",
				Reason:       "configured_model",
			}, nil
		}
	}
	return Decision{}, fmt.Errorf(
		"resolved virtual model %q is not an allowed candidate",
		input.ResolvedVirtualModel,
	)
}

var _ Router = Passthrough{}
