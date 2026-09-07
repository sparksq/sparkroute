package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const requestSelectColumns = `
	request_id, started_at, completed_at, protocol, operation, stream,
	principal_id, principal_type, principal_subject, principal_roles_json,
	tenant_id, attribution_json,
	requested_model, virtual_model, response_presented_model, config_revision,
	final_provider, final_deployment, final_upstream_model, attempt_count,
	http_status, outcome, failure_class, latency_ns, time_to_first_byte_ns,
	input_tokens, output_tokens, total_tokens, cached_input_tokens,
	cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
	accepted_prediction_tokens, rejected_prediction_tokens,
	provider_components_json, raw_usage_json, usage_completeness,
	normalization_version
`

const attemptSelectColumns = `
	request_id, attempt_no, started_at, first_byte_at, completed_at,
	provider, deployment, upstream_model, pool_priority, http_status,
	upstream_request_id, outcome, failure_class, retried, latency_ns,
	input_tokens, output_tokens, total_tokens, cached_input_tokens,
	cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
	accepted_prediction_tokens, rejected_prediction_tokens,
	provider_components_json, raw_usage_json, usage_completeness,
	normalization_version
`

const runtimeEventSelectColumns = `
	event_id, occurred_at, controller, binding_revision, virtual_model,
	deployment, endpoint_instance, cluster_id, job_id, recipe_revision,
	prior_state, new_state, reason, latency_ns, queue_depth, waiter_count, outcome
`

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *Store) GetRequest(
	ctx context.Context,
	requestID string,
) (ledger.RequestRecord, error) {
	if requestID == "" {
		return ledger.RequestRecord{}, fmt.Errorf("request_id is required")
	}
	if len(requestID) > 1024 {
		return ledger.RequestRecord{}, fmt.Errorf("request_id exceeds 1024 bytes")
	}
	record, err := scanRequest(s.db.QueryRowContext(
		ctx,
		"SELECT "+requestSelectColumns+" FROM llm_requests WHERE request_id = ?",
		requestID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return ledger.RequestRecord{}, fmt.Errorf("%w: request %q", ledger.ErrNotFound, requestID)
	}
	if err != nil {
		return ledger.RequestRecord{}, fmt.Errorf("read SQLite request record: %w", err)
	}
	return record, nil
}

func (s *Store) ListRequests(
	ctx context.Context,
	query ledger.RequestQuery,
) (ledger.RequestPage, error) {
	plan, err := ledger.PlanRequestQuery(&query)
	if err != nil {
		return ledger.RequestPage{}, err
	}

	statement := strings.Builder{}
	statement.WriteString("SELECT ")
	statement.WriteString(requestSelectColumns)
	statement.WriteString(" FROM llm_requests WHERE 1 = 1")
	args := make([]any, 0, 10)
	appendCondition := func(condition string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(condition)
		args = append(args, values...)
	}
	if !query.StartedAtOrAfter.IsZero() {
		appendCondition("started_at_unix_ns >= ?", query.StartedAtOrAfter.UnixNano())
	}
	if !query.StartedAtBefore.IsZero() {
		appendCondition("started_at_unix_ns < ?", query.StartedAtBefore.UnixNano())
	}
	if query.VirtualModel != "" {
		appendCondition("virtual_model = ?", query.VirtualModel)
	}
	if query.TenantID != "" {
		appendCondition("tenant_id = ?", query.TenantID)
	}
	if query.Outcome != "" {
		appendCondition("outcome = ?", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(
			"(started_at_unix_ns < ? OR (started_at_unix_ns = ? AND request_id < ?))",
			plan.CursorStartedAt.UnixNano(),
			plan.CursorStartedAt.UnixNano(),
			plan.CursorRequestID,
		)
	}
	statement.WriteString(
		" ORDER BY started_at_unix_ns DESC, request_id DESC LIMIT ?",
	)
	args = append(args, plan.Limit+1)

	rows, err := s.db.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return ledger.RequestPage{}, fmt.Errorf("list SQLite request records: %w", err)
	}
	defer rows.Close()

	records := make([]ledger.RequestRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanRequest(rows)
		if err != nil {
			return ledger.RequestPage{}, fmt.Errorf("scan SQLite request record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.RequestPage{}, fmt.Errorf("iterate SQLite request records: %w", err)
	}
	page := ledger.RequestPage{Records: records}
	if len(records) > plan.Limit {
		page.Records = records[:plan.Limit]
		page.NextCursor, err = ledger.RequestNextCursor(page.Records[plan.Limit-1])
		if err != nil {
			return ledger.RequestPage{}, err
		}
	}
	return page, nil
}

func (s *Store) ListAttempts(
	ctx context.Context,
	query ledger.AttemptQuery,
) (ledger.AttemptPage, error) {
	plan, err := ledger.PlanAttemptQuery(&query)
	if err != nil {
		return ledger.AttemptPage{}, err
	}

	statement := strings.Builder{}
	statement.WriteString("SELECT ")
	statement.WriteString(attemptSelectColumns)
	statement.WriteString(" FROM llm_attempts WHERE 1 = 1")
	args := make([]any, 0, 14)
	appendCondition := func(condition string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(condition)
		args = append(args, values...)
	}
	if !query.StartedAtOrAfter.IsZero() {
		appendCondition("started_at_unix_ns >= ?", query.StartedAtOrAfter.UnixNano())
	}
	if !query.StartedAtBefore.IsZero() {
		appendCondition("started_at_unix_ns < ?", query.StartedAtBefore.UnixNano())
	}
	if query.RequestID != "" {
		appendCondition("request_id = ?", query.RequestID)
	}
	if query.Provider != "" {
		appendCondition("provider = ?", query.Provider)
	}
	if query.Deployment != "" {
		appendCondition("deployment = ?", query.Deployment)
	}
	if query.Outcome != "" {
		appendCondition("outcome = ?", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(`
			(
				started_at_unix_ns < ? OR
				(
					started_at_unix_ns = ? AND
					(
						request_id < ? OR
						(request_id = ? AND attempt_no < ?)
					)
				)
			)
		`,
			plan.CursorStartedAt.UnixNano(),
			plan.CursorStartedAt.UnixNano(),
			plan.CursorRequestID,
			plan.CursorRequestID,
			plan.CursorAttempt,
		)
	}
	statement.WriteString(`
		ORDER BY started_at_unix_ns DESC, request_id DESC, attempt_no DESC
		LIMIT ?
	`)
	args = append(args, plan.Limit+1)

	rows, err := s.db.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return ledger.AttemptPage{}, fmt.Errorf("list SQLite attempt records: %w", err)
	}
	defer rows.Close()

	records := make([]ledger.AttemptRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanAttempt(rows)
		if err != nil {
			return ledger.AttemptPage{}, fmt.Errorf("scan SQLite attempt record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.AttemptPage{}, fmt.Errorf("iterate SQLite attempt records: %w", err)
	}
	page := ledger.AttemptPage{Records: records}
	if len(records) > plan.Limit {
		page.Records = records[:plan.Limit]
		page.NextCursor, err = ledger.AttemptNextCursor(page.Records[plan.Limit-1])
		if err != nil {
			return ledger.AttemptPage{}, err
		}
	}
	return page, nil
}

func (s *Store) ListRuntimeEvents(
	ctx context.Context,
	query ledger.RuntimeEventQuery,
) (ledger.RuntimeEventPage, error) {
	plan, err := ledger.PlanRuntimeEventQuery(&query)
	if err != nil {
		return ledger.RuntimeEventPage{}, err
	}
	statement := strings.Builder{}
	statement.WriteString("SELECT ")
	statement.WriteString(runtimeEventSelectColumns)
	statement.WriteString(" FROM model_runtime_events WHERE 1 = 1")
	args := make([]any, 0, 12)
	appendCondition := func(condition string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(condition)
		args = append(args, values...)
	}
	if !query.OccurredAtOrAfter.IsZero() {
		appendCondition("occurred_at_unix_ns >= ?", query.OccurredAtOrAfter.UnixNano())
	}
	if !query.OccurredAtBefore.IsZero() {
		appendCondition("occurred_at_unix_ns < ?", query.OccurredAtBefore.UnixNano())
	}
	if query.Controller != "" {
		appendCondition("controller = ?", query.Controller)
	}
	if query.VirtualModel != "" {
		appendCondition("virtual_model = ?", query.VirtualModel)
	}
	if query.Deployment != "" {
		appendCondition("deployment = ?", query.Deployment)
	}
	if query.Outcome != "" {
		appendCondition("outcome = ?", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(
			"(occurred_at_unix_ns < ? OR (occurred_at_unix_ns = ? AND event_id < ?))",
			plan.CursorStartedAt.UnixNano(),
			plan.CursorStartedAt.UnixNano(),
			plan.CursorRequestID,
		)
	}
	statement.WriteString(" ORDER BY occurred_at_unix_ns DESC, event_id DESC LIMIT ?")
	args = append(args, plan.Limit+1)

	rows, err := s.db.QueryContext(ctx, statement.String(), args...)
	if err != nil {
		return ledger.RuntimeEventPage{}, fmt.Errorf("list SQLite runtime events: %w", err)
	}
	defer rows.Close()
	records := make([]ledger.RuntimeEventRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanRuntimeEvent(rows)
		if err != nil {
			return ledger.RuntimeEventPage{}, fmt.Errorf("scan SQLite runtime event: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.RuntimeEventPage{}, fmt.Errorf("iterate SQLite runtime events: %w", err)
	}
	page := ledger.RuntimeEventPage{Records: records}
	if len(records) > plan.Limit {
		page.Records = records[:plan.Limit]
		page.NextCursor, err = ledger.RuntimeEventNextCursor(page.Records[plan.Limit-1])
		if err != nil {
			return ledger.RuntimeEventPage{}, err
		}
	}
	return page, nil
}

func (s *Store) Prune(
	ctx context.Context,
	policy ledger.RetentionPolicy,
) (ledger.PruneResult, error) {
	policy, err := ledger.ValidateRetentionPolicy(policy)
	if err != nil {
		return ledger.PruneResult{}, err
	}
	cutoff := policy.DeleteBefore.UnixNano()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ledger.PruneResult{}, fmt.Errorf("begin SQLite ledger pruning: %w", err)
	}
	rollback := func(err error) (ledger.PruneResult, error) {
		_ = tx.Rollback()
		return ledger.PruneResult{}, err
	}
	attemptResult, err := tx.ExecContext(
		ctx,
		"DELETE FROM llm_attempts WHERE started_at_unix_ns < ?",
		cutoff,
	)
	if err != nil {
		return rollback(fmt.Errorf("prune SQLite attempt records: %w", err))
	}
	runtimeResult, err := tx.ExecContext(
		ctx,
		"DELETE FROM model_runtime_events WHERE occurred_at_unix_ns < ?",
		cutoff,
	)
	if err != nil {
		return rollback(fmt.Errorf("prune SQLite runtime events: %w", err))
	}
	requestResult, err := tx.ExecContext(
		ctx,
		"DELETE FROM llm_requests WHERE started_at_unix_ns < ?",
		cutoff,
	)
	if err != nil {
		return rollback(fmt.Errorf("prune SQLite request records: %w", err))
	}
	attemptsDeleted, err := attemptResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count pruned SQLite attempt records: %w", err))
	}
	requestsDeleted, err := requestResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count pruned SQLite request records: %w", err))
	}
	runtimeEventsDeleted, err := runtimeResult.RowsAffected()
	if err != nil {
		return rollback(fmt.Errorf("count pruned SQLite runtime events: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("commit SQLite ledger pruning: %w", err)
	}
	return ledger.PruneResult{
		RequestsDeleted:      requestsDeleted,
		AttemptsDeleted:      attemptsDeleted,
		RuntimeEventsDeleted: runtimeEventsDeleted,
	}, nil
}

func scanRequest(scanner rowScanner) (ledger.RequestRecord, error) {
	var (
		record             ledger.RequestRecord
		started            string
		completed          string
		stream             int64
		principalID        sql.NullString
		principalType      sql.NullString
		principalSubject   sql.NullString
		principalRoles     sql.NullString
		tenantID           sql.NullString
		attribution        sql.NullString
		requestedModel     sql.NullString
		virtualModel       sql.NullString
		presentedModel     sql.NullString
		configRevision     sql.NullString
		finalProvider      sql.NullString
		finalDeployment    sql.NullString
		finalUpstreamModel sql.NullString
		httpStatus         sql.NullInt64
		failureClass       sql.NullString
		latencyNS          int64
		ttfbNS             sql.NullInt64
		usage              usageScan
	)
	if err := scanner.Scan(
		&record.RequestID,
		&started,
		&completed,
		&record.Protocol,
		&record.Operation,
		&stream,
		&principalID,
		&principalType,
		&principalSubject,
		&principalRoles,
		&tenantID,
		&attribution,
		&requestedModel,
		&virtualModel,
		&presentedModel,
		&configRevision,
		&finalProvider,
		&finalDeployment,
		&finalUpstreamModel,
		&record.AttemptCount,
		&httpStatus,
		&record.Outcome,
		&failureClass,
		&latencyNS,
		&ttfbNS,
		&usage.input,
		&usage.output,
		&usage.total,
		&usage.cachedInput,
		&usage.cacheCreation,
		&usage.reasoning,
		&usage.toolUsePrompt,
		&usage.acceptedPrediction,
		&usage.rejectedPrediction,
		&usage.providerComponents,
		&usage.raw,
		&usage.completeness,
		&usage.normalizationVersion,
	); err != nil {
		return ledger.RequestRecord{}, err
	}
	var err error
	record.StartedAt, err = parseTime(started)
	if err != nil {
		return ledger.RequestRecord{}, err
	}
	record.CompletedAt, err = parseTime(completed)
	if err != nil {
		return ledger.RequestRecord{}, err
	}
	record.Stream = stream != 0
	record.PrincipalID = principalID.String
	record.PrincipalType = principalType.String
	record.PrincipalSubject = principalSubject.String
	record.TenantID = tenantID.String
	if principalRoles.Valid {
		if err := json.Unmarshal([]byte(principalRoles.String), &record.PrincipalRoles); err != nil {
			return ledger.RequestRecord{}, fmt.Errorf(
				"decode SQLite principal roles: %w",
				err,
			)
		}
	}
	if attribution.Valid {
		if err := json.Unmarshal([]byte(attribution.String), &record.Attribution); err != nil {
			return ledger.RequestRecord{}, fmt.Errorf(
				"decode SQLite attribution: %w",
				err,
			)
		}
	}
	record.RequestedModel = requestedModel.String
	record.VirtualModel = virtualModel.String
	record.ResponsePresentedModel = presentedModel.String
	record.ConfigRevision = configRevision.String
	record.FinalProvider = finalProvider.String
	record.FinalDeployment = finalDeployment.String
	record.FinalUpstreamModel = finalUpstreamModel.String
	record.HTTPStatus = int(httpStatus.Int64)
	record.FailureClass = failureClass.String
	record.Latency = time.Duration(latencyNS)
	record.TimeToFirstByte = durationPointer(ttfbNS)
	record.Usage, err = usage.value()
	if err != nil {
		return ledger.RequestRecord{}, err
	}
	return record, nil
}

func scanAttempt(scanner rowScanner) (ledger.AttemptRecord, error) {
	var (
		record            ledger.AttemptRecord
		started           string
		firstByte         sql.NullString
		completed         string
		httpStatus        sql.NullInt64
		upstreamRequestID sql.NullString
		failureClass      sql.NullString
		retried           int64
		latencyNS         int64
		usage             usageScan
	)
	if err := scanner.Scan(
		&record.RequestID,
		&record.Attempt,
		&started,
		&firstByte,
		&completed,
		&record.Provider,
		&record.Deployment,
		&record.UpstreamModel,
		&record.PoolPriority,
		&httpStatus,
		&upstreamRequestID,
		&record.Outcome,
		&failureClass,
		&retried,
		&latencyNS,
		&usage.input,
		&usage.output,
		&usage.total,
		&usage.cachedInput,
		&usage.cacheCreation,
		&usage.reasoning,
		&usage.toolUsePrompt,
		&usage.acceptedPrediction,
		&usage.rejectedPrediction,
		&usage.providerComponents,
		&usage.raw,
		&usage.completeness,
		&usage.normalizationVersion,
	); err != nil {
		return ledger.AttemptRecord{}, err
	}
	var err error
	record.StartedAt, err = parseTime(started)
	if err != nil {
		return ledger.AttemptRecord{}, err
	}
	record.CompletedAt, err = parseTime(completed)
	if err != nil {
		return ledger.AttemptRecord{}, err
	}
	if firstByte.Valid {
		value, err := parseTime(firstByte.String)
		if err != nil {
			return ledger.AttemptRecord{}, err
		}
		record.FirstByteAt = &value
	}
	record.HTTPStatus = int(httpStatus.Int64)
	record.UpstreamRequestID = upstreamRequestID.String
	record.FailureClass = failureClass.String
	record.Retried = retried != 0
	record.Latency = time.Duration(latencyNS)
	record.Usage, err = usage.value()
	if err != nil {
		return ledger.AttemptRecord{}, err
	}
	return record, nil
}

func scanRuntimeEvent(scanner rowScanner) (ledger.RuntimeEventRecord, error) {
	var (
		record           ledger.RuntimeEventRecord
		occurred         string
		bindingRevision  sql.NullString
		virtualModel     sql.NullString
		deployment       sql.NullString
		endpointInstance sql.NullString
		clusterID        sql.NullString
		jobID            sql.NullString
		recipeRevision   sql.NullString
		priorState       sql.NullString
		reason           sql.NullString
		latencyNS        sql.NullInt64
	)
	if err := scanner.Scan(
		&record.EventID,
		&occurred,
		&record.Controller,
		&bindingRevision,
		&virtualModel,
		&deployment,
		&endpointInstance,
		&clusterID,
		&jobID,
		&recipeRevision,
		&priorState,
		&record.NewState,
		&reason,
		&latencyNS,
		&record.QueueDepth,
		&record.WaiterCount,
		&record.Outcome,
	); err != nil {
		return ledger.RuntimeEventRecord{}, err
	}
	var err error
	record.OccurredAt, err = parseTime(occurred)
	if err != nil {
		return ledger.RuntimeEventRecord{}, err
	}
	record.BindingRevision = bindingRevision.String
	record.VirtualModel = virtualModel.String
	record.Deployment = deployment.String
	record.EndpointInstance = endpointInstance.String
	record.ClusterID = clusterID.String
	record.JobID = jobID.String
	record.RecipeRevision = recipeRevision.String
	record.PriorState = priorState.String
	record.Reason = reason.String
	record.Latency = durationPointer(latencyNS)
	return record, nil
}

type usageScan struct {
	input                sql.NullInt64
	output               sql.NullInt64
	total                sql.NullInt64
	cachedInput          sql.NullInt64
	cacheCreation        sql.NullInt64
	reasoning            sql.NullInt64
	toolUsePrompt        sql.NullInt64
	acceptedPrediction   sql.NullInt64
	rejectedPrediction   sql.NullInt64
	providerComponents   sql.NullString
	raw                  sql.NullString
	completeness         string
	normalizationVersion sql.NullString
}

func (usage usageScan) value() (ledger.TokenUsage, error) {
	result := ledger.TokenUsage{
		InputTokens:              int64Pointer(usage.input),
		OutputTokens:             int64Pointer(usage.output),
		TotalTokens:              int64Pointer(usage.total),
		CachedInputTokens:        int64Pointer(usage.cachedInput),
		CacheCreationTokens:      int64Pointer(usage.cacheCreation),
		ReasoningTokens:          int64Pointer(usage.reasoning),
		ToolUsePromptTokens:      int64Pointer(usage.toolUsePrompt),
		AcceptedPredictionTokens: int64Pointer(usage.acceptedPrediction),
		RejectedPredictionTokens: int64Pointer(usage.rejectedPrediction),
		Completeness:             ledger.UsageCompleteness(usage.completeness),
		NormalizationVersion:     usage.normalizationVersion.String,
	}
	if usage.providerComponents.Valid {
		if err := json.Unmarshal(
			[]byte(usage.providerComponents.String),
			&result.ProviderComponents,
		); err != nil {
			return ledger.TokenUsage{}, fmt.Errorf(
				"decode SQLite provider usage components: %w",
				err,
			)
		}
	}
	if usage.raw.Valid {
		result.Raw = json.RawMessage(append([]byte(nil), usage.raw.String...))
	}
	return result, nil
}

func int64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func durationPointer(value sql.NullInt64) *time.Duration {
	if !value.Valid {
		return nil
	}
	result := time.Duration(value.Int64)
	return &result
}

func parseTime(value string) (time.Time, error) {
	result, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse SQLite ledger timestamp %q: %w", value, err)
	}
	return result.UTC(), nil
}

var (
	_ ledger.Reader             = (*Store)(nil)
	_ ledger.RuntimeEventReader = (*Store)(nil)
	_ ledger.Pruner             = (*Store)(nil)
)
