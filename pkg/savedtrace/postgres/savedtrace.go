// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

const savedTraceSelectColumns = `
	request_id, started_at, completed_at, tenant_id, principal_id,
	principal_type, principal_subject, conversation_id, response_id,
	parent_response_id, session_id, capture_session_id, metadata_json, protocol, operation,
	stream, requested_model, virtual_model, response_presented_model,
	config_revision, final_provider, final_deployment, final_upstream_model,
	attempt_count, http_status, outcome, failure_class, request_content_type,
	request_body, request_truncated, response_content_type, response_body,
	response_truncated, capture_outcome
`

const savedTraceInsert = `
	INSERT INTO llm_saved_traces (
		request_id, started_at, completed_at, tenant_id, principal_id,
		principal_type, principal_subject, conversation_id, response_id,
		parent_response_id, session_id, capture_session_id, metadata_json, protocol, operation,
		stream, requested_model, virtual_model, response_presented_model,
		config_revision, final_provider, final_deployment,
		final_upstream_model, attempt_count, http_status, outcome,
		failure_class, request_content_type, request_body, request_truncated,
		response_content_type, response_body, response_truncated, capture_outcome
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, $34
	)
	ON CONFLICT (request_id) DO NOTHING
`

func (s *Store) Append(ctx context.Context, record savedtrace.Record) error {
	args, err := postgresSavedTraceArgs(record)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, savedTraceInsert, args...); err != nil {
		return fmt.Errorf("append PostgreSQL saved trace: %w", err)
	}
	return nil
}

func (s *Store) AppendTraceBatch(ctx context.Context, records []savedtrace.Record) error {
	if len(records) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, record := range records {
		args, err := postgresSavedTraceArgs(record)
		if err != nil {
			return err
		}
		batch.Queue(savedTraceInsert, args...)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin PostgreSQL saved trace batch: %w", err)
	}
	results := tx.SendBatch(ctx, batch)
	for range records {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			_ = tx.Rollback(ctx)
			return fmt.Errorf("append PostgreSQL saved trace batch: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		_ = tx.Rollback(ctx)
		return fmt.Errorf("close PostgreSQL saved trace batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL saved trace batch: %w", err)
	}
	return nil
}

func postgresSavedTraceArgs(record savedtrace.Record) ([]any, error) {
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("validate PostgreSQL saved trace: %w", err)
	}
	metadata, err := json.Marshal(record.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encode PostgreSQL saved trace metadata: %w", err)
	}
	if record.Metadata == nil {
		metadata = []byte("{}")
	}
	return []any{
		record.RequestID,
		normalizePostgresTime(record.StartedAt),
		normalizePostgresTime(record.CompletedAt),
		nullString(record.TenantID),
		nullString(record.PrincipalID),
		nullString(record.PrincipalType),
		nullString(record.PrincipalSubject),
		nullString(record.ConversationID),
		nullString(record.ResponseID),
		nullString(record.ParentResponseID),
		nullString(record.SessionID),
		nullString(record.CaptureSessionID),
		json.RawMessage(metadata),
		record.Protocol,
		record.Operation,
		record.Stream,
		nullString(record.RequestedModel),
		nullString(record.VirtualModel),
		nullString(record.ResponsePresentedModel),
		nullString(record.ConfigRevision),
		nullString(record.FinalProvider),
		nullString(record.FinalDeployment),
		nullString(record.FinalUpstreamModel),
		record.AttemptCount,
		nullInt(record.HTTPStatus),
		record.Outcome,
		nullString(record.FailureClass),
		nullString(record.Request.ContentType),
		record.Request.Body,
		record.Request.Truncated,
		nullString(record.Response.ContentType),
		record.Response.Body,
		record.Response.Truncated,
		string(record.EffectiveCaptureOutcome()),
	}, nil
}

