// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package lifecycle

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
)

func BenchmarkRegressionMatrixActivationReadyPath(b *testing.B) {
	registry := endpointregistry.NewMemory()
	ready := make(chan struct{})
	close(ready)
	controller := &fakeController{
		registry: registry,
		endpoint: benchmarkActivationEndpoint(),
		started:  make(chan struct{}), ready: ready,
	}
	coordinator, err := NewAdmissionCoordinator(
		[]Target{activationTarget(100_000, 1<<40)},
		AdmissionOptions{
			Registry: registry, Controllers: map[string]Controller{"controller": controller},
			Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }),
			InstanceID: "benchmark", PollInterval: 10 * time.Millisecond,
		},
	)
	if err != nil {
		b.Fatal(err)
	}
	lease, err := coordinator.Acquire(context.Background(), benchmarkAdmissionRequest())
	if err != nil {
		b.Fatal(err)
	}
	if err := coordinator.Release(context.Background(), lease, RequestOutcome{Success: true}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease, acquireErr := coordinator.Acquire(context.Background(), benchmarkAdmissionRequest())
			if acquireErr != nil {
				b.Error(acquireErr)
				return
			}
			if releaseErr := coordinator.Release(context.Background(), lease, RequestOutcome{Success: true}); releaseErr != nil {
				b.Error(releaseErr)
				return
			}
		}
	})
}

func BenchmarkRegressionMatrixActivationColdCoalescing(b *testing.B) {
	const callers = 64
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		registry := endpointregistry.NewMemory()
		controller := &fakeController{
			registry: registry, endpoint: benchmarkActivationEndpoint(),
			started: make(chan struct{}), ready: make(chan struct{}),
		}
		coordinator, err := NewAdmissionCoordinator(
			[]Target{activationTarget(callers+1, 1<<40)},
			AdmissionOptions{
				Registry: registry, Controllers: map[string]Controller{"controller": controller},
				Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }),
				InstanceID: "benchmark", PollInterval: 10 * time.Millisecond,
			},
		)
		if err != nil {
			b.Fatal(err)
		}
		results := make(chan error, callers)
		leases := make(chan *AdmissionLease, callers)
		var wait sync.WaitGroup
		wait.Add(callers)
		b.StartTimer()
		for range callers {
			go func() {
				defer wait.Done()
				lease, acquireErr := coordinator.Acquire(context.Background(), benchmarkAdmissionRequest())
				if acquireErr == nil {
					leases <- lease
				}
				results <- acquireErr
			}()
		}
		<-controller.started
		deadline := time.Now().Add(5 * time.Second)
		for {
			snapshot, snapshotErr := coordinator.Snapshot(context.Background())
			if snapshotErr == nil && len(snapshot.Bindings) == 1 && snapshot.Bindings[0].QueuedWaiters == callers {
				break
			}
			if time.Now().After(deadline) {
				b.Fatal("cold-start waiters did not coalesce")
			}
			time.Sleep(100 * time.Microsecond)
		}
		close(controller.ready)
		wait.Wait()
		close(results)
		close(leases)
		for acquireErr := range results {
			if acquireErr != nil {
				b.Fatal(acquireErr)
			}
		}
		for lease := range leases {
			if err := coordinator.Release(context.Background(), lease, RequestOutcome{Success: true}); err != nil {
				b.Fatal(err)
			}
		}
		controller.mu.Lock()
		launches := controller.launches
		controller.mu.Unlock()
		if launches != 1 {
			b.Fatalf("activation launches = %d, want 1", launches)
		}
	}
	b.ReportMetric(callers, "waiters/activation")
}

func benchmarkActivationEndpoint() endpointregistry.Endpoint {
	now := time.Now()
	return endpointregistry.Endpoint{
		ID: "benchmark-endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
		Controller: "controller", Protocol: "openai", ServedModels: []string{"upstream"},
		State: endpointregistry.StateReady, RegisteredAt: now, HeartbeatAt: now,
	}
}

func benchmarkAdmissionRequest() AdmissionRequest {
	return AdmissionRequest{
		Deployment: "deployment", BodyBytes: 1024,
		Features: RequestFeatures{VirtualModel: "benchmark", Protocol: "openai"},
	}
}
