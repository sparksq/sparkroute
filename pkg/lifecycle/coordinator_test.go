package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

type fakeController struct {
	registry *endpointregistry.Memory
	endpoint endpointregistry.Endpoint
	started  chan struct{}
	ready    chan struct{}
	once     sync.Once

	mu          sync.Mutex
	isReady     bool
	launches    int
	ensureCalls int
	releases    int
}

func (c *fakeController) EnsureReady(
	ctx context.Context,
	binding Binding,
	_ RequestFeatures,
) (Lease, error) {
	c.mu.Lock()
	c.ensureCalls++
	ready := c.isReady
	if !ready {
		c.launches++
		c.once.Do(func() { close(c.started) })
	}
	c.mu.Unlock()
	if !ready {
		select {
		case <-ctx.Done():
			return Lease{}, ctx.Err()
		case <-c.ready:
		}
		if binding.FencingToken > 0 {
			c.endpoint.BindingRevision = binding.Revision
			c.endpoint.RecipeRevision = binding.RecipeRevision
			c.endpoint.FencingToken = binding.FencingToken
		}
		if err := c.registry.Register(ctx, c.endpoint); err != nil {
			return Lease{}, err
		}
		c.mu.Lock()
		c.isReady = true
		c.mu.Unlock()
	}
	c.mu.Lock()
	id := fmt.Sprintf("lease-%d", c.ensureCalls)
	c.mu.Unlock()
	return Lease{ID: id, Endpoint: c.endpoint}, nil
}

func (c *fakeController) Release(context.Context, Lease, RequestOutcome) error {
	c.mu.Lock()
	c.releases++
	c.mu.Unlock()
	return nil
}

func (c *fakeController) Status(context.Context, Binding) (Status, error) {
	c.mu.Lock()
	ready := c.isReady
	c.mu.Unlock()
	if ready {
		endpoint := c.endpoint
		return Status{State: endpointregistry.StateReady, Endpoint: &endpoint, UpdatedAt: time.Now()}, nil
	}
	return Status{State: endpointregistry.StateActivating, UpdatedAt: time.Now()}, nil
}

func (c *fakeController) Stop(context.Context, Binding, StopReason) error { return nil }

type recordCollector struct {
	mu      sync.Mutex
	records []ledger.Record
}

