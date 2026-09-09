// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package lifecycle

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
)

const (
	MaxStatusControllers = 64
	MaxStatusBindings    = 500
	MaxReasonBytes       = 64
)

type ControllerHealth string

const (
	ControllerHealthy     ControllerHealth = "healthy"
	ControllerDegraded    ControllerHealth = "degraded"
	ControllerUnavailable ControllerHealth = "unavailable"
	ControllerUnknown     ControllerHealth = "unknown"
)

// ControllerStatus is a bounded, content-free operational projection. It does
// not carry controller credentials, addresses, commands, or raw errors.
type ControllerStatus struct {
	Controller      string           `json:"controller"`
	Health          ControllerHealth `json:"health"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Bindings        int              `json:"bindings"`
	Endpoints       int              `json:"endpoints"`
	ReadyEndpoints  int              `json:"ready_endpoints"`
	ActiveLeases    int              `json:"active_leases"`
	QueuedWaiters   int              `json:"queued_waiters"`
	QueuedBodyBytes int64            `json:"queued_body_bytes"`
	Reason          string           `json:"reason,omitempty"`
}

// BindingStatus describes one configured activation binding and its bounded
// cold-start admission state.
type BindingStatus struct {
	PluginsInUse       []string               `json:"plugins_in_use,omitempty"`
	LifecycleActions   []string               `json:"lifecycle_actions,omitempty"`
	Owned              *bool                  `json:"owned,omitempty"`
	Phase              string                 `json:"phase,omitempty"`
	JobID              string                 `json:"job_id,omitempty"`
	ClusterCandidates  []string               `json:"cluster_candidates,omitempty"`
	Controller         string                 `json:"controller"`
	BindingRevision    string                 `json:"binding_revision"`
	VirtualModel       string                 `json:"virtual_model,omitempty"`
	Deployment         string                 `json:"deployment"`
	State              endpointregistry.State `json:"state"`
	UpdatedAt          time.Time              `json:"updated_at"`
	ActivationStarted  *time.Time             `json:"activation_started_at,omitempty"`
	ActivationDeadline *time.Time             `json:"activation_deadline,omitempty"`
	EndpointID         string                 `json:"endpoint_id,omitempty"`
	ActiveLeases       int                    `json:"active_leases"`
	QueuedWaiters      int                    `json:"queued_waiters"`
	QueuedBodyBytes    int64                  `json:"queued_body_bytes"`
	MaxQueuedWaiters   int                    `json:"max_queued_waiters"`
	MaxQueuedBodyBytes int64                  `json:"max_queued_body_bytes"`
	Reason             string                 `json:"reason,omitempty"`
}

type Snapshot struct {
	ObservedAt  time.Time          `json:"observed_at"`
	Controllers []ControllerStatus `json:"controllers"`
	Bindings    []BindingStatus    `json:"bindings"`
}

// StatusSource is implemented by lifecycle controllers or an adapter that
// merges their current state. Snapshot must be safe for concurrent callers.
type StatusSource interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// MemoryStatusSource is an atomic process-local status projection suitable for
// standalone controllers and tests.
type MemoryStatusSource struct {
	mu       sync.RWMutex
	snapshot Snapshot
	now      func() time.Time
}

func NewMemoryStatusSource() *MemoryStatusSource {
	return &MemoryStatusSource{now: time.Now}
}

// Publish validates, clones, sorts, and atomically replaces current status.
func (s *MemoryStatusSource) Publish(snapshot Snapshot) error {
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = s.now().UTC()
	} else {
		snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	}
	if len(snapshot.Controllers) > MaxStatusControllers {
		return fmt.Errorf("lifecycle snapshot has too many controllers")
	}
	if len(snapshot.Bindings) > MaxStatusBindings {
		return fmt.Errorf("lifecycle snapshot has too many bindings")
	}
	for index := range snapshot.Controllers {
		if err := validateControllerStatus(&snapshot.Controllers[index]); err != nil {
			return err
		}
	}
	for index := range snapshot.Bindings {
		if err := validateBindingStatus(&snapshot.Bindings[index]); err != nil {
			return err
		}
	}
	sort.Slice(snapshot.Controllers, func(i, j int) bool {
		return snapshot.Controllers[i].Controller < snapshot.Controllers[j].Controller
	})
	sort.Slice(snapshot.Bindings, func(i, j int) bool {
		left, right := snapshot.Bindings[i], snapshot.Bindings[j]
		if left.Controller != right.Controller {
			return left.Controller < right.Controller
		}
		if left.Deployment != right.Deployment {
			return left.Deployment < right.Deployment
		}
		return left.BindingRevision < right.BindingRevision
	})
	s.mu.Lock()
	s.snapshot = cloneSnapshot(snapshot)
	s.mu.Unlock()
	return nil
}

func (s *MemoryStatusSource) Snapshot(ctx context.Context) (Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	s.mu.RLock()
	snapshot := cloneSnapshot(s.snapshot)
	s.mu.RUnlock()
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = s.now().UTC()
	}
	if snapshot.Controllers == nil {
		snapshot.Controllers = []ControllerStatus{}
	}
	if snapshot.Bindings == nil {
		snapshot.Bindings = []BindingStatus{}
	}
	return snapshot, nil
}

func validateControllerStatus(status *ControllerStatus) error {
	if err := validateStatusString("controller", status.Controller, true); err != nil {
		return err
	}
	if status.Health == "" {
		status.Health = ControllerUnknown
	}
	switch status.Health {
	case ControllerHealthy, ControllerDegraded, ControllerUnavailable, ControllerUnknown:
	default:
		return fmt.Errorf("unsupported controller health %q", status.Health)
	}
	if status.UpdatedAt.IsZero() {
		return fmt.Errorf("controller updated_at is required")
	}
	status.UpdatedAt = status.UpdatedAt.UTC()
	if status.Bindings < 0 || status.Endpoints < 0 || status.ReadyEndpoints < 0 ||
		status.ActiveLeases < 0 || status.QueuedWaiters < 0 || status.QueuedBodyBytes < 0 {
		return fmt.Errorf("controller status counters must not be negative")
	}
	return validateReason(status.Reason)
}

func validateBindingStatus(status *BindingStatus) error {
	if len(status.PluginsInUse) > 64 || len(status.LifecycleActions) > 3 {
		return fmt.Errorf("too many workload plugin capabilities")
	}
	for _, name := range status.PluginsInUse {
		if len(name) > 64 {
			return fmt.Errorf("plugin name too long")
		}
		if err := validateStatusString("plugin", name, true); err != nil {
			return err
		}
	}
	for _, action := range status.LifecycleActions {
		if action != "status" && action != "sleep" && action != "wake" {
			return fmt.Errorf("invalid workload lifecycle action")
		}
	}

	if err := validateReason(status.Phase); err != nil {
		return err
	}
	if len(status.ClusterCandidates) > 64 {
		return fmt.Errorf("too many cluster candidates")
	}
	for _, name := range status.ClusterCandidates {
		if err := validateStatusString("cluster", name, true); err != nil {
			return err
		}
	}
	for name, value := range map[string]string{
		"controller":       status.Controller,
		"binding_revision": status.BindingRevision,
		"virtual_model":    status.VirtualModel,
		"deployment":       status.Deployment,
		"endpoint_id":      status.EndpointID,
		"job_id":           status.JobID,
	} {
		required := name == "controller" || name == "binding_revision" || name == "deployment"
		if err := validateStatusString(name, value, required); err != nil {
			return err
		}
	}
	if !validEndpointState(status.State) {
		return fmt.Errorf("unsupported lifecycle binding state %q", status.State)
	}
	if status.UpdatedAt.IsZero() {
		return fmt.Errorf("lifecycle binding updated_at is required")
	}
	status.UpdatedAt = status.UpdatedAt.UTC()
	for _, value := range []*time.Time{status.ActivationStarted, status.ActivationDeadline} {
		if value != nil {
			utc := value.UTC()
			*value = utc
		}
	}
	if status.ActivationStarted != nil && status.ActivationDeadline != nil &&
		!status.ActivationStarted.Before(*status.ActivationDeadline) {
		return fmt.Errorf("activation start must precede deadline")
	}
	if status.ActiveLeases < 0 || status.QueuedWaiters < 0 || status.QueuedBodyBytes < 0 ||
		status.MaxQueuedWaiters < 0 || status.MaxQueuedBodyBytes < 0 {
		return fmt.Errorf("lifecycle binding counters must not be negative")
	}
	return validateReason(status.Reason)
}

func validateStatusString(name, value string, required bool) error {
	if required && value == "" {
		return fmt.Errorf("lifecycle %s is required", name)
	}
	if len(value) > 1024 {
		return fmt.Errorf("lifecycle %s exceeds 1024 bytes", name)
	}
	return nil
}

func validEndpointState(state endpointregistry.State) bool {
	switch state {
	case endpointregistry.StateOffline, endpointregistry.StateActivating,
		endpointregistry.StateReady, endpointregistry.StateDraining,
		endpointregistry.StateDeactivating, endpointregistry.StateFailed,
		endpointregistry.StateUnknown:
		return true
	default:
		return false
	}
}

func validateReason(reason string) error {
	if reason == "" {
		return nil
	}
	if reason != SanitizeReason(reason) {
		return fmt.Errorf("lifecycle reason must be a sanitized low-cardinality identifier")
	}
	return nil
}

// SanitizeReason converts controller-provided classes into a bounded identifier
// suitable for administration, telemetry, and runtime transition records.
func SanitizeReason(reason string) string {
	reason = strings.ToLower(strings.TrimSpace(reason))
	var result strings.Builder
	result.Grow(min(len(reason), MaxReasonBytes))
	lastSeparator := false
	for _, current := range reason {
		valid := current >= 'a' && current <= 'z' ||
			current >= '0' && current <= '9' ||
			current == '-' || current == '_'
		if !valid {
			current = '_'
		}
		separator := current == '_' || current == '-'
		if separator && lastSeparator {
			continue
		}
		if result.Len()+len(string(current)) > MaxReasonBytes {
			break
		}
		result.WriteRune(current)
		lastSeparator = separator
	}
	value := strings.Trim(result.String(), "_-")
	if value == "" {
		return "unspecified"
	}
	return value
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	result := snapshot
	result.Controllers = append([]ControllerStatus(nil), snapshot.Controllers...)
	result.Bindings = append([]BindingStatus(nil), snapshot.Bindings...)
	for index := range result.Bindings {
		if result.Bindings[index].Owned != nil {
			value := *result.Bindings[index].Owned
			result.Bindings[index].Owned = &value
		}
		result.Bindings[index].PluginsInUse = append([]string(nil), result.Bindings[index].PluginsInUse...)
		result.Bindings[index].LifecycleActions = append([]string(nil), result.Bindings[index].LifecycleActions...)
		result.Bindings[index].ClusterCandidates = append([]string(nil), result.Bindings[index].ClusterCandidates...)
		if result.Bindings[index].ActivationStarted != nil {
			value := *result.Bindings[index].ActivationStarted
			result.Bindings[index].ActivationStarted = &value
		}
		if result.Bindings[index].ActivationDeadline != nil {
			value := *result.Bindings[index].ActivationDeadline
			result.Bindings[index].ActivationDeadline = &value
		}
	}
	return result
}

var _ StatusSource = (*MemoryStatusSource)(nil)
