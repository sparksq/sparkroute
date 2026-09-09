// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package routing

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

type AdmissionReason string

const (
	AdmissionCircuitOpen      AdmissionReason = "circuit_open"
	AdmissionHalfOpenInFlight AdmissionReason = "half_open_probe_in_flight"
	AdmissionConcurrencyLimit AdmissionReason = "concurrency_limit"
	AdmissionUnknownTarget    AdmissionReason = "unknown_target"
)

type TargetResult string

const (
	TargetSuccess TargetResult = "success"
	TargetFailure TargetResult = "failure"
	TargetNeutral TargetResult = "neutral"
)

const (
	// MaxRecentTargetFailures bounds the replica-local failure projection
	// returned for each deployment target.
	MaxRecentTargetFailures = 8
	maxFailureClassLength   = 96
)

type RecentTargetFailure struct {
	OccurredAt   time.Time
	FailureClass string
}

type CircuitTransition struct {
	Deployment    string
	From          CircuitState
	To            CircuitState
	Reason        string
	EjectionCount int
	EjectedUntil  time.Time
}

type AdmissionRejection struct {
	Deployment string
	Reason     AdmissionReason
}

// TargetObserver receives bounded deployment/state events outside target locks.
// Implementations must not retain secrets or request content.
type TargetObserver interface {
	TargetAdmissionChanged(deployment string, delta int64)
	TargetAdmissionRejected(event AdmissionRejection)
	TargetCircuitChanged(event CircuitTransition)
}

type TargetManagerOptions struct {
	Now      func() time.Time
	Observer TargetObserver
}

type TargetManager struct {
	targets  map[string]*targetRuntime
	now      func() time.Time
	observer TargetObserver
}

type targetRuntime struct {
	mu sync.Mutex

	deployment     string
	maxConcurrency int
	policy         config.EffectiveCircuitPolicy
	state          CircuitState
	active         int

	consecutiveFailures int
	samples             []bool
	sampleCount         int
	sampleCursor        int
	failuresInWindow    int

	ejectionCount  int
	ejectedUntil   time.Time
	generation     uint64
	probeInFlight  bool
	recentFailures []RecentTargetFailure
}

type TargetLease struct {
	Deployment     string
	CircuitState   CircuitState
	HalfOpenProbe  bool
	ActiveRequests int

	target            *targetRuntime
	circuitGeneration uint64
	completed         atomic.Bool
}

type TargetStatus struct {
	Deployment          string
	CircuitState        CircuitState
	ActiveRequests      int
	MaxConcurrency      int
	ConsecutiveFailures int
	Samples             int
	Failures            int
	EjectionCount       int
	EjectedUntil        time.Time
	HalfOpenProbeActive bool
	RecentFailures      []RecentTargetFailure
}

// TargetStatusSource exposes a point-in-time, content-free view of target
// admission and circuit state. Implementations must return deployment names
// only; provider credentials and request content never belong in this surface.
type TargetStatusSource interface {
	Statuses() []TargetStatus
}

// AdmissionAvailable reports whether a new request could pass the status
// snapshot's circuit and replica-local concurrency gates. Acquire always
// rechecks those gates atomically, so this remains an observational hint.
func (s TargetStatus) AdmissionAvailable() bool {
	if s.CircuitState == CircuitOpen ||
		s.CircuitState == CircuitHalfOpen && s.HalfOpenProbeActive {
		return false
	}
	return s.MaxConcurrency <= 0 || s.ActiveRequests < s.MaxConcurrency
}

func (s TargetStatus) AtCapacity() bool {
	return s.MaxConcurrency > 0 && s.ActiveRequests >= s.MaxConcurrency
}

var _ Eligibility = (*TargetManager)(nil)
var _ TargetStatusSource = (*TargetManager)(nil)

func NewTargetManager(
	deployments []config.Deployment,
	options TargetManagerOptions,
) (*TargetManager, error) {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	manager := &TargetManager{
		targets:  make(map[string]*targetRuntime, len(deployments)),
		now:      now,
		observer: options.Observer,
	}
	for index, deployment := range deployments {
		if deployment.Name == "" {
			return nil, fmt.Errorf("deployments[%d].name is required", index)
		}
		if _, exists := manager.targets[deployment.Name]; exists {
			return nil, fmt.Errorf("duplicate deployment %q", deployment.Name)
		}
		if deployment.MaxConcurrency < 0 {
			return nil, fmt.Errorf(
				"deployment %q max_concurrency must not be negative",
				deployment.Name,
			)
		}
		if err := deployment.Circuit.Validate(); err != nil {
			return nil, fmt.Errorf("deployment %q circuit: %w", deployment.Name, err)
		}
		policy := deployment.Circuit.Effective()
		manager.targets[deployment.Name] = &targetRuntime{
			deployment:     deployment.Name,
			maxConcurrency: deployment.MaxConcurrency,
			policy:         policy,
			state:          CircuitClosed,
			samples:        make([]bool, policy.SampleWindow),
		}
	}
	return manager, nil
}

