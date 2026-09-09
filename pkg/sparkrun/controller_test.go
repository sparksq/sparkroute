// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"errors"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

type fakeBridge struct {
	mu sync.Mutex

	capabilities Capabilities
	discovered   DiscoverResult
	ensured      EnsureResult
	ensureErr    error
	ensureCalls  int
	stopCalls    int
	stopped      chan string
}

func (b *fakeBridge) Capabilities(context.Context) (Capabilities, error) {
	return b.capabilities, nil
}

func (b *fakeBridge) EnsureReady(
	context.Context,
	Binding,
	time.Duration,
) (EnsureResult, error) {
	b.mu.Lock()
	b.ensureCalls++
	b.mu.Unlock()
	return b.ensured, b.ensureErr
}

func (b *fakeBridge) Discover(context.Context, *Binding) (DiscoverResult, error) {
	return b.discovered, nil
}

func (b *fakeBridge) Stop(
	_ context.Context,
	_ Binding,
	clusterID string,
) (StopResult, error) {
	b.mu.Lock()
	b.stopCalls++
	b.mu.Unlock()
	if b.stopped != nil {
		b.stopped <- clusterID
	}
	return StopResult{State: "offline", ClusterIDs: []string{clusterID}}, nil
}

func TestControllerActivatesRegistersAndUsesLocalFastPath(t *testing.T) {
	t.Parallel()

	registry := endpointregistry.NewMemory()
	bridge := &fakeBridge{
		ensured: EnsureResult{State: "ready", Endpoint: &Endpoint{
			State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "sparkrun_aaaaaaaaaaaa_bbbbbbbb",
			JobID: "sparkrun_aaaaaaaaaaaa_bbbbbbbb", Host: "10.0.0.2", Port: 8000,
			Protocol: "openai", ServedModels: []string{"Qwen/Qwen3-32B"},
			RecipeRevision: "abc123abc123", Runtime: "vllm",
		}},
	}
	target := activationTarget()
	controller, err := New(Options{
		Bridge: bridge, Registry: registry, Targets: []lifecycle.Target{target},
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := target.Binding
	binding.FencingToken = 7
	first, err := controller.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	secondBinding := binding
	secondBinding.FencingToken = 0
	second, err := controller.EnsureReady(context.Background(), secondBinding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	bridge.mu.Lock()
	ensureCalls := bridge.ensureCalls
	bridge.mu.Unlock()
	if ensureCalls != 1 {
		t.Fatalf("bridge EnsureReady calls = %d, want 1", ensureCalls)
	}
	if first.Endpoint.ID != second.Endpoint.ID || first.Endpoint.FencingToken != 7 ||
		first.Endpoint.BaseURL != "http://10.0.0.2:8000/v1" {
		t.Fatalf("leases = %#v / %#v", first, second)
	}
	ready, err := registry.Ready(context.Background(), target.Deployment)
	if err != nil || len(ready) != 1 || ready[0].ID != first.Endpoint.ID {
		t.Fatalf("Ready() = %#v, %v", ready, err)
	}
	if err := controller.AuthorizeEndpoint(context.Background(), ready[0]); err != nil {
		t.Fatalf("AuthorizeEndpoint() error = %v", err)
	}
	tampered := ready[0]
	tampered.BaseURL = "http://169.254.169.254/v1"
	if err := controller.AuthorizeEndpoint(context.Background(), tampered); err == nil {
		t.Fatal("tampered endpoint was authorized")
	}
	if err := controller.Release(context.Background(), first, lifecycle.RequestOutcome{Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := controller.Release(context.Background(), second, lifecycle.RequestOutcome{Success: true}); err != nil {
		t.Fatal(err)
	}
}

func TestControllerRejectsBridgeEndpointIdentity(t *testing.T) {
	t.Parallel()

	target := activationTarget()
	bridge := &fakeBridge{ensured: EnsureResult{State: "ready", Endpoint: &Endpoint{
		State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "cluster", JobID: "job", Host: "host/path", Port: 8000,
		Protocol: "openai", ServedModels: []string{target.UpstreamModel},
		RecipeRevision: target.Binding.RecipeRevision,
	}}}
	controller, err := New(Options{
		Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target},
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := target.Binding
	binding.FencingToken = 1
	if _, err := controller.EnsureReady(
		context.Background(), binding, lifecycle.RequestFeatures{},
	); err == nil {
		t.Fatal("invalid bridge endpoint was accepted")
	}
}

func TestControllerPublishesAndWithdrawsDiscoveredModelMetadata(t *testing.T) {
	t.Parallel()

	contextLimit := 65_536
	size := 32.5
	price := 0.25
	target := lifecycle.Target{
		Deployment: "qwen", UpstreamModel: "Qwen/Qwen3-32B",
		LogicalModels: []string{"local-chat"}, Protocol: "openai",
		Source: lifecycle.EndpointDiscovered, Controller: ControllerName,
	}
	bridge := &fakeBridge{discovered: DiscoverResult{Endpoints: []Endpoint{{
		State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "cluster", JobID: "job", Host: "10.0.0.2", Port: 8000,
		Protocol: "openai", ServedModels: []string{target.UpstreamModel}, Runtime: "vllm",
		ModelMetadata: map[string]ModelMetadata{
			target.UpstreamModel: {
				SizeB: &size, InputPrice: &price, Context: &contextLimit,
				Tags: []string{"local", "vllm"},
			},
		},
	}}}}
	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 3, DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "smallest", Models: []string{"local-chat"}},
		},
		Models: map[string]modelrouter.ModelMetadata{"local-chat": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(Options{
		Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target},
		MetadataPublisher: router, MetadataSource: "sparkrun:test-generation",
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := router.DiscoveredMetadata()
	metadata := state.Effective["local-chat"]
	if len(state.Sources) != 1 || metadata.SizeB == nil || *metadata.SizeB != size ||
		metadata.InputPrice == nil || *metadata.InputPrice != price ||
		metadata.Context == nil || *metadata.Context != contextLimit ||
		len(metadata.Tags) != 2 {
		t.Fatalf("discovered metadata state = %#v", state)
	}
	if policy := router.Policy(); policy.Revision != 3 || policy.Models["local-chat"].SizeB != 0 {
		t.Fatalf("discovery mutated policy = %#v", policy)
	}
	bridge.discovered = DiscoverResult{}
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state := router.DiscoveredMetadata(); len(state.Effective) != 0 || len(state.Sources) != 1 {
		t.Fatalf("missing endpoint metadata was retained = %#v", state)
	}
	controller.Close()
	if state := router.DiscoveredMetadata(); len(state.Sources) != 0 {
		t.Fatalf("closed controller source was retained = %#v", state)
	}
}

func TestControllerIgnoresInvalidDiscoveredModelMetadata(t *testing.T) {
	t.Parallel()

	target := lifecycle.Target{
		Deployment: "qwen", UpstreamModel: "Qwen/Qwen3-32B",
		LogicalModels: []string{"local-chat"}, Protocol: "openai",
		Source: lifecycle.EndpointDiscovered, Controller: ControllerName,
	}
	invalidContext := -1
	bridge := &fakeBridge{discovered: DiscoverResult{Endpoints: []Endpoint{{
		State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "cluster", JobID: "job", Host: "10.0.0.2", Port: 8000,
		Protocol: "openai", ServedModels: []string{target.UpstreamModel},
		ModelMetadata: map[string]ModelMetadata{
			target.UpstreamModel: {Context: &invalidContext},
		},
	}}}}
	registry := endpointregistry.NewMemory()
	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 1, DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "smallest", Models: []string{"local-chat"}},
		},
		Models: map[string]modelrouter.ModelMetadata{"local-chat": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := New(Options{
		Bridge: bridge, Registry: registry, Targets: []lifecycle.Target{target},
		MetadataPublisher: router,
		EndpointTTL:       time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready, err := registry.Ready(context.Background(), target.Deployment)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 {
		t.Fatalf("valid endpoint was rejected with its invalid metadata: %#v", ready)
	}
	if state := router.DiscoveredMetadata(); len(state.Effective) != 0 {
		t.Fatalf("invalid metadata was published: %#v", state)
	}
}

func TestControllerStopsAfterIdleTTL(t *testing.T) {
	t.Parallel()

	target := activationTarget()
	target.Binding.IdleTTL = 20 * time.Millisecond
	bridge := &fakeBridge{
		ensured: EnsureResult{State: "ready", Endpoint: &Endpoint{
			State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "cluster-id", JobID: "job-id", Host: "127.0.0.1", Port: 8000,
			Protocol: "openai", ServedModels: []string{target.UpstreamModel},
			RecipeRevision: target.Binding.RecipeRevision,
		}},
		stopped: make(chan string, 1),
	}
	controller, err := New(Options{
		Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target},
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second, StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := target.Binding
	binding.FencingToken = 1
	lease, err := controller.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Release(context.Background(), lease, lifecycle.RequestOutcome{Success: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case clusterID := <-bridge.stopped:
		if clusterID != "cluster-id" {
			t.Fatalf("stopped cluster = %q", clusterID)
		}
	case <-time.After(time.Second):
		t.Fatal("idle stop did not run")
	}
}

func TestControllerReconcilesDiscoveredAndPinnedEndpoints(t *testing.T) {
	t.Parallel()

	discoveredTarget := lifecycle.Target{
		Deployment: "discovered", UpstreamModel: "embed-model", Protocol: "openai",
		Source: lifecycle.EndpointDiscovered, Controller: ControllerName,
	}
	activation := activationTarget()
	activationEndpoint := Endpoint{
		State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "llm-cluster", JobID: "llm-job", Host: "spark-b", Port: 8002,
		Protocol: "openai", ServedModels: []string{activation.UpstreamModel},
		RecipeRevision: activation.Binding.RecipeRevision,
	}
	bridge := &fakeBridge{
		ensured: EnsureResult{State: "ready", Endpoint: &activationEndpoint},
		discovered: DiscoverResult{Endpoints: []Endpoint{
			{
				State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "embed-cluster", JobID: "embed-job", Host: "spark-a", Port: 8001,
				Protocol: "openai", ServedModels: []string{"embed-model"}, RecipeRevision: "other-revision",
			},
			activationEndpoint,
		}}}
	registry := endpointregistry.NewMemory()
	controller, err := New(Options{
		Bridge: bridge, Registry: registry, Targets: []lifecycle.Target{discoveredTarget, activation},
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := activation.Binding
	binding.FencingToken = 1
	lease, err := controller.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Release(context.Background(), lease, lifecycle.RequestOutcome{Success: true}); err != nil {
		t.Fatal(err)
	}
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, target := range []lifecycle.Target{discoveredTarget, activation} {
		ready, err := registry.Ready(context.Background(), target.Deployment)
		if err != nil || len(ready) != 1 {
			t.Fatalf("Ready(%q) = %#v, %v", target.Deployment, ready, err)
		}
		if err := controller.AuthorizeEndpoint(context.Background(), ready[0]); err != nil {
			t.Fatal(err)
		}
	}
	bridge.discovered.Endpoints = bridge.discovered.Endpoints[:1]
	if err := controller.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ready, err := registry.Ready(context.Background(), activation.Deployment)
	if err != nil || len(ready) != 0 {
		t.Fatalf("Ready(%q) after disappearance = %#v, %v", activation.Deployment, ready, err)
	}
	status, err := controller.Status(context.Background(), activation.Binding)
	if err != nil || status.State != endpointregistry.StateOffline || status.Endpoint != nil {
		t.Fatalf("Status() after disappearance = %#v, %v", status, err)
	}
}

func TestControllerCapabilityNegotiation(t *testing.T) {
	t.Parallel()

	target := activationTarget()
	bridge := &fakeBridge{capabilities: Capabilities{
		ProtocolVersion: ProtocolVersion,
		Operations:      []string{"discover", "ensure_ready", "stop"},
	}}
	controller, err := New(Options{
		Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target},
		EndpointTTL: time.Minute, ReconcileInterval: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.CheckCapabilities(context.Background()); err != nil {
		t.Fatal(err)
	}
	bridge.capabilities.Operations = []string{"discover", "stop"}
	if err := controller.CheckCapabilities(context.Background()); err == nil {
		t.Fatal("missing operation was accepted")
	}
}

func TestBridgeErrorDoesNotExposeMessage(t *testing.T) {
	t.Parallel()
	err := &BridgeError{Code: "activation_failed", Message: "secret command output"}
	if errors.Is(err, context.Canceled) || err.Error() != "sparkrun bridge operation failed: activation_failed" {
		t.Fatalf("Error() = %q", err.Error())
	}
}

func TestClientCommandIntegration(t *testing.T) {
	command := os.Getenv("SPARKRUN_BRIDGE_COMMAND")
	if command == "" {
		t.Skip("SPARKRUN_BRIDGE_COMMAND is not set")
	}
	client, err := NewClient(command)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities.ProtocolVersion != ProtocolVersion ||
		!slices.Contains(capabilities.Operations, "ensure_ready") {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}

func activationTarget() lifecycle.Target {
	return lifecycle.Target{
		Deployment: "qwen", UpstreamModel: "Qwen/Qwen3-32B", Protocol: "openai",
		Source: lifecycle.EndpointActivatable, Controller: ControllerName,
		Binding: lifecycle.Binding{
			Controller: ControllerName, Revision: "binding-v1", Deployment: "qwen",
			Recipe: "@local/qwen", RecipeRevision: "abc123abc123",
			ActivationTimeout: time.Minute, IdleTTL: time.Minute,
		},
	}
}

func TestClusterNameIsAdvisoryAndDoesNotChangeEndpointIdentity(t *testing.T) {
	target := activationTarget()
	controller, err := New(Options{Bridge: &fakeBridge{}, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	original := Endpoint{State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "opaque-job", JobID: "opaque-job", Host: "127.0.0.1", Port: 8000, Protocol: "openai", ServedModels: []string{target.UpstreamModel}, RecipeRevision: target.Binding.RecipeRevision}
	before, _, err := controller.endpointFromBridge(target, original, 7)
	if err != nil {
		t.Fatal(err)
	}
	original.ClusterName = "spark-a"
	after, _, err := controller.endpointFromBridge(target, original, 7)
	if err != nil {
		t.Fatal(err)
	}
	if after.Metadata["cluster_name"] != "spark-a" || before.ID != after.ID || before.ClusterID != after.ClusterID || before.BindingRevision != after.BindingRevision || before.FencingToken != after.FencingToken {
		t.Fatal("display metadata changed identity or fencing")
	}
	original.ClusterName = "bad\nlabel"
	invalid, _, err := controller.endpointFromBridge(target, original, 7)
	if err != nil || invalid.Metadata["cluster_name"] != "" {
		t.Fatal("invalid display metadata affected an otherwise valid endpoint")
	}
}
