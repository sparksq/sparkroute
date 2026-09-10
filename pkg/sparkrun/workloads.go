// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

// Workloads lives for the gateway process, across configuration generations.
// Leases count physical jobs, so a draining generation cannot idle-stop a job
// while the replacement generation is serving it.
type Workloads struct {
	recoveries map[string]*recoveryState
	wait       sync.WaitGroup
	mu         sync.Mutex
	gates      map[string]chan struct{}
	jobs       map[string]*workload
	policies   map[string]lifecycle.Binding
	closed     bool
}

type workload struct {
	prepared      *Endpoint
	preparedUntil time.Time
	recovering    bool
	pausedPhase   string
	idleSince     time.Time
	stopping      bool
	binding       lifecycle.Binding
	bridge        Bridge
	timeout       time.Duration
	active        int
	owned         bool
	stopped       bool
	cluster       string
	timer         *time.Timer
	epoch         uint64
}

func NewWorkloads() *Workloads {
	return &Workloads{gates: map[string]chan struct{}{}, jobs: map[string]*workload{}, recoveries: map[string]*recoveryState{}}
}

func (w *Workloads) lock(ctx context.Context, binding lifecycle.Binding) (func(), error) {
	// Different model revisions never wait behind a slow cold start. The
	// same revision is serialized even when fallback cluster lists overlap.
	w.mu.Lock()
	gate := w.gates[binding.RecipeRevision]
	if gate == nil {
		gate = make(chan struct{}, 1)
		w.gates[binding.RecipeRevision] = gate
	}
	w.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *Workloads) Configure(targets []lifecycle.Target) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.policies = map[string]lifecycle.Binding{}
	for _, target := range targets {
		if target.Source != lifecycle.EndpointActivatable {
			continue
		}
		if len(target.Binding.ClusterCandidates) == 0 {
			w.policies[recoveryKey(target.Binding.RecipeRevision, "")] = cloneBinding(target.Binding)
		}
		for _, cluster := range target.Binding.ClusterCandidates {
			w.policies[target.Binding.RecipeRevision+"\x00"+cluster] = cloneBinding(target.Binding)
		}
	}
	for id, job := range w.jobs {
		policy, exists := w.policyLocked(job.binding.RecipeRevision, job.cluster)
		if exists {
			job.binding = policy
		} else {
			job.binding.IdleTTL = 0
			job.binding.Recovery = config.RecoveryPolicy{}
		}
		w.configureRecoveryLocked(job)
		w.scheduleLocked(id, job)
	}
}

func (w *Workloads) observe(binding lifecycle.Binding, endpoint Endpoint, bridge Bridge, timeout time.Duration, acquire bool, prepared ...bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	job := w.jobs[endpoint.ClusterID]
	if job == nil {
		job = &workload{binding: cloneBinding(binding), bridge: bridge, timeout: timeout, cluster: endpoint.ClusterName}
		if w.policies != nil {
			if policy, exists := w.policyLocked(binding.RecipeRevision, endpoint.ClusterName); exists {
				job.binding = cloneBinding(policy)
			} else {
				job.binding.IdleTTL = 0
				job.binding.Recovery = config.RecoveryPolicy{}
			}
		}
		w.jobs[endpoint.ClusterID] = job
	}
	job.owned = endpoint.Owned
	if job.prepared != nil || len(prepared) > 0 && prepared[0] {
		copy := endpoint
		job.prepared = &copy
		job.preparedUntil = time.Now().Add(90 * time.Second)
	}
	w.observeRecoveryLocked(endpoint.ClusterID, job)
	if acquire {
		job.active++
		job.idleSince = time.Time{}
		job.stopped = false
		job.epoch++
		if job.timer != nil {
			job.timer.Stop()
			job.timer = nil
		}
	} else if job.timer == nil && !job.stopped && job.active == 0 {
		w.scheduleLocked(endpoint.ClusterID, job)
	}
}

func (w *Workloads) acquire(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil {
		job.active++
		job.idleSince = time.Time{}
		job.epoch++
		job.stopped = false
		if job.timer != nil {
			job.timer.Stop()
			job.timer = nil
		}
	}
}

func (w *Workloads) release(id string, outcomes ...lifecycle.RequestOutcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil && job.active > 0 {
		job.active--
		if len(outcomes) > 0 {
			w.recoveryOutcomeLocked(id, job, outcomes[0])
		}
		if job.active == 0 {
			job.idleSince = time.Now()
			w.scheduleLocked(id, job)
		}
	}
}

