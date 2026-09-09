// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package credentials defines provider-neutral secret resolution contracts.
package credentials

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ref is an opaque, scheme-qualified reference to secret material.
type Ref string

// Validate checks only the reference shape. Individual sources own the
// provider-specific path, namespace, and authorization rules.
func (r Ref) Validate() error {
	value := string(r)
	scheme, rest, ok := strings.Cut(value, "://")
	if !ok || scheme == "" || rest == "" {
		return fmt.Errorf("credential reference must be scheme-qualified")
	}
	for index, ch := range scheme {
		if index == 0 && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') {
			return fmt.Errorf("credential reference scheme must start with a letter")
		}
		if !isSchemeChar(ch) {
			return fmt.Errorf("credential reference has invalid scheme %q", scheme)
		}
	}
	return nil
}

func isSchemeChar(ch rune) bool {
	return ch >= 'a' && ch <= 'z' ||
		ch >= 'A' && ch <= 'Z' ||
		ch >= '0' && ch <= '9' ||
		ch == '+' || ch == '-' || ch == '.'
}

// Material is resolved secret data and optional rotation metadata. Callers must
// never log Value.
type Material struct {
	Value   []byte
	Version string
}

// Source resolves secret references. Implementations may cache and atomically
// rotate values, but returned bytes are still treated as sensitive.
type Source interface {
	Resolve(ctx context.Context, ref Ref) (Material, error)
}

// ResolutionStatus is a content-free summary of credential availability.
type ResolutionStatus string

const (
	ResolutionHealthy     ResolutionStatus = "healthy"
	ResolutionDegraded    ResolutionStatus = "degraded"
	ResolutionUnavailable ResolutionStatus = "unavailable"
	ResolutionUnknown     ResolutionStatus = "unknown"
	ResolutionUnused      ResolutionStatus = "unused"
)

// Status is a bounded, source-level credential projection. It intentionally
// contains neither credential references nor provider/source error text.
type Status struct {
	ObservedAt           time.Time        `json:"observed_at"`
	Status               ResolutionStatus `json:"status"`
	ConfiguredReferences int              `json:"configured_references"`
	ResolvedReferences   int              `json:"resolved_references"`
	FailingReferences    int              `json:"failing_references"`
	UnresolvedReferences int              `json:"unresolved_references"`
	Sources              []SourceStatus   `json:"sources"`
}

// SourceStatus aggregates all configured references for one URI scheme.
// Resolution and rotation counters are process-local and reset when a new
// configuration snapshot is installed.
type SourceStatus struct {
	Scheme               string           `json:"scheme"`
	Status               ResolutionStatus `json:"status"`
	ConfiguredReferences int              `json:"configured_references"`
	ResolvedReferences   int              `json:"resolved_references"`
	FailingReferences    int              `json:"failing_references"`
	UnresolvedReferences int              `json:"unresolved_references"`
	ResolutionAttempts   uint64           `json:"resolution_attempts"`
	ResolutionFailures   uint64           `json:"resolution_failures"`
	Rotations            uint64           `json:"rotations"`
	LastResolvedAt       *time.Time       `json:"last_resolved_at,omitempty"`
	LastFailedAt         *time.Time       `json:"last_failed_at,omitempty"`
	LastRotatedAt        *time.Time       `json:"last_rotated_at,omitempty"`
}

// StatusSource exposes only the bounded aggregate above. Implementations must
// never add secret values, opaque references, version strings, or raw errors.
type StatusSource interface {
	CredentialStatus() Status
}

type aggregateStatusSource struct {
	sources []StatusSource
}

// AggregateStatusSources combines independent trusted credential registries
// into one source-level projection. Schemes shared by data-plane, caller-auth,
// and admin-auth registries are merged, so subsystem-specific references are
// not exposed through the aggregate shape.
func AggregateStatusSources(sources ...StatusSource) StatusSource {
	filtered := make([]StatusSource, 0, len(sources))
	for _, source := range sources {
		if source != nil {
			filtered = append(filtered, source)
		}
	}
	return aggregateStatusSource{sources: filtered}
}

