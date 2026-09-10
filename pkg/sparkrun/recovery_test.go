// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

func recoveryFixture(t *testing.T, action string) (*Workloads, lifecycle.Target, *fakeBridge) {
	t.Helper()
	w := NewWorkloads()
	target := activationTarget()
	target.Binding.IdleTTL = 0
	target.Binding.MaxQueuedWaiters = 10
	target.Binding.MaxQueuedBodyBytes = 1 << 20
	target.Binding.ClusterCandidates = []string{"spark-a"}
	target.Binding.Recovery = config.RecoveryPolicy{Action: action, FailedProbes: 2, UnhealthyFor: config.Duration(time.Minute), DrainTimeout: config.Duration(time.Second), Backoff: config.Duration(time.Minute), MaxBackoff: config.Duration(4 * time.Minute), MaxRestarts: 3}
	endpoint := Endpoint{State: "ready", Owned: true, ClusterName: "spark-a", ClusterID: "replacement", JobID: "replacement", Host: "127.0.0.1", Port: 8000, Protocol: "openai", RecipeRevision: target.Binding.RecipeRevision, ServedModels: []string{target.UpstreamModel}}
	bridge := &fakeBridge{ensured: EnsureResult{State: "ready", Endpoint: &endpoint}}
	w.Configure([]lifecycle.Target{target})
	initial := endpoint
	initial.ClusterID, initial.JobID = "original", "original"
	w.observe(target.Binding, initial, bridge, time.Second, false)
	return w, target, bridge
}

func failedProbe(w *Workloads, id string, halfOpen bool) {
	w.acquire(id)
	w.release(id, lifecycle.RequestOutcome{HealthObserved: true, CircuitOpened: true, HalfOpenProbe: halfOpen, BackendFailure: true, Error: "upstream_http_500"})
}

func tripRecovery(w *Workloads, id string) {
	failedProbe(w, id, false)
	failedProbe(w, id, true)
	failedProbe(w, id, true)
}

func TestRecoveryRequiresBothFailedProbesAndUnhealthyDuration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		failedProbe(w, "original", false)
		time.Sleep(2 * time.Minute)
		if recoveryStops(bridge) != 0 {
			t.Fatal("initial circuit opening restarted workload")
		}
		failedProbe(w, "original", true)
		synctest.Wait()
		if recoveryStops(bridge) != 0 {
			t.Fatal("insufficient failed probes restarted workload")
		}
		failedProbe(w, "original", true)
		synctest.Wait()
		if recoveryStops(bridge) != 1 || recoveryStarts(bridge) != 1 {
			t.Fatalf("stop/start = %d/%d", recoveryStops(bridge), recoveryStarts(bridge))
		}
		if w.available("original") || !w.available("replacement") {
			t.Fatal("replacement did not fence old process")
		}
		if s := w.recoveryStatus(target.Binding); s.Attempts != 1 || s.Phase != "healthy" {
			t.Fatalf("status = %+v", s)
		}
	})
}

func TestRecoveryProbesDoNotResetDeadlineAndOverrideIdleSleep(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		target.Binding.IdleTTL = 30 * time.Second
		target.Binding.IdleAction = "sleep"
		w.Configure([]lifecycle.Target{target})
		tripRecovery(w, "original")
		time.Sleep(29 * time.Second)
		failedProbe(w, "original", true)
		time.Sleep(29 * time.Second)
		failedProbe(w, "original", true)
		if recoveryStops(bridge) != 0 {
			t.Fatal("recovered before unhealthy duration")
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if recoveryStops(bridge) != 1 || recoveryStarts(bridge) != 1 {
			t.Fatal("probe activity postponed recovery, or idle sleep won")
		}
	})
}

func TestSuccessfulProbeCancelsScheduledRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, _, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		tripRecovery(w, "original")
		w.acquire("original")
		w.release("original", lifecycle.RequestOutcome{HealthObserved: true, Success: true})
		time.Sleep(2 * time.Minute)
		if recoveryStops(bridge) != 0 {
			t.Fatal("restarted a recovered workload")
		}
	})
}

