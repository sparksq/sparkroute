package ledger

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

const (
	// DefaultPageSize is used when a reader query does not specify a limit.
	DefaultPageSize = 100
	// MaxPageSize bounds memory and response growth for one reader operation.
	MaxPageSize = 500
	// MaxAggregateRange bounds one aggregate scan. Callers can page request
	// records over arbitrary history, but dashboards must ask for an explicit,
	// operationally bounded window.
	MaxAggregateRange = 31 * 24 * time.Hour
	// MaxAggregateGroups bounds each high-cardinality summary breakdown.
	MaxAggregateGroups = 20
	maxCursorBytes     = 2048
	cursorVersion      = 1
)

var (
	// ErrNotFound indicates that a logical ledger record does not exist.
	ErrNotFound = errors.New("ledger record not found")
	// ErrInvalidCursor indicates a malformed, unsupported, or cross-kind cursor.
	ErrInvalidCursor = errors.New("invalid ledger cursor")
)

// RequestQuery selects request records in reverse chronological order.
// StartedAtBefore is exclusive and StartedAtOrAfter is inclusive.
type RequestQuery struct {
	StartedAtOrAfter time.Time
	StartedAtBefore  time.Time
	TenantID         string
	VirtualModel     string
	Outcome          Outcome
	Limit            int
	Cursor           string
}

// RequestPage is one bounded page of reverse-chronological request records.
type RequestPage struct {
	Records    []RequestRecord `json:"records"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// AttemptQuery selects attempt records in reverse chronological order.
// StartedAtBefore is exclusive and StartedAtOrAfter is inclusive.
type AttemptQuery struct {
	StartedAtOrAfter time.Time
	StartedAtBefore  time.Time
	RequestID        string
	Provider         string
	Deployment       string
	Outcome          Outcome
	Limit            int
	Cursor           string
}

// AttemptPage is one bounded page of reverse-chronological attempt records.
type AttemptPage struct {
	Records    []AttemptRecord `json:"records"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// RuntimeEventQuery selects lifecycle transitions in reverse chronological
// order. OccurredAtBefore is exclusive and OccurredAtOrAfter is inclusive.
type RuntimeEventQuery struct {
	OccurredAtOrAfter time.Time
	OccurredAtBefore  time.Time
	Controller        string
	VirtualModel      string
	Deployment        string
	Outcome           RuntimeOutcome
	Limit             int
	Cursor            string
}

type RuntimeEventPage struct {
	Records    []RuntimeEventRecord `json:"records"`
	NextCursor string               `json:"next_cursor,omitempty"`
}

// RuntimeEventReader is separate from request history so deployments may
// expose only the lifecycle history their controller records.
type RuntimeEventReader interface {
	ListRuntimeEvents(ctx context.Context, query RuntimeEventQuery) (RuntimeEventPage, error)
}

// Reader is intentionally separate from Store so inference-only deployments do
// not need to expose or initialize an administrative query surface.
type Reader interface {
	GetRequest(ctx context.Context, requestID string) (RequestRecord, error)
	ListRequests(ctx context.Context, query RequestQuery) (RequestPage, error)
	ListAttempts(ctx context.Context, query AttemptQuery) (AttemptPage, error)
}

// AggregateQuery selects request-level operational measurements. Both time
// bounds are required; StartedAtBefore is exclusive and StartedAtOrAfter is
// inclusive. Provider and Deployment match the request's final route.
type AggregateQuery struct {
	StartedAtOrAfter time.Time
	StartedAtBefore  time.Time
	TenantID         string
	VirtualModel     string
	Provider         string
	Deployment       string
	Outcome          Outcome
}

// AggregateLatency contains end-to-end request latency in nanoseconds, matching
// time.Duration's JSON representation on individual ledger records. Percentiles
// use the nearest observed rank and are zero when no requests match.
type AggregateLatency struct {
	Average time.Duration `json:"average"`
	P50     time.Duration `json:"p50"`
	P95     time.Duration `json:"p95"`
	P99     time.Duration `json:"p99"`
}

// AggregateTokenTotals sums the normalized values that were reported. Missing
// token fields contribute zero; UsageCompleteness makes that coverage explicit.
type AggregateTokenTotals struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"`
	CacheCreationTokens      int64 `json:"cache_creation_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	ToolUsePromptTokens      int64 `json:"tool_use_prompt_tokens"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens"`
}