// Eligible is a non-reserving planning hint. Acquire rechecks the same gates
// atomically immediately before an attempt.
func (m *TargetManager) Eligible(deployment string) bool {
	target, exists := m.targets[deployment]
	if !exists {
		return false
	}
	now := m.now()
	target.mu.Lock()
	transition := target.advance(now)
	var reason AdmissionReason
	switch {
	case target.state == CircuitOpen:
		reason = AdmissionCircuitOpen
	case target.state == CircuitHalfOpen && target.probeInFlight:
		reason = AdmissionHalfOpenInFlight
	case target.maxConcurrency > 0 && target.active >= target.maxConcurrency:
		reason = AdmissionConcurrencyLimit
	}
	target.mu.Unlock()
	m.observeTransition(transition)
	if reason != "" {
		m.observeRejection(deployment, reason)
		return false
	}
	return true
}

func (m *TargetManager) Acquire(
	deployment string,
) (*TargetLease, AdmissionReason) {
	target, exists := m.targets[deployment]
	if !exists {
		reason := AdmissionUnknownTarget
		m.observeRejection(deployment, reason)
		return nil, reason
	}
	now := m.now()
	target.mu.Lock()
	transition := target.advance(now)
	var reason AdmissionReason
	switch {
	case target.state == CircuitOpen:
		reason = AdmissionCircuitOpen
	case target.state == CircuitHalfOpen && target.probeInFlight:
		reason = AdmissionHalfOpenInFlight
	case target.maxConcurrency > 0 && target.active >= target.maxConcurrency:
		reason = AdmissionConcurrencyLimit
	}
	if reason != "" {
		target.mu.Unlock()
		m.observeTransition(transition)
		m.observeRejection(deployment, reason)
		return nil, reason
	}
	target.active++
	halfOpen := target.state == CircuitHalfOpen
	if halfOpen {
		target.probeInFlight = true
	}
	lease := &TargetLease{
		Deployment:        deployment,
		CircuitState:      target.state,
		HalfOpenProbe:     halfOpen,
		ActiveRequests:    target.active,
		target:            target,
		circuitGeneration: target.generation,
	}
	target.mu.Unlock()
	m.observeTransition(transition)
	m.observeAdmission(deployment, 1)
	return lease, ""
}

// Complete releases one admission and records a health result. It returns false
// for a nil or already-completed lease.
func (m *TargetManager) Complete(lease *TargetLease, result TargetResult) bool {
	return m.CompleteWithFailureClass(lease, result, "")
}

// CompleteWithFailureClass releases one admission and records a health result
// plus an optional normalized, content-free failure class. Arbitrary error
// text is replaced with a fixed fallback before entering target state.
func (m *TargetManager) CompleteWithFailureClass(
	lease *TargetLease,
	result TargetResult,
	failureClass string,
) bool {
	if lease == nil || !lease.completed.CompareAndSwap(false, true) {
		return false
	}
	target := lease.target
	now := m.now()
	target.mu.Lock()
	if target.active > 0 {
		target.active--
	}
	if result == TargetFailure {
		target.recordFailure(now, failureClass)
	}
	var transition *CircuitTransition
	switch {
	case lease.HalfOpenProbe &&
		target.state == CircuitHalfOpen &&
		target.generation == lease.circuitGeneration:
		target.probeInFlight = false
		switch result {
		case TargetSuccess:
			transition = target.close("half_open_success")
		case TargetFailure:
			transition = target.open(now, "half_open_failure")
		}
	case !lease.HalfOpenProbe &&
		target.state == CircuitClosed &&
		target.generation == lease.circuitGeneration:
		transition = target.recordClosedResult(now, result)
	}
	target.mu.Unlock()
	m.observeAdmission(lease.Deployment, -1)
	m.observeTransition(transition)
	return true
}

// SanitizeTargetFailureClass returns one bounded, low-cardinality identifier
// suitable for content-free status surfaces.
func SanitizeTargetFailureClass(value string) string {
	if value == "" {
		return "upstream_failure"
	}
	if len(value) > maxFailureClassLength {
		return "upstream_failure"
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' ||
			char >= '0' && char <= '9' ||
			char == '_' || char == '-' || char == '.' {
			continue
		}
		return "upstream_failure"
	}
	return value
}

func (m *TargetManager) Status(deployment string) (TargetStatus, bool) {
	target, exists := m.targets[deployment]
	if !exists {
		return TargetStatus{}, false
	}
	now := m.now()
	target.mu.Lock()
	transition := target.advance(now)
	status := target.status()
	target.mu.Unlock()
	m.observeTransition(transition)
	return status, true
}

func (m *TargetManager) Statuses() []TargetStatus {
	names := make([]string, 0, len(m.targets))
	for name := range m.targets {
		names = append(names, name)
	}
	sort.Strings(names)
	statuses := make([]TargetStatus, 0, len(names))
	for _, name := range names {
		status, _ := m.Status(name)
		statuses = append(statuses, status)
	}
	return statuses
}