func (s *Store) List(ctx context.Context, query savedtrace.Query) (savedtrace.Page, error) {
	plan, err := savedtrace.PlanQuery(&query)
	if err != nil {
		return savedtrace.Page{}, err
	}
	statement := strings.Builder{}
	statement.WriteString("SELECT ")
	statement.WriteString(savedTraceSelectColumns)
	statement.WriteString(" FROM llm_saved_traces WHERE TRUE")
	args := make([]any, 0, 24)
	appendCondition := func(format string, values ...any) {
		statement.WriteString(" AND ")
		_, _ = fmt.Fprintf(&statement, format, parameterNumbers(len(args)+1, len(values))...)
		args = append(args, values...)
	}
	if !query.StartedAtOrAfter.IsZero() {
		appendCondition("started_at >= %s", normalizePostgresTime(query.StartedAtOrAfter))
	}
	if !query.StartedAtBefore.IsZero() {
		appendCondition("started_at < %s", normalizePostgresTime(query.StartedAtBefore))
	}
	for column, value := range map[string]string{
		"tenant_id":          query.TenantID,
		"conversation_id":    query.ConversationID,
		"response_id":        query.ResponseID,
		"parent_response_id": query.ParentResponseID,
		"session_id":         query.SessionID,
		"capture_session_id": query.CaptureSessionID,
		"protocol":           query.Protocol,
		"operation":          query.Operation,
		"requested_model":    query.RequestedModel,
		"virtual_model":      query.VirtualModel,
		"final_provider":     query.Provider,
		"final_deployment":   query.Deployment,
		"outcome":            query.Outcome,
		"capture_outcome":    string(query.CaptureOutcome),
	} {
		if value != "" {
			appendCondition(column+" = %s", value)
		}
	}
	if len(query.Metadata) > 0 {
		encoded, err := json.Marshal(query.Metadata)
		if err != nil {
			return savedtrace.Page{}, fmt.Errorf("encode saved trace metadata filter: %w", err)
		}
		appendCondition("metadata_json @> %s::jsonb", string(encoded))
	}
	if plan.CursorRequestID != "" {
		appendCondition(
			"(started_at < %s OR (started_at = %s AND request_id < %s))",
			normalizePostgresTime(plan.CursorStartedAt),
			normalizePostgresTime(plan.CursorStartedAt),
			plan.CursorRequestID,
		)
	}
	statement.WriteString(" ORDER BY started_at DESC, request_id DESC LIMIT ")
	_, _ = fmt.Fprintf(&statement, "$%d", len(args)+1)
	args = append(args, plan.Limit+1)
	rows, err := s.pool.Query(ctx, statement.String(), args...)
	if err != nil {
		return savedtrace.Page{}, fmt.Errorf("list PostgreSQL saved traces: %w", err)
	}
	defer rows.Close()
	records := make([]savedtrace.Record, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanSavedTrace(rows)
		if err != nil {
			return savedtrace.Page{}, fmt.Errorf("scan PostgreSQL saved trace: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return savedtrace.Page{}, fmt.Errorf("iterate PostgreSQL saved traces: %w", err)
	}
	page := savedtrace.Page{Records: records}
	if len(records) > plan.Limit {
		page.Records = records[:plan.Limit]
		page.NextCursor, err = savedtrace.NextCursor(page.Records[plan.Limit-1])
		if err != nil {
			return savedtrace.Page{}, err
		}
	}
	return page, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSavedTrace(scanner rowScanner) (savedtrace.Record, error) {
	record := savedtrace.Record{Version: savedtrace.SchemaVersion}
	var (
		tenantID               *string
		principalID            *string
		principalType          *string
		principalSubject       *string
		conversationID         *string
		responseID             *string
		parentResponseID       *string
		sessionID              *string
		captureSessionID       *string
		requestedModel         *string
		virtualModel           *string
		responsePresentedModel *string
		configRevision         *string
		finalProvider          *string
		finalDeployment        *string
		finalUpstreamModel     *string
		httpStatus             *int32
		failureClass           *string
		requestContentType     *string
		responseContentType    *string
		captureOutcome         string
	)
	if err := scanner.Scan(
		&record.RequestID,
		&record.StartedAt,
		&record.CompletedAt,
		&tenantID,
		&principalID,
		&principalType,
		&principalSubject,
		&conversationID,
		&responseID,
		&parentResponseID,
		&sessionID,
		&captureSessionID,
		&record.Metadata,
		&record.Protocol,
		&record.Operation,
		&record.Stream,
		&requestedModel,
		&virtualModel,
		&responsePresentedModel,
		&configRevision,
		&finalProvider,
		&finalDeployment,
		&finalUpstreamModel,
		&record.AttemptCount,
		&httpStatus,
		&record.Outcome,
		&failureClass,
		&requestContentType,
		&record.Request.Body,
		&record.Request.Truncated,
		&responseContentType,
		&record.Response.Body,
		&record.Response.Truncated,
		&captureOutcome,
	); err != nil {
		return savedtrace.Record{}, err
	}
	record.StartedAt = record.StartedAt.UTC()
	record.CompletedAt = record.CompletedAt.UTC()
	record.TenantID = stringValue(tenantID)
	record.PrincipalID = stringValue(principalID)
	record.PrincipalType = stringValue(principalType)
	record.PrincipalSubject = stringValue(principalSubject)
	record.ConversationID = stringValue(conversationID)
	record.ResponseID = stringValue(responseID)
	record.ParentResponseID = stringValue(parentResponseID)
	record.SessionID = stringValue(sessionID)
	record.CaptureSessionID = stringValue(captureSessionID)
	record.RequestedModel = stringValue(requestedModel)
	record.VirtualModel = stringValue(virtualModel)
	record.ResponsePresentedModel = stringValue(responsePresentedModel)
	record.ConfigRevision = stringValue(configRevision)
	record.FinalProvider = stringValue(finalProvider)
	record.FinalDeployment = stringValue(finalDeployment)
	record.FinalUpstreamModel = stringValue(finalUpstreamModel)
	record.HTTPStatus = int32Value(httpStatus)
	record.FailureClass = stringValue(failureClass)
	record.Request.ContentType = stringValue(requestContentType)
	record.Response.ContentType = stringValue(responseContentType)
	record.CaptureOutcome = savedtrace.CaptureOutcome(captureOutcome)
	if err := record.Validate(); err != nil {
		return savedtrace.Record{}, err
	}
	return record, nil
}

func parameterNumbers(first, count int) []any {
	result := make([]any, count)
	for index := range result {
		result[index] = fmt.Sprintf("$%d", first+index)
	}
	return result
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

var _ savedtrace.Store = (*Store)(nil)
var _ savedtrace.BatchStore = (*Store)(nil)
