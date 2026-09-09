// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package endpointregistry

import (
	"context"
	"testing"
	"time"
)

func TestMemoryReturnsOnlyReadyUnexpiredEndpoints(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 27, 0, 0, 0, 0, time.UTC)
	registry := NewMemory()
	registry.now = func() time.Time { return now }
	for _, endpoint := range []Endpoint{
		{ID: "ready", Target: "model", BaseURL: "http://127.0.0.1:8000", State: StateReady},
		{
			ID:        "expired",
			Target:    "model",
			BaseURL:   "http://127.0.0.1:8001",
			State:     StateReady,
			ExpiresAt: now.Add(-time.Second),
		},
		{ID: "starting", Target: "model", BaseURL: "http://127.0.0.1:8002", State: StateActivating},
	} {
		if err := registry.Register(context.Background(), endpoint); err != nil {
			t.Fatalf("Register(%s) error = %v", endpoint.ID, err)
		}
	}

	endpoints, err := registry.Ready(context.Background(), "model")
	if err != nil {
		t.Fatalf("Ready() error = %v", err)
	}
	if len(endpoints) != 1 || endpoints[0].ID != "ready" {
		t.Fatalf("Ready() = %#v", endpoints)
	}
}

func TestMemoryListsBoundedFilteredClones(t *testing.T) {
	t.Parallel()

	registry := NewMemory()
	for _, endpoint := range []Endpoint{
		{
			ID: "endpoint-c", Target: "model-b", BaseURL: "http://127.0.0.1:8002",
			Controller: "sparkrun", State: StateFailed,
		},
		{
			ID: "endpoint-a", Target: "model-a", BaseURL: "http://127.0.0.1:8000",
			Controller: "sparkrun", State: StateReady,
			ServedModels: []string{"upstream-a"}, Metadata: map[string]string{"private": "value"},
		},
		{
			ID: "endpoint-b", Target: "model-a", BaseURL: "http://127.0.0.1:8001",
			Controller: "sparkrun", State: StateReady,
		},
	} {
		if err := registry.Register(context.Background(), endpoint); err != nil {
			t.Fatalf("Register(%s) error = %v", endpoint.ID, err)
		}
	}
	first, err := registry.List(context.Background(), Query{
		Target: "model-a",
		State:  StateReady,
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("List() first error = %v", err)
	}
	if len(first.Endpoints) != 1 || first.Endpoints[0].ID != "endpoint-a" || first.NextCursor == "" {
		t.Fatalf("List() first = %#v", first)
	}
	first.Endpoints[0].ServedModels[0] = "mutated"
	first.Endpoints[0].Metadata["private"] = "mutated"
	second, err := registry.List(context.Background(), Query{
		Target: "model-a",
		State:  StateReady,
		Limit:  1,
		Cursor: first.NextCursor,
	})
	if err != nil {
		t.Fatalf("List() second error = %v", err)
	}
	if len(second.Endpoints) != 1 || second.Endpoints[0].ID != "endpoint-b" || second.NextCursor != "" {
		t.Fatalf("List() second = %#v", second)
	}
	reloaded, err := registry.List(context.Background(), Query{Target: "model-a"})
	if err != nil || reloaded.Endpoints[0].ServedModels[0] != "upstream-a" ||
		reloaded.Endpoints[0].Metadata["private"] != "value" {
		t.Fatalf("List() clone = %#v, %v", reloaded, err)
	}
	if _, err := registry.List(context.Background(), Query{Cursor: "invalid"}); err == nil {
		t.Fatal("List() invalid cursor error = nil")
	}
}

func TestMemoryRejectsStaleFencedRegistration(t *testing.T) {
	t.Parallel()

	registry := NewMemory()
	endpoint := Endpoint{
		ID: "endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
		Controller: "controller", BindingRevision: "revision", FencingToken: 2,
		State: StateReady,
	}
	if err := registry.Register(context.Background(), endpoint); err != nil {
		t.Fatal(err)
	}
	endpoint.FencingToken = 1
	endpoint.BaseURL = "http://127.0.0.1:9001/v1"
	if err := registry.Register(context.Background(), endpoint); err == nil {
		t.Fatal("stale Register() error = nil")
	}
	ready, err := registry.Ready(context.Background(), "deployment")
	if err != nil || len(ready) != 1 || ready[0].BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("Ready() = %#v, %v", ready, err)
	}
}

func TestMemoryRejectsUnsafeEndpointInventory(t *testing.T) {
	t.Parallel()

	registry := NewMemory()
	tests := []Endpoint{
		{ID: "endpoint", Target: "model", BaseURL: "http://127.0.0.1", State: "secret-state"},
		{ID: "endpoint", Target: "model", BaseURL: "http://127.0.0.1", ActiveRequests: -1},
		{ID: "endpoint", Target: "model", BaseURL: "http://127.0.0.1", Metadata: map[string]string{"": "value"}},
	}
	for _, endpoint := range tests {
		if err := registry.Register(context.Background(), endpoint); err == nil {
			t.Fatalf("Register(%#v) error = nil", endpoint)
		}
	}
}
