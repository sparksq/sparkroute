// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package identity defines provider-neutral caller authentication and trusted
// attribution contracts. Protocol handlers consume only the resulting
// content-free identity; profile-specific trust policy remains outside core.
package identity

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrMissingCredentials = errors.New("caller credentials are required")
	ErrInvalidCredentials = errors.New("caller credentials are invalid")
	ErrInvalidAttribution = errors.New("caller attribution is invalid")
	ErrAttributionDenied  = errors.New("caller attribution is not permitted")
)

const (
	AttributeTenant             = "sparkroute.tenant"
	AttributeUser               = "sparkroute.user"
	AttributeSource             = "sparkroute.source"
	AttributeWorkspace          = "sparkroute.workspace"
	AttributeThreadID           = "sparkroute.thread_id"
	AttributeTaskID             = "sparkroute.task_id"
	AttributeExperiment         = "sparkroute.experiment"
	AttributeSession            = "sparkroute.session"
	AttributeMLflowExperimentID = "mlflow.experiment_id"
)

// Principal is the authenticated gateway caller. Tenant and fixed attribution
// are authoritative. AllowedAttribution names the request-scoped fields the
// principal may supply; an attribution policy owns the vocabulary.
type Principal struct {
	ID                 string
	Type               string
	Tenant             string
	Subject            string
	Roles              []string
	AllowedAttribution []string
	FixedAttribution   map[string]string
}

type Attribution map[string]string

type Identity struct {
	Principal   Principal
	Attribution Attribution
}

// Authenticator verifies transport credentials without assigning trust to
// caller attribution headers.
type Authenticator interface {
	Authenticate(
		ctx context.Context,
		request *http.Request,
	) (Principal, error)
}

// ChallengeAuthenticator optionally controls the HTTP authentication challenge
// emitted after application-layer authentication fails. Transport mechanisms
// such as mTLS return an empty challenge because certificate negotiation has
// already happened before HTTP.
type ChallengeAuthenticator interface {
	Authenticator
	AuthenticationChallenge(realm string) string
}

// AttributionPolicy derives bounded trusted attribution after authentication.
type AttributionPolicy interface {
	Resolve(
		ctx context.Context,
		request *http.Request,
		principal Principal,
	) (Attribution, error)
}

type contextKey struct{}

func WithContext(ctx context.Context, value Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, cloneIdentity(value))
}

func FromContext(ctx context.Context) (Identity, bool) {
	value, ok := ctx.Value(contextKey{}).(Identity)
	if !ok {
		return Identity{}, false
	}
	return cloneIdentity(value), true
}

func cloneIdentity(value Identity) Identity {
	value.Principal.Roles = append([]string(nil), value.Principal.Roles...)
	value.Principal.AllowedAttribution = append(
		[]string(nil),
		value.Principal.AllowedAttribution...,
	)
	value.Principal.FixedAttribution = cloneMap(value.Principal.FixedAttribution)
	value.Attribution = cloneMap(value.Attribution)
	return value
}

func cloneMap[M ~map[string]string](source M) M {
	if source == nil {
		return nil
	}
	result := make(M, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
