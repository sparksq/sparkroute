// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"testing"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

func manualControlFixture(t *testing.T, owned bool) (WorkloadControl, *lifecycleFakeBridge, lifecycle.Target) {
	t.Helper()
	target := activationTarget()
	target.Binding.ClusterCandidates = []string{"spark-a"}
	target.Binding.ColdStart = lifecycle.ColdStartReject
	target.Binding.MaxQueuedWaiters = 100
	target.Binding.MaxQueuedBodyBytes = 1 << 20
	endpoint := &Endpoint{State: "ready", Owned: owned, ClusterName: "spark-a", ClusterID: "job", JobID: "job",
		Host: "127.0.0.1", Port: 8000, Protocol: "openai", ServedModels: []string{target.UpstreamModel}, RecipeRevision: target.Binding.RecipeRevision}
	bridge := &lifecycleFakeBridge{fakeBridge: &fakeBridge{ensured: EnsureResult{State: "ready", Endpoint: endpoint}},
		report: WorkloadReport{Workloads: []WorkloadInfo{{JobID: "job", Owned: owned, ClusterName: "spark-a", RecipeRevision: target.Binding.RecipeRevision, LifecycleState: "running"}}}}
	registry := endpointregistry.NewMemory()
	controller, err := New(Options{Bridge: bridge, Registry: registry, Targets: []lifecycle.Target{target}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(controller.Close)
	admission, err := lifecycle.NewAdmissionCoordinator([]lifecycle.Target{target}, lifecycle.AdmissionOptions{
		Registry: registry, Authorizer: controller, Controllers: map[string]lifecycle.Controller{ControllerName: controller},
	})
	if err != nil {
		t.Fatal(err)
	}
	return WorkloadControl{Controller: controller, Admission: admission}, bridge, target
}

func TestManualStartStopAndRestartWithoutColdSnap(t *testing.T) {
	controls, bridge, target := manualControlFixture(t, true)
	ctx := context.Background()
	// Inference rejects a cold model; an explicit Start can prewarm it.
	if _, err := controls.Admission.Acquire(ctx, lifecycle.AdmissionRequest{Deployment: target.Deployment}); err == nil {
		t.Fatal("cold-start rejection policy was ignored")
	}
	started, err := controls.WorkloadAction(ctx, target.Deployment, "", "start")
	if err != nil || started.State != "ready" || started.JobID != "job" || len(started.PluginsInUse) != 0 {
		t.Fatal(started, err)
	}
	snapshot, err := controls.Admission.Snapshot(ctx)
	if err != nil || snapshot.Bindings[0].ActiveLeases != 0 || snapshot.Bindings[0].State != endpointregistry.StateReady {
		t.Fatal("Start leaked its readiness lease", snapshot, err)
	}
	lease, err := controls.Admission.Acquire(ctx, lifecycle.AdmissionRequest{Deployment: target.Deployment})
	if err != nil || lease.Endpoint.FencingToken <= 0 {
		t.Fatal("Start did not register a fenced endpoint", lease, err)
	}
	firstFence := lease.Endpoint.FencingToken
	if _, err := controls.WorkloadAction(ctx, target.Deployment, "job", "stop"); err == nil || bridge.stopCalls != 0 {
		t.Fatal("stopped a workload with an active request", err)
	}
	if err := controls.Admission.Release(ctx, lease, lifecycle.RequestOutcome{Success: true}); err != nil {
		t.Fatal(err)
	}
	stopped, err := controls.WorkloadAction(ctx, target.Deployment, "job", "stop")
	if err != nil || stopped.State != "offline" || bridge.stopCalls != 1 {
		t.Fatal(stopped, err)
	}
	status, err := controls.Controller.Status(ctx, target.Binding)
	if err != nil || status.State != endpointregistry.StateOffline || status.Endpoint != nil {
		t.Fatal("Stop left a serving endpoint", status, err)
	}
	snapshot, err = controls.Admission.Snapshot(ctx)
	if err != nil || snapshot.Bindings[0].State != endpointregistry.StateOffline || snapshot.Bindings[0].Phase != "offline" {
		t.Fatal("runtime status did not expose the stopped binding", snapshot, err)
	}
	if err := controls.Controller.AuthorizeEndpoint(ctx, lease.Endpoint); err == nil {
		t.Fatal("stopped endpoint still admitted requests")
	}
	if _, err := controls.WorkloadAction(ctx, target.Deployment, "", "start"); err != nil {
		t.Fatal("could not restart stopped workload", err)
	}
	lease, err = controls.Admission.Acquire(ctx, lifecycle.AdmissionRequest{Deployment: target.Deployment})
	if err != nil || lease.Endpoint.FencingToken <= firstFence || bridge.ensureCalls != 2 {
		t.Fatal("restart did not obtain a fresh activation", lease, bridge.ensureCalls, err)
	}
	_ = controls.Admission.Release(ctx, lease, lifecycle.RequestOutcome{Success: true})
}

func TestManualStopRejectsUnownedAndMismatchedReceipts(t *testing.T) {
	for _, mismatch := range []string{"unowned", "job", "recipe", "cluster"} {
		t.Run(mismatch, func(t *testing.T) {
			controls, bridge, target := manualControlFixture(t, true)
			switch mismatch {
			case "unowned":
				bridge.report.Workloads[0].Owned = false
			case "job":
				bridge.report.Workloads[0].JobID = "another-job"
			case "recipe":
				bridge.report.Workloads[0].RecipeRevision = "another-recipe"
			case "cluster":
				bridge.report.Workloads[0].ClusterName = "another-cluster"
			}
			if _, err := controls.WorkloadAction(context.Background(), target.Deployment, "job", "stop"); err == nil || bridge.stopCalls != 0 {
				t.Fatal("stopped an unauthorized workload", err)
			}
		})
	}
}

func TestManualStartRequiresConfiguredActivationBinding(t *testing.T) {
	controls, bridge, target := manualControlFixture(t, true)
	if _, err := controls.WorkloadAction(context.Background(), "unknown", "", "start"); err == nil {
		t.Fatal("started an unknown deployment")
	}
	controls.Controller.targets[target.Deployment] = lifecycle.Target{Source: lifecycle.EndpointDiscovered}
	if _, err := controls.WorkloadAction(context.Background(), target.Deployment, "", "start"); err == nil || bridge.ensureCalls != 0 {
		t.Fatal("started a deployment without an activation binding", err)
	}
}