// AggregateUsageCompleteness counts requests by normalized usage coverage.
type AggregateUsageCompleteness struct {
	Missing  int64 `json:"missing"`
	Partial  int64 `json:"partial"`
	Complete int64 `json:"complete"`
}

// AggregateGroup is one value in a dimension breakdown.
type AggregateGroup struct {
	Value string `json:"value"`
	Count int64  `json:"count"`
}

// AggregateBreakdown contains the most frequent values for a dimension.
// Truncated is true when more than MaxAggregateGroups values matched.
type AggregateBreakdown struct {
	Groups    []AggregateGroup `json:"groups"`
	Truncated bool             `json:"truncated"`
}

// AggregateSummary is a content-free, request-level dashboard snapshot.
// Retries is the sum of max(attempt_count-1, 0), while RetriedRequests counts
// requests whose attempt_count exceeded one.
type AggregateSummary struct {
	StartedAtOrAfter time.Time `json:"started_at_or_after"`
	StartedAtBefore  time.Time `json:"started_at_before"`

	Requests           int64 `json:"requests"`
	SuccessfulRequests int64 `json:"successful_requests"`
	StreamingRequests  int64 `json:"streaming_requests"`
	Attempts           int64 `json:"attempts"`
	RetriedRequests    int64 `json:"retried_requests"`
	Retries            int64 `json:"retries"`

	Latency           AggregateLatency           `json:"latency"`
	Tokens            AggregateTokenTotals       `json:"tokens"`
	UsageCompleteness AggregateUsageCompleteness `json:"usage_completeness"`

	ByOutcome      AggregateBreakdown `json:"by_outcome"`
	ByVirtualModel AggregateBreakdown `json:"by_virtual_model"`
	ByProvider     AggregateBreakdown `json:"by_provider"`
	ByDeployment   AggregateBreakdown `json:"by_deployment"`
}

// Aggregator is separate from Reader so adding dashboard support does not
// expand the contract required by existing administrative reader adapters.
type Aggregator interface {
	AggregateRequests(ctx context.Context, query AggregateQuery) (AggregateSummary, error)
}

// RetentionPolicy removes records whose start time is strictly before
// DeleteBefore. Implementations must remove logical-key lookup rows together
// with their physical records.
type RetentionPolicy struct {
	DeleteBefore time.Time
}

// PruneResult reports the logical records removed by one retention operation.
type PruneResult struct {
	RequestsDeleted      int64 `json:"requests_deleted"`
	AttemptsDeleted      int64 `json:"attempts_deleted"`
	RuntimeEventsDeleted int64 `json:"runtime_events_deleted"`
}

// Pruner is separate from Store because retention is an administrative task.
type Pruner interface {
	Prune(ctx context.Context, policy RetentionPolicy) (PruneResult, error)
}

type pageCursor struct {
	Version   int    `json:"v"`
	Kind      Kind   `json:"k"`
	StartedNS int64  `json:"t"`
	RequestID string `json:"r"`
	Attempt   int    `json:"a,omitempty"`
}

func validateRequestQuery(query RequestQuery) (RequestQuery, pageCursor, error) {
	cursorValue := query.Cursor
	tenantID := query.TenantID
	if err := validateBoundedString("tenant_id", query.TenantID, 1024); err != nil {
		return RequestQuery{}, pageCursor{}, err
	}
	query, err := normalizeQuery(
		query.StartedAtOrAfter,
		query.StartedAtBefore,
		query.VirtualModel,
		query.Outcome,
		query.Limit,
	)
	if err != nil {
		return RequestQuery{}, pageCursor{}, err
	}
	query.TenantID = tenantID
	query.Cursor = cursorValue
	var cursor pageCursor
	if query.Cursor != "" {
		cursor, err = decodePageCursor(query.Cursor, KindRequest)
		if err != nil {
			return RequestQuery{}, pageCursor{}, err
		}
	}
	return query, cursor, nil
}

