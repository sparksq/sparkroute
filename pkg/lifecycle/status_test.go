package lifecycle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
)

func TestMemoryStatusSourcePublishesBoundedClone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.FixedZone("test", -5*60*60))
	started := now.Add(-time.Minute)
	deadline := now.Add(time.Minute)
	source := NewMemoryStatusSource()
	if err := source.Publish(Snapshot{
		ObservedAt: now,
		Controllers: []ControllerStatus{
			{Controller: "z-controller", UpdatedAt: now, Health: ControllerHealthy},
			{Controller: "a-controller", UpdatedAt: now, Health: ControllerDegraded, Reason: "capacity_exhausted"},
		},
		Bindings: []BindingStatus{
			{
				Controller: "z-controller", BindingRevision: "revision-b",
				Deployment: "deployment-b", State: endpointregistry.StateActivating,
				UpdatedAt: now, ActivationStarted: &started, ActivationDeadline: &deadline,
				QueuedWaiters: 2, QueuedBodyBytes: 32,
			},
			{
				Controller: "a-controller", BindingRevision: "revision-a",
				Deployment: "deployment-a", State: endpointregistry.StateReady,
				UpdatedAt: now,
			},
		},
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	snapshot, err := source.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if snapshot.ObservedAt.Location() != time.UTC ||
		len(snapshot.Controllers) != 2 || snapshot.Controllers[0].Controller != "a-controller" ||
		len(snapshot.Bindings) != 2 || snapshot.Bindings[0].Deployment != "deployment-a" {
		t.Fatalf("Snapshot() = %#v", snapshot)
	}
	*snapshot.Bindings[1].ActivationStarted = time.Time{}
	reloaded, err := source.Snapshot(context.Background())
	if err != nil || reloaded.Bindings[1].ActivationStarted.IsZero() {
		t.Fatalf("Snapshot() clone = %#v, %v", reloaded, err)
	}
}

func TestMemoryStatusSourceRejectsUnsafeStatus(t *testing.T) {
	t.Parallel()

	now := time.Now()
	tests := []Snapshot{
		{Controllers: []ControllerStatus{{Controller: "controller", UpdatedAt: now, Reason: "raw error: secret"}}},
		{Controllers: []ControllerStatus{{Controller: "controller", UpdatedAt: now, QueuedWaiters: -1}}},
		{Bindings: []BindingStatus{{Controller: "controller", BindingRevision: "revision", Deployment: "deployment", UpdatedAt: now, State: "private-state"}}},
	}
	for _, snapshot := range tests {
		if err := NewMemoryStatusSource().Publish(snapshot); err == nil {
			t.Fatalf("Publish(%#v) error = nil", snapshot)
		}
	}
}

func TestSanitizeReasonIsASCIIAndBounded(t *testing.T) {
	t.Parallel()

	value := SanitizeReason("  HTTP 503: Über secret/path " + strings.Repeat("x", 100))
	if value != "http_503_ber_secret_path_"+strings.Repeat("x", 39) {
		t.Fatalf("SanitizeReason() = %q (%d bytes)", value, len(value))
	}
	if value := SanitizeReason("☃☃"); value != "unspecified" {
		t.Fatalf("SanitizeReason(snowmen) = %q", value)
	}
}
