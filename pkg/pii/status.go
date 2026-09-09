// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"sort"
	"sync/atomic"
	"time"

	privacycontract "github.com/sparksq/sparkroute/pkg/privacy"
)

const (
	StatusDisabled    = privacycontract.StatusDisabled
	StatusHealthy     = privacycontract.StatusHealthy
	StatusUnavailable = privacycontract.StatusUnavailable
)

// MappingStoreStatus is a content-free aggregate projection of encrypted
// conversation mappings. It deliberately excludes partition values, tokens,
// ciphertext, key identifiers, and raw storage errors.
type MappingStoreStatus = privacycontract.MappingStoreStatus

// MappingStatusSource is an optional backend inspection seam. MappingBackend
// implementations need not expose it, but the built-in SQLite and PostgreSQL
// stores do.
type MappingStatusSource interface {
	PIIMappingStatus(context.Context, time.Time) (MappingStoreStatus, error)
}

// DirectoryStatus adds configured, non-secret retention limits to backend
// aggregates.
type DirectoryStatus struct {
	MappingStoreStatus
	MaximumMappings      int   `json:"maximum_mappings"`
	MaximumOriginalBytes int   `json:"maximum_original_bytes"`
	MaximumConversations int   `json:"maximum_conversations"`
	SlidingTTLSeconds    int64 `json:"sliding_ttl_seconds"`
	AbsoluteTTLSeconds   int64 `json:"absolute_ttl_seconds"`
}

// Status is the safe privacy projection returned by the admin status API.
type Status = privacycontract.Status

type StatusSource = privacycontract.StatusSource

// Monitor combines immutable configuration/detector facts, backend aggregates,
// and content-free process-local counters.
type Monitor struct {
	directory            *Directory
	detectorEntities     []privacycontract.Entity
	configuredModels     int
	conversationModels   int
	mediaRequiredModels  int
	fileInspectionModels int
	inspections          atomic.Uint64
	failures             atomic.Uint64
}

type MonitorOptions struct {
	ConfiguredModels     int
	ConversationModels   int
	MediaRequiredModels  int
	FileInspectionModels int
	Detector             Detector
	Directory            *Directory
}

func NewMonitor(options MonitorOptions) *Monitor {
	monitor := &Monitor{
		directory:            options.Directory,
		configuredModels:     options.ConfiguredModels,
		conversationModels:   options.ConversationModels,
		mediaRequiredModels:  options.MediaRequiredModels,
		fileInspectionModels: options.FileInspectionModels,
	}
	if options.Detector != nil {
		for _, entity := range []Entity{
			EntityEmail, EntityPhone, EntitySSN, EntityCreditCard, EntityIPv4,
			EntityPerson, EntityAddress,
		} {
			if options.Detector.Supports(entity) {
				monitor.detectorEntities = append(monitor.detectorEntities, privacycontract.Entity(entity))
			}
		}
		sort.Slice(monitor.detectorEntities, func(i, j int) bool {
			return monitor.detectorEntities[i] < monitor.detectorEntities[j]
		})
	}
	return monitor
}

func (m *Monitor) RecordInspection() {
	if m != nil {
		m.inspections.Add(1)
	}
}

func (m *Monitor) RecordFailure() {
	if m != nil {
		m.failures.Add(1)
	}
}

func (m *Monitor) PrivacyStatus(ctx context.Context) Status {
	now := time.Now().UTC()
	if m == nil || m.configuredModels == 0 {
		return Status{
			ObservedAt: now, State: StatusDisabled, Backend: "disabled",
			DetectorEntities: []privacycontract.Entity{},
		}
	}
	status := Status{
		ObservedAt: now, State: StatusHealthy, Enabled: true,
		ConfiguredModels:     m.configuredModels,
		ConversationModels:   m.conversationModels,
		MediaRequiredModels:  m.mediaRequiredModels,
		FileInspectionModels: m.fileInspectionModels,
		DetectorEntities:     append([]privacycontract.Entity(nil), m.detectorEntities...),
		Backend:              "none", Inspections: m.inspections.Load(), Failures: m.failures.Load(),
	}
	if m.conversationModels == 0 {
		return status
	}
	if m.directory == nil {
		status.State = StatusUnavailable
		status.Backend = "unavailable"
		return status
	}
	directory, err := m.directory.Status(ctx)
	status.Backend = directory.Backend
	status.LiveMappings = directory.LiveMappings
	status.LiveConversations = directory.LiveConversations
	status.LiveOriginalBytes = directory.LiveOriginalBytes
	status.ExpiredPendingPrune = directory.ExpiredPendingPrune
	status.MaximumMappings = directory.MaximumMappings
	status.MaximumOriginalBytes = directory.MaximumOriginalBytes
	status.MaximumConversations = directory.MaximumConversations
	status.SlidingTTLSeconds = directory.SlidingTTLSeconds
	status.AbsoluteTTLSeconds = directory.AbsoluteTTLSeconds
	if err != nil {
		status.State = StatusUnavailable
		if status.Backend == "" {
			status.Backend = "unavailable"
		}
		return status
	}
	return status
}