func validateAttemptQuery(query AttemptQuery) (AttemptQuery, pageCursor, error) {
	if err := validateBoundedString("request_id", query.RequestID, 1024); err != nil {
		return AttemptQuery{}, pageCursor{}, err
	}
	if err := validateBoundedString("provider", query.Provider, 1024); err != nil {
		return AttemptQuery{}, pageCursor{}, err
	}
	normalized, err := normalizeQuery(
		query.StartedAtOrAfter,
		query.StartedAtBefore,
		query.Deployment,
		query.Outcome,
		query.Limit,
	)
	if err != nil {
		return AttemptQuery{}, pageCursor{}, err
	}
	query.StartedAtOrAfter = normalized.StartedAtOrAfter
	query.StartedAtBefore = normalized.StartedAtBefore
	query.Limit = normalized.Limit
	var cursor pageCursor
	if query.Cursor != "" {
		cursor, err = decodePageCursor(query.Cursor, KindAttempt)
		if err != nil {
			return AttemptQuery{}, pageCursor{}, err
		}
	}
	return query, cursor, nil
}

func validateRuntimeEventQuery(
	query RuntimeEventQuery,
) (RuntimeEventQuery, pageCursor, error) {
	if !query.OccurredAtOrAfter.IsZero() {
		query.OccurredAtOrAfter = query.OccurredAtOrAfter.UTC()
	}
	if !query.OccurredAtBefore.IsZero() {
		query.OccurredAtBefore = query.OccurredAtBefore.UTC()
	}
	if !query.OccurredAtOrAfter.IsZero() && !query.OccurredAtBefore.IsZero() &&
		!query.OccurredAtOrAfter.Before(query.OccurredAtBefore) {
		return RuntimeEventQuery{}, pageCursor{}, fmt.Errorf(
			"runtime event range must have occurred-at-or-after before occurred-at-before",
		)
	}
	for name, value := range map[string]string{
		"controller":    query.Controller,
		"virtual_model": query.VirtualModel,
		"deployment":    query.Deployment,
	} {
		if err := validateBoundedString(name, value, 1024); err != nil {
			return RuntimeEventQuery{}, pageCursor{}, err
		}
	}
	if query.Outcome != "" && !validRuntimeOutcome(query.Outcome) {
		return RuntimeEventQuery{}, pageCursor{}, fmt.Errorf(
			"invalid runtime outcome %q", query.Outcome,
		)
	}
	switch {
	case query.Limit < 0:
		return RuntimeEventQuery{}, pageCursor{}, fmt.Errorf(
			"runtime event page limit must not be negative",
		)
	case query.Limit == 0:
		query.Limit = DefaultPageSize
	case query.Limit > MaxPageSize:
		return RuntimeEventQuery{}, pageCursor{}, fmt.Errorf(
			"runtime event page limit exceeds %d", MaxPageSize,
		)
	}
	var cursor pageCursor
	var err error
	if query.Cursor != "" {
		cursor, err = decodePageCursor(query.Cursor, KindRuntimeEvent)
		if err != nil {
			return RuntimeEventQuery{}, pageCursor{}, err
		}
	}
	return query, cursor, nil
}

