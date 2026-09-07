package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/routing"
)

func TestHealthReadiness(t *testing.T) {
	t.Parallel()

	health := NewHealth()
	request := httptest.NewRequest(http.MethodGet, "/health/ready", nil)

	response := httptest.NewRecorder()
	health.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("initial status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}

	health.SetReady(true)
	response = httptest.NewRecorder()
	health.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", response.Code, http.StatusOK)
	}
}

type staticTargetStatuses []routing.TargetStatus

func (s staticTargetStatuses) Statuses() []routing.TargetStatus {
	return append([]routing.TargetStatus(nil), s...)
}

type staticCredentialStatus struct {
	status credentials.Status
}

func (s staticCredentialStatus) CredentialStatus() credentials.Status {
	return s.status
}

func TestHealthTargetSummary(t *testing.T) {
	t.Parallel()

	ejectedUntil := time.Unix(200, 0).UTC()
	health := NewHealth()
	health.SetTargetStatusSource(staticTargetStatuses{
		{
			Deployment:     "available",
			CircuitState:   routing.CircuitClosed,
			ActiveRequests: 1,
			MaxConcurrency: 2,
		},
		{
			Deployment:     "saturated",
			CircuitState:   routing.CircuitClosed,
			ActiveRequests: 1,
			MaxConcurrency: 1,
		},
		{
			Deployment:   "ejected",
			CircuitState: routing.CircuitOpen,
			EjectedUntil: ejectedUntil,
		},
	})
	response := httptest.NewRecorder()
	health.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/health/targets", nil),
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var status TargetHealth
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if status.Status != TargetHealthDegraded ||
		status.Total != 3 ||
		status.Available != 1 ||
		status.Saturated != 1 ||
		status.Ejected != 1 {
		t.Fatalf("target health = %#v", status)
	}
	if status.Deployments[0].Deployment != "available" ||
		status.Deployments[1].Deployment != "ejected" ||
		status.Deployments[2].Deployment != "saturated" {
		t.Fatalf("target order = %#v", status.Deployments)
	}
	if status.Deployments[1].EjectedUntil == nil ||
		!status.Deployments[1].EjectedUntil.Equal(ejectedUntil) {
		t.Fatalf("ejected target = %#v", status.Deployments[1])
	}
}

func TestHealthTargetsUnavailableDoesNotChangeReadiness(t *testing.T) {
	t.Parallel()

	health := NewHealth()
	health.SetReady(true)
	health.SetTargetStatusSource(staticTargetStatuses{{
		Deployment:   "ejected",
		CircuitState: routing.CircuitOpen,
	}})
	targets := httptest.NewRecorder()
	health.Handler().ServeHTTP(
		targets,
		httptest.NewRequest(http.MethodGet, "/health/targets", nil),
	)
	if targets.Code != http.StatusServiceUnavailable {
		t.Fatalf("target status = %d, want %d", targets.Code, http.StatusServiceUnavailable)
	}
	ready := httptest.NewRecorder()
	health.Handler().ServeHTTP(
		ready,
		httptest.NewRequest(http.MethodGet, "/health/ready", nil),
	)
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", ready.Code, http.StatusOK)
	}
}

func TestHealthCredentialSummaryAndReadinessIndependence(t *testing.T) {
	t.Parallel()

	health := NewHealth()
	health.SetReady(true)
	health.SetCredentialStatusSource(staticCredentialStatus{status: credentials.Status{
		ObservedAt:           time.Unix(200, 0).UTC(),
		Status:               credentials.ResolutionUnavailable,
		ConfiguredReferences: 1,
		FailingReferences:    1,
		Sources: []credentials.SourceStatus{{
			Scheme:               "k8s",
			Status:               credentials.ResolutionUnavailable,
			ConfiguredReferences: 1,
			FailingReferences:    1,
			ResolutionAttempts:   3,
			ResolutionFailures:   1,
		}},
	}})
	response := httptest.NewRecorder()
	health.Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/health/credentials", nil),
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	var status credentials.Status
	if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if status.Status != credentials.ResolutionUnavailable ||
		len(status.Sources) != 1 ||
		status.Sources[0].Scheme != "k8s" {
		t.Fatalf("credential health = %#v", status)
	}
	ready := httptest.NewRecorder()
	health.Handler().ServeHTTP(
		ready,
		httptest.NewRequest(http.MethodGet, "/health/ready", nil),
	)
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness status = %d, want %d", ready.Code, http.StatusOK)
	}
}

func TestHealthRejectsMutationMethods(t *testing.T) {
	t.Parallel()

	response := httptest.NewRecorder()
	NewHealth().Handler().ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/health/live", nil),
	)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}
