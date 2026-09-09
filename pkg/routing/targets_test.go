// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

type collectingTargetObserver struct {
	mu          sync.Mutex
	admissions  int64
	rejections  []AdmissionRejection
	transitions []CircuitTransition
}

func (o *collectingTargetObserver) TargetAdmissionChanged(_ string, delta int64) {
	atomic.AddInt64(&o.admissions, delta)
}

func (o *collectingTargetObserver) TargetAdmissionRejected(event AdmissionRejection) {
	o.mu.Lock()
	o.rejections = append(o.rejections, event)
	o.mu.Unlock()
}

func (o *collectingTargetObserver) TargetCircuitChanged(event CircuitTransition) {
	o.mu.Lock()
	o.transitions = append(o.transitions, event)
	o.mu.Unlock()
}

func TestTargetManagerCircuitOpenHalfOpenAndRecovery(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	observer := &collectingTargetObserver{}
	manager := newTestTargetManager(t, clock, observer, config.Deployment{
		Name: "target",
		Circuit: config.CircuitPolicy{
			ConsecutiveFailures: 2,
			MinimumSamples:      10,
			SampleWindow:        10,
			BaseEjectionTime:    config.Duration(5 * time.Second),
			MaxEjectionTime:     config.Duration(time.Minute),
		},
	})

	first, reason := manager.Acquire("target")
	if reason != "" {
		t.Fatalf("first Acquire() reason = %q", reason)
	}
	if !manager.Complete(first, TargetFailure) {
		t.Fatal("first Complete() = false")
	}
	if status, _ := manager.Status("target"); status.CircuitState != CircuitClosed {
		t.Fatalf("state after first failure = %s, want closed", status.CircuitState)
	}

	second, _ := manager.Acquire("target")
	manager.Complete(second, TargetFailure)
	status, _ := manager.Status("target")
	if status.CircuitState != CircuitOpen || status.EjectionCount != 1 {
		t.Fatalf("status after threshold = %#v", status)
	}
	if manager.Eligible("target") {
		t.Fatal("open target is eligible")
	}
	if lease, got := manager.Acquire("target"); lease != nil || got != AdmissionCircuitOpen {
		t.Fatalf("open Acquire() = %#v, %q", lease, got)
	}

	clock.Advance(5 * time.Second)
	if !manager.Eligible("target") {
		t.Fatal("target should be eligible for a half-open probe")
	}
	probe, got := manager.Acquire("target")
	if got != "" || probe == nil || !probe.HalfOpenProbe {
		t.Fatalf("probe Acquire() = %#v, %q", probe, got)
	}
	if secondProbe, got := manager.Acquire("target"); secondProbe != nil ||
		got != AdmissionHalfOpenInFlight {
		t.Fatalf("second probe Acquire() = %#v, %q", secondProbe, got)
	}
	manager.Complete(probe, TargetSuccess)
	status, _ = manager.Status("target")
	if status.CircuitState != CircuitClosed || status.EjectionCount != 0 {
		t.Fatalf("status after recovery = %#v", status)
	}
	if got := atomic.LoadInt64(&observer.admissions); got != 0 {
		t.Fatalf("observer active admissions = %d, want 0", got)
	}
}

func TestTargetManagerHalfOpenFailureUsesExponentialEjection(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	manager := newTestTargetManager(t, clock, nil, config.Deployment{
		Name: "target",
		Circuit: config.CircuitPolicy{
			ConsecutiveFailures: 1,
			MinimumSamples:      10,
			SampleWindow:        10,
			BaseEjectionTime:    config.Duration(5 * time.Second),
			MaxEjectionTime:     config.Duration(12 * time.Second),
		},
	})
	lease, _ := manager.Acquire("target")
	manager.Complete(lease, TargetFailure)
	first, _ := manager.Status("target")
	if got := first.EjectedUntil.Sub(clock.Now()); got != 5*time.Second {
		t.Fatalf("first ejection = %s, want 5s", got)
	}

	clock.Advance(5 * time.Second)
	probe, _ := manager.Acquire("target")
	manager.Complete(probe, TargetFailure)
	second, _ := manager.Status("target")
	if second.EjectionCount != 2 {
		t.Fatalf("ejection count = %d, want 2", second.EjectionCount)
	}
	if got := second.EjectedUntil.Sub(clock.Now()); got != 10*time.Second {
		t.Fatalf("second ejection = %s, want 10s", got)
	}

	clock.Advance(10 * time.Second)
	probe, _ = manager.Acquire("target")
	manager.Complete(probe, TargetFailure)
	third, _ := manager.Status("target")
	if got := third.EjectedUntil.Sub(clock.Now()); got != 12*time.Second {
		t.Fatalf("capped ejection = %s, want 12s", got)
	}
}

