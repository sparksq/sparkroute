// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

// Recovery state belongs to a recipe/cluster, so replacing a process or a
// configuration generation cannot reset its automatic restart budget.
// All fields are protected by Workloads.mu.
type recoveryState struct {
	jobID        string
	policy       config.RecoveryPolicy
	phase        string
	reason       string
	firstFailure time.Time
	failedProbes int
	attempts     int
	lastAttempt  time.Time
	nextAttempt  time.Time
	epoch        uint64
	timer        *time.Timer
	cancel       context.CancelFunc
}

func recoveryKey(revision, cluster string) string { return revision + "\x00" + cluster }

func (w *Workloads) observeRecoveryLocked(id string, job *workload) {
	key := recoveryKey(job.binding.RecipeRevision, job.cluster)
	r := w.recoveries[key]
	if r == nil {
		r = &recoveryState{policy: job.binding.Recovery.Effective()}
		w.recoveries[key] = r
	}
	if r.jobID != id {
		// A fresh endpoint retires health evidence for its predecessor, but
		// retains the budget. Late releases from that predecessor are ignored.
		w.cancelRecoveryLocked(r)
		r.jobID, r.firstFailure, r.failedProbes = id, time.Time{}, 0
		r.phase, r.reason = "", ""
	}
}

func (w *Workloads) cancelRecoveryLocked(r *recoveryState) {
	r.epoch++
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
	r.nextAttempt = time.Time{}
}

func (w *Workloads) configureRecoveryLocked(job *workload) {
	r := w.recoveries[recoveryKey(job.binding.RecipeRevision, job.cluster)]
	if r == nil || w.jobs[r.jobID] != job {
		return
	}
	p := job.binding.Recovery.Effective()
	if p == r.policy {
		return
	}
	w.cancelRecoveryLocked(r)
	r.policy = p
	r.phase, r.reason, r.firstFailure, r.failedProbes = "", "", time.Time{}, 0
	// An interrupted remote operation may have completed. Cached serving
	// remains fenced; normal ensure_ready must reconcile the receipt.
	if job.recovering {
		job.stopped = true
		job.pausedPhase = "unknown"
	}
	job.recovering = false
}

