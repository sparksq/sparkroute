package routing

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

type collectingRetryBudgetObserver struct {
	active     atomic.Int64
	mu         sync.Mutex
	rejections []RetryBudgetRejection
}

func (o *collectingRetryBudgetObserver) RetryBudgetActiveChanged(
	_ string,
	delta int64,
) {
	o.active.Add(delta)
}

func (o *collectingRetryBudgetObserver) RetryBudgetRejected(
	event RetryBudgetRejection,
) {
	o.mu.Lock()
	o.rejections = append(o.rejections, event)
	o.mu.Unlock()
}

func TestRetryBudgetUsesRatioAcrossActiveRequests(t *testing.T) {
	observer := &collectingRetryBudgetObserver{}
	manager := NewRetryBudgetManager(observer)
	policy := config.EffectiveRetryBudgetPolicy{
		Ratio:          0.2,
		MinConcurrency: 1,
	}
	requests := make([]RetryBudgetRequest, 10)
	for index := range requests {
		requests[index] = manager.BeginRequest("model", policy)
	}
	first := requests[0].TryAcquireRetry()
	second := requests[1].TryAcquireRetry()
	if first == nil || second == nil {
		t.Fatal("first two retries should fit the 20% budget")
	}
	if third := requests[2].TryAcquireRetry(); third != nil {
		t.Fatal("third concurrent retry exceeded the budget")
	}
	status, _ := manager.Status("model")
	if status.ActiveRequests != 10 || status.ActiveRetries != 2 {
		t.Fatalf("status = %#v", status)
	}

	first.Release()
	replacement := requests[2].TryAcquireRetry()
	if replacement == nil {
		t.Fatal("released capacity was not reusable")
	}
	replacement.Release()
	second.Release()
	for _, request := range requests {
		request.End()
	}
	if got := observer.active.Load(); got != 0 {
		t.Fatalf("observed active retries = %d, want 0", got)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if len(observer.rejections) != 1 ||
		observer.rejections[0].AllowedRetries != 2 {
		t.Fatalf("rejections = %#v", observer.rejections)
	}
}

func TestRetryBudgetMinimumAndDisabledModes(t *testing.T) {
	manager := NewRetryBudgetManager(nil)
	minimumPolicy := config.EffectiveRetryBudgetPolicy{
		Ratio:          0.01,
		MinConcurrency: 2,
	}
	request := manager.BeginRequest("minimum", minimumPolicy)
	first := request.TryAcquireRetry()
	second := request.TryAcquireRetry()
	if first == nil || second == nil {
		t.Fatal("minimum concurrency did not admit two retries")
	}
	if third := request.TryAcquireRetry(); third != nil {
		t.Fatal("minimum policy admitted a third retry")
	}
	first.Release()
	second.Release()
	request.End()

	unlimited := manager.BeginRequest("disabled", config.EffectiveRetryBudgetPolicy{
		Disabled:       true,
		Ratio:          0.01,
		MinConcurrency: 1,
	})
	leases := make([]RetryBudgetLease, 100)
	for index := range leases {
		leases[index] = unlimited.TryAcquireRetry()
		if leases[index] == nil {
			t.Fatalf("disabled budget rejected retry %d", index)
		}
	}
	for _, lease := range leases {
		lease.Release()
		lease.Release()
	}
	unlimited.End()
	unlimited.End()
}

func TestRetryBudgetRejectsAcquireAfterRequestEnd(t *testing.T) {
	manager := NewRetryBudgetManager(nil)
	request := manager.BeginRequest("model", config.EffectiveRetryBudgetPolicy{
		Ratio:          1,
		MinConcurrency: 1,
	})
	request.End()
	if lease := request.TryAcquireRetry(); lease != nil {
		t.Fatal("ended request acquired a retry")
	}
	status, _ := manager.Status("model")
	if status.ActiveRequests != 0 || status.ActiveRetries != 0 {
		t.Fatalf("status = %#v", status)
	}
}
