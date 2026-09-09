// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"fmt"
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

type lifecycleFakeBridge struct {
	*fakeBridge
	lifecycleCalls chan string
	report         WorkloadReport
}

func (b *lifecycleFakeBridge) InspectWorkloads(context.Context) (WorkloadReport, error) {
	return b.report, nil
}
func (b *lifecycleFakeBridge) Lifecycle(_ context.Context, _ Binding, job, action string, _ time.Duration) (WorkloadInfo, error) {
	b.lifecycleCalls <- action
	return WorkloadInfo{JobID: job, Owned: true, PluginsInUse: []string{"coldsnap"}, LifecycleActions: []string{"status", "sleep", "wake"}, LifecycleState: "sleeping"}, nil
}
func TestIdleSleepWaitsForLastLeaseAndUsesPluginLifecycle(t *testing.T) {
	w := NewWorkloads()
	defer w.Close()
	target := activationTarget()
	target.Binding.IdleTTL = 10 * time.Millisecond
	target.Binding.IdleAction = "sleep"
	bridge := &lifecycleFakeBridge{fakeBridge: &fakeBridge{stopped: make(chan string, 1)}, lifecycleCalls: make(chan string, 1)}
	w.observe(target.Binding, Endpoint{ClusterID: "job", Owned: true}, bridge, time.Second, true)
	select {
	case <-bridge.lifecycleCalls:
		t.Fatal("slept with active lease")
	case <-time.After(30 * time.Millisecond):
	}
	w.release("job")
	select {
	case action := <-bridge.lifecycleCalls:
		if action != "sleep" {
			t.Fatal(action)
		}
	case <-time.After(time.Second):
		t.Fatal("idle sleep not called")
	}
	select {
	case <-bridge.stopped:
		t.Fatal("sleep called stop")
	default:
	}
}
func TestManualSleepRejectsActiveAndUnownedWorkloads(t *testing.T) {
	target := activationTarget()
	bridge := &lifecycleFakeBridge{fakeBridge: &fakeBridge{}, lifecycleCalls: make(chan string, 1), report: WorkloadReport{Workloads: []WorkloadInfo{{JobID: "job", ClusterName: "spark-a", RecipeRevision: target.Binding.RecipeRevision, Owned: true, LifecycleActions: []string{"sleep"}}}}}
	w := NewWorkloads()
	defer w.Close()
	c, err := New(Options{Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target}, Workloads: w})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	w.observe(target.Binding, Endpoint{ClusterID: "job", Owned: true}, bridge, time.Second, true)
	if _, err := c.WorkloadAction(context.Background(), target.Deployment, "job", "sleep"); err == nil {
		t.Fatal("slept active workload")
	}
	w.release("job")
	bridge.report.Workloads[0].Owned = false
	if _, err := c.WorkloadAction(context.Background(), target.Deployment, "job", "sleep"); err == nil {
		t.Fatal("slept unowned workload")
	}
	select {
	case <-bridge.lifecycleCalls:
		t.Fatal("rejected control called bridge")
	default:
	}
}

type uncertainStopBridge struct{ *fakeBridge }

func (b *uncertainStopBridge) Stop(context.Context, Binding, string) (StopResult, error) {
	return StopResult{}, fmt.Errorf("reply lost after stop")
}
func TestUncertainIdleStopBlocksCachedAdmission(t *testing.T) {
	w := NewWorkloads()
	defer w.Close()
	target := activationTarget()
	target.Binding.IdleTTL = 10 * time.Millisecond
	bridge := &uncertainStopBridge{&fakeBridge{}}
	w.observe(target.Binding, Endpoint{ClusterID: "job", Owned: true}, bridge, time.Second, false)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if w.phase("job") == "unknown" {
			if w.available("job") {
				t.Fatal("uncertain stop restored cached admission")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("uncertain stop was not fenced")
}
