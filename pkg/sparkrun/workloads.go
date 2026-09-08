package sparkrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

// Workloads lives for the gateway process, across configuration generations.
// Leases count physical jobs, so a draining generation cannot idle-stop a job
// while the replacement generation is serving it.
type Workloads struct {
	mu       sync.Mutex
	gates    map[string]chan struct{}
	jobs     map[string]*workload
	policies map[string]lifecycle.Binding
	closed   bool
}

type workload struct {
	idleSince time.Time
	stopping  bool
	binding   lifecycle.Binding
	bridge    Bridge
	timeout   time.Duration
	active    int
	owned     bool
	stopped   bool
	cluster   string
	timer     *time.Timer
	epoch     uint64
}

func NewWorkloads() *Workloads {
	return &Workloads{gates: map[string]chan struct{}{}, jobs: map[string]*workload{}}
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
		for _, cluster := range target.Binding.ClusterCandidates {
			w.policies[target.Binding.RecipeRevision+"\x00"+cluster] = cloneBinding(target.Binding)
		}
	}
	for id, job := range w.jobs {
		policy, exists := w.policies[job.binding.RecipeRevision+"\x00"+job.cluster]
		if exists {
			job.binding = policy
		} else {
			job.binding.IdleTTL = 0
		}
		w.scheduleLocked(id, job)
	}
}

func (w *Workloads) observe(binding lifecycle.Binding, endpoint Endpoint, bridge Bridge, timeout time.Duration, acquire bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	job := w.jobs[endpoint.ClusterID]
	if job == nil {
		job = &workload{binding: cloneBinding(binding), bridge: bridge, timeout: timeout, cluster: endpoint.ClusterName}
		if w.policies != nil {
			if policy, exists := w.policies[binding.RecipeRevision+"\x00"+endpoint.ClusterName]; exists {
				job.binding = cloneBinding(policy)
			} else {
				job.binding.IdleTTL = 0
			}
		}
		w.jobs[endpoint.ClusterID] = job
	}
	job.owned = endpoint.Owned
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

func (w *Workloads) release(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil && job.active > 0 {
		job.active--
		if job.active == 0 {
			job.idleSince = time.Now()
			w.scheduleLocked(id, job)
		}
	}
}

func (w *Workloads) available(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.jobs[id] == nil || (!w.jobs[id].stopped && !w.jobs[id].stopping)
}

func (w *Workloads) canStop(id string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if job := w.jobs[id]; job != nil && job.active != 0 {
		return fmt.Errorf("SparkRun workload still has active requests")
	}
	return nil
}

func (w *Workloads) scheduleLocked(id string, job *workload) {
	job.epoch++
	if job.timer != nil {
		job.timer.Stop()
		job.timer = nil
	}
	if w.closed || !job.owned || job.active != 0 || job.stopped || job.binding.IdleTTL <= 0 {
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
		if w.closed || job.epoch != epoch || job.active != 0 {
			w.mu.Unlock()
			return
		}
		job.stopping = true
		w.mu.Unlock()
		_, err = job.bridge.Stop(ctx, bridgeBinding(binding), id)
		w.mu.Lock()
		defer w.mu.Unlock()
		job.timer = nil
		job.stopping = false
		if err == nil {
			job.stopped = true
		} // failures remain retryable on the next observation
	})
}

func (w *Workloads) Close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	for _, job := range w.jobs {
		if job.timer != nil {
			job.timer.Stop()
		}
	}
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
			return "offline"
		}
	}
	return ""
}