func TestTargetManagerIgnoresCompletionFromEarlierCircuitGeneration(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	manager := newTestTargetManager(t, clock, nil, config.Deployment{
		Name: "target",
		Circuit: config.CircuitPolicy{
			ConsecutiveFailures: 1,
			MinimumSamples:      10,
			SampleWindow:        10,
			BaseEjectionTime:    config.Duration(time.Second),
			MaxEjectionTime:     config.Duration(time.Minute),
		},
	})
	stale, _ := manager.Acquire("target")
	failing, _ := manager.Acquire("target")
	manager.Complete(failing, TargetFailure)
	clock.Advance(time.Second)
	probe, reason := manager.Acquire("target")
	if reason != "" || probe == nil || !probe.HalfOpenProbe {
		t.Fatalf("probe Acquire() = %#v, %q", probe, reason)
	}
	manager.Complete(probe, TargetSuccess)
	manager.Complete(stale, TargetFailure)

	status, _ := manager.Status("target")
	if status.CircuitState != CircuitClosed ||
		status.ConsecutiveFailures != 0 ||
		status.Samples != 0 {
		t.Fatalf("stale completion changed recovered circuit: %#v", status)
	}
}

func TestTargetManagerRollingFailureRate(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	manager := newTestTargetManager(t, clock, nil, config.Deployment{
		Name: "target",
		Circuit: config.CircuitPolicy{
			ConsecutiveFailures: 100,
			MinimumSamples:      4,
			SampleWindow:        4,
			FailureRate:         0.5,
			BaseEjectionTime:    config.Duration(time.Minute),
			MaxEjectionTime:     config.Duration(time.Minute),
		},
	})
	for _, result := range []TargetResult{
		TargetFailure,
		TargetSuccess,
		TargetSuccess,
		TargetFailure,
	} {
		lease, reason := manager.Acquire("target")
		if reason != "" {
			t.Fatalf("Acquire() reason = %q", reason)
		}
		manager.Complete(lease, result)
	}
	status, _ := manager.Status("target")
	if status.CircuitState != CircuitOpen {
		t.Fatalf("state = %s, want open at 50%% failure rate", status.CircuitState)
	}
}

func TestTargetManagerConcurrencyAdmission(t *testing.T) {
	manager := newTestTargetManager(t, nil, nil, config.Deployment{
		Name:           "target",
		MaxConcurrency: 1,
	})
	lease, reason := manager.Acquire("target")
	if reason != "" {
		t.Fatalf("Acquire() reason = %q", reason)
	}
	if manager.Eligible("target") {
		t.Fatal("target at concurrency limit is eligible")
	}
	if extra, got := manager.Acquire("target"); extra != nil ||
		got != AdmissionConcurrencyLimit {
		t.Fatalf("extra Acquire() = %#v, %q", extra, got)
	}
	if !manager.Complete(lease, TargetNeutral) {
		t.Fatal("Complete() = false")
	}
	if manager.Complete(lease, TargetSuccess) {
		t.Fatal("duplicate Complete() = true")
	}
	if !manager.Eligible("target") {
		t.Fatal("target should be eligible after release")
	}
}

func TestTargetStatusAdmissionAvailability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     TargetStatus
		available  bool
		atCapacity bool
	}{
		{
			name:      "unlimited closed",
			status:    TargetStatus{CircuitState: CircuitClosed},
			available: true,
		},
		{
			name: "below capacity",
			status: TargetStatus{
				CircuitState:   CircuitClosed,
				ActiveRequests: 1,
				MaxConcurrency: 2,
			},
			available: true,
		},
		{
			name: "at capacity",
			status: TargetStatus{
				CircuitState:   CircuitClosed,
				ActiveRequests: 2,
				MaxConcurrency: 2,
			},
			atCapacity: true,
		},
		{
			name: "open",
			status: TargetStatus{
				CircuitState: CircuitOpen,
			},
		},
		{
			name: "half open available",
			status: TargetStatus{
				CircuitState: CircuitHalfOpen,
			},
			available: true,
		},
		{
			name: "half open probe active",
			status: TargetStatus{
				CircuitState:        CircuitHalfOpen,
				HalfOpenProbeActive: true,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.status.AdmissionAvailable(); got != test.available {
				t.Fatalf("AdmissionAvailable() = %t, want %t", got, test.available)
			}
			if got := test.status.AtCapacity(); got != test.atCapacity {
				t.Fatalf("AtCapacity() = %t, want %t", got, test.atCapacity)
			}
		})
	}
}

