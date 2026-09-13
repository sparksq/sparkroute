// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

func (s *Store) BindFile(
	ctx context.Context,
	affinity responsesstate.ResourceAffinity,
	record responsesstate.FileRecord,
) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	if affinity.Kind != responsesstate.ResourceFile {
		return fmt.Errorf("file affinity must use resource kind %q", responsesstate.ResourceFile)
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Scope != affinity.Scope || record.ID != affinity.ResourceID {
		return fmt.Errorf("file metadata does not match routing affinity")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL file bind: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := bindResourceTx(ctx, tx, affinity); err != nil {
		return err
	}
	var expiresAt any
	if record.ExpiresAt != 0 {
		expiresAt = record.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_file_index (
			owner_scope, resource_kind, file_id, bytes, created_at,
			expires_at, filename, purpose
		) VALUES ($1, 'file', $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_scope, file_id) DO NOTHING
	`, record.Scope, record.ID, record.Bytes, record.CreatedAt, expiresAt,
		record.Filename, record.Purpose); err != nil {
		return fmt.Errorf("insert PostgreSQL file index: %w", err)
	}
	existing, err := scanPostgresFile(tx.QueryRow(ctx, `
		SELECT owner_scope, file_id, bytes, created_at, expires_at, filename, purpose
		FROM llm_file_index
		WHERE owner_scope = $1 AND file_id = $2
		FOR UPDATE
	`, record.Scope, record.ID))
	if err != nil {
		return fmt.Errorf("verify PostgreSQL file index: %w", err)
	}
	if existing != record {
		return responsesstate.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL file bind: %w", err)
	}
	return nil
}

func (s *Store) ListFiles(
	ctx context.Context,
	query responsesstate.FileQuery,
) (responsesstate.FilePage, error) {
	query, err := responsesstate.NormalizeFileQuery(query)
	if err != nil {
		return responsesstate.FilePage{}, err
	}
	var cursor responsesstate.FileRecord
	if query.After != "" {
		cursor, err = scanPostgresFile(s.pool.QueryRow(ctx, `
			SELECT owner_scope, file_id, bytes, created_at, expires_at, filename, purpose
			FROM llm_file_index
			WHERE owner_scope = $1 AND file_id = $2
		`, query.Scope, query.After))
		if errors.Is(err, pgx.ErrNoRows) ||
			err == nil && query.Purpose != "" && cursor.Purpose != query.Purpose {
			return responsesstate.FilePage{}, responsesstate.ErrInvalidFileCursor
		}
		if err != nil {
			return responsesstate.FilePage{}, fmt.Errorf("resolve PostgreSQL file cursor: %w", err)
		}
	}

	conditions := []string{
		"f.owner_scope = $1",
		"a.deleted_at IS NULL",
		"(a.expires_at IS NULL OR a.expires_at > now())",
		"(f.expires_at IS NULL OR f.expires_at > EXTRACT(EPOCH FROM now()))",
	}
	arguments := []any{query.Scope}
	addArgument := func(value any) string {
		arguments = append(arguments, value)
		return fmt.Sprintf("$%d", len(arguments))
	}
	if query.Purpose != "" {
		conditions = append(conditions, "f.purpose = "+addArgument(query.Purpose))
	}
	direction := "DESC"
	if query.Order == responsesstate.FileOrderAscending {
		direction = "ASC"
	}
	if query.After != "" {
		operator := "<"
		if query.Order == responsesstate.FileOrderAscending {
			operator = ">"
		}
		createdPlaceholder := addArgument(cursor.CreatedAt)
		idPlaceholder := addArgument(cursor.ID)
		conditions = append(conditions, fmt.Sprintf(
			"(f.created_at %s %s OR (f.created_at = %s AND f.file_id %s %s))",
			operator, createdPlaceholder, createdPlaceholder, operator, idPlaceholder,
		))
	}
	limitPlaceholder := addArgument(query.Limit + 1)
	statement := `
		SELECT f.owner_scope, f.file_id, f.bytes, f.created_at, f.expires_at,
		       f.filename, f.purpose
		FROM llm_file_index f
		JOIN llm_resource_affinities a
		  ON a.owner_scope = f.owner_scope
		 AND a.resource_kind = f.resource_kind
		 AND a.resource_id = f.file_id
		WHERE ` + strings.Join(conditions, " AND ") + `
		ORDER BY f.created_at ` + direction + `, f.file_id ` + direction + `
		LIMIT ` + limitPlaceholder
	rows, err := s.pool.Query(ctx, statement, arguments...)
	if err != nil {
		return responsesstate.FilePage{}, fmt.Errorf("list PostgreSQL files: %w", err)
	}
	defer rows.Close()
	page := responsesstate.FilePage{Records: make([]responsesstate.FileRecord, 0, query.Limit)}
	for rows.Next() {
		record, scanErr := scanPostgresFile(rows)
		if scanErr != nil {
			return responsesstate.FilePage{}, fmt.Errorf("scan PostgreSQL file index: %w", scanErr)
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return responsesstate.FilePage{}, fmt.Errorf("iterate PostgreSQL file index: %w", err)
	}
	if len(page.Records) > query.Limit {
		page.HasMore = true
		page.Records = page.Records[:query.Limit]
	}
	return page, nil
}

type postgresFileScanner interface {
	Scan(...any) error
}

func scanPostgresFile(scanner postgresFileScanner) (responsesstate.FileRecord, error) {
	var record responsesstate.FileRecord
	var expiresAt *int64
	if err := scanner.Scan(
		&record.Scope,
		&record.ID,
		&record.Bytes,
		&record.CreatedAt,
		&expiresAt,
		&record.Filename,
		&record.Purpose,
	); err != nil {
		return responsesstate.FileRecord{}, err
	}
	record.Object = "file"
	if expiresAt != nil {
		record.ExpiresAt = *expiresAt
	}
	return record, nil
}

var _ responsesstate.FileStore = (*Store)(nil)
