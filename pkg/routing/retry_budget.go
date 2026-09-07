package routing

import (
	"math"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/sparksq/sparkroute/pkg/config"
)

// RetryBudget limits retry concurrency across requests for one virtual model.
// BeginRequest must be paired with RetryBudgetRequest.End.
type RetryBudget interface {
	BeginRequest(
		virtualModel string,
		policy config.EffectiveRetryBudgetPolicy,
	) RetryBudgetRequest
}

type RetryBudgetRequest interface {
	TryAcquireRetry() RetryBudgetLease
	End()
}

type RetryBudgetLease interface {
	Release()
}

type RetryBudgetRejection struct {
	VirtualModel   string
	ActiveRequests int
	ActiveRetries  int
	AllowedRetries int
}

type RetryBudgetObserver interface {
	RetryBudgetActiveChanged(virtualModel string, delta int64)
	RetryBudgetRejected(event RetryBudgetRejection)
}

type RetryBudgetManager struct {
	mu       sync.Mutex
	models   map[string]*retryBudgetState
	observer RetryBudgetObserver
}

type retryBudgetState struct {
	activeRequests int
	activeRetries  int
}

type retryBudgetRequest struct {
	mu           sync.Mutex
	manager      *RetryBudgetManager
	state        *retryBudgetState
	virtualModel string
	policy       config.EffectiveRetryBudgetPolicy
	ended        bool
}

type retryBudgetLease struct {
	manager      *RetryBudgetManager
	state        *retryBudgetState
	virtualModel string
	released     atomic.Bool
}

type RetryBudgetStatus struct {
	VirtualModel   string
	ActiveRequests int
	ActiveRetries  int
}

func NewRetryBudgetManager(observer RetryBudgetObserver) *RetryBudgetManager {
	return &RetryBudgetManager{
		models:   make(map[string]*retryBudgetState),
		observer: observer,
	}
}

func (m *RetryBudgetManager) BeginRequest(
	virtualModel string,
	policy config.EffectiveRetryBudgetPolicy,
) RetryBudgetRequest {
	m.mu.Lock()
	state := m.models[virtualModel]
	if state == nil {
		state = &retryBudgetState{}
		m.models[virtualModel] = state
	}
	state.activeRequests++
	m.mu.Unlock()
	return &retryBudgetRequest{
		manager:      m,
		state:        state,
		virtualModel: virtualModel,
		policy:       policy,
	}
}

func (r *retryBudgetRequest) TryAcquireRetry() RetryBudgetLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return nil
	}

	r.manager.mu.Lock()
	allowed := allowedRetryConcurrency(r.state.activeRequests, r.policy)
	if !r.policy.Disabled && r.state.activeRetries >= allowed {
		event := RetryBudgetRejection{
			VirtualModel:   r.virtualModel,
			ActiveRequests: r.state.activeRequests,
			ActiveRetries:  r.state.activeRetries,
			AllowedRetries: allowed,
		}
		r.manager.mu.Unlock()
		r.manager.observeRejection(event)
		return nil
	}
	r.state.activeRetries++
	r.manager.mu.Unlock()
	r.manager.observeActive(r.virtualModel, 1)
	return &retryBudgetLease{
		manager:      r.manager,
		state:        r.state,
		virtualModel: r.virtualModel,
	}
}

func (r *retryBudgetRequest) End() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ended {
		return
	}
	r.ended = true
	r.manager.mu.Lock()
	if r.state.activeRequests > 0 {
		r.state.activeRequests--
	}
	r.manager.mu.Unlock()
}

func (l *retryBudgetLease) Release() {
	if l == nil || !l.released.CompareAndSwap(false, true) {
		return
	}
	l.manager.mu.Lock()
	if l.state.activeRetries > 0 {
		l.state.activeRetries--
	}
	l.manager.mu.Unlock()
	l.manager.observeActive(l.virtualModel, -1)
}

func (m *RetryBudgetManager) Status(
	virtualModel string,
) (RetryBudgetStatus, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.models[virtualModel]
	if state == nil {
		return RetryBudgetStatus{}, false
	}
	return RetryBudgetStatus{
		VirtualModel:   virtualModel,
		ActiveRequests: state.activeRequests,
		ActiveRetries:  state.activeRetries,
	}, true
}

func (m *RetryBudgetManager) Statuses() []RetryBudgetStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	names := make([]string, 0, len(m.models))
	for name := range m.models {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := make([]RetryBudgetStatus, 0, len(names))
	for _, name := range names {
		state := m.models[name]
		statuses = append(statuses, RetryBudgetStatus{
			VirtualModel:   name,
			ActiveRequests: state.activeRequests,
			ActiveRetries:  state.activeRetries,
		})
	}
	return statuses
}

func allowedRetryConcurrency(
	activeRequests int,
	policy config.EffectiveRetryBudgetPolicy,
) int {
	allowed := int(math.Ceil(float64(activeRequests) * policy.Ratio))
	if allowed < policy.MinConcurrency {
		allowed = policy.MinConcurrency
	}
	return allowed
}

func (m *RetryBudgetManager) observeActive(
	virtualModel string,
	delta int64,
) {
	if m.observer != nil {
		m.observer.RetryBudgetActiveChanged(virtualModel, delta)
	}
}

func (m *RetryBudgetManager) observeRejection(event RetryBudgetRejection) {
	if m.observer != nil {
		m.observer.RetryBudgetRejected(event)
	}
}

var _ RetryBudget = (*RetryBudgetManager)(nil)
