// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

const requestSelectColumns = `
	r.request_id, r.started_at, r.completed_at, r.protocol, r.operation, r.stream,
	r.principal_id, r.principal_type, r.principal_subject,
	r.principal_roles_json, r.tenant_id, r.attribution_json,
	r.requested_model, r.virtual_model, r.response_presented_model,
	r.config_revision, r.final_provider, r.final_deployment,
	r.final_upstream_model, r.attempt_count, r.http_status, r.outcome,
	r.failure_class, r.latency_ns, r.time_to_first_byte_ns, r.input_tokens,
	r.output_tokens, r.total_tokens, r.cached_input_tokens,
	r.cache_creation_tokens, r.reasoning_tokens, r.tool_use_prompt_tokens,
	r.accepted_prediction_tokens, r.rejected_prediction_tokens,
	r.provider_components_json, r.raw_usage_json, r.usage_completeness,
	r.normalization_version
`

const attemptSelectColumns = `
	a.request_id, a.attempt_no, a.attempt_started_at, a.first_byte_at,
	a.completed_at, a.provider, a.deployment, a.upstream_model, a.pool_priority,
	a.http_status, a.upstream_request_id, a.outcome, a.failure_class, a.retried,
	a.latency_ns, a.input_tokens, a.output_tokens, a.total_tokens,
	a.cached_input_tokens, a.cache_creation_tokens, a.reasoning_tokens,
	a.tool_use_prompt_tokens, a.accepted_prediction_tokens,
	a.rejected_prediction_tokens, a.provider_components_json, a.raw_usage_json,
	a.usage_completeness, a.normalization_version
`