func (a aggregateStatusSource) CredentialStatus() Status {
	byScheme := make(map[string]*SourceStatus)
	result := Status{Sources: []SourceStatus{}}
	for _, source := range a.sources {
		current := source.CredentialStatus()
		if current.ObservedAt.After(result.ObservedAt) {
			result.ObservedAt = current.ObservedAt
		}
		for _, item := range current.Sources {
			combined := byScheme[item.Scheme]
			if combined == nil {
				combined = &SourceStatus{Scheme: item.Scheme}
				byScheme[item.Scheme] = combined
			}
			combined.ConfiguredReferences += item.ConfiguredReferences
			combined.ResolvedReferences += item.ResolvedReferences
			combined.FailingReferences += item.FailingReferences
			combined.UnresolvedReferences += item.UnresolvedReferences
			combined.ResolutionAttempts += item.ResolutionAttempts
			combined.ResolutionFailures += item.ResolutionFailures
			combined.Rotations += item.Rotations
			setNewestTime(&combined.LastResolvedAt, valueTime(item.LastResolvedAt))
			setNewestTime(&combined.LastFailedAt, valueTime(item.LastFailedAt))
			setNewestTime(&combined.LastRotatedAt, valueTime(item.LastRotatedAt))
		}
	}
	if result.ObservedAt.IsZero() {
		result.ObservedAt = time.Now().UTC()
	}
	for _, item := range byScheme {
		item.Status = resolutionStatus(
			item.ConfiguredReferences,
			item.ResolvedReferences,
			item.FailingReferences,
			item.UnresolvedReferences,
		)
		result.ConfiguredReferences += item.ConfiguredReferences
		result.ResolvedReferences += item.ResolvedReferences
		result.FailingReferences += item.FailingReferences
		result.UnresolvedReferences += item.UnresolvedReferences
		result.Sources = append(result.Sources, *item)
	}
	sort.Slice(result.Sources, func(i, j int) bool {
		return result.Sources[i].Scheme < result.Sources[j].Scheme
	})
	result.Status = resolutionStatus(
		result.ConfiguredReferences,
		result.ResolvedReferences,
		result.FailingReferences,
		result.UnresolvedReferences,
	)
	return result
}

func valueTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

type referenceResolutionStatus uint8

const (
	referenceUnresolved referenceResolutionStatus = iota
	referenceResolved
	referenceFailing
)

type referenceStatus struct {
	scheme             string
	status             referenceResolutionStatus
	fingerprint        [sha256.Size]byte
	hasFingerprint     bool
	resolutionAttempts uint64
	resolutionFailures uint64
	rotations          uint64
	lastResolvedAt     time.Time
	lastFailedAt       time.Time
	lastRotatedAt      time.Time
}

// Registry dispatches references to sources by URI scheme.
type Registry struct {
	mu         sync.RWMutex
	sources    map[string]Source
	references map[Ref]*referenceStatus
	now        func() time.Time
	hmacKey    [sha256.Size]byte
}

func NewRegistry() *Registry {
	registry := &Registry{
		sources:    make(map[string]Source),
		references: make(map[Ref]*referenceStatus),
		now:        time.Now,
	}
	if _, err := rand.Read(registry.hmacKey[:]); err != nil {
		panic("initialize credential rotation fingerprint: " + err.Error())
	}
	return registry
}

func (r *Registry) Register(scheme string, source Source) error {
	if source == nil {
		return fmt.Errorf("credential source is required")
	}
	if scheme == "" || strings.Contains(scheme, "://") {
		return fmt.Errorf("credential scheme must not be empty or contain ://")
	}
	if err := Ref(scheme + "://registered").Validate(); err != nil {
		return fmt.Errorf("credential scheme: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sources[strings.ToLower(scheme)]; exists {
		return fmt.Errorf("credential scheme %q is already registered", scheme)
	}
	r.sources[strings.ToLower(scheme)] = source
	return nil
}

// TrackReferences declares the trusted, finite reference set that status may
// observe. References resolved outside this set are still dispatched normally
// but cannot expand or influence the status projection.
func (r *Registry) TrackReferences(references []Ref) error {
	tracked := make(map[Ref]*referenceStatus, len(references))
	for _, ref := range references {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("credential reference: %w", err)
		}
		scheme, _, _ := strings.Cut(string(ref), "://")
		tracked[ref] = &referenceStatus{scheme: strings.ToLower(scheme)}
	}
	r.mu.Lock()
	r.references = tracked
	r.mu.Unlock()
	return nil
}

