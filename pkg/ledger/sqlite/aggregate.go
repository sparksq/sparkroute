// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

const sqliteAggregateSelect = `
	WITH filtered AS (
		SELECT
			stream, attempt_count, outcome, latency_ns,
			input_tokens, output_tokens, total_tokens, cached_input_tokens,
			cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
			accepted_prediction_tokens, rejected_prediction_tokens,
			usage_completeness
		FROM llm_requests
		%s
	), ranked AS (
		SELECT
			*,
			ROW_NUMBER() OVER (ORDER BY latency_ns) AS latency_rank,
			COUNT(*) OVER () AS latency_count
		FROM filtered
	)
	SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN outcome = 'success' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN stream <> 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(attempt_count), 0),
		COALESCE(SUM(CASE WHEN attempt_count > 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN attempt_count > 1 THEN attempt_count - 1 ELSE 0 END), 0),
		COALESCE(CAST(AVG(latency_ns) AS INTEGER), 0),
		COALESCE(MAX(CASE WHEN latency_rank = (latency_count * 50 + 99) / 100 THEN latency_ns END), 0),
		COALESCE(MAX(CASE WHEN latency_rank = (latency_count * 95 + 99) / 100 THEN latency_ns END), 0),
		COALESCE(MAX(CASE WHEN latency_rank = (latency_count * 99 + 99) / 100 THEN latency_ns END), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(cached_input_tokens), 0),
		COALESCE(SUM(cache_creation_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(tool_use_prompt_tokens), 0),
		COALESCE(SUM(accepted_prediction_tokens), 0),
		COALESCE(SUM(rejected_prediction_tokens), 0),
		COALESCE(SUM(CASE WHEN usage_completeness = 'missing' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN usage_completeness = 'partial' THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN usage_completeness = 'complete' THEN 1 ELSE 0 END), 0)
	FROM ranked
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
	where, args := sqliteAggregateWhere(query)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ledger.AggregateSummary{}, fmt.Errorf("begin SQLite ledger aggregate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	summary := ledger.AggregateSummary{
		StartedAtOrAfter: query.StartedAtOrAfter,
		StartedAtBefore:  query.StartedAtBefore,
	}
	var average, p50, p95, p99 int64
	err = tx.QueryRowContext(
		ctx,
		fmt.Sprintf(sqliteAggregateSelect, where),
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
		return ledger.AggregateSummary{}, fmt.Errorf("aggregate SQLite request records: %w", err)
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
		*current.target, err = sqliteAggregateBreakdown(
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
	if err := tx.Commit(); err != nil {
		return ledger.AggregateSummary{}, fmt.Errorf("commit SQLite ledger aggregate: %w", err)
	}
	return summary, nil
}

func sqliteAggregateWhere(query ledger.AggregateQuery) (string, []any) {
	conditions := []string{
		"started_at_unix_ns >= ?",
		"started_at_unix_ns < ?",
	}
	args := []any{
		query.StartedAtOrAfter.UnixNano(),
		query.StartedAtBefore.UnixNano(),
	}
	appendCondition := func(column, value string) {
		if value != "" {
			conditions = append(conditions, column+" = ?")
			args = append(args, value)
		}
	}
	appendCondition("tenant_id", query.TenantID)
	appendCondition("virtual_model", query.VirtualModel)
	appendCondition("final_provider", query.Provider)
	appendCondition("final_deployment", query.Deployment)
	appendCondition("outcome", string(query.Outcome))
	return "WHERE " + strings.Join(conditions, " AND "), args
}

func sqliteAggregateBreakdown(
	ctx context.Context,
	tx *sql.Tx,
	column string,
	where string,
	args []any,
) (ledger.AggregateBreakdown, error) {
	rows, err := tx.QueryContext(
		ctx,
		fmt.Sprintf(`
			SELECT COALESCE(%s, ''), COUNT(*)
			FROM llm_requests
			%s
			GROUP BY COALESCE(%s, '')
			ORDER BY COUNT(*) DESC, COALESCE(%s, '') ASC
			LIMIT ?
		`, column, where, column, column),
		append(append([]any(nil), args...), ledger.MaxAggregateGroups+1)...,
	)
	if err != nil {
		return ledger.AggregateBreakdown{}, fmt.Errorf(
			"aggregate SQLite request %s breakdown: %w",
			column,
			err,
		)
	}
	defer func() { _ = rows.Close() }()
	groups := make([]ledger.AggregateGroup, 0, ledger.MaxAggregateGroups+1)
	for rows.Next() {
		var group ledger.AggregateGroup
		if err := rows.Scan(&group.Value, &group.Count); err != nil {
			return ledger.AggregateBreakdown{}, fmt.Errorf(
				"scan SQLite request %s breakdown: %w",
				column,
				err,
			)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return ledger.AggregateBreakdown{}, fmt.Errorf(
			"iterate SQLite request %s breakdown: %w",
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