func normalizeQuery(
	after time.Time,
	before time.Time,
	filter string,
	outcome Outcome,
	limit int,
) (RequestQuery, error) {
	if !after.IsZero() {
		after = after.UTC()
	}
	if !before.IsZero() {
		before = before.UTC()
	}
	if !after.IsZero() && !before.IsZero() && !after.Before(before) {
		return RequestQuery{}, fmt.Errorf(
			"ledger start range must have started-at-or-after before started-at-before",
		)
	}
	if err := validateBoundedString("query filter", filter, 1024); err != nil {
		return RequestQuery{}, err
	}
	if outcome != "" {
		if err := validateOutcome(outcome); err != nil {
			return RequestQuery{}, err
		}
	}
	switch {
	case limit < 0:
		return RequestQuery{}, fmt.Errorf("ledger page limit must not be negative")
	case limit == 0:
		limit = DefaultPageSize
	case limit > MaxPageSize:
		return RequestQuery{}, fmt.Errorf(
			"ledger page limit exceeds %d",
			MaxPageSize,
		)
	}
	return RequestQuery{
		StartedAtOrAfter: after,
		StartedAtBefore:  before,
		VirtualModel:     filter,
		Outcome:          outcome,
		Limit:            limit,
	}, nil
}

func validateRetentionPolicy(policy RetentionPolicy) (RetentionPolicy, error) {
	if policy.DeleteBefore.IsZero() {
		return RetentionPolicy{}, fmt.Errorf("ledger retention cutoff is required")
	}
	policy.DeleteBefore = policy.DeleteBefore.UTC()
	return policy, nil
}

func encodeRequestCursor(record RequestRecord) (string, error) {
	if record.RequestID == "" || record.StartedAt.IsZero() {
		return "", fmt.Errorf("request cursor requires request_id and started_at")
	}
	return encodePageCursor(pageCursor{
		Version:   cursorVersion,
		Kind:      KindRequest,
		StartedNS: record.StartedAt.UnixNano(),
		RequestID: record.RequestID,
	})
}

func encodeAttemptCursor(record AttemptRecord) (string, error) {
	if record.RequestID == "" || record.Attempt <= 0 || record.StartedAt.IsZero() {
		return "", fmt.Errorf(
			"attempt cursor requires request_id, positive attempt, and started_at",
		)
	}
	return encodePageCursor(pageCursor{
		Version:   cursorVersion,
		Kind:      KindAttempt,
		StartedNS: record.StartedAt.UnixNano(),
		RequestID: record.RequestID,
		Attempt:   record.Attempt,
	})
}