func TestManualStartUsesBoundedAdmissionAndReleasesLease(t *testing.T) {
	registry := endpointregistry.NewMemory()
	controller := &fakeController{registry: registry, started: make(chan struct{}), ready: make(chan struct{}), endpoint: endpointregistry.Endpoint{
		ID: "endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1", Controller: "controller", Protocol: "openai",
		ServedModels: []string{"upstream"}, State: endpointregistry.StateReady, RegisteredAt: time.Now(), HeartbeatAt: time.Now(),
	}}
	target := activationTarget(1, 32)
	target.Binding.ColdStart = ColdStartReject
	coordinator, err := NewAdmissionCoordinator([]Target{target}, AdmissionOptions{
		Registry: registry, Controllers: map[string]Controller{"controller": controller},
		Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := coordinator.Start(ctx, "deployment"); done <- err }()
	select {
	case <-controller.started:
	case <-ctx.Done():
		t.Fatal("manual activation did not start")
	}
	if _, err := coordinator.Start(ctx, "deployment"); !errors.Is(err, ErrQueueFull) {
		t.Fatal("manual Start bypassed the admission queue limit", err)
	}
	close(controller.ready)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snapshot, err := coordinator.Snapshot(ctx)
	if err != nil || snapshot.Bindings[0].ActiveLeases != 0 || snapshot.Bindings[0].QueuedWaiters != 0 || controller.releases != 1 {
		t.Fatal("manual activation leaked admission resources", snapshot, err)
	}
}

func (c *recordCollector) Record(record ledger.Record) {
	c.mu.Lock()
	c.records = append(c.records, record)
	c.mu.Unlock()
}

func TestAdmissionCoordinatorCoalescesAndBoundsColdStart(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	controller := &fakeController{
		registry: registry,
		endpoint: endpointregistry.Endpoint{
			ID: "endpoint-1", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
			Controller: "controller", Protocol: "openai", ServedModels: []string{"upstream"},
			State: endpointregistry.StateReady, RegisteredAt: time.Now(), HeartbeatAt: time.Now(),
		},
		started: make(chan struct{}),
		ready:   make(chan struct{}),
	}
	recorder := &recordCollector{}
	coordinator, err := NewAdmissionCoordinator(
		[]Target{activationTarget(2, 32)},
		AdmissionOptions{
			Registry: registry, Controllers: map[string]Controller{"controller": controller},
			Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }),
			Recorder:   recorder, InstanceID: "gateway-a", PollInterval: 10 * time.Millisecond,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		lease *AdmissionLease
		err   error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			lease, acquireErr := coordinator.Acquire(context.Background(), AdmissionRequest{
				Deployment: "deployment", BodyBytes: 8,
				Features: RequestFeatures{VirtualModel: "virtual", Protocol: "openai"},
			})
			results <- result{lease: lease, err: acquireErr}
		}()
	}
	select {
	case <-controller.started:
	case <-time.After(time.Second):
		t.Fatal("activation did not start")
	}
	waitForQueuedWaiters(t, coordinator, 2)
	_, err = coordinator.Acquire(context.Background(), AdmissionRequest{
		Deployment: "deployment", BodyBytes: 1,
	})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("third Acquire() error = %v, want queue full", err)
	}
	close(controller.ready)
	leases := make([]*AdmissionLease, 0, 2)
	for range 2 {
		select {
		case acquired := <-results:
			if acquired.err != nil || acquired.lease == nil {
				t.Fatalf("Acquire() = %#v, %v", acquired.lease, acquired.err)
			}
			leases = append(leases, acquired.lease)
		case <-time.After(2 * time.Second):
			t.Fatal("Acquire() did not complete")
		}
	}
	controller.mu.Lock()
	launches, calls := controller.launches, controller.ensureCalls
	controller.mu.Unlock()
	if launches != 1 || calls != 2 {
		t.Fatalf("controller launches/calls = %d/%d, want 1/2", launches, calls)
	}
	for _, lease := range leases {
		if err := coordinator.Release(context.Background(), lease, RequestOutcome{Success: true}); err != nil {
			t.Fatal(err)
		}
		if err := coordinator.Release(context.Background(), lease, RequestOutcome{Success: true}); err != nil {
			t.Fatal(err)
		}
	}
	controller.mu.Lock()
	releases := controller.releases
	controller.mu.Unlock()
	if releases != 2 {
		t.Fatalf("controller releases = %d, want 2", releases)
	}
	snapshot, err := coordinator.Snapshot(context.Background())
	if err != nil || len(snapshot.Bindings) != 1 || snapshot.Bindings[0].ActiveLeases != 0 ||
		snapshot.Bindings[0].QueuedWaiters != 0 || snapshot.Bindings[0].State != endpointregistry.StateReady {
		t.Fatalf("Snapshot() = %#v, %v", snapshot, err)
	}
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.records) < 3 {
		t.Fatalf("runtime events = %d, want activation and rejection events", len(recorder.records))
	}
	for _, record := range recorder.records {
		if err := record.Validate(); err != nil {
			t.Fatalf("runtime event is invalid: %#v: %v", record, err)
		}
	}
}

func TestAdmissionCoordinatorBoundsQueuedBytes(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	controller := &fakeController{
		registry: registry,
		endpoint: endpointregistry.Endpoint{
			ID: "endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
			Controller: "controller", Protocol: "openai", State: endpointregistry.StateReady,
		},
		started: make(chan struct{}), ready: make(chan struct{}),
	}
	coordinator, err := NewAdmissionCoordinator([]Target{activationTarget(5, 10)}, AdmissionOptions{
		Registry: registry, Controllers: map[string]Controller{"controller": controller},
		Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }),
		InstanceID: "gateway", PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, acquireErr := coordinator.Acquire(ctx, AdmissionRequest{Deployment: "deployment", BodyBytes: 8})
		done <- acquireErr
	}()
	select {
	case <-controller.started:
	case <-time.After(time.Second):
		t.Fatal("activation did not start")
	}
	_, err = coordinator.Acquire(context.Background(), AdmissionRequest{Deployment: "deployment", BodyBytes: 3})
	if !errors.Is(err, ErrQueueBytes) {
		t.Fatalf("Acquire() error = %v, want queued-byte limit", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled acquisition did not return")
	}
}

