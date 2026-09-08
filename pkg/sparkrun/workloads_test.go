package sparkrun

import (
	"context"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

func TestLeasesSurviveConfigurationGenerationRetirement(t *testing.T) {
	shared := NewWorkloads()
	defer shared.Close()
	target := activationTarget()
	target.Binding.IdleTTL = 30 * time.Millisecond
	endpoint := Endpoint{State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "physical-job", JobID: "physical-job", Host: "127.0.0.1", Port: 8001, Protocol: "openai", ServedModels: []string{target.UpstreamModel}, RecipeRevision: target.Binding.RecipeRevision}
	bridge := &fakeBridge{ensured: EnsureResult{State: "ready", Endpoint: &endpoint}, stopped: make(chan string, 2)}
	makeController := func() *Controller {
		c, err := New(Options{Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target}, Workloads: shared})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	old, next := makeController(), makeController()
	defer next.Close()
	binding := target.Binding
	binding.FencingToken = 1
	one, err := old.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := next.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
	if err != nil {
		t.Fatal(err)
	}
	_ = old.Release(context.Background(), one, lifecycle.RequestOutcome{})
	old.Close()
	select {
	case <-bridge.stopped:
		t.Fatal("stopped during a request in the replacement generation")
	case <-time.After(60 * time.Millisecond):
	}
	_ = next.Release(context.Background(), two, lifecycle.RequestOutcome{})
	select {
	case <-bridge.stopped:
	case <-time.After(time.Second):
		t.Fatal("idle timer did not survive retirement")
	}
}

func TestBorrowedWorkloadNeverGetsAnIdleStop(t *testing.T) {
	w := NewWorkloads()
	defer w.Close()
	target := activationTarget()
	target.Binding.IdleTTL = time.Millisecond
	bridge := &fakeBridge{stopped: make(chan string, 1)}
	w.observe(target.Binding, Endpoint{ClusterID: "borrowed", Owned: false}, bridge, time.Second, true)
	w.release("borrowed")
	select {
	case <-bridge.stopped:
		t.Fatal("borrowed workload was stopped")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestActivationLocksArePerRecipeAndRespectCancellation(t *testing.T) {
	w := NewWorkloads()
	defer w.Close()
	binding := activationTarget().Binding
	unlock, err := w.lock(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	other := binding
	other.RecipeRevision = "other-model"
	second, err := w.lock(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	second()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.lock(ctx, binding); err == nil {
		t.Fatal("cancelled waiter acquired occupied lock")
	}
}

func TestWrongOrUnknownClusterCannotBeAdopted(t *testing.T) {
	target := activationTarget()
	target.Binding.ClusterCandidates = []string{"lab"}
	for _, cluster := range []string{"other", ""} {
		endpoint := Endpoint{State: "ready", ClusterName: cluster, Protocol: target.Protocol, RecipeRevision: target.Binding.RecipeRevision, ServedModels: []string{target.UpstreamModel}}
		if matchesTarget(target, endpoint) {
			t.Fatalf("adopted cluster %q", cluster)
		}
	}
}
