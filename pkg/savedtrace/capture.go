// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package savedtrace

import (
	"context"
	"fmt"
	"time"
)

type CaptureOutcome string

const (
	CaptureComplete   CaptureOutcome = "complete"
	CaptureTruncated  CaptureOutcome = "truncated"
	CaptureIncomplete CaptureOutcome = "incomplete"
)

// EffectiveCaptureOutcome preserves compatibility with version-one records
// written before capture_outcome became explicit.
func (r Record) EffectiveCaptureOutcome() CaptureOutcome {
	if r.CaptureOutcome != "" {
		return r.CaptureOutcome
	}
	if r.Request.Truncated || r.Response.Truncated {
		return CaptureTruncated
	}
	return CaptureComplete
}

type CaptureSession struct {
	ID            string     `json:"id"`
	StartedAt     time.Time  `json:"started_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Mode          string     `json:"mode"`
	Accepted      uint64     `json:"accepted"`
	Persisted     uint64     `json:"persisted"`
	Pending       uint64     `json:"pending"`
	Lost          uint64     `json:"lost"`
	Invalid       uint64     `json:"invalid"`
	QueueFull     uint64     `json:"queue_full"`
	Closed        uint64     `json:"closed"`
	StoreFailures uint64     `json:"store_failures"`
}

func (s CaptureSession) Validate() error {
	if err := validateString("capture session ID", s.ID, 128); err != nil {
		return err
	}
	if s.ID == "" || s.StartedAt.IsZero() || s.UpdatedAt.IsZero() || s.Mode == "" {
		return fmt.Errorf("capture session ID, timestamps, and mode are required")
	}
	if s.UpdatedAt.Before(s.StartedAt) {
		return fmt.Errorf("capture session update precedes start")
	}
	if s.CompletedAt != nil && s.CompletedAt.Before(s.StartedAt) {
		return fmt.Errorf("capture session completion precedes start")
	}
	if s.Persisted+s.Pending > s.Accepted {
		return fmt.Errorf("capture session counters are inconsistent")
	}
	return nil
}

type CaptureSessionQuery struct {
	StartedBefore time.Time
	UpdatedAfter  time.Time
}

// CaptureSessionStore is optional. Dataset exporters use it to prove capture
// completeness across every gateway replica sharing the trace store.
type CaptureSessionStore interface {
	UpsertCaptureSession(context.Context, CaptureSession) error
	ListCaptureSessions(context.Context, CaptureSessionQuery) ([]CaptureSession, error)
}