func TestTargetManagerEligibilityRejectionIsObserved(t *testing.T) {
	observer := &collectingTargetObserver{}
	manager := newTestTargetManager(t, nil, observer, config.Deployment{
		Name:           "target",
		MaxConcurrency: 1,
	})
	lease, _ := manager.Acquire("target")
	if manager.Eligible("target") {
		t.Fatal("target at concurrency limit is eligible")
	}
	manager.Complete(lease, TargetSuccess)

	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.rejections) != 1 ||
		observer.rejections[0].Reason != AdmissionConcurrencyLimit {
		t.Fatalf("rejections = %#v", observer.rejections)
	}
}

func TestTargetManagerRecentFailuresAreBoundedAndContentFree(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0).UTC()}
	manager := newTestTargetManager(t, clock, nil, config.Deployment{
		Name:    "target",
		Circuit: config.CircuitPolicy{Disabled: true},
	})

	for index := range MaxRecentTargetFailures + 2 {
		lease, reason := manager.Acquire("target")
		if reason != "" {
			t.Fatalf("Acquire() reason = %q", reason)
		}
		failureClass := fmt.Sprintf("upstream_http_%d", 500+index)
		if !manager.CompleteWithFailureClass(
			lease,
			TargetFailure,
			failureClass,
		) {
			t.Fatal("CompleteWithFailureClass() = false")
		}
		clock.Advance(time.Second)
	}

	status, _ := manager.Status("target")
	if len(status.RecentFailures) != MaxRecentTargetFailures {
		t.Fatalf(
			"recent failures = %d, want %d",
			len(status.RecentFailures),
			MaxRecentTargetFailures,
		)
	}
	for index, failure := range status.RecentFailures {
		want := fmt.Sprintf("upstream_http_%d", 509-index)
		if failure.FailureClass != want {
			t.Fatalf(
				"recent failures[%d] = %#v, want class %q",
				index,
				failure,
				want,
			)
		}
	}

	lease, _ := manager.Acquire("target")
	if !manager.CompleteWithFailureClass(
		lease,
		TargetFailure,
		"secret token\nprovider response",
	) {
		t.Fatal("unsafe CompleteWithFailureClass() = false")
	}
	status, _ = manager.Status("target")
	if status.RecentFailures[0].FailureClass != "upstream_failure" {
		t.Fatalf("unsafe failure class was not redacted: %#v", status.RecentFailures[0])
	}
	status.RecentFailures[0].FailureClass = "mutated"
	again, _ := manager.Status("target")
	if again.RecentFailures[0].FailureClass != "upstream_failure" {
		t.Fatalf("Statuses() exposed mutable target state: %#v", again.RecentFailures)
	}

	success, _ := manager.Acquire("target")
	manager.CompleteWithFailureClass(success, TargetSuccess, "ignored")
	afterSuccess, _ := manager.Status("target")
	if len(afterSuccess.RecentFailures) != MaxRecentTargetFailures ||
		afterSuccess.RecentFailures[0].FailureClass != "upstream_failure" {
		t.Fatalf("success changed recent failures: %#v", afterSuccess.RecentFailures)
	}
}

func TestTargetManagerConcurrentAcquireHonorsLimit(t *testing.T) {
	const limit = 8
	manager := newTestTargetManager(t, nil, nil, config.Deployment{
		Name:           "target",
		MaxConcurrency: limit,
	})
	start := make(chan struct{})
	release := make(chan struct{})
	var admitted atomic.Int64
	var maximum atomic.Int64
	var wait sync.WaitGroup
	for range 100 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			lease, _ := manager.Acquire("target")
			if lease == nil {
				return
			}
			current := admitted.Add(1)
			for {
				previous := maximum.Load()
				if current <= previous || maximum.CompareAndSwap(previous, current) {
					break
				}
			}
			<-release
			admitted.Add(-1)
			manager.Complete(lease, TargetSuccess)
		}()
	}
	close(start)
	deadline := time.After(5 * time.Second)
	for {
		status, _ := manager.Status("target")
		if status.ActiveRequests == limit {
			break
		}
		select {
		case <-deadline:
			close(release)
			wait.Wait()
			t.Fatalf(
				"timed out waiting for %d admissions; status = %#v",
				limit,
				status,
			)
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	wait.Wait()
	if got := maximum.Load(); got > limit {
		t.Fatalf("maximum concurrent admissions = %d, limit %d", got, limit)
	}
	status, _ := manager.Status("target")
	if status.ActiveRequests != 0 {
		t.Fatalf("active requests = %d, want 0", status.ActiveRequests)
	}
}

func newTestTargetManager(
	t *testing.T,
	clock *fakeClock,
	observer TargetObserver,
	deployment config.Deployment,
) *TargetManager {
	t.Helper()
	options := TargetManagerOptions{Observer: observer}
	if clock != nil {
		options.Now = clock.Now
	}
	manager, err := NewTargetManager([]config.Deployment{deployment}, options)
	if err != nil {
		t.Fatalf("NewTargetManager() error = %v", err)
	}
	return manager
}