func (w *Workloads) available(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobs[id] == nil || (!w.jobs[id].stopped && !w.jobs[id].stopping && !w.jobs[id].recovering)
}

func (w *Workloads) canStop(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil && job.active != 0 {
		return fmt.Errorf("sparkrun workload still has active requests")
	}
	return nil
}

func (w *Workloads) scheduleLocked(id string, job *workload) {
	job.epoch++
	if job.timer != nil {
		job.timer.Stop()
		job.timer = nil
	}
	if w.closed || !job.owned || job.active != 0 || job.stopped || job.recovering || job.binding.IdleTTL <= 0 || w.recoveryScheduledLocked(job) {
		return
	}
	epoch := job.epoch
	if job.idleSince.IsZero() {
		job.idleSince = time.Now()
	}
	remaining := job.binding.IdleTTL - time.Since(job.idleSince)
	job.timer = time.AfterFunc(max(remaining, 0), func() {
		ctx, cancel := context.WithTimeout(context.Background(), job.timeout)
		defer cancel()
		// Read the binding while holding the mutex; Configure can change it.
		w.mu.Lock()
		binding := cloneBinding(job.binding)
		w.mu.Unlock()
		unlock, err := w.lock(ctx, binding)
		if err != nil {
			return
		}
		defer unlock()
		w.mu.Lock()
		if w.closed || job.epoch != epoch || job.active != 0 || job.recovering || w.recoveryScheduledLocked(job) {
			w.mu.Unlock()
			return
		}
		job.stopping = true
		w.mu.Unlock()
		if binding.IdleAction == "sleep" {
			if bridge, ok := job.bridge.(WorkloadBridge); ok {
				_, err = bridge.Lifecycle(ctx, bridgeBinding(binding), id, "sleep", job.timeout)
			} else {
				err = fmt.Errorf("idle sleep is unavailable")
			}
		} else {
			_, err = job.bridge.Stop(ctx, bridgeBinding(binding), id)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		job.timer = nil
		job.stopping = false
		if err == nil {
			job.stopped = true
			if binding.IdleAction == "sleep" {
				job.pausedPhase = "sleeping"
			} else {
				job.pausedPhase = "offline"
			}
		} else {
			// The remote transition may have succeeded before its response was
			// lost. Block cached endpoints until a fresh status/wake resolves it.
			job.stopped = true
			job.pausedPhase = "unknown"
		}
	})
}

func (w *Workloads) Close() {
	w.mu.Lock()
	w.closed = true
	for _, r := range w.recoveries {
		w.cancelRecoveryLocked(r)
	}
	for _, job := range w.jobs {
		if job.timer != nil {
			job.timer.Stop()
		}
	}
	w.mu.Unlock()
	w.wait.Wait()
}

func (w *Workloads) stopped(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil {
		job.stopped = true
		job.epoch++
		if job.timer != nil {
			job.timer.Stop()
			job.timer = nil
		}
	}
}

func (w *Workloads) phase(id string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil {
		if job.stopping {
			return "deactivating"
		}
		if job.stopped {
			if job.pausedPhase != "" {
				return job.pausedPhase
			}
			return "offline"
		}
	}
	return ""
}

func (w *Workloads) suspend(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil {
		if r := w.recoveries[recoveryKey(job.binding.RecipeRevision, job.cluster)]; r != nil && r.jobID == id {
			w.cancelRecoveryLocked(r)
			r.firstFailure, r.failedProbes, r.phase, r.reason = time.Time{}, 0, "", ""
		}
		job.recovering = false
		job.stopping = true
		job.stopped = true
		job.epoch++
		if job.timer != nil {
			job.timer.Stop()
			job.timer = nil
		}
	}
}
func (w *Workloads) finishAction(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil {
		job.stopping = false
	}
}
func (w *Workloads) running(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil && !job.stopping {
		job.stopped = false
	}
}

func (w *Workloads) policyLocked(revision, cluster string) (lifecycle.Binding, bool) {
	policy, exists := w.policies[recoveryKey(revision, cluster)]
	if !exists {
		policy, exists = w.policies[recoveryKey(revision, "")]
	}
	return policy, exists
}
