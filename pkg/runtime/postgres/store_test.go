// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

func TestRuntimeMigrationContainsFencedClaimsAndEndpointIndexes(t *testing.T) {
	t.Parallel()

	content, err := migrationFiles.ReadFile("migrations/001_runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(content)
	for _, required := range []string{
		"llm_runtime_activation_claims",
		"fencing_token BIGINT NOT NULL",
		"claim_state IN ('active', 'completed')",
		"llm_runtime_endpoints",
		"llm_runtime_endpoints_target_ready_idx",
		"llm_runtime_endpoints_controller_state_idx",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("runtime migration does not contain %q", required)
		}
	}
}

func TestPostgresRuntimeStoreIntegration(t *testing.T) {
	url := os.Getenv("SPARKROUTE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("SPARKROUTE_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, Options{URL: url, AutoMigrate: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	binding := lifecycle.Binding{Controller: "integration-controller", Revision: "integration-revision"}
	key := lifecycle.ActivationKey(binding)
	first, err := store.Begin(ctx, key, "gateway-a", time.Minute)
	if err != nil || first.Disposition != lifecycle.ActivationOwner {
		t.Fatalf("first Begin() = %#v, %v", first, err)
	}
	observer, err := store.Begin(ctx, key, "gateway-b", time.Minute)
	if err != nil || observer.Disposition != lifecycle.ActivationObserver ||
		observer.FencingToken != first.FencingToken {
		t.Fatalf("observer Begin() = %#v, %v", observer, err)
	}
	renewed, err := store.Renew(ctx, first, 2*time.Minute)
	if err != nil || !renewed.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("Renew() = %#v, %v", renewed, err)
	}

	oldEndpoint := endpointregistry.Endpoint{
		ID: "runtime-integration-old", Target: "runtime-integration-target",
		BaseURL: "http://127.0.0.1:9000/v1", Protocol: "openai",
		ServedModels: []string{"model"}, Controller: binding.Controller,
		BindingRevision: binding.Revision, FencingToken: renewed.FencingToken,
		State: endpointregistry.StateReady, RegisteredAt: time.Now(), HeartbeatAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Minute), Metadata: map[string]string{"zone": "test"},
	}
	defer func() {
		_ = store.Remove(context.Background(), oldEndpoint.ID)
		_ = store.Remove(context.Background(), "runtime-integration-new")
	}()
	if err := store.Register(ctx, oldEndpoint); err != nil {
		t.Fatalf("Register(old) error = %v", err)
	}
	ready, err := store.Ready(ctx, oldEndpoint.Target)
	if err != nil || len(ready) != 1 || ready[0].ID != oldEndpoint.ID ||
		ready[0].Metadata["zone"] != "test" {
		t.Fatalf("Ready(old) = %#v, %v", ready, err)
	}
	if err := store.Complete(ctx, renewed); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	second, err := store.Begin(ctx, key, "gateway-b", time.Minute)
	if err != nil || second.Disposition != lifecycle.ActivationOwner ||
		second.FencingToken <= renewed.FencingToken {
		t.Fatalf("second Begin() = %#v, %v", second, err)
	}
	if err := store.Register(ctx, oldEndpoint); !errors.Is(err, lifecycle.ErrActivationClaimLost) {
		t.Fatalf("stale Register() error = %v", err)
	}
	ready, err = store.Ready(ctx, oldEndpoint.Target)
	if err != nil || len(ready) != 0 {
		t.Fatalf("Ready(stale) = %#v, %v", ready, err)
	}
	newEndpoint := oldEndpoint
	newEndpoint.ID = "runtime-integration-new"
	newEndpoint.FencingToken = second.FencingToken
	if err := store.Register(ctx, newEndpoint); err != nil {
		t.Fatalf("Register(new) error = %v", err)
	}
	ready, err = store.Ready(ctx, oldEndpoint.Target)
	if err != nil || len(ready) != 1 || ready[0].ID != newEndpoint.ID {
		t.Fatalf("Ready(new) = %#v, %v", ready, err)
	}
	page, err := store.List(ctx, endpointregistry.Query{
		Target: oldEndpoint.Target, Limit: 1,
	})
	if err != nil || len(page.Endpoints) != 1 || page.NextCursor == "" {
		t.Fatalf("List(first) = %#v, %v", page, err)
	}
	next, err := store.List(ctx, endpointregistry.Query{
		Target: oldEndpoint.Target, Limit: 1, Cursor: page.NextCursor,
	})
	if err != nil || len(next.Endpoints) != 1 || next.Endpoints[0].ID == page.Endpoints[0].ID {
		t.Fatalf("List(next) = %#v, %v", next, err)
	}
	if err := store.Complete(ctx, renewed); !errors.Is(err, lifecycle.ErrActivationClaimLost) {
		t.Fatalf("stale Complete() error = %v", err)
	}
	if err := store.Complete(ctx, second); err != nil {
		t.Fatalf("second Complete() error = %v", err)
	}

	expiredBinding := lifecycle.Binding{
		Controller: "integration-controller", Revision: "integration-expired-revision",
	}
	expiredClaim, err := store.Begin(
		ctx,
		lifecycle.ActivationKey(expiredBinding),
		"gateway-expired",
		20*time.Millisecond,
	)
	if err != nil || expiredClaim.Disposition != lifecycle.ActivationOwner {
		t.Fatalf("expired Begin() = %#v, %v", expiredClaim, err)
	}
	time.Sleep(50 * time.Millisecond)
	expiredEndpoint := oldEndpoint
	expiredEndpoint.ID = "runtime-integration-expired"
	expiredEndpoint.BindingRevision = expiredBinding.Revision
	expiredEndpoint.FencingToken = expiredClaim.FencingToken
	if err := store.Register(ctx, expiredEndpoint); !errors.Is(err, lifecycle.ErrActivationClaimLost) {
		t.Fatalf("expired Register() error = %v", err)
	}
	if err := store.Complete(ctx, expiredClaim); !errors.Is(err, lifecycle.ErrActivationClaimLost) {
		t.Fatalf("expired Complete() error = %v", err)
	}
}