func TestAdmissionCoordinatorRejectsColdAndUnauthorizedEndpoints(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	controller := &fakeController{
		registry: registry,
		endpoint: endpointregistry.Endpoint{
			ID: "endpoint", Target: "deployment", BaseURL: "http://169.254.169.254/v1",
			Controller: "controller", Protocol: "openai", State: endpointregistry.StateReady,
		},
		started: make(chan struct{}), ready: make(chan struct{}),
	}
	target := activationTarget(1, 10)
	target.Binding.ColdStart = ColdStartReject
	coordinator, err := NewAdmissionCoordinator([]Target{target}, AdmissionOptions{
		Registry: registry, Controllers: map[string]Controller{"controller": controller},
		Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error {
			return errors.New("private address denied")
		}),
		InstanceID: "gateway", PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = coordinator.Acquire(context.Background(), AdmissionRequest{Deployment: "deployment"})
	if !errors.Is(err, ErrColdStartRejected) {
		t.Fatalf("Acquire() error = %v, want cold-start rejection", err)
	}
	controller.mu.Lock()
	calls := controller.ensureCalls
	controller.mu.Unlock()
	if calls != 0 {
		t.Fatalf("controller calls = %d, want 0", calls)
	}
}

func TestAdmissionCoordinatorRejectsReadyEndpointWithoutControllerIdentity(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	if err := registry.Register(context.Background(), endpointregistry.Endpoint{
		ID: "endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
		Protocol: "openai", State: endpointregistry.StateReady,
	}); err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewAdmissionCoordinator([]Target{{
		Deployment: "deployment", UpstreamModel: "upstream", Protocol: "openai",
		Source: EndpointDiscovered, Controller: "sparkrun",
	}}, AdmissionOptions{
		Registry: registry,
		Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error {
			return nil
		}),
		InstanceID: "gateway", PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Acquire(
		context.Background(),
		AdmissionRequest{Deployment: "deployment"},
	); !errors.Is(err, ErrEndpointNotReady) {
		t.Fatalf("Acquire() error = %v, want endpoint not ready", err)
	}
}

func TestAdmissionCoordinatorRequiresExactRegistryLeaseIdentity(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	registered := endpointregistry.Endpoint{
		ID: "endpoint", Target: "deployment", BaseURL: "http://127.0.0.1:9000/v1",
		Controller: "controller", ClusterID: "registered-cluster", Protocol: "openai",
		ServedModels: []string{"upstream"}, BindingRevision: "revision",
		RecipeRevision: "recipe-revision", FencingToken: 1,
		State: endpointregistry.StateReady,
	}
	if err := registry.Register(context.Background(), registered); err != nil {
		t.Fatal(err)
	}
	returned := registered
	returned.ClusterID = "different-cluster"
	controller := &fakeController{
		registry: registry, endpoint: returned, isReady: true,
		started: make(chan struct{}), ready: make(chan struct{}),
	}
	coordinator, err := NewAdmissionCoordinator([]Target{activationTarget(1, 10)}, AdmissionOptions{
		Registry: registry, Controllers: map[string]Controller{"controller": controller},
		Authorizer: EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error {
			return nil
		}),
		InstanceID: "gateway", PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Acquire(
		context.Background(),
		AdmissionRequest{Deployment: "deployment"},
	); !errors.Is(err, ErrInvalidEndpoint) {
		t.Fatalf("Acquire() error = %v, want invalid endpoint", err)
	}
}

func activationTarget(maxWaiters int, maxBytes int64) Target {
	return Target{
		Deployment: "deployment", UpstreamModel: "upstream",
		Protocol: "openai", Source: EndpointActivatable, Controller: "controller",
		Binding: Binding{
			Controller: "controller", Revision: "revision", Deployment: "deployment",
			Recipe: "recipe", RecipeRevision: "recipe-revision",
			ActivationTimeout: time.Second, MaxQueuedWaiters: maxWaiters,
			MaxQueuedBodyBytes: maxBytes, ColdStart: ColdStartWait,
		},
	}
}

func waitForQueuedWaiters(t *testing.T, coordinator *AdmissionCoordinator, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := coordinator.Snapshot(context.Background())
		if err == nil && len(snapshot.Bindings) == 1 && snapshot.Bindings[0].QueuedWaiters == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queued waiters did not reach %d", want)
}
