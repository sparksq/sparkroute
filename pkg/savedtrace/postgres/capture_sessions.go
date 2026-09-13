// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func (s *Store) UpsertCaptureSession(ctx context.Context, session savedtrace.CaptureSession) error {
	if err := session.Validate(); err != nil {
		return fmt.Errorf("validate PostgreSQL capture session: %w", err)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO llm_saved_trace_capture_sessions (
			id, started_at, updated_at, completed_at, mode, accepted, persisted,
			pending, lost, invalid, queue_full, closed, store_failures
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT(id) DO UPDATE SET updated_at=EXCLUDED.updated_at,
			completed_at=EXCLUDED.completed_at, mode=EXCLUDED.mode,
			accepted=EXCLUDED.accepted, persisted=EXCLUDED.persisted,
			pending=EXCLUDED.pending, lost=EXCLUDED.lost, invalid=EXCLUDED.invalid,
			queue_full=EXCLUDED.queue_full, closed=EXCLUDED.closed,
			store_failures=EXCLUDED.store_failures
	`, session.ID, normalizePostgresTime(session.StartedAt), normalizePostgresTime(session.UpdatedAt),
		session.CompletedAt, session.Mode, session.Accepted, session.Persisted,
		session.Pending, session.Lost, session.Invalid, session.QueueFull,
		session.Closed, session.StoreFailures)
	if err != nil {
		return fmt.Errorf("upsert PostgreSQL capture session: %w", err)
	}
	return nil
}

func (s *Store) ListCaptureSessions(ctx context.Context, query savedtrace.CaptureSessionQuery) ([]savedtrace.CaptureSession, error) {
	statement := `SELECT id, started_at, updated_at, completed_at, mode,
		accepted, persisted, pending, lost, invalid, queue_full, closed, store_failures
		FROM llm_saved_trace_capture_sessions WHERE TRUE`
	args := make([]any, 0, 2)
	if !query.StartedBefore.IsZero() {
		args = append(args, normalizePostgresTime(query.StartedBefore))
		statement += fmt.Sprintf(" AND started_at < $%d", len(args))
	}
	if !query.UpdatedAfter.IsZero() {
		args = append(args, normalizePostgresTime(query.UpdatedAfter))
		statement += fmt.Sprintf(" AND updated_at >= $%d", len(args))
	}
	statement += " ORDER BY started_at, id"
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list PostgreSQL capture sessions: %w", err)
	}
	defer rows.Close()
	var sessions []savedtrace.CaptureSession
	for rows.Next() {
		var session savedtrace.CaptureSession
		if err := rows.Scan(&session.ID, &session.StartedAt, &session.UpdatedAt,
			&session.CompletedAt, &session.Mode, &session.Accepted, &session.Persisted,
			&session.Pending, &session.Lost, &session.Invalid, &session.QueueFull,
			&session.Closed, &session.StoreFailures); err != nil {
			return nil, fmt.Errorf("scan PostgreSQL capture session: %w", err)
		}
		session.StartedAt = session.StartedAt.UTC()
		session.UpdatedAt = session.UpdatedAt.UTC()
		if session.CompletedAt != nil {
			value := session.CompletedAt.UTC()
			session.CompletedAt = &value
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

var _ savedtrace.CaptureSessionStore = (*Store)(nil)
