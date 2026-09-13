// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func (s *Store) Append(ctx context.Context, record savedtrace.Record) error {
	return s.savedTraces.Append(ctx, record)
}

func (s *Store) List(ctx context.Context, query savedtrace.Query) (savedtrace.Page, error) {
	return s.savedTraces.List(ctx, query)
}

func (s *Store) AppendTraceBatch(ctx context.Context, records []savedtrace.Record) error {
	return s.savedTraces.AppendTraceBatch(ctx, records)
}

func (s *Store) UpsertCaptureSession(ctx context.Context, session savedtrace.CaptureSession) error {
	return s.savedTraces.UpsertCaptureSession(ctx, session)
}

func (s *Store) ListCaptureSessions(ctx context.Context, query savedtrace.CaptureSessionQuery) ([]savedtrace.CaptureSession, error) {
	return s.savedTraces.ListCaptureSessions(ctx, query)
}

var _ savedtrace.Store = (*Store)(nil)
var _ savedtrace.BatchStore = (*Store)(nil)
var _ savedtrace.CaptureSessionStore = (*Store)(nil)