func (r *Registry) Resolve(ctx context.Context, ref Ref) (Material, error) {
	if err := ref.Validate(); err != nil {
		return Material{}, err
	}
	scheme, _, _ := strings.Cut(string(ref), "://")
	r.mu.RLock()
	source := r.sources[strings.ToLower(scheme)]
	r.mu.RUnlock()
	if source == nil {
		return Material{}, fmt.Errorf("credential scheme %q is not configured", scheme)
	}
	material, err := source.Resolve(ctx, ref)
	r.recordResolution(ref, material, err)
	return material, err
}

func (r *Registry) recordResolution(ref Ref, material Material, resolveErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	status := r.references[ref]
	if status == nil {
		return
	}
	now := r.now().UTC()
	status.resolutionAttempts++
	if resolveErr != nil {
		status.status = referenceFailing
		status.resolutionFailures++
		status.lastFailedAt = now
		return
	}
	fingerprint := credentialFingerprint(r.hmacKey[:], material)
	if status.hasFingerprint && !hmac.Equal(status.fingerprint[:], fingerprint[:]) {
		status.rotations++
		status.lastRotatedAt = now
	}
	status.fingerprint = fingerprint
	status.hasFingerprint = true
	status.status = referenceResolved
	status.lastResolvedAt = now
}

func credentialFingerprint(key []byte, material Material) [sha256.Size]byte {
	digest := hmac.New(sha256.New, key)
	_, _ = digest.Write([]byte(material.Version))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(material.Value)
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

// CredentialStatus returns a deterministic aggregate of the latest observed
// result for each configured reference. It never resolves a credential itself.
func (r *Registry) CredentialStatus() Status {
	r.mu.RLock()
	defer r.mu.RUnlock()
	byScheme := make(map[string]*SourceStatus, len(r.sources))
	for scheme := range r.sources {
		byScheme[scheme] = &SourceStatus{Scheme: scheme}
	}
	for _, reference := range r.references {
		source := byScheme[reference.scheme]
		if source == nil {
			source = &SourceStatus{Scheme: reference.scheme}
			byScheme[reference.scheme] = source
		}
		source.ConfiguredReferences++
		source.ResolutionAttempts += reference.resolutionAttempts
		source.ResolutionFailures += reference.resolutionFailures
		source.Rotations += reference.rotations
		setNewestTime(&source.LastResolvedAt, reference.lastResolvedAt)
		setNewestTime(&source.LastFailedAt, reference.lastFailedAt)
		setNewestTime(&source.LastRotatedAt, reference.lastRotatedAt)
		switch reference.status {
		case referenceResolved:
			source.ResolvedReferences++
		case referenceFailing:
			source.FailingReferences++
		default:
			source.UnresolvedReferences++
		}
	}
	result := Status{
		ObservedAt: r.now().UTC(),
		Sources:    make([]SourceStatus, 0, len(byScheme)),
	}
	for _, source := range byScheme {
		source.Status = resolutionStatus(
			source.ConfiguredReferences,
			source.ResolvedReferences,
			source.FailingReferences,
			source.UnresolvedReferences,
		)
		result.ConfiguredReferences += source.ConfiguredReferences
		result.ResolvedReferences += source.ResolvedReferences
		result.FailingReferences += source.FailingReferences
		result.UnresolvedReferences += source.UnresolvedReferences
		result.Sources = append(result.Sources, *source)
	}
	sort.Slice(result.Sources, func(i, j int) bool {
		return result.Sources[i].Scheme < result.Sources[j].Scheme
	})
	result.Status = resolutionStatus(
		result.ConfiguredReferences,
		result.ResolvedReferences,
		result.FailingReferences,
		result.UnresolvedReferences,
	)
	return result
}

func resolutionStatus(configured, resolved, failing, unresolved int) ResolutionStatus {
	switch {
	case configured == 0:
		return ResolutionUnused
	case failing == configured:
		return ResolutionUnavailable
	case failing > 0:
		return ResolutionDegraded
	case unresolved > 0:
		return ResolutionUnknown
	case resolved == configured:
		return ResolutionHealthy
	default:
		return ResolutionUnknown
	}
}

func setNewestTime(destination **time.Time, candidate time.Time) {
	if candidate.IsZero() || *destination != nil && !candidate.After(**destination) {
		return
	}
	copy := candidate
	*destination = &copy
}

var _ Source = (*Registry)(nil)
var _ StatusSource = (*Registry)(nil)
