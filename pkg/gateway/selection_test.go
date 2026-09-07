package gateway

import (
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
)

func TestTrustedSelectionKeyUsesOnlyTrustedIdentity(t *testing.T) {
	t.Parallel()

	caller := identity.Identity{
		Principal: identity.Principal{ID: "principal-a", Tenant: "tenant-a"},
		Attribution: identity.Attribution{
			identity.AttributeThreadID: "thread-a",
		},
	}
	policy := config.SelectionPolicy{
		Mode: config.SelectionWeightedHash, HashKey: config.SelectionHashThreadID,
	}
	got := trustedSelectionKey(policy, caller, "revision-a", "model-a")
	want := "v1\x00revision-a\x00model-a\x00thread_id\x00thread-a"
	if got != want {
		t.Fatalf("trustedSelectionKey() = %q, want %q", got, want)
	}
	caller.Attribution = nil
	if got := trustedSelectionKey(policy, caller, "revision-a", "model-a"); got != "" {
		t.Fatalf("missing trusted key = %q, want empty fallback", got)
	}
}

func TestTrustedSelectionKeyUsesPrincipalTenant(t *testing.T) {
	t.Parallel()

	got := trustedSelectionKey(
		config.SelectionPolicy{Mode: config.SelectionWeightedHash, HashKey: config.SelectionHashTenant},
		identity.Identity{Principal: identity.Principal{Tenant: "tenant-a"}},
		"revision-a",
		"model-a",
	)
	if got == "" {
		t.Fatal("trustedSelectionKey() = empty")
	}
}
