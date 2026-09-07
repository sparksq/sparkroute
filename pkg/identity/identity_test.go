package identity

import (
	"context"
	"testing"
)

func TestContextIdentityIsCopiedAtBothBoundaries(t *testing.T) {
	t.Parallel()

	original := Identity{
		Principal: Principal{
			ID:                 "client",
			Roles:              []string{"inference"},
			AllowedAttribution: []string{"workspace"},
			FixedAttribution:   map[string]string{AttributeTenant: "tenant-a"},
		},
		Attribution: Attribution{AttributeWorkspace: "workspace-a"},
	}
	ctx := WithContext(context.Background(), original)
	original.Principal.Roles[0] = "mutated"
	original.Principal.FixedAttribution[AttributeTenant] = "mutated"
	original.Attribution[AttributeWorkspace] = "mutated"

	first, ok := FromContext(ctx)
	if !ok ||
		first.Principal.Roles[0] != "inference" ||
		first.Principal.FixedAttribution[AttributeTenant] != "tenant-a" ||
		first.Attribution[AttributeWorkspace] != "workspace-a" {
		t.Fatalf("FromContext() = %#v, %t", first, ok)
	}
	first.Principal.Roles[0] = "again"
	first.Attribution[AttributeWorkspace] = "again"
	second, _ := FromContext(ctx)
	if second.Principal.Roles[0] != "inference" ||
		second.Attribution[AttributeWorkspace] != "workspace-a" {
		t.Fatalf("second FromContext() = %#v", second)
	}
}
