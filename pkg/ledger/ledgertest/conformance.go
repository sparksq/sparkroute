// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgertest provides storage conformance checks for ledger backends.
package ledgertest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

type Store interface {
	ledger.Store
	ledger.Reader
	ledger.RuntimeEventReader
	ledger.Aggregator
	ledger.Pruner
}

// RunConformance exercises the storage-neutral reader and retention contract.
// normalizeTime should match the backend's documented timestamp precision.
func RunConformance(
	t *testing.T,
	store Store,
	normalizeTime func(time.Time) time.Time,
) {
	t.Helper()
	if normalizeTime == nil {
		normalizeTime = func(value time.Time) time.Time { return value.UTC() }
	}
	ctx := context.Background()
	prefix := fmt.Sprintf("conformance-%d-", time.Now().UnixNano())
	base := normalizeTime(time.Date(1975, 6, 1, 12, 0, 0, 123456000, time.UTC))
	pageBefore := normalizeTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
	boundaryOld := normalizeTime(time.Date(1980, 1, 1, 10, 0, 0, 0, time.UTC))
	cutoff := normalizeTime(time.Date(1980, 1, 1, 12, 0, 0, 0, time.UTC))
	boundaryKeep := normalizeTime(time.Date(1980, 1, 1, 14, 0, 0, 0, time.UTC))
	keepTime := normalizeTime(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	zero := int64(0)
	input := int64(7)

	request := func(
		suffix string,
		started time.Time,
		virtualModel string,
		outcome ledger.Outcome,
		usage ledger.TokenUsage,
	) ledger.Record {
		tenantID := "tenant-a"
		if suffix == "b" {
			tenantID = "tenant-b"
		}
		attemptCount := 1
		if suffix == "a" {
			attemptCount = 2
		}
		latency := time.Second
		if suffix == "b" {
			latency = 2 * time.Second
		}
		if suffix == "c" {
			latency = 3 * time.Second
		}
		return ledger.NewRequestRecord(ledger.RequestRecord{
			RequestID:        prefix + suffix,
			StartedAt:        started,
			CompletedAt:      started.Add(latency),
			PrincipalID:      "client-a",
			PrincipalType:    "machine",
			PrincipalSubject: "service-account:a",
			PrincipalRoles:   []string{"inference"},
			TenantID:         tenantID,
			Attribution: map[string]string{
				"sparkroute.tenant":    tenantID,
				"sparkroute.workspace": "workspace-a",
			},
			Protocol:       "openai",
			Operation:      "chat_completions",
			RequestedModel: "requested",
			VirtualModel:   virtualModel,
			AttemptCount:   attemptCount,
			HTTPStatus:     200,
			Outcome:        outcome,
			Latency:        latency,
			Usage:          usage,
		})
	}
	attempt := func(
		requestSuffix string,
		number int,
		started time.Time,
		provider string,
		deployment string,
	) ledger.Record {
		firstByte := started.Add(100 * time.Millisecond)
		return ledger.NewAttemptRecord(ledger.AttemptRecord{
			RequestID:     prefix + requestSuffix,
			Attempt:       number,
			StartedAt:     started,
			FirstByteAt:   &firstByte,
			CompletedAt:   started.Add(time.Second),
			Provider:      provider,
			Deployment:    deployment,
			UpstreamModel: "upstream",
			HTTPStatus:    200,
			Outcome:       ledger.OutcomeSuccess,
			Retried:       number > 1,
			Latency:       time.Second,
			Usage:         ledger.TokenUsage{Completeness: ledger.UsageMissing},
		})
	}
	runtimeEvent := func(
		suffix string,
		occurred time.Time,
		controller string,
		deployment string,
		outcome ledger.RuntimeOutcome,
	) ledger.Record {
		latency := 2 * time.Second
		return ledger.NewRuntimeEventRecord(ledger.RuntimeEventRecord{
			EventID:          prefix + "event-" + suffix,
			OccurredAt:       occurred,
			Controller:       controller,
			BindingRevision:  "binding-1",
			VirtualModel:     "blue",
			Deployment:       deployment,
			EndpointInstance: "endpoint-1",
			ClusterID:        "cluster-1",
			JobID:            "job-1",
			RecipeRevision:   "recipe-1",
			PriorState:       "activating",
			NewState:         "ready",
			Reason:           "readiness_succeeded",
			Latency:          &latency,
			QueueDepth:       2,
			WaiterCount:      1,
			Outcome:          outcome,
		})
	}
	usage := ledger.TokenUsage{
		InputTokens:          &input,
		OutputTokens:         &zero,
		ProviderComponents:   map[string]int64{"cache_read": 3},
		Raw:                  []byte(`{"prompt_tokens":7,"completion_tokens":0}`),
		Completeness:         ledger.UsagePartial,
		NormalizationVersion: "conformance/v1",
	}
	records := []ledger.Record{
		runtimeEvent("a", base, "controller-a", "deployment-a", ledger.RuntimeOutcomeSuccess),
		runtimeEvent("b", base, "controller-a", "deployment-b", ledger.RuntimeOutcomeFailure),
		runtimeEvent("c", base.Add(-time.Second), "controller-b", "deployment-a", ledger.RuntimeOutcomeSuccess),
		runtimeEvent("boundary-old", boundaryOld, "controller-a", "deployment-a", ledger.RuntimeOutcomeSuccess),
		runtimeEvent("boundary-keep", boundaryKeep, "controller-a", "deployment-a", ledger.RuntimeOutcomeSuccess),
		runtimeEvent("keep", keepTime, "controller-a", "deployment-a", ledger.RuntimeOutcomeSuccess),
		attempt("a", 1, base, "provider-a", "deployment-a"),
		attempt("a", 2, base, "provider-a", "deployment-a"),
		attempt("b", 1, base, "provider-b", "deployment-b"),
		attempt("boundary-old", 1, boundaryOld, "provider-a", "deployment-a"),
		attempt("boundary-keep", 1, boundaryKeep, "provider-a", "deployment-a"),
		attempt("keep", 1, keepTime, "provider-a", "deployment-a"),
		request("a", base, "blue", ledger.OutcomeSuccess, usage),
		request(
			"b",
			base,
			"blue",
			ledger.OutcomeUpstreamError,
			ledger.TokenUsage{Completeness: ledger.UsageMissing},
		),
		request(
			"c",
			base.Add(-time.Second),
			"green",
			ledger.OutcomeSuccess,
			ledger.TokenUsage{Completeness: ledger.UsageMissing},
		),
		request(
			"boundary-old",
			boundaryOld,
			"green",
			ledger.OutcomeSuccess,
			ledger.TokenUsage{Completeness: ledger.UsageMissing},
		),
		request(
			"boundary-keep",
			boundaryKeep,
			"green",
			ledger.OutcomeSuccess,
			ledger.TokenUsage{Completeness: ledger.UsageMissing},
		),
		request(
			"keep",
			keepTime,
			"green",
			ledger.OutcomeSuccess,
			ledger.TokenUsage{Completeness: ledger.UsageMissing},
		),
	}
	if err := store.AppendBatch(ctx, records); err != nil {
		t.Fatalf("AppendBatch() conformance error = %v", err)
	}

	got, err := store.GetRequest(ctx, prefix+"a")
	if err != nil {
		t.Fatalf("GetRequest() error = %v", err)
	}
	if got.RequestID != prefix+"a" ||
		got.PrincipalID != "client-a" ||
		got.PrincipalType != "machine" ||
		got.PrincipalSubject != "service-account:a" ||
		len(got.PrincipalRoles) != 1 ||
		got.PrincipalRoles[0] != "inference" ||
		got.TenantID != "tenant-a" ||
		got.Attribution["sparkroute.workspace"] != "workspace-a" ||
		got.Usage.OutputTokens == nil ||
		*got.Usage.OutputTokens != 0 ||
		got.Usage.TotalTokens != nil ||
		got.Usage.ProviderComponents["cache_read"] != 3 ||
		!equalJSON(got.Usage.Raw, usage.Raw) {
		t.Fatalf("GetRequest() record = %#v", got)
	}
	if _, err := store.GetRequest(ctx, prefix+"missing"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRequest() missing error = %v, want ErrNotFound", err)
	}

	first, err := store.ListRequests(ctx, ledger.RequestQuery{
		StartedAtBefore: pageBefore,
		Limit:           2,
	})
	if err != nil {
		t.Fatalf("ListRequests() first page error = %v", err)
	}
	assertRequestIDs(t, first.Records, prefix+"b", prefix+"a")
	if first.NextCursor == "" {
		t.Fatal("ListRequests() first page has no next cursor")
	}
	second, err := store.ListRequests(ctx, ledger.RequestQuery{
		StartedAtBefore: pageBefore,
		Limit:           2,
		Cursor:          first.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListRequests() second page error = %v", err)
	}
	assertRequestIDs(t, second.Records, prefix+"c")
	if second.NextCursor != "" {
		t.Fatalf("ListRequests() terminal cursor = %q, want empty", second.NextCursor)
	}

	filtered, err := store.ListRequests(ctx, ledger.RequestQuery{
		StartedAtBefore: pageBefore,
		VirtualModel:    "blue",
		Outcome:         ledger.OutcomeSuccess,
	})
	if err != nil {
		t.Fatalf("ListRequests() filtered error = %v", err)
	}
	assertRequestIDs(t, filtered.Records, prefix+"a")

	tenantFiltered, err := store.ListRequests(ctx, ledger.RequestQuery{
		StartedAtBefore: pageBefore,
		TenantID:        "tenant-b",
	})
	if err != nil {
		t.Fatalf("ListRequests() tenant filter error = %v", err)
	}
	assertRequestIDs(t, tenantFiltered.Records, prefix+"b")

	attempts, err := store.ListAttempts(ctx, ledger.AttemptQuery{
		StartedAtBefore: pageBefore,
		RequestID:       prefix + "a",
		Provider:        "provider-a",
		Deployment:      "deployment-a",
		Limit:           1,
	})
	if err != nil {
		t.Fatalf("ListAttempts() first page error = %v", err)
	}
	assertAttempts(t, attempts.Records, prefix+"a", 2)
	if attempts.NextCursor == "" {
		t.Fatal("ListAttempts() first page has no next cursor")
	}
	attempts, err = store.ListAttempts(ctx, ledger.AttemptQuery{
		StartedAtBefore: pageBefore,
		RequestID:       prefix + "a",
		Provider:        "provider-a",
		Deployment:      "deployment-a",
		Limit:           1,
		Cursor:          attempts.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListAttempts() second page error = %v", err)
	}
	assertAttempts(t, attempts.Records, prefix+"a", 1)
	if attempts.NextCursor != "" {
		t.Fatalf("ListAttempts() terminal cursor = %q, want empty", attempts.NextCursor)
	}

	if _, err := store.ListRequests(
		ctx,
		ledger.RequestQuery{Cursor: "not-a-cursor"},
	); !errors.Is(err, ledger.ErrInvalidCursor) {
		t.Fatalf("ListRequests() invalid cursor error = %v", err)
	}

	runtimeEvents, err := store.ListRuntimeEvents(ctx, ledger.RuntimeEventQuery{
		OccurredAtBefore: pageBefore,
		Controller:       "controller-a",
		VirtualModel:     "blue",
		Limit:            1,
	})
	if err != nil {
		t.Fatalf("ListRuntimeEvents() first page error = %v", err)
	}
	assertRuntimeEvents(t, runtimeEvents.Records, prefix+"event-b")
	if runtimeEvents.NextCursor == "" {
		t.Fatal("ListRuntimeEvents() first page has no next cursor")
	}
	runtimeEvents, err = store.ListRuntimeEvents(ctx, ledger.RuntimeEventQuery{
		OccurredAtBefore: pageBefore,
		Controller:       "controller-a",
		VirtualModel:     "blue",
		Outcome:          ledger.RuntimeOutcomeSuccess,
		Limit:            1,
		Cursor:           runtimeEvents.NextCursor,
	})
	if err != nil {
		t.Fatalf("ListRuntimeEvents() second page error = %v", err)
	}
	assertRuntimeEvents(t, runtimeEvents.Records, prefix+"event-a")
	if record := runtimeEvents.Records[0]; record.Latency == nil ||
		*record.Latency != 2*time.Second || record.Reason != "readiness_succeeded" ||
		record.QueueDepth != 2 || record.WaiterCount != 1 {
		t.Fatalf("ListRuntimeEvents() record = %#v", record)
	}
	if _, err := store.ListRuntimeEvents(
		ctx,
		ledger.RuntimeEventQuery{Cursor: "not-a-cursor"},
	); !errors.Is(err, ledger.ErrInvalidCursor) {
		t.Fatalf("ListRuntimeEvents() invalid cursor error = %v", err)
	}

	summary, err := store.AggregateRequests(ctx, ledger.AggregateQuery{
		StartedAtOrAfter: base.Add(-time.Minute),
		StartedAtBefore:  base.Add(time.Minute),
		TenantID:         "tenant-a",
		VirtualModel:     "blue",
	})
	if err != nil {
		t.Fatalf("AggregateRequests() error = %v", err)
	}
	if summary.Requests != 1 ||
		summary.SuccessfulRequests != 1 ||
		summary.Attempts != 2 ||
		summary.RetriedRequests != 1 ||
		summary.Retries != 1 ||
		summary.Latency.P95 != time.Second ||
		summary.Tokens.InputTokens != 7 ||
		summary.Tokens.OutputTokens != 0 ||
		summary.UsageCompleteness.Partial != 1 ||
		summary.UsageCompleteness.Missing != 0 ||
		len(summary.ByVirtualModel.Groups) != 1 ||
		summary.ByVirtualModel.Groups[0].Value != "blue" ||
		summary.ByVirtualModel.Groups[0].Count != 1 {
		t.Fatalf("AggregateRequests() = %#v", summary)
	}
	latencySummary, err := store.AggregateRequests(ctx, ledger.AggregateQuery{
		StartedAtOrAfter: base.Add(-time.Minute),
		StartedAtBefore:  base.Add(time.Minute),
		TenantID:         "tenant-a",
	})
	if err != nil {
		t.Fatalf("AggregateRequests() latency error = %v", err)
	}
	if latencySummary.Requests != 2 ||
		latencySummary.Latency.Average != 2*time.Second ||
		latencySummary.Latency.P50 != time.Second ||
		latencySummary.Latency.P95 != 3*time.Second ||
		latencySummary.Latency.P99 != 3*time.Second {
		t.Fatalf("AggregateRequests() latency = %#v", latencySummary)
	}

	pruned, err := store.Prune(ctx, ledger.RetentionPolicy{
		DeleteBefore: cutoff,
	})
	if err != nil {
		t.Fatalf("Prune() error = %v", err)
	}
	if pruned.RequestsDeleted != 4 || pruned.AttemptsDeleted != 4 ||
		pruned.RuntimeEventsDeleted != 4 {
		t.Fatalf("Prune() = %#v, want four requests, attempts, and runtime events", pruned)
	}
	if _, err := store.GetRequest(ctx, prefix+"a"); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRequest() pruned record error = %v, want ErrNotFound", err)
	}
	if _, err := store.GetRequest(
		ctx,
		prefix+"boundary-old",
	); !errors.Is(err, ledger.ErrNotFound) {
		t.Fatalf("GetRequest() boundary-pruned error = %v, want ErrNotFound", err)
	}
	if remaining, err := store.GetRequest(ctx, prefix+"keep"); err != nil {
		t.Fatalf("GetRequest() retained record error = %v", err)
	} else if remaining.RequestID != prefix+"keep" {
		t.Fatalf("GetRequest() retained ID = %q", remaining.RequestID)
	}
	if remaining, err := store.GetRequest(ctx, prefix+"boundary-keep"); err != nil {
		t.Fatalf("GetRequest() boundary-retained record error = %v", err)
	} else if remaining.RequestID != prefix+"boundary-keep" {
		t.Fatalf("GetRequest() boundary-retained ID = %q", remaining.RequestID)
	}
}

func equalJSON(left, right []byte) bool {
	var leftValue, rightValue any
	if json.Unmarshal(left, &leftValue) != nil || json.Unmarshal(right, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func assertRequestIDs(
	t *testing.T,
	records []ledger.RequestRecord,
	expected ...string,
) {
	t.Helper()
	if len(records) != len(expected) {
		t.Fatalf("request count = %d, want %d: %#v", len(records), len(expected), records)
	}
	for index := range expected {
		if records[index].RequestID != expected[index] {
			t.Fatalf(
				"request %d ID = %q, want %q",
				index,
				records[index].RequestID,
				expected[index],
			)
		}
	}
}

func assertAttempts(
	t *testing.T,
	records []ledger.AttemptRecord,
	requestID string,
	attempt int,
) {
	t.Helper()
	if len(records) != 1 ||
		records[0].RequestID != requestID ||
		records[0].Attempt != attempt {
		t.Fatalf(
			"attempt records = %#v, want request %q attempt %d",
			records,
			requestID,
			attempt,
		)
	}
}

func assertRuntimeEvents(
	t *testing.T,
	records []ledger.RuntimeEventRecord,
	eventID string,
) {
	t.Helper()
	if len(records) != 1 || records[0].EventID != eventID {
		t.Fatalf("runtime events = %#v, want event %q", records, eventID)
	}
}
