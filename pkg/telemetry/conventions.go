// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package telemetry provides OpenTelemetry instrumentation for the gateway data
// plane. It deliberately separates backend-specific attributes from the core
// GenAI vocabulary so a profile can add sink compatibility without coupling
// core routing code to that sink.
package telemetry

import "go.opentelemetry.io/otel/attribute"

// Conventions adds backend-specific attributes to gateway spans. Implementations
// must return only bounded, non-secret metadata; request and response bodies are
// intentionally not part of this contract.
type Conventions interface {
	Request(RequestStart) []attribute.KeyValue
	Attempt(AttemptStart) []attribute.KeyValue
}

// NoConventions leaves spans with only the core OpenTelemetry vocabulary.
type NoConventions struct{}

func (NoConventions) Request(RequestStart) []attribute.KeyValue { return nil }
func (NoConventions) Attempt(AttemptStart) []attribute.KeyValue { return nil }
