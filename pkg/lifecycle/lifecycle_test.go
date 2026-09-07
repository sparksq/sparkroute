package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestMemoryActivationCoordinatorOwnershipAndFencing(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	coordinator := NewMemoryActivationCoordinator()
	coordinator.now = func() time.Time { return now }
	ctx := context.Background()

	owner, err := coordinator.Begin(ctx, "controller\x00revision", "gateway-a", time.Minute)
	if err != nil || owner.Disposition != ActivationOwner || owner.FencingToken != 1 {
		t.Fatalf("owner = %#v, %v", owner, err)
	}
	observer, err := coordinator.Begin(ctx, owner.Key, "gateway-b", time.Minute)
	if err != nil || observer.Disposition != ActivationObserver ||
		observer.FencingToken != owner.FencingToken || observer.Holder != owner.Holder {
		t.Fatalf("observer = %#v, %v", observer, err)
	}
	renewed, err := coordinator.Renew(ctx, owner, 2*time.Minute)
	if err != nil || !renewed.ExpiresAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("renewed = %#v, %v", renewed, err)
	}
	if err := coordinator.Complete(ctx, observer); !errors.Is(err, ErrActivationClaimLost) {
		t.Fatalf("observer Complete() error = %v", err)
	}
	if err := coordinator.Complete(ctx, renewed); err != nil {
		t.Fatalf("owner Complete() error = %v", err)
	}

	next, err := coordinator.Begin(ctx, owner.Key, "gateway-b", time.Minute)
	if err != nil || next.Disposition != ActivationOwner || next.FencingToken != 2 {
		t.Fatalf("next owner = %#v, %v", next, err)
	}
}

func TestTargetsFromDocumentCarriesLogicalModelMappings(t *testing.T) {
	t.Parallel()

	document := config.Document{
		Providers: []config.Provider{{
			Name: "local", Type: "openai_compatible", BaseURL: "http://127.0.0.1:8000/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "runtime", Provider: "local", Model: "Qwen/Qwen3-32B",
			EndpointSource: config.EndpointSource{
				Type: config.EndpointSourceDiscovered, Controller: "sparkrun",
			},
		}},
		VirtualModels: []config.VirtualModel{
			{Name: "chat", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "runtime", Weight: 1}}}}},
			{Name: "chat-fast", Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{{Deployment: "runtime", Weight: 1}}}}},
		},
	}
	targets, err := TargetsFromDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || len(targets[0].LogicalModels) != 2 ||
		targets[0].LogicalModels[0] != "chat" || targets[0].LogicalModels[1] != "chat-fast" {
		t.Fatalf("targets = %#v", targets)
	}
}

func TestMemoryActivationCoordinatorExpiredOwnerCannotMutate(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	coordinator := NewMemoryActivationCoordinator()
	coordinator.now = func() time.Time { return now }
	ctx := context.Background()
	stale, err := coordinator.Begin(ctx, "key", "gateway-a", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := coordinator.Complete(ctx, stale); !errors.Is(err, ErrActivationClaimLost) {
		t.Fatalf("expired Complete() error = %v", err)
	}
	current, err := coordinator.Begin(ctx, "key", "gateway-b", time.Minute)
	if err != nil || current.FencingToken <= stale.FencingToken {
		t.Fatalf("current = %#v, %v", current, err)
	}
	if _, err := coordinator.Renew(ctx, stale, time.Minute); !errors.Is(err, ErrActivationClaimLost) {
		t.Fatalf("stale Renew() error = %v", err)
	}
	if err := coordinator.Complete(ctx, stale); !errors.Is(err, ErrActivationClaimLost) {
		t.Fatalf("stale Complete() error = %v", err)
	}
}
