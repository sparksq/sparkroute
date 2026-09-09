// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/routing"
)

// Health owns process liveness and configuration readiness.
type Health struct {
	ready             atomic.Bool
	targets           atomic.Value
	credentialSources atomic.Value
}

type targetStatusSourceValue struct {
	source routing.TargetStatusSource
}

type credentialStatusSourceValue struct {
	source credentials.StatusSource
}

type TargetHealthStatus string

const (
	TargetHealthHealthy     TargetHealthStatus = "healthy"
	TargetHealthDegraded    TargetHealthStatus = "degraded"
	TargetHealthUnavailable TargetHealthStatus = "unavailable"
)

type TargetHealth struct {
	Status      TargetHealthStatus  `json:"status"`
	Total       int                 `json:"total"`
	Available   int                 `json:"available"`
	Saturated   int                 `json:"saturated"`
	Ejected     int                 `json:"ejected"`
	HalfOpen    int                 `json:"half_open"`
	Deployments []TargetHealthEntry `json:"deployments"`
}

type TargetHealthEntry struct {
	Deployment          string               `json:"deployment"`
	CircuitState        routing.CircuitState `json:"circuit_state"`
	AdmissionAvailable  bool                 `json:"admission_available"`
	ActiveRequests      int                  `json:"active_requests"`
	MaxConcurrency      int                  `json:"max_concurrency"`
	EjectedUntil        *time.Time           `json:"ejected_until,omitempty"`
	HalfOpenProbeActive bool                 `json:"half_open_probe_active"`
}

func NewHealth() *Health {
	health := &Health{}
	health.targets.Store(targetStatusSourceValue{})
	health.credentialSources.Store(credentialStatusSourceValue{})
	return health
}

func (h *Health) SetReady(ready bool) {
	h.ready.Store(ready)
}

func (h *Health) Ready() bool {
	return h.ready.Load()
}

// SetTargetStatusSource changes the runtime target state observed on the
// operations listener. It is safe to call during an atomic configuration
// replacement.
func (h *Health) SetTargetStatusSource(source routing.TargetStatusSource) {
	h.targets.Store(targetStatusSourceValue{source: source})
}

// SetCredentialStatusSource changes the active, content-free credential
// projection observed on the operations listener.
func (h *Health) SetCredentialStatusSource(source credentials.StatusSource) {
	h.credentialSources.Store(credentialStatusSourceValue{source: source})
}

func (h *Health) CredentialStatus() credentials.Status {
	value := h.credentialSources.Load().(credentialStatusSourceValue)
	if value.source == nil {
		return credentials.Status{
			ObservedAt: time.Now().UTC(),
			Status:     credentials.ResolutionUnavailable,
			Sources:    []credentials.SourceStatus{},
		}
	}
	return value.source.CredentialStatus()
}

func (h *Health) TargetHealth() TargetHealth {
	value := h.targets.Load().(targetStatusSourceValue)
	if value.source == nil {
		return TargetHealth{
			Status:      TargetHealthUnavailable,
			Deployments: []TargetHealthEntry{},
		}
	}
	statuses := append([]routing.TargetStatus(nil), value.source.Statuses()...)
	sort.Slice(statuses, func(i, j int) bool {
		return statuses[i].Deployment < statuses[j].Deployment
	})
	result := TargetHealth{
		Status:      TargetHealthHealthy,
		Total:       len(statuses),
		Deployments: make([]TargetHealthEntry, 0, len(statuses)),
	}
	for _, status := range statuses {
		entry := TargetHealthEntry{
			Deployment:          status.Deployment,
			CircuitState:        status.CircuitState,
			AdmissionAvailable:  status.AdmissionAvailable(),
			ActiveRequests:      status.ActiveRequests,
			MaxConcurrency:      status.MaxConcurrency,
			HalfOpenProbeActive: status.HalfOpenProbeActive,
		}
		if !status.EjectedUntil.IsZero() {
			ejectedUntil := status.EjectedUntil
			entry.EjectedUntil = &ejectedUntil
		}
		if entry.AdmissionAvailable {
			result.Available++
		}
		if status.AtCapacity() {
			result.Saturated++
		}
		switch status.CircuitState {
		case routing.CircuitOpen:
			result.Ejected++
		case routing.CircuitHalfOpen:
			result.HalfOpen++
		}
		result.Deployments = append(result.Deployments, entry)
	}
	switch {
	case result.Total == 0 || result.Available == 0:
		result.Status = TargetHealthUnavailable
	case result.Ejected > 0 || result.HalfOpen > 0 || result.Saturated > 0:
		result.Status = TargetHealthDegraded
	}
	return result
}

func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, request *http.Request) {
		if !allowGET(w, request) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, request *http.Request) {
		if !allowGET(w, request) {
			return
		}
		if !h.Ready() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "not_ready"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("/health/targets", func(w http.ResponseWriter, request *http.Request) {
		if !allowGET(w, request) {
			return
		}
		status := h.TargetHealth()
		httpStatus := http.StatusOK
		if status.Status == TargetHealthUnavailable {
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(w, httpStatus, status)
	})
	mux.HandleFunc("/health/credentials", func(w http.ResponseWriter, request *http.Request) {
		if !allowGET(w, request) {
			return
		}
		status := h.CredentialStatus()
		httpStatus := http.StatusOK
		if status.Status == credentials.ResolutionUnavailable {
			httpStatus = http.StatusServiceUnavailable
		}
		writeJSON(w, httpStatus, status)
	})
	return mux
}

func allowGET(w http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	return false
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