func encodePageCursor(cursor pageCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode ledger cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodePageCursor(value string, expected Kind) (pageCursor, error) {
	if len(value) > maxCursorBytes {
		return pageCursor{}, fmt.Errorf("%w: cursor is too large", ErrInvalidCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return pageCursor{}, fmt.Errorf("%w: malformed encoding", ErrInvalidCursor)
	}
	var cursor pageCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return pageCursor{}, fmt.Errorf("%w: malformed payload", ErrInvalidCursor)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return pageCursor{}, fmt.Errorf("%w: trailing payload", ErrInvalidCursor)
	}
	if cursor.Version != cursorVersion ||
		cursor.Kind != expected ||
		cursor.RequestID == "" ||
		len(cursor.RequestID) > 1024 ||
		expected == KindAttempt && cursor.Attempt <= 0 ||
		expected != KindAttempt && cursor.Attempt != 0 {
		return pageCursor{}, fmt.Errorf("%w: unsupported payload", ErrInvalidCursor)
	}
	return cursor, nil
}

// QueryPlan is the validated, backend-facing representation of an opaque seek
// cursor. It is not part of the admin wire format.
type QueryPlan struct {
	Limit           int
	CursorStartedAt time.Time
	CursorRequestID string
	CursorAttempt   int
}

// PlanRequestQuery validates and normalizes query in place and decodes its
// opaque cursor for use by a storage backend.
func PlanRequestQuery(query *RequestQuery) (QueryPlan, error) {
	if query == nil {
		return QueryPlan{}, fmt.Errorf("request query is required")
	}
	validated, cursor, err := validateRequestQuery(*query)
	if err != nil {
		return QueryPlan{}, err
	}
	*query = validated
	return cursorPlan(validated.Limit, cursor), nil
}

// PlanAttemptQuery validates and normalizes query in place and decodes its
// opaque cursor for use by a storage backend.
func PlanAttemptQuery(query *AttemptQuery) (QueryPlan, error) {
	if query == nil {
		return QueryPlan{}, fmt.Errorf("attempt query is required")
	}
	validated, cursor, err := validateAttemptQuery(*query)
	if err != nil {
		return QueryPlan{}, err
	}
	*query = validated
	return cursorPlan(validated.Limit, cursor), nil
}

// PlanRuntimeEventQuery validates and normalizes a lifecycle history query.
func PlanRuntimeEventQuery(query *RuntimeEventQuery) (QueryPlan, error) {
	if query == nil {
		return QueryPlan{}, fmt.Errorf("runtime event query is required")
	}
	validated, cursor, err := validateRuntimeEventQuery(*query)
	if err != nil {
		return QueryPlan{}, err
	}
	*query = validated
	return cursorPlan(validated.Limit, cursor), nil
}

func cursorPlan(limit int, cursor pageCursor) QueryPlan {
	plan := QueryPlan{Limit: limit}
	if cursor.RequestID != "" {
		plan.CursorStartedAt = time.Unix(0, cursor.StartedNS).UTC()
		plan.CursorRequestID = cursor.RequestID
		plan.CursorAttempt = cursor.Attempt
	}
	return plan
}

// RequestNextCursor encodes the exclusive seek key following record.
func RequestNextCursor(record RequestRecord) (string, error) {
	return encodeRequestCursor(record)
}

// AttemptNextCursor encodes the exclusive seek key following record.
func AttemptNextCursor(record AttemptRecord) (string, error) {
	return encodeAttemptCursor(record)
}

// RuntimeEventNextCursor encodes the exclusive seek key following record.
func RuntimeEventNextCursor(record RuntimeEventRecord) (string, error) {
	if record.EventID == "" || record.OccurredAt.IsZero() {
		return "", fmt.Errorf("runtime event cursor requires event_id and occurred_at")
	}
	return encodePageCursor(pageCursor{
		Version:   cursorVersion,
		Kind:      KindRuntimeEvent,
		StartedNS: record.OccurredAt.UnixNano(),
		RequestID: record.EventID,
	})
}

// ValidateRetentionPolicy validates and normalizes a retention policy.
func ValidateRetentionPolicy(policy RetentionPolicy) (RetentionPolicy, error) {
	return validateRetentionPolicy(policy)
}

// ValidateAggregateQuery validates and UTC-normalizes an aggregate window.
func ValidateAggregateQuery(query AggregateQuery) (AggregateQuery, error) {
	if query.StartedAtOrAfter.IsZero() || query.StartedAtBefore.IsZero() {
		return AggregateQuery{}, fmt.Errorf("ledger aggregate requires both time bounds")
	}
	query.StartedAtOrAfter = query.StartedAtOrAfter.UTC()
	query.StartedAtBefore = query.StartedAtBefore.UTC()
	if !query.StartedAtOrAfter.Before(query.StartedAtBefore) {
		return AggregateQuery{}, fmt.Errorf(
			"ledger aggregate start range must have started-at-or-after before started-at-before",
		)
	}
	if query.StartedAtBefore.Sub(query.StartedAtOrAfter) > MaxAggregateRange {
		return AggregateQuery{}, fmt.Errorf(
			"ledger aggregate range exceeds %s",
			MaxAggregateRange,
		)
	}
	for name, value := range map[string]string{
		"tenant_id":     query.TenantID,
		"virtual_model": query.VirtualModel,
		"provider":      query.Provider,
		"deployment":    query.Deployment,
	} {
		if err := validateBoundedString(name, value, 1024); err != nil {
			return AggregateQuery{}, err
		}
	}
	if query.Outcome != "" {
		if err := validateOutcome(query.Outcome); err != nil {
			return AggregateQuery{}, err
		}
	}
	return query, nil
}
