// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"bytes"
	"errors"
	"testing"
	"time"
)

func TestRecordRejectsOversizedRawUsage(t *testing.T) {
	t.Parallel()

	record := testRequestRecord("request-1")
	record.Request.Usage.Raw = bytes.Repeat([]byte{'0'}, MaxRawUsageBytes+1)
	if err := record.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want raw-usage size error")
	}
}

func TestRecordRejectsMismatchedUnion(t *testing.T) {
	t.Parallel()

	record := testRequestRecord("request-1")
	record.Attempt = testAttemptRecord("request-1", 1).Attempt
	if err := record.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want typed-union error")
	}
}

func TestRecordRejectsUnboundedAttribution(t *testing.T) {
	t.Parallel()

	record := testRequestRecord("request-1")
	record.Request.Attribution = map[string]string{
		"sparkroute.workspace": string(bytes.Repeat([]byte{'x'}, 1025)),
	}
	if err := record.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want attribution size error")
	}
}

func TestRuntimeEventValidationAndCursor(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 3, 12, 0, 0, 123, time.UTC)
	latency := time.Second
	record := NewRuntimeEventRecord(RuntimeEventRecord{
		EventID: "event-1", OccurredAt: now, Controller: "sparkrun",
		PriorState: "activating", NewState: "ready", Reason: "readiness_succeeded",
		Latency: &latency, QueueDepth: 2, WaiterCount: 1, Outcome: RuntimeOutcomeSuccess,
	})
	if err := record.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	cursor, err := RuntimeEventNextCursor(*record.RuntimeEvent)
	if err != nil {
		t.Fatalf("RuntimeEventNextCursor() error = %v", err)
	}
	query := RuntimeEventQuery{Cursor: cursor}
	plan, err := PlanRuntimeEventQuery(&query)
	if err != nil || plan.CursorRequestID != "event-1" || !plan.CursorStartedAt.Equal(now) {
		t.Fatalf("PlanRuntimeEventQuery() = %#v, %v", plan, err)
	}
	requestQuery := RequestQuery{Cursor: cursor}
	if _, err := PlanRequestQuery(&requestQuery); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("PlanRequestQuery() cross-kind error = %v", err)
	}

	for name, mutate := range map[string]func(*RuntimeEventRecord){
		"raw reason":       func(value *RuntimeEventRecord) { value.Reason = "raw error: secret" },
		"invalid state":    func(value *RuntimeEventRecord) { value.NewState = "private" },
		"invalid outcome":  func(value *RuntimeEventRecord) { value.Outcome = "private" },
		"negative counter": func(value *RuntimeEventRecord) { value.QueueDepth = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *record.RuntimeEvent
			mutate(&copy)
			if err := NewRuntimeEventRecord(copy).Validate(); err == nil {
				t.Fatal("Validate() error = nil")
			}
		})
	}
}

func TestPageCursorRoundTripAndKindBinding(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 7, 28, 12, 0, 0, 123456789, time.UTC)
	cursor, err := RequestNextCursor(RequestRecord{
		RequestID: "request-1",
		StartedAt: started,
	})
	if err != nil {
		t.Fatalf("RequestNextCursor() error = %v", err)
	}
	query := RequestQuery{Cursor: cursor}
	plan, err := PlanRequestQuery(&query)
	if err != nil {
		t.Fatalf("PlanRequestQuery() error = %v", err)
	}
	if plan.Limit != DefaultPageSize ||
		plan.CursorRequestID != "request-1" ||
		!plan.CursorStartedAt.Equal(started) {
		t.Fatalf("PlanRequestQuery() = %#v", plan)
	}
	attemptQuery := AttemptQuery{Cursor: cursor}
	if _, err := PlanAttemptQuery(&attemptQuery); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("PlanAttemptQuery() cross-kind error = %v", err)
	}
}

func TestQueryAndRetentionValidation(t *testing.T) {
	t.Parallel()

	after := time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC)
	before := after.Add(-time.Hour)
	for name, query := range map[string]RequestQuery{
		"negative limit": {Limit: -1},
		"oversized limit": {
			Limit: MaxPageSize + 1,
		},
		"inverted range": {
			StartedAtOrAfter: after,
			StartedAtBefore:  before,
		},
		"invalid outcome": {
			Outcome: Outcome("not-an-outcome"),
		},
	} {
		query := query
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := PlanRequestQuery(&query); err == nil {
				t.Fatal("PlanRequestQuery() error = nil")
			}
		})
	}
	if _, err := ValidateRetentionPolicy(RetentionPolicy{}); err == nil {
		t.Fatal("ValidateRetentionPolicy() error = nil")
	}
}
