// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

const postgresAggregateSelect = `
	SELECT
		COUNT(*)::bigint,
		COUNT(*) FILTER (WHERE outcome = 'success')::bigint,
		COUNT(*) FILTER (WHERE stream)::bigint,
		COALESCE(SUM(attempt_count), 0)::bigint,
		COUNT(*) FILTER (WHERE attempt_count > 1)::bigint,
		COALESCE(SUM(GREATEST(attempt_count - 1, 0)), 0)::bigint,
		FLOOR(COALESCE(AVG(latency_ns), 0))::bigint,
		COALESCE(PERCENTILE_DISC(0.50) WITHIN GROUP (ORDER BY latency_ns), 0)::bigint,
		COALESCE(PERCENTILE_DISC(0.95) WITHIN GROUP (ORDER BY latency_ns), 0)::bigint,
		COALESCE(PERCENTILE_DISC(0.99) WITHIN GROUP (ORDER BY latency_ns), 0)::bigint,
		COALESCE(SUM(input_tokens), 0)::bigint,
		COALESCE(SUM(output_tokens), 0)::bigint,
		COALESCE(SUM(total_tokens), 0)::bigint,
		COALESCE(SUM(cached_input_tokens), 0)::bigint,
		COALESCE(SUM(cache_creation_tokens), 0)::bigint,
		COALESCE(SUM(reasoning_tokens), 0)::bigint,
		COALESCE(SUM(tool_use_prompt_tokens), 0)::bigint,
		COALESCE(SUM(accepted_prediction_tokens), 0)::bigint,
		COALESCE(SUM(rejected_prediction_tokens), 0)::bigint,
		COUNT(*) FILTER (WHERE usage_completeness = 'missing')::bigint,
		COUNT(*) FILTER (WHERE usage_completeness = 'partial')::bigint,
		COUNT(*) FILTER (WHERE usage_completeness = 'complete')::bigint
	FROM llm_requests AS r
	%s
`

// AggregateRequests computes bounded request-level dashboard measurements.
func (s *Store) AggregateRequests(
	ctx context.Context,
	query ledger.AggregateQuery,
) (ledger.AggregateSummary, error) {
	query, err := ledger.ValidateAggregateQuery(query)
	if err != nil {
		return ledger.AggregateSummary{}, err
	}
	where, args := postgresAggregateWhere(query)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return ledger.AggregateSummary{}, fmt.Errorf("begin PostgreSQL ledger aggregate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	summary := ledger.AggregateSummary{
		StartedAtOrAfter: query.StartedAtOrAfter,
		StartedAtBefore:  query.StartedAtBefore,
	}
	var average, p50, p95, p99 int64
	err = tx.QueryRow(
		ctx,
		fmt.Sprintf(postgresAggregateSelect, where),
		args...,
	).Scan(
		&summary.Requests,
		&summary.SuccessfulRequests,
		&summary.StreamingRequests,
		&summary.Attempts,
		&summary.RetriedRequests,
		&summary.Retries,
		&average,
		&p50,
		&p95,
		&p99,
		&summary.Tokens.InputTokens,
		&summary.Tokens.OutputTokens,
		&summary.Tokens.TotalTokens,
		&summary.Tokens.CachedInputTokens,
		&summary.Tokens.CacheCreationTokens,
		&summary.Tokens.ReasoningTokens,
		&summary.Tokens.ToolUsePromptTokens,
		&summary.Tokens.AcceptedPredictionTokens,
		&summary.Tokens.RejectedPredictionTokens,
		&summary.UsageCompleteness.Missing,
		&summary.UsageCompleteness.Partial,
		&summary.UsageCompleteness.Complete,
	)
	if err != nil {
		return ledger.AggregateSummary{}, fmt.Errorf("aggregate PostgreSQL request records: %w", err)
	}
	summary.Latency = ledger.AggregateLatency{
		Average: time.Duration(average),
		P50:     time.Duration(p50),
		P95:     time.Duration(p95),
		P99:     time.Duration(p99),
	}

	breakdowns := []struct {
		column string
		target *ledger.AggregateBreakdown
	}{
		{column: "outcome", target: &summary.ByOutcome},
		{column: "virtual_model", target: &summary.ByVirtualModel},
		{column: "final_provider", target: &summary.ByProvider},
		{column: "final_deployment", target: &summary.ByDeployment},
	}
	for _, current := range breakdowns {
		*current.target, err = postgresAggregateBreakdown(
			ctx,
			tx,
			current.column,
			where,
			args,
		)
		if err != nil {
			return ledger.AggregateSummary{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.AggregateSummary{}, fmt.Errorf("commit PostgreSQL ledger aggregate: %w", err)
	}
	return summary, nil
}

func postgresAggregateWhere(query ledger.AggregateQuery) (string, []any) {
	conditions := []string{"r.started_at >= $1", "r.started_at < $2"}
	args := []any{
		normalizePostgresTime(query.StartedAtOrAfter),
		normalizePostgresTime(query.StartedAtBefore),
	}
	appendCondition := func(column, value string) {
		if value != "" {
			args = append(args, value)
			conditions = append(
				conditions,
				fmt.Sprintf("r.%s = $%d", column, len(args)),
			)
		}
	}
	appendCondition("tenant_id", query.TenantID)
	appendCondition("virtual_model", query.VirtualModel)
	appendCondition("final_provider", query.Provider)
	appendCondition("final_deployment", query.Deployment)
	appendCondition("outcome", string(query.Outcome))
	return "WHERE " + strings.Join(conditions, " AND "), args
}

func postgresAggregateBreakdown(
	ctx context.Context,
	tx pgx.Tx,
	column string,
	where string,
	args []any,
) (ledger.AggregateBreakdown, error) {
	groupArgs := append(append([]any(nil), args...), ledger.MaxAggregateGroups+1)
	rows, err := tx.Query(
		ctx,
		fmt.Sprintf(`
			SELECT COALESCE(r.%s, ''), COUNT(*)::bigint
			FROM llm_requests AS r
			%s
			GROUP BY COALESCE(r.%s, '')
			ORDER BY COUNT(*) DESC, COALESCE(r.%s, '') ASC
			LIMIT $%d
		`, column, where, column, column, len(groupArgs)),
		groupArgs...,
	)
	if err != nil {
		return ledger.AggregateBreakdown{}, fmt.Errorf(
			"aggregate PostgreSQL request %s breakdown: %w",
			column,
			err,
		)
	}
	defer rows.Close()
	groups := make([]ledger.AggregateGroup, 0, ledger.MaxAggregateGroups+1)
	for rows.Next() {
		var group ledger.AggregateGroup
		if err := rows.Scan(&group.Value, &group.Count); err != nil {
			return ledger.AggregateBreakdown{}, fmt.Errorf(
				"scan PostgreSQL request %s breakdown: %w",
				column,
				err,
			)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return ledger.AggregateBreakdown{}, fmt.Errorf(
			"iterate PostgreSQL request %s breakdown: %w",
			column,
			err,
		)
	}
	result := ledger.AggregateBreakdown{Groups: groups}
	if len(groups) > ledger.MaxAggregateGroups {
		result.Groups = groups[:ledger.MaxAggregateGroups]
		result.Truncated = true
	}
	return result, nil
}