func (w *Workloads) recoveryOutcomeLocked(id string, job *workload, outcome lifecycle.RequestOutcome) {
	r := w.recoveries[recoveryKey(job.binding.RecipeRevision, job.cluster)]
	if r == nil || r.jobID != id || r.policy.Action == "" || !job.owned || job.stopped || job.recovering || !outcome.HealthObserved {
		return
	}
	if outcome.Success {
		w.cancelRecoveryLocked(r)
		r.firstFailure, r.failedProbes, r.phase, r.reason = time.Time{}, 0, "", ""
		return
	}
	if !outcome.CircuitOpened || !outcome.BackendFailure {
		return
	}
	if r.firstFailure.IsZero() {
		r.firstFailure = time.Now()
	}
	if outcome.HalfOpenProbe {
		r.failedProbes++
	}
	if r.timer != nil || r.cancel != nil {
		return
	}
	r.phase = "monitoring"
	if r.failedProbes < r.policy.FailedProbes {
		return
	}
	if r.attempts >= r.policy.MaxRestarts {
		job.recovering = true
		w.recoveryFailedLocked(r, "recovery_exhausted")
		return
	}
	r.nextAttempt = r.firstFailure.Add(r.policy.UnhealthyFor.Value())
	if r.attempts > 0 {
		next := r.lastAttempt.Add(recoveryBackoff(r.policy, r.attempts))
		if next.After(r.nextAttempt) {
			r.nextAttempt = next
		}
	}
	r.phase = "scheduled"
	r.epoch++
	epoch := r.epoch
	r.timer = time.AfterFunc(max(time.Until(r.nextAttempt), 0), func() {
		w.mu.Lock()
		if w.closed || r.epoch != epoch {
			w.mu.Unlock()
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		r.timer = nil
		binding, bridge, timeout := cloneBinding(job.binding), job.bridge, job.timeout
		w.wait.Add(1)
		w.mu.Unlock()
		defer w.wait.Done()
		defer cancel()
		w.recoverJob(ctx, id, job, r, epoch, binding, bridge, timeout)
	})
}

func recoveryBackoff(p config.RecoveryPolicy, attempts int) time.Duration {
	delay := p.Backoff.Value()
	for i := 1; i < attempts && delay < p.MaxBackoff.Value(); i++ {
		delay = min(delay*2, p.MaxBackoff.Value())
	}
	return delay
}

func (w *Workloads) recoveryFailedLocked(r *recoveryState, reason string) {
	r.phase, r.reason, r.nextAttempt = "failed", reason, time.Time{}
	if reason == "recovery_exhausted" {
		r.phase = "exhausted"
	}
}

func (w *Workloads) recoverJob(ctx context.Context, id string, job *workload, r *recoveryState, epoch uint64, binding lifecycle.Binding, bridge Bridge, timeout time.Duration) {
	// This gate also covers activation, idle stop, manual actions, aliases,
	// and draining configuration generations. Release does not need the gate.
	drainCtx, cancelDrain := context.WithTimeout(ctx, binding.Recovery.Effective().DrainTimeout.Value())
	defer cancelDrain()
	unlock, err := w.lock(drainCtx, binding)
	if err != nil {
		w.mu.Lock()
		if !w.closed && r.epoch == epoch {
			job.recovering = true
			w.recoveryFailedLocked(r, "recovery_drain_timeout")
		}
		w.mu.Unlock()
		return
	}
	defer unlock()
	w.mu.Lock()
	if w.closed || r.epoch != epoch || job.stopped || !job.owned {
		w.mu.Unlock()
		return
	}
	job.recovering = true
	job.epoch++
	if job.timer != nil {
		job.timer.Stop()
		job.timer = nil
	}
	r.phase, r.nextAttempt = "draining", time.Time{}
	w.mu.Unlock()
	for {
		w.mu.Lock()
		valid, active := !w.closed && r.epoch == epoch, job.active
		w.mu.Unlock()
		if !valid {
			return
		}
		if active == 0 {
			break
		}
		select {
		case <-drainCtx.Done():
			w.mu.Lock()
			if r.epoch == epoch {
				w.recoveryFailedLocked(r, "recovery_drain_timeout")
			}
			w.mu.Unlock()
			return
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Recovery always stops, even if the idle policy normally sleeps. Pin the
	// actual cluster so a retry cannot wake or replace a different workload.
	binding.ClusterCandidates = []string{job.cluster}
	w.mu.Lock()
	if w.closed || r.epoch != epoch {
		w.mu.Unlock()
		return
	}
	r.attempts++
	r.lastAttempt, r.phase = time.Now(), "stopping"
	job.stopped, job.pausedPhase = true, "unknown"
	w.mu.Unlock()
	stopCtx, cancelStop := context.WithTimeout(ctx, timeout)
	stopped, err := bridge.Stop(stopCtx, bridgeBinding(binding), id)
	if err == nil && (stopped.State != "offline" || !slices.Contains(stopped.ClusterIDs, id)) {
		err = fmt.Errorf("stop receipt did not confirm the exact workload")
	}
	cancelStop()
	w.mu.Lock()
	if w.closed || r.epoch != epoch {
		w.mu.Unlock()
		return
	}
	if err != nil {
		w.recoveryFailedLocked(r, "recovery_stop_failed")
		w.mu.Unlock()
		return
	}
	job.pausedPhase = "offline"
	if r.policy.Action == "stop" {
		job.recovering = false
		r.phase, r.reason = "awaiting_demand", ""
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	for {
		w.mu.Lock()
		if w.closed || r.epoch != epoch {
			w.mu.Unlock()
			return
		}
		r.phase, r.reason, r.nextAttempt = "restarting", "", time.Time{}
		w.mu.Unlock()
		activationTimeout := binding.ActivationTimeout
		if activationTimeout <= 0 {
			activationTimeout = 30 * time.Minute
		}
		startCtx, cancelStart := context.WithTimeout(ctx, activationTimeout)
		result, startErr := bridge.EnsureReady(startCtx, bridgeBinding(binding), activationTimeout)
		cancelStart()
		w.mu.Lock()
		if w.closed || r.epoch != epoch {
			w.mu.Unlock()
			return
		}
		if startErr == nil && (result.State != "ready" || result.Endpoint == nil || !result.Endpoint.Owned ||
			result.Endpoint.State != "ready" || result.Endpoint.Protocol != "openai" || validateHost(result.Endpoint.Host) != nil || result.Endpoint.Port < 1 || result.Endpoint.Port > 65535 || len(result.Endpoint.ServedModels) == 0 ||
			result.Endpoint.ClusterID == "" || result.Endpoint.JobID == "" || result.Endpoint.ClusterID == id ||
			result.Endpoint.RecipeRevision != binding.RecipeRevision || !slices.Contains(binding.ClusterCandidates, result.Endpoint.ClusterName)) {
			// Do not repeatedly adopt a stale or foreign receipt as a restart.
			w.recoveryFailedLocked(r, "recovery_invalid_endpoint")
			w.mu.Unlock()
			return
		}
		if startErr == nil {
			job.recovering = false
			// Observation attaches the same budget to the replacement. It is
			// registered for serving only through normal fenced admission.
			w.mu.Unlock()
			w.observe(binding, *result.Endpoint, bridge, timeout, false, true)
			return
		}
		if r.attempts >= r.policy.MaxRestarts {
			w.recoveryFailedLocked(r, "recovery_exhausted")
			w.mu.Unlock()
			return
		}
		delay := recoveryBackoff(r.policy, r.attempts)
		r.phase, r.reason, r.nextAttempt = "backoff", "recovery_start_failed", time.Now().Add(delay)
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		w.mu.Lock()
		if w.closed || r.epoch != epoch {
			w.mu.Unlock()
			return
		}
		r.attempts++
		r.lastAttempt = time.Now()
		w.mu.Unlock()
	}
}

func (w *Workloads) recoveryAdmission(binding lifecycle.Binding) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.bindingRecoveriesLocked(binding) {
		switch r.phase {
		case "failed", "exhausted":
			return fmt.Errorf("%s: inspect the workload and use Start to retry explicitly", r.reason)
		case "draining", "stopping", "restarting", "backoff":
			return fmt.Errorf("workload recovery is in progress")
		}
	}
	return nil
}

func (w *Workloads) bindingRecoveriesLocked(binding lifecycle.Binding) []*recoveryState {
	var result []*recoveryState
	for key, r := range w.recoveries {
		job := w.jobs[r.jobID]
		if job != nil && key == recoveryKey(binding.RecipeRevision, job.cluster) && r.policy.Action != "" &&
			(len(binding.ClusterCandidates) == 0 || slices.Contains(binding.ClusterCandidates, job.cluster)) {
			result = append(result, r)
		}
	}
	return result
}

func (w *Workloads) recoveryStatus(binding lifecycle.Binding) *lifecycle.RecoveryStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	matches := w.bindingRecoveriesLocked(binding)
	if len(matches) == 0 {
		return nil
	}
	r := matches[0]
	status := &lifecycle.RecoveryStatus{Action: r.policy.Action, Phase: r.phase, Reason: r.reason,
		FailedProbes: r.failedProbes, Attempts: r.attempts, MaxRestarts: r.policy.MaxRestarts}
	if status.Phase == "" {
		status.Phase = "healthy"
	}
	if !r.nextAttempt.IsZero() {
		next := r.nextAttempt
		status.NextAttempt = &next
	}
	return status
}

// A successful operator stop resets the automatic budget.
func (w *Workloads) resetRecovery(revision, cluster string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r := w.recoveries[recoveryKey(revision, cluster)]; r != nil {
		w.cancelRecoveryLocked(r)
		r.attempts, r.failedProbes, r.firstFailure, r.phase, r.reason = 0, 0, time.Time{}, "", ""
		if job := w.jobs[r.jobID]; job != nil {
			job.recovering = false
		}
	}
}

func (w *Workloads) recoveryScheduledLocked(job *workload) bool {
	r := w.recoveries[recoveryKey(job.binding.RecipeRevision, job.cluster)]
	return r != nil && (r.timer != nil || r.cancel != nil)
}

// Caller holds the workload gate. A deliberate Start may retry even when a
// failed launch left no job receipt for the operator to stop. EnsureReady still
// reconciles existing jobs and verifies readiness before publishing anything.
func (w *Workloads) prepareManualStart(binding lifecycle.Binding) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	recoveries := w.bindingRecoveriesLocked(binding)
	for _, r := range recoveries {
		if job := w.jobs[r.jobID]; job != nil && job.active != 0 {
			return fmt.Errorf("sparkrun workload still has active requests")
		}
	}
	for _, r := range recoveries {
		if job := w.jobs[r.jobID]; job != nil {
			if job.recovering {
				job.stopped, job.pausedPhase = true, "unknown"
			}
			job.recovering = false
		}
		w.cancelRecoveryLocked(r)
		r.attempts, r.failedProbes, r.firstFailure, r.phase, r.reason = 0, 0, time.Time{}, "", ""
	}
	return nil
}

// Prepared receipts expire unless discovery refreshes readiness. Registering one
// still requires the admission coordinator's current activation claim and fence.
func (w *Workloads) preparedEndpoint(binding lifecycle.Binding) *Endpoint {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, r := range w.bindingRecoveriesLocked(binding) {
		if job := w.jobs[r.jobID]; job != nil && r.phase == "" && job.owned && !job.stopped && !job.stopping && !job.recovering && job.prepared != nil && job.preparedUntil.After(time.Now()) {
			endpoint := *job.prepared
			return &endpoint
		}
	}
	return nil
}
