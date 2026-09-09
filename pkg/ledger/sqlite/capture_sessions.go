// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func (s *Store) UpsertCaptureSession(ctx context.Context, session savedtrace.CaptureSession) error {
	if err := session.Validate(); err != nil {
		return fmt.Errorf("validate SQLite capture session: %w", err)
	}
	var completed any
	if session.CompletedAt != nil {
		completed = formatTime(*session.CompletedAt)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO llm_saved_trace_capture_sessions (
			id, started_at, started_at_unix_ns, updated_at, updated_at_unix_ns,
			completed_at, mode, accepted, persisted, pending, lost, invalid,
			queue_full, closed, store_failures
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			updated_at=excluded.updated_at, updated_at_unix_ns=excluded.updated_at_unix_ns,
			completed_at=excluded.completed_at, mode=excluded.mode,
			accepted=excluded.accepted, persisted=excluded.persisted,
			pending=excluded.pending, lost=excluded.lost, invalid=excluded.invalid,
			queue_full=excluded.queue_full, closed=excluded.closed,
			store_failures=excluded.store_failures
	`, session.ID, formatTime(session.StartedAt), session.StartedAt.UnixNano(),
		formatTime(session.UpdatedAt), session.UpdatedAt.UnixNano(), completed,
		session.Mode, session.Accepted, session.Persisted, session.Pending,
		session.Lost, session.Invalid, session.QueueFull, session.Closed,
		session.StoreFailures)
	if err != nil {
		return fmt.Errorf("upsert SQLite capture session: %w", err)
	}
	return nil
}

func (s *Store) ListCaptureSessions(ctx context.Context, query savedtrace.CaptureSessionQuery) ([]savedtrace.CaptureSession, error) {
	statement := `SELECT id, started_at, updated_at, completed_at, mode,
		accepted, persisted, pending, lost, invalid, queue_full, closed, store_failures
		FROM llm_saved_trace_capture_sessions WHERE 1=1`
	args := make([]any, 0, 2)
	if !query.StartedBefore.IsZero() {
		statement += " AND started_at_unix_ns < ?"
		args = append(args, query.StartedBefore.UTC().UnixNano())
	}
	if !query.UpdatedAfter.IsZero() {
		statement += " AND updated_at_unix_ns >= ?"
		args = append(args, query.UpdatedAfter.UTC().UnixNano())
	}
	statement += " ORDER BY started_at_unix_ns, id"
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("list SQLite capture sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []savedtrace.CaptureSession
	for rows.Next() {
		var session savedtrace.CaptureSession
		var started, updated string
		var completed sql.NullString
		if err := rows.Scan(&session.ID, &started, &updated, &completed, &session.Mode,
			&session.Accepted, &session.Persisted, &session.Pending, &session.Lost,
			&session.Invalid, &session.QueueFull, &session.Closed, &session.StoreFailures); err != nil {
			return nil, fmt.Errorf("scan SQLite capture session: %w", err)
		}
		session.StartedAt, err = time.Parse(time.RFC3339Nano, started)
		if err != nil {
			return nil, err
		}
		session.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		if completed.Valid {
			value, parseErr := time.Parse(time.RFC3339Nano, completed.String)
			if parseErr != nil {
				return nil, parseErr
			}
			session.CompletedAt = &value
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

var _ savedtrace.CaptureSessionStore = (*Store)(nil)
