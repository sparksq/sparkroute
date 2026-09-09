// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package responsesstate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMemoryStoreResolveConflictExpiryAndBound(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore(MemoryOptions{
		MaxEntries: 2,
		Now:        func() time.Time { return now },
	})
	first := testAffinity("resp_1", "deployment-a", now)
	if err := store.Bind(context.Background(), first); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	if err := store.Bind(context.Background(), first); err != nil {
		t.Fatalf("idempotent Bind() error = %v", err)
	}
	conflict := first
	conflict.Deployment = "deployment-b"
	if err := store.Bind(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting Bind() error = %v, want ErrConflict", err)
	}
	got, found, err := store.Resolve(context.Background(), first.Scope, first.ResponseID)
	if err != nil || !found || !SameRoute(got, first) {
		t.Fatalf("Resolve() = %#v, %v, %v", got, found, err)
	}
	if err := store.Bind(context.Background(), testAffinity("resp_2", "deployment-a", now)); err != nil {
		t.Fatalf("Bind(resp_2) error = %v", err)
	}
	if err := store.Bind(context.Background(), testAffinity("resp_3", "deployment-a", now)); err != nil {
		t.Fatalf("Bind(resp_3) error = %v", err)
	}
	if _, found, err := store.Resolve(context.Background(), first.Scope, "resp_1"); err != nil || found {
		t.Fatalf("evicted Resolve() found = %v, error = %v", found, err)
	}
	now = now.Add(DefaultTTL)
	if _, found, err := store.Resolve(context.Background(), first.Scope, "resp_2"); err != nil || found {
		t.Fatalf("expired Resolve() found = %v, error = %v", found, err)
	}
}

func TestValidateResponseID(t *testing.T) {
	t.Parallel()
	for _, invalid := range []string{"", "resp 1", "resp\n1"} {
		if err := ValidateResponseID(invalid); err == nil {
			t.Errorf("ValidateResponseID(%q) error = nil", invalid)
		}
	}
	if err := ValidateResponseID("resp_1"); err != nil {
		t.Fatalf("ValidateResponseID() error = %v", err)
	}
}

func TestMemoryStoreIsolatesScopes(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	store := NewMemoryStore(MemoryOptions{})
	left := testAffinity("resp_shared", "deployment-a", now)
	right := testAffinity("resp_shared", "deployment-b", now)
	right.Scope = "scope-b"
	if err := store.Bind(context.Background(), left); err != nil {
		t.Fatalf("Bind(left) error = %v", err)
	}
	if err := store.Bind(context.Background(), right); err != nil {
		t.Fatalf("Bind(right) error = %v", err)
	}
	got, found, err := store.Resolve(context.Background(), right.Scope, right.ResponseID)
	if err != nil || !found || got.Deployment != "deployment-b" {
		t.Fatalf("Resolve(right) = %#v, %v, %v", got, found, err)
	}
}

func TestMemoryResourceStoreDurableAffinityAndTombstone(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore(MemoryOptions{
		Now: func() time.Time { return now },
	})
	affinity := ResourceAffinity{
		ResourceKey: ResourceKey{
			Scope:      "scope-a",
			Kind:       ResourceConversation,
			ResourceID: "conv_1",
		},
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment-a",
		UpstreamModel: "upstream",
		BoundAt:       now,
	}
	if err := store.BindResource(context.Background(), affinity); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	now = now.Add(365 * 24 * time.Hour)
	got, found, err := store.ResolveResource(context.Background(), affinity.ResourceKey)
	if err != nil || !found || got.Deleted() || !SameResourceRoute(got, affinity) {
		t.Fatalf("ResolveResource() = %#v, %v, %v", got, found, err)
	}
	deletedAt := now
	expiresAt := now.Add(DefaultTombstoneTTL)
	if err := store.TombstoneResource(
		context.Background(),
		affinity.ResourceKey,
		deletedAt,
		expiresAt,
	); err != nil {
		t.Fatalf("TombstoneResource() error = %v", err)
	}
	got, found, err = store.ResolveResource(context.Background(), affinity.ResourceKey)
	if err != nil || !found || !got.Deleted() {
		t.Fatalf("tombstone ResolveResource() = %#v, %v, %v", got, found, err)
	}
	if err := store.BindResource(context.Background(), affinity); !errors.Is(err, ErrConflict) {
		t.Fatalf("BindResource() over tombstone error = %v", err)
	}
	now = expiresAt
	if _, found, err := store.ResolveResource(
		context.Background(),
		affinity.ResourceKey,
	); err != nil || found {
		t.Fatalf("expired tombstone found = %v, error = %v", found, err)
	}
}

func TestMemoryResourceStorePromotesSameRouteToDurable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore(MemoryOptions{Now: func() time.Time { return now }})
	affinity := ResourceAffinity{
		ResourceKey: ResourceKey{
			Scope:      "scope-a",
			Kind:       ResourceItem,
			ResourceID: "item_1",
		},
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}
	if err := store.BindResource(context.Background(), affinity); err != nil {
		t.Fatalf("BindResource() expiring error = %v", err)
	}
	durable := affinity
	durable.BoundAt = now.Add(time.Minute)
	durable.ExpiresAt = time.Time{}
	if err := store.BindResource(context.Background(), durable); err != nil {
		t.Fatalf("BindResource() durable error = %v", err)
	}
	got, found, err := store.ResolveResource(
		context.Background(),
		affinity.ResourceKey,
	)
	if err != nil || !found || !got.ExpiresAt.IsZero() {
		t.Fatalf("ResolveResource() = %#v, %v, %v", got, found, err)
	}
}

func TestResourceAffinityValidation(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	affinity := ResourceAffinity{
		ResourceKey: ResourceKey{
			Scope:      "scope-a",
			Kind:       ResourceConversation,
			ResourceID: "conv_1",
		},
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream",
		BoundAt:       now,
	}
	if err := affinity.Validate(); err != nil {
		t.Fatalf("durable affinity validation error = %v", err)
	}
	affinity.DeletedAt = now.Add(time.Minute)
	if err := affinity.Validate(); err == nil {
		t.Fatal("deleted affinity without expiry validation error = nil")
	}
	affinity.ExpiresAt = affinity.DeletedAt.Add(DefaultTombstoneTTL)
	if err := affinity.Validate(); err != nil {
		t.Fatalf("tombstone affinity validation error = %v", err)
	}
}

func testAffinity(responseID, deployment string, now time.Time) Affinity {
	return Affinity{
		Scope:         "scope-a",
		ResponseID:    responseID,
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    deployment,
		UpstreamModel: "upstream",
		BoundAt:       now,
		ExpiresAt:     now.Add(DefaultTTL),
	}
}
