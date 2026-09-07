package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type staticSource struct{}

func (staticSource) Resolve(context.Context, Ref) (Material, error) {
	return Material{Value: []byte("secret")}, nil
}

func TestRegistryDispatchesByScheme(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if err := registry.Register("env", staticSource{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	material, err := registry.Resolve(context.Background(), "ENV://TOKEN")
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if string(material.Value) != "secret" {
		t.Fatalf("Value = %q, want secret", material.Value)
	}
}

func TestRefRejectsSchemeStartingWithDigit(t *testing.T) {
	t.Parallel()

	if err := Ref("1env://TOKEN").Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid scheme")
	}
}

type mutableSource struct {
	material Material
	err      error
}

func (s *mutableSource) Resolve(context.Context, Ref) (Material, error) {
	return s.material, s.err
}

func TestRegistryCredentialStatusTracksResolutionAndRotationWithoutReferences(t *testing.T) {
	t.Parallel()

	source := &mutableSource{material: Material{
		Value:   []byte("first-secret"),
		Version: "version-one",
	}}
	registry := NewRegistry()
	now := time.Unix(100, 0).UTC()
	registry.now = func() time.Time { return now }
	if err := registry.Register("env", source); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if err := registry.TrackReferences([]Ref{"env://PRIVATE_TOKEN"}); err != nil {
		t.Fatalf("TrackReferences() error = %v", err)
	}

	initial := registry.CredentialStatus()
	if initial.Status != ResolutionUnknown ||
		initial.UnresolvedReferences != 1 ||
		len(initial.Sources) != 1 ||
		initial.Sources[0].Scheme != "env" {
		t.Fatalf("initial status = %#v", initial)
	}
	if _, err := registry.Resolve(context.Background(), "env://PRIVATE_TOKEN"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	resolved := registry.CredentialStatus()
	if resolved.Status != ResolutionHealthy ||
		resolved.ResolvedReferences != 1 ||
		resolved.Sources[0].Rotations != 0 ||
		resolved.Sources[0].LastResolvedAt == nil ||
		!resolved.Sources[0].LastResolvedAt.Equal(now) {
		t.Fatalf("resolved status = %#v", resolved)
	}

	now = now.Add(time.Minute)
	source.material = Material{Value: []byte("second-secret"), Version: "version-two"}
	if _, err := registry.Resolve(context.Background(), "env://PRIVATE_TOKEN"); err != nil {
		t.Fatalf("rotated Resolve() error = %v", err)
	}
	rotated := registry.CredentialStatus()
	if rotated.Status != ResolutionHealthy ||
		rotated.Sources[0].Rotations != 1 ||
		rotated.Sources[0].LastRotatedAt == nil ||
		!rotated.Sources[0].LastRotatedAt.Equal(now) {
		t.Fatalf("rotated status = %#v", rotated)
	}

	now = now.Add(time.Minute)
	source.err = errors.New("raw failure containing env://PRIVATE_TOKEN and second-secret")
	if _, err := registry.Resolve(context.Background(), "env://PRIVATE_TOKEN"); err == nil {
		t.Fatal("failing Resolve() error = nil")
	}
	failing := registry.CredentialStatus()
	if failing.Status != ResolutionUnavailable ||
		failing.FailingReferences != 1 ||
		failing.Sources[0].ResolutionFailures != 1 ||
		failing.Sources[0].LastFailedAt == nil {
		t.Fatalf("failing status = %#v", failing)
	}
	raw, err := json.Marshal(failing)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	projection := strings.ToLower(strings.TrimSpace(string(raw)))
	for _, forbidden := range []string{
		"private_token", "first-secret", "second-secret", "raw failure", "version-one", "version-two",
	} {
		if strings.Contains(projection, forbidden) {
			t.Fatalf("credential status exposed %q: %s", forbidden, projection)
		}
	}
}

func TestRegistryCredentialStatusIgnoresUntrackedReferences(t *testing.T) {
	t.Parallel()

	registry := NewRegistry()
	if err := registry.Register("env", staticSource{}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if _, err := registry.Resolve(context.Background(), "env://UNTRACKED"); err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	status := registry.CredentialStatus()
	if status.Status != ResolutionUnused ||
		status.ConfiguredReferences != 0 ||
		len(status.Sources) != 1 ||
		status.Sources[0].Status != ResolutionUnused {
		t.Fatalf("status = %#v", status)
	}
}

type fixedStatusSource Status

func (s fixedStatusSource) CredentialStatus() Status { return Status(s) }

func TestAggregateStatusSourcesMergesSchemesWithoutSubsystemDetail(t *testing.T) {
	t.Parallel()

	resolvedAt := time.Unix(200, 0).UTC()
	status := AggregateStatusSources(
		fixedStatusSource{
			ObservedAt:           time.Unix(200, 0).UTC(),
			Status:               ResolutionHealthy,
			ConfiguredReferences: 1,
			ResolvedReferences:   1,
			Sources: []SourceStatus{{
				Scheme:               "env",
				Status:               ResolutionHealthy,
				ConfiguredReferences: 1,
				ResolvedReferences:   1,
				ResolutionAttempts:   2,
				Rotations:            1,
				LastResolvedAt:       &resolvedAt,
			}},
		},
		fixedStatusSource{
			ObservedAt:           time.Unix(300, 0).UTC(),
			Status:               ResolutionUnavailable,
			ConfiguredReferences: 1,
			FailingReferences:    1,
			Sources: []SourceStatus{{
				Scheme:               "env",
				Status:               ResolutionUnavailable,
				ConfiguredReferences: 1,
				FailingReferences:    1,
				ResolutionAttempts:   3,
				ResolutionFailures:   2,
			}},
		},
	).CredentialStatus()
	if status.Status != ResolutionDegraded ||
		status.ConfiguredReferences != 2 ||
		status.ResolvedReferences != 1 ||
		status.FailingReferences != 1 ||
		len(status.Sources) != 1 ||
		status.Sources[0].Scheme != "env" ||
		status.Sources[0].ResolutionAttempts != 5 ||
		status.Sources[0].Rotations != 1 ||
		!status.ObservedAt.Equal(time.Unix(300, 0).UTC()) {
		t.Fatalf("aggregate status = %#v", status)
	}
}
