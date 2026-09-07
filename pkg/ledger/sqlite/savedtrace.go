package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	INSERT OR IGNORE INTO llm_saved_traces (
		request_id, started_at, started_at_unix_ns, completed_at,
		tenant_id, principal_id, principal_type, principal_subject,
		conversation_id, response_id, parent_response_id, session_id, capture_session_id,
		metadata_json, protocol, operation, stream, requested_model,
		virtual_model, response_presented_model, config_revision,
		final_provider, final_deployment, final_upstream_model, attempt_count,
		http_status, outcome, failure_class, request_content_type,
		request_body, request_truncated, response_content_type,
		response_body, response_truncated, capture_outcome
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	)
`

func (s *Store) Append(ctx context.Context, record savedtrace.Record) error {
	args, err := sqliteSavedTraceArgs(record)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, savedTraceInsert, args...); err != nil {
		return fmt.Errorf("append SQLite saved trace: %w", err)
	}
	return nil
}

func (s *Store) AppendTraceBatch(ctx context.Context, records []savedtrace.Record) error {
	if len(records) == 0 {
		return nil
	}
	arguments := make([][]any, len(records))
	for index, record := range records {
		args, err := sqliteSavedTraceArgs(record)
		if err != nil {
			return err
		}
		arguments[index] = args
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite saved trace batch: %w", err)
	}
	for _, args := range arguments {
		if _, err := tx.ExecContext(ctx, savedTraceInsert, args...); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("append SQLite saved trace batch: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite saved trace batch: %w", err)
	}
	return nil
}

func sqliteSavedTraceArgs(record savedtrace.Record) ([]any, error) {
	if err := record.Validate(); err != nil {
		return nil, fmt.Errorf("validate SQLite saved trace: %w", err)
	}
	metadata, err := json.Marshal(record.Metadata)
	if err != nil {
		return nil, fmt.Errorf("encode SQLite saved trace metadata: %w", err)
	}
	return []any{
		record.RequestID,
		formatTime(record.StartedAt),
		record.StartedAt.UTC().UnixNano(),
		formatTime(record.CompletedAt),
		nullString(record.TenantID),
		nullString(record.PrincipalID),
		nullString(record.PrincipalType),
		nullString(record.PrincipalSubject),
		nullString(record.ConversationID),
		nullString(record.ResponseID),
		nullString(record.ParentResponseID),
		nullString(record.SessionID),
		nullString(record.CaptureSessionID),
		string(metadata),
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

func (s *Store) List(
	ctx context.Context,
	query savedtrace.Query,
) (savedtrace.Page, error) {
	plan, err := savedtrace.PlanQuery(&query)
	if err != nil {
		return savedtrace.Page{}, err
	}
	statement := strings.Builder{}
	statement.WriteString("SELECT ")
	statement.WriteString(savedTraceSelectColumns)
	statement.WriteString(" FROM llm_saved_traces WHERE 1 = 1")
	args := make([]any, 0, 24)
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
			appendCondition(column+" = ?", value)
		}
	}
	for key, value := range query.Metadata {
		appendCondition(
			"EXISTS (SELECT 1 FROM json_each(metadata_json) WHERE key = ? AND value = ?)",
			key,
			value,
		)
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
		return savedtrace.Page{}, fmt.Errorf("list SQLite saved traces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]savedtrace.Record, 0, plan.Limit+1)
	for rows.Next() {
		record, err := scanSavedTrace(rows)
		if err != nil {
			return savedtrace.Page{}, fmt.Errorf("scan SQLite saved trace: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return savedtrace.Page{}, fmt.Errorf("iterate SQLite saved traces: %w", err)
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

func scanSavedTrace(row rowScanner) (savedtrace.Record, error) {
	record := savedtrace.Record{Version: savedtrace.SchemaVersion}
	var (
		startedAt              string
		completedAt            string
		tenantID               sql.NullString
		principalID            sql.NullString
		principalType          sql.NullString
		principalSubject       sql.NullString
		conversationID         sql.NullString
		responseID             sql.NullString
		parentResponseID       sql.NullString
		sessionID              sql.NullString
		captureSessionID       sql.NullString
		metadata               string
		requestedModel         sql.NullString
		virtualModel           sql.NullString
		responsePresentedModel sql.NullString
		configRevision         sql.NullString
		finalProvider          sql.NullString
		finalDeployment        sql.NullString
		finalUpstreamModel     sql.NullString
		httpStatus             sql.NullInt64
		failureClass           sql.NullString
		requestContentType     sql.NullString
		responseContentType    sql.NullString
		captureOutcome         string
	)
	if err := row.Scan(
		&record.RequestID,
		&startedAt,
		&completedAt,
		&tenantID,
		&principalID,
		&principalType,
		&principalSubject,
		&conversationID,
		&responseID,
		&parentResponseID,
		&sessionID,
		&captureSessionID,
		&metadata,
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
	var err error
	record.StartedAt, err = time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return savedtrace.Record{}, err
	}
	record.CompletedAt, err = time.Parse(time.RFC3339Nano, completedAt)
	if err != nil {
		return savedtrace.Record{}, err
	}
	if err := json.Unmarshal([]byte(metadata), &record.Metadata); err != nil {
		return savedtrace.Record{}, err
	}
	record.TenantID = tenantID.String
	record.PrincipalID = principalID.String
	record.PrincipalType = principalType.String
	record.PrincipalSubject = principalSubject.String
	record.ConversationID = conversationID.String
	record.ResponseID = responseID.String
	record.ParentResponseID = parentResponseID.String
	record.SessionID = sessionID.String
	record.CaptureSessionID = captureSessionID.String
	record.RequestedModel = requestedModel.String
	record.VirtualModel = virtualModel.String
	record.ResponsePresentedModel = responsePresentedModel.String
	record.ConfigRevision = configRevision.String
	record.FinalProvider = finalProvider.String
	record.FinalDeployment = finalDeployment.String
	record.FinalUpstreamModel = finalUpstreamModel.String
	record.HTTPStatus = int(httpStatus.Int64)
	record.FailureClass = failureClass.String
	record.Request.ContentType = requestContentType.String
	record.Response.ContentType = responseContentType.String
	record.CaptureOutcome = savedtrace.CaptureOutcome(captureOutcome)
	if err := record.Validate(); err != nil {
		return savedtrace.Record{}, err
	}
	return record, nil
}

var _ savedtrace.Store = (*Store)(nil)
var _ savedtrace.BatchStore = (*Store)(nil)