func (t *targetRuntime) advance(now time.Time) *CircuitTransition {
	if t.state != CircuitOpen || now.Before(t.ejectedUntil) {
		return nil
	}
	from := t.state
	t.state = CircuitHalfOpen
	t.generation++
	t.probeInFlight = false
	return &CircuitTransition{
		Deployment:    t.deployment,
		From:          from,
		To:            t.state,
		Reason:        "ejection_elapsed",
		EjectionCount: t.ejectionCount,
	}
}

func (t *targetRuntime) recordClosedResult(
	now time.Time,
	result TargetResult,
) *CircuitTransition {
	if t.policy.Disabled || result == TargetNeutral {
		return nil
	}
	failed := result == TargetFailure
	t.recordSample(failed)
	if failed {
		t.consecutiveFailures++
		if t.consecutiveFailures >= t.policy.ConsecutiveFailures {
			return t.open(now, "consecutive_failures")
		}
		if t.sampleCount >= t.policy.MinimumSamples &&
			float64(t.failuresInWindow)/float64(t.sampleCount) >= t.policy.FailureRate {
			return t.open(now, "failure_rate")
		}
		return nil
	}
	t.consecutiveFailures = 0
	return nil
}

func (t *targetRuntime) recordSample(failed bool) {
	if t.sampleCount < len(t.samples) {
		t.samples[t.sampleCursor] = failed
		t.sampleCount++
	} else {
		if t.samples[t.sampleCursor] {
			t.failuresInWindow--
		}
		t.samples[t.sampleCursor] = failed
	}
	if failed {
		t.failuresInWindow++
	}
	t.sampleCursor = (t.sampleCursor + 1) % len(t.samples)
}

func (t *targetRuntime) recordFailure(now time.Time, failureClass string) {
	event := RecentTargetFailure{
		OccurredAt:   now,
		FailureClass: SanitizeTargetFailureClass(failureClass),
	}
	if len(t.recentFailures) == MaxRecentTargetFailures {
		copy(t.recentFailures, t.recentFailures[1:])
		t.recentFailures[len(t.recentFailures)-1] = event
		return
	}
	t.recentFailures = append(t.recentFailures, event)
}

func (t *targetRuntime) open(now time.Time, reason string) *CircuitTransition {
	from := t.state
	t.state = CircuitOpen
	t.ejectionCount++
	t.ejectedUntil = now.Add(t.ejectionDuration())
	t.generation++
	t.probeInFlight = false
	return &CircuitTransition{
		Deployment:    t.deployment,
		From:          from,
		To:            t.state,
		Reason:        reason,
		EjectionCount: t.ejectionCount,
		EjectedUntil:  t.ejectedUntil,
	}
}

func (t *targetRuntime) close(reason string) *CircuitTransition {
	from := t.state
	t.state = CircuitClosed
	t.consecutiveFailures = 0
	t.sampleCount = 0
	t.sampleCursor = 0
	t.failuresInWindow = 0
	t.ejectionCount = 0
	t.ejectedUntil = time.Time{}
	t.generation++
	t.probeInFlight = false
	clear(t.samples)
	return &CircuitTransition{
		Deployment: t.deployment,
		From:       from,
		To:         t.state,
		Reason:     reason,
	}
}

func (t *targetRuntime) ejectionDuration() time.Duration {
	duration := t.policy.BaseEjectionTime
	for count := 1; count < t.ejectionCount; count++ {
		if duration >= t.policy.MaxEjectionTime ||
			duration > t.policy.MaxEjectionTime/2 {
			return t.policy.MaxEjectionTime
		}
		duration *= 2
	}
	if duration > t.policy.MaxEjectionTime {
		return t.policy.MaxEjectionTime
	}
	return duration
}

func (t *targetRuntime) status() TargetStatus {
	recentFailures := make([]RecentTargetFailure, len(t.recentFailures))
	for index := range t.recentFailures {
		recentFailures[index] = t.recentFailures[len(t.recentFailures)-1-index]
	}
	return TargetStatus{
		Deployment:          t.deployment,
		CircuitState:        t.state,
		ActiveRequests:      t.active,
		MaxConcurrency:      t.maxConcurrency,
		ConsecutiveFailures: t.consecutiveFailures,
		Samples:             t.sampleCount,
		Failures:            t.failuresInWindow,
		EjectionCount:       t.ejectionCount,
		EjectedUntil:        t.ejectedUntil,
		HalfOpenProbeActive: t.probeInFlight,
		RecentFailures:      recentFailures,
	}
}

func (m *TargetManager) observeAdmission(deployment string, delta int64) {
	if m.observer != nil {
		m.observer.TargetAdmissionChanged(deployment, delta)
	}
}

func (m *TargetManager) observeRejection(
	deployment string,
	reason AdmissionReason,
) {
	if m.observer != nil {
		m.observer.TargetAdmissionRejected(AdmissionRejection{
			Deployment: deployment,
			Reason:     reason,
		})
	}
}

func (m *TargetManager) observeTransition(transition *CircuitTransition) {
	if transition != nil && m.observer != nil {
		m.observer.TargetCircuitChanged(*transition)
	}
}