func TestRecoveryIsOptInOwnedAndIgnoresNonBackendFailures(t *testing.T) {
	for _, mode := range []string{"disabled", "borrowed", "overload", "stale"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, target, bridge := recoveryFixture(t, "restart")
				defer w.Close()
				if mode == "disabled" {
					target.Binding.Recovery = config.RecoveryPolicy{}
					w.Configure([]lifecycle.Target{target})
				}
				if mode == "borrowed" {
					w.jobs["original"].owned = false
				}
				for i := 0; i < 5; i++ {
					w.acquire("original")
					w.release("original", lifecycle.RequestOutcome{HealthObserved: mode != "stale", CircuitOpened: true, HalfOpenProbe: true, BackendFailure: mode != "overload"})
				}
				time.Sleep(2 * time.Minute)
				if recoveryStops(bridge) != 0 {
					t.Fatal("ineligible workload was restarted")
				}
			})
		})
	}
}

func TestRecoveryDrainsLeasesAcrossGenerationsAndTimesOutSafely(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "drained", true: "timeout"}[timeout], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, target, bridge := recoveryFixture(t, "restart")
				defer w.Close()
				// A lease held by an alias or retiring generation is still physical.
				w.acquire("original")
				tripRecovery(w, "original")
				w.Configure([]lifecycle.Target{target})
				time.Sleep(time.Minute)
				synctest.Wait()
				if recoveryStops(bridge) != 0 || w.available("original") {
					t.Fatal("did not fence and drain active workload")
				}
				if timeout {
					time.Sleep(2 * time.Second)
					if s := w.recoveryStatus(target.Binding); s.Reason != "recovery_drain_timeout" {
						t.Fatalf("status = %+v", s)
					}
					if recoveryStops(bridge) != 0 || w.recoveryAdmission(target.Binding) == nil {
						t.Fatal("drain timeout did not fail closed")
					}
				} else {
					w.release("original")
					time.Sleep(20 * time.Millisecond)
					synctest.Wait()
					if recoveryStops(bridge) != 1 {
						t.Fatal("did not restart after drain")
					}
				}
			})
		})
	}
}

func TestRecoveryStopDefersActivationUntilDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "stop")
		defer w.Close()
		tripRecovery(w, "original")
		time.Sleep(time.Minute)
		synctest.Wait()
		if recoveryStops(bridge) != 1 || recoveryStarts(bridge) != 0 {
			t.Fatal("stop recovery launched a replacement")
		}
		if s := w.recoveryStatus(target.Binding); s.Phase != "awaiting_demand" {
			t.Fatalf("status = %+v", s)
		}
		controller, err := New(Options{Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target}, Workloads: w})
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Close()
		binding := target.Binding
		binding.FencingToken = 2
		lease, err := controller.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{})
		if err != nil {
			t.Fatal(err)
		}
		if lease.Endpoint.ClusterID != "replacement" || recoveryStarts(bridge) != 1 {
			t.Fatal("demand did not activate a replacement")
		}
		_ = controller.Release(context.Background(), lease, lifecycle.RequestOutcome{Success: true})
	})
}

func TestRecoveryStartFailuresBackOffAndExhaustBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		bridge.ensureErr = errors.New("unavailable")
		tripRecovery(w, "original")
		time.Sleep(time.Minute)
		synctest.Wait()
		if recoveryStarts(bridge) != 1 {
			t.Fatal("first start missing")
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if recoveryStarts(bridge) != 2 {
			t.Fatal("first backoff incorrect")
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if recoveryStarts(bridge) != 3 || recoveryStops(bridge) != 1 {
			t.Fatal("restarted the stopped job again, or retry count wrong")
		}
		if s := w.recoveryStatus(target.Binding); s.Phase != "exhausted" || s.Attempts != 3 {
			t.Fatalf("status = %+v", s)
		}
		time.Sleep(time.Hour)
		if recoveryStarts(bridge) != 3 || w.recoveryAdmission(target.Binding) == nil {
			t.Fatal("exhausted recovery admitted traffic")
		}
		registry := endpointregistry.NewMemory()
		controller, err := New(Options{Bridge: bridge, Registry: registry, Targets: []lifecycle.Target{target}, Workloads: w})
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Close()
		status, _ := controller.Status(context.Background(), target.Binding)
		if status.Recovery == nil || status.Recovery.Phase != "exhausted" || status.State != endpointregistry.StateFailed {
			t.Fatal("replacement generation lost recovery status", status)
		}
		coordinator, err := lifecycle.NewAdmissionCoordinator([]lifecycle.Target{target}, lifecycle.AdmissionOptions{Registry: registry, Inspector: registry, Controllers: map[string]lifecycle.Controller{ControllerName: controller}, Authorizer: controller})
		if err != nil {
			t.Fatal(err)
		}
		bridge.ensureErr = nil
		endpoint, err := coordinator.Start(context.Background(), target.Deployment)
		if err != nil || endpoint.JobID != "replacement" {
			t.Fatal("manual Start did not retry normal activation", endpoint, err)
		}
		if w.recoveryAdmission(target.Binding) != nil || w.recoveryStatus(target.Binding).Attempts != 0 {
			t.Fatal("manual Start did not reset budget")
		}
	})
}