const runtimeEventSelectColumns = `
	e.event_id, e.occurred_at, e.controller, e.binding_revision,
	e.virtual_model, e.deployment, e.endpoint_instance, e.cluster_id, e.job_id,
	e.recipe_revision, e.prior_state, e.new_state, e.reason, e.latency_ns,
	e.queue_depth, e.waiter_count, e.outcome
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
	record, err := scanRequest(s.pool.QueryRow(ctx, `
		SELECT `+requestSelectColumns+`
		FROM llm_request_lookup AS lookup
		JOIN llm_requests AS r
		  ON r.request_id = lookup.request_id
		 AND r.started_at = lookup.started_at
		WHERE lookup.request_id = $1
	`, requestID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ledger.RequestRecord{}, fmt.Errorf("%w: request %q", ledger.ErrNotFound, requestID)
	}
	if err != nil {
		return ledger.RequestRecord{}, fmt.Errorf("read PostgreSQL request record: %w", err)
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
	statement.WriteString(" FROM llm_requests AS r WHERE TRUE")
	args := make([]any, 0, 10)
	appendCondition := func(format string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(fmt.Sprintf(format, parameterNumbers(len(args)+1, len(values))...))
		args = append(args, values...)
	}
	if !query.StartedAtOrAfter.IsZero() {
		appendCondition("r.started_at >= %s", normalizePostgresTime(query.StartedAtOrAfter))
	}
	if !query.StartedAtBefore.IsZero() {
		appendCondition("r.started_at < %s", normalizePostgresTime(query.StartedAtBefore))
	}
	if query.VirtualModel != "" {
		appendCondition("r.virtual_model = %s", query.VirtualModel)
	}
	if query.TenantID != "" {
		appendCondition("r.tenant_id = %s", query.TenantID)
	}
	if query.Outcome != "" {
		appendCondition("r.outcome = %s", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(
			"(r.started_at < %s OR (r.started_at = %s AND r.request_id < %s))",
			normalizePostgresTime(plan.CursorStartedAt),
			normalizePostgresTime(plan.CursorStartedAt),
			plan.CursorRequestID,
		)
	}
	statement.WriteString(" ORDER BY r.started_at DESC, r.request_id DESC LIMIT ")
	statement.WriteString(fmt.Sprintf("$%d", len(args)+1))
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx, statement.String(), args...)
	if err != nil {
		return ledger.RequestPage{}, fmt.Errorf("list PostgreSQL request records: %w", err)
	}
	defer rows.Close()
	records := make([]ledger.RequestRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanRequest(rows)
		if err != nil {
			return ledger.RequestPage{}, fmt.Errorf("scan PostgreSQL request record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.RequestPage{}, fmt.Errorf("iterate PostgreSQL request records: %w", err)
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
	statement.WriteString(" FROM llm_attempts AS a WHERE TRUE")
	args := make([]any, 0, 14)
	appendCondition := func(format string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(fmt.Sprintf(format, parameterNumbers(len(args)+1, len(values))...))
		args = append(args, values...)
	}
	if !query.StartedAtOrAfter.IsZero() {
		appendCondition(
			"a.attempt_started_at >= %s",
			normalizePostgresTime(query.StartedAtOrAfter),
		)
	}
	if !query.StartedAtBefore.IsZero() {
		appendCondition(
			"a.attempt_started_at < %s",
			normalizePostgresTime(query.StartedAtBefore),
		)
	}
	if query.RequestID != "" {
		appendCondition("a.request_id = %s", query.RequestID)
	}
	if query.Provider != "" {
		appendCondition("a.provider = %s", query.Provider)
	}
	if query.Deployment != "" {
		appendCondition("a.deployment = %s", query.Deployment)
	}
	if query.Outcome != "" {
		appendCondition("a.outcome = %s", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(`
			(
				a.attempt_started_at < %s OR
				(
					a.attempt_started_at = %s AND
					(
						a.request_id < %s OR
						(a.request_id = %s AND a.attempt_no < %s)
					)
				)
			)
		`,
			normalizePostgresTime(plan.CursorStartedAt),
			normalizePostgresTime(plan.CursorStartedAt),
			plan.CursorRequestID,
			plan.CursorRequestID,
			plan.CursorAttempt,
		)
	}
	statement.WriteString(`
		ORDER BY a.attempt_started_at DESC, a.request_id DESC, a.attempt_no DESC
		LIMIT
	`)
	statement.WriteString(fmt.Sprintf("$%d", len(args)+1))
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx, statement.String(), args...)
	if err != nil {
		return ledger.AttemptPage{}, fmt.Errorf("list PostgreSQL attempt records: %w", err)
	}
	defer rows.Close()
	records := make([]ledger.AttemptRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanAttempt(rows)
		if err != nil {
			return ledger.AttemptPage{}, fmt.Errorf("scan PostgreSQL attempt record: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.AttemptPage{}, fmt.Errorf("iterate PostgreSQL attempt records: %w", err)
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
	statement.WriteString(" FROM model_runtime_events AS e WHERE TRUE")
	args := make([]any, 0, 12)
	appendCondition := func(format string, values ...any) {
		statement.WriteString(" AND ")
		statement.WriteString(fmt.Sprintf(format, parameterNumbers(len(args)+1, len(values))...))
		args = append(args, values...)
	}
	if !query.OccurredAtOrAfter.IsZero() {
		appendCondition("e.occurred_at >= %s", normalizePostgresTime(query.OccurredAtOrAfter))
	}
	if !query.OccurredAtBefore.IsZero() {
		appendCondition("e.occurred_at < %s", normalizePostgresTime(query.OccurredAtBefore))
	}
	if query.Controller != "" {
		appendCondition("e.controller = %s", query.Controller)
	}
	if query.VirtualModel != "" {
		appendCondition("e.virtual_model = %s", query.VirtualModel)
	}
	if query.Deployment != "" {
		appendCondition("e.deployment = %s", query.Deployment)
	}
	if query.Outcome != "" {
		appendCondition("e.outcome = %s", string(query.Outcome))
	}
	if plan.CursorRequestID != "" {
		appendCondition(
			"(e.occurred_at < %s OR (e.occurred_at = %s AND e.event_id < %s))",
			normalizePostgresTime(plan.CursorStartedAt),
			normalizePostgresTime(plan.CursorStartedAt),
			plan.CursorRequestID,
		)
	}
	statement.WriteString(" ORDER BY e.occurred_at DESC, e.event_id DESC LIMIT ")
	statement.WriteString(fmt.Sprintf("$%d", len(args)+1))
	args = append(args, plan.Limit+1)

	rows, err := s.pool.Query(ctx, statement.String(), args...)
	if err != nil {
		return ledger.RuntimeEventPage{}, fmt.Errorf("list PostgreSQL runtime events: %w", err)
	}
	defer rows.Close()
	records := make([]ledger.RuntimeEventRecord, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanRuntimeEvent(rows)
		if err != nil {
			return ledger.RuntimeEventPage{}, fmt.Errorf("scan PostgreSQL runtime event: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return ledger.RuntimeEventPage{}, fmt.Errorf("iterate PostgreSQL runtime events: %w", err)
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
	cutoff := normalizePostgresTime(policy.DeleteBefore)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ledger.PruneResult{}, fmt.Errorf("begin PostgreSQL ledger pruning: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	var runtimeEventsDeleted int64
	if err := tx.QueryRow(
		ctx,
		"SELECT COUNT(*) FROM model_runtime_events WHERE occurred_at < $1",
		cutoff,
	).Scan(&runtimeEventsDeleted); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("count old PostgreSQL runtime events: %w", err)
	}

	if s.backend == BackendTimescaleDB {
		// drop_chunks handles the bulk, whole-day range efficiently. The
		// ordinary DELETE statements below handle only a cutoff's partial
		// chunk and preserve exact retention semantics.
		for _, table := range []string{
			"llm_attempts",
			"llm_requests",
			"model_runtime_events",
		} {
			if _, err := tx.Exec(
				ctx,
				"SELECT drop_chunks($1, older_than => $2::timestamptz)",
				table,
				cutoff,
			); err != nil {
				return ledger.PruneResult{}, fmt.Errorf(
					"drop old TimescaleDB ledger chunks for %s: %w",
					table,
					err,
				)
			}
		}
	}
	if _, err := tx.Exec(
		ctx,
		"DELETE FROM llm_attempts WHERE attempt_started_at < $1",
		cutoff,
	); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("prune PostgreSQL attempt boundary: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		"DELETE FROM llm_requests WHERE started_at < $1",
		cutoff,
	); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("prune PostgreSQL request boundary: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		"DELETE FROM model_runtime_events WHERE occurred_at < $1",
		cutoff,
	); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("prune PostgreSQL runtime event boundary: %w", err)
	}
	attemptTag, err := tx.Exec(
		ctx,
		"DELETE FROM llm_attempt_lookup WHERE attempt_started_at < $1",
		cutoff,
	)
	if err != nil {
		return ledger.PruneResult{}, fmt.Errorf("prune PostgreSQL attempt lookup: %w", err)
	}
	requestTag, err := tx.Exec(
		ctx,
		"DELETE FROM llm_request_lookup WHERE started_at < $1",
		cutoff,
	)
	if err != nil {
		return ledger.PruneResult{}, fmt.Errorf("prune PostgreSQL request lookup: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.PruneResult{}, fmt.Errorf("commit PostgreSQL ledger pruning: %w", err)
	}
	return ledger.PruneResult{
		RequestsDeleted:      requestTag.RowsAffected(),
		AttemptsDeleted:      attemptTag.RowsAffected(),
		RuntimeEventsDeleted: runtimeEventsDeleted,
	}, nil
}

func parameterNumbers(first, count int) []any {
	result := make([]any, count)
	for index := range result {
		result[index] = fmt.Sprintf("$%d", first+index)
	}
	return result
}

func scanRequest(scanner rowScanner) (ledger.RequestRecord, error) {
	var (
		record             ledger.RequestRecord
		principalID        *string
		principalType      *string
		principalSubject   *string
		principalRoles     []string
		tenantID           *string
		attribution        map[string]string
		requestedModel     *string
		virtualModel       *string
		presentedModel     *string
		configRevision     *string
		finalProvider      *string
		finalDeployment    *string
		finalUpstreamModel *string
		httpStatus         *int32
		failureClass       *string
		latencyNS          int64
		ttfbNS             *int64
		usage              usageScan
	)
	if err := scanner.Scan(
		&record.RequestID,
		&record.StartedAt,
		&record.CompletedAt,
		&record.Protocol,
		&record.Operation,
		&record.Stream,
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
	record.StartedAt = record.StartedAt.UTC()
	record.CompletedAt = record.CompletedAt.UTC()
	record.PrincipalID = stringValue(principalID)
	record.PrincipalType = stringValue(principalType)
	record.PrincipalSubject = stringValue(principalSubject)
	record.PrincipalRoles = principalRoles
	record.TenantID = stringValue(tenantID)
	record.Attribution = attribution
	record.RequestedModel = stringValue(requestedModel)
	record.VirtualModel = stringValue(virtualModel)
	record.ResponsePresentedModel = stringValue(presentedModel)
	record.ConfigRevision = stringValue(configRevision)
	record.FinalProvider = stringValue(finalProvider)
	record.FinalDeployment = stringValue(finalDeployment)
	record.FinalUpstreamModel = stringValue(finalUpstreamModel)
	record.HTTPStatus = int32Value(httpStatus)
	record.FailureClass = stringValue(failureClass)
	record.Latency = time.Duration(latencyNS)
	record.TimeToFirstByte = durationPointer(ttfbNS)
	record.Usage = usage.value()
	return record, nil
}

func scanAttempt(scanner rowScanner) (ledger.AttemptRecord, error) {
	var (
		record            ledger.AttemptRecord
		firstByte         *time.Time
		httpStatus        *int32
		upstreamRequestID *string
		failureClass      *string
		latencyNS         int64
		usage             usageScan
	)
	if err := scanner.Scan(
		&record.RequestID,
		&record.Attempt,
		&record.StartedAt,
		&firstByte,
		&record.CompletedAt,
		&record.Provider,
		&record.Deployment,
		&record.UpstreamModel,
		&record.PoolPriority,
		&httpStatus,
		&upstreamRequestID,
		&record.Outcome,
		&failureClass,
		&record.Retried,
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
	record.StartedAt = record.StartedAt.UTC()
	record.CompletedAt = record.CompletedAt.UTC()
	if firstByte != nil {
		value := firstByte.UTC()
		record.FirstByteAt = &value
	}
	record.HTTPStatus = int32Value(httpStatus)
	record.UpstreamRequestID = stringValue(upstreamRequestID)
	record.FailureClass = stringValue(failureClass)
	record.Latency = time.Duration(latencyNS)
	record.Usage = usage.value()
	return record, nil
}

func scanRuntimeEvent(scanner rowScanner) (ledger.RuntimeEventRecord, error) {
	var (
		record           ledger.RuntimeEventRecord
		bindingRevision  *string
		virtualModel     *string
		deployment       *string
		endpointInstance *string
		clusterID        *string
		jobID            *string
		recipeRevision   *string
		priorState       *string
		reason           *string
		latencyNS        *int64
	)
	if err := scanner.Scan(
		&record.EventID,
		&record.OccurredAt,
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
	record.OccurredAt = record.OccurredAt.UTC()
	record.BindingRevision = stringValue(bindingRevision)
	record.VirtualModel = stringValue(virtualModel)
	record.Deployment = stringValue(deployment)
	record.EndpointInstance = stringValue(endpointInstance)
	record.ClusterID = stringValue(clusterID)
	record.JobID = stringValue(jobID)
	record.RecipeRevision = stringValue(recipeRevision)
	record.PriorState = stringValue(priorState)
	record.Reason = stringValue(reason)
	record.Latency = durationPointer(latencyNS)
	return record, nil
}

type usageScan struct {
	input                *int64
	output               *int64
	total                *int64
	cachedInput          *int64
	cacheCreation        *int64
	reasoning            *int64
	toolUsePrompt        *int64
	acceptedPrediction   *int64
	rejectedPrediction   *int64
	providerComponents   map[string]int64
	raw                  json.RawMessage
	completeness         string
	normalizationVersion *string
}

func (usage usageScan) value() ledger.TokenUsage {
	return ledger.TokenUsage{
		InputTokens:              usage.input,
		OutputTokens:             usage.output,
		TotalTokens:              usage.total,
		CachedInputTokens:        usage.cachedInput,
		CacheCreationTokens:      usage.cacheCreation,
		ReasoningTokens:          usage.reasoning,
		ToolUsePromptTokens:      usage.toolUsePrompt,
		AcceptedPredictionTokens: usage.acceptedPrediction,
		RejectedPredictionTokens: usage.rejectedPrediction,
		ProviderComponents:       usage.providerComponents,
		Raw:                      append(json.RawMessage(nil), usage.raw...),
		Completeness:             ledger.UsageCompleteness(usage.completeness),
		NormalizationVersion:     stringValue(usage.normalizationVersion),
	}
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int32Value(value *int32) int {
	if value == nil {
		return 0
	}
	return int(*value)
}

func durationPointer(value *int64) *time.Duration {
	if value == nil {
		return nil
	}
	result := time.Duration(*value)
	return &result
}

var (
	_ ledger.Reader             = (*Store)(nil)
	_ ledger.RuntimeEventReader = (*Store)(nil)
	_ ledger.Pruner             = (*Store)(nil)
)