func TestRecoveryBudgetSurvivesReplacementAndIgnoresOldResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		target.Binding.Recovery.MaxRestarts = 1
		w.Configure([]lifecycle.Target{target})
		tripRecovery(w, "original")
		time.Sleep(time.Minute)
		synctest.Wait()
		tripRecovery(w, "replacement")
		if s := w.recoveryStatus(target.Binding); s.Phase != "exhausted" || s.Attempts != 1 {
			t.Fatalf("status = %+v", s)
		}
		// A delayed successful completion cannot resurrect a retired job.
		w.acquire("original")
		w.release("original", lifecycle.RequestOutcome{HealthObserved: true, Success: true})
		if w.recoveryStatus(target.Binding).Phase != "exhausted" || recoveryStops(bridge) != 1 {
			t.Fatal("old result cleared replacement recovery")
		}
	})
}

func TestRecoveryCancelledByRemovalOrPolicyChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, _, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		tripRecovery(w, "original")
		w.Configure(nil)
		time.Sleep(2 * time.Minute)
		if recoveryStops(bridge) != 0 {
			t.Fatal("removed deployment was restarted")
		}
	})
}

func TestRecoveryUncertainStopAndStaleEndpointFailClosed(t *testing.T) {
	for _, mode := range []string{"stop_reply_lost", "same_job", "borrowed", "wrong_cluster"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				w, target, bridge := recoveryFixture(t, "restart")
				defer w.Close()
				switch mode {
				case "stop_reply_lost":
					w.jobs["original"].bridge = &uncertainStopBridge{bridge}
				case "same_job":
					bridge.ensured.Endpoint.ClusterID, bridge.ensured.Endpoint.JobID = "original", "original"
				case "borrowed":
					bridge.ensured.Endpoint.Owned = false
				case "wrong_cluster":
					bridge.ensured.Endpoint.ClusterName = "elsewhere"
				}
				tripRecovery(w, "original")
				time.Sleep(time.Minute)
				synctest.Wait()
				if w.recoveryStatus(target.Binding).Phase != "failed" || w.recoveryAdmission(target.Binding) == nil {
					t.Fatal("uncertain recovery admitted requests")
				}
				if mode == "stop_reply_lost" && recoveryStarts(bridge) != 0 {
					t.Fatal("uncertain stop started another process")
				}
			})
		})
	}
}

func recoveryStops(b *fakeBridge) int  { b.mu.Lock(); defer b.mu.Unlock(); return b.stopCalls }
func recoveryStarts(b *fakeBridge) int { b.mu.Lock(); defer b.mu.Unlock(); return b.ensureCalls }

func TestPreparedRegistrationCannotLaunchAfterReadinessExpires(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w, target, bridge := recoveryFixture(t, "restart")
		defer w.Close()
		tripRecovery(w, "original")
		time.Sleep(time.Minute)
		synctest.Wait()
		if w.preparedEndpoint(target.Binding) == nil {
			t.Fatal("recovery did not prewarm the replacement")
		}
		time.Sleep(91 * time.Second)
		controller, err := New(Options{Bridge: bridge, Registry: endpointregistry.NewMemory(), Targets: []lifecycle.Target{target}, Workloads: w})
		if err != nil {
			t.Fatal(err)
		}
		defer controller.Close()
		binding := target.Binding
		binding.FencingToken = 2
		_, err = controller.EnsureReady(context.Background(), binding, lifecycle.RequestFeatures{PreparedOnly: true})
		if !errors.Is(err, lifecycle.ErrEndpointNotReady) || recoveryStarts(bridge) != 1 {
			t.Fatal("expired prepared receipt launched a workload", err)
		}
		status, _ := controller.Status(context.Background(), binding)
		if status.State == endpointregistry.StateActivating || status.Prepared {
			t.Fatal("expired receipt remained activating or prepared", status)
		}
	})
}
