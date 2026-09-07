package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

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

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite file bind: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := bindResourceTx(ctx, tx, affinity); err != nil {
		return err
	}
	var expiresAt any
	if record.ExpiresAt != 0 {
		expiresAt = record.ExpiresAt
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO llm_file_index (
			owner_scope, resource_kind, file_id, bytes, created_at,
			expires_at, filename, purpose
		) VALUES (?, 'file', ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_scope, file_id) DO NOTHING
	`, record.Scope, record.ID, record.Bytes, record.CreatedAt, expiresAt,
		record.Filename, record.Purpose); err != nil {
		return fmt.Errorf("insert SQLite file index: %w", err)
	}
	existing, err := scanSQLiteFile(tx.QueryRowContext(ctx, `
		SELECT owner_scope, file_id, bytes, created_at, expires_at, filename, purpose
		FROM llm_file_index
		WHERE owner_scope = ? AND file_id = ?
	`, record.Scope, record.ID))
	if err != nil {
		return fmt.Errorf("verify SQLite file index: %w", err)
	}
	if existing != record {
		return responsesstate.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite file bind: %w", err)
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
		cursor, err = scanSQLiteFile(s.db.QueryRowContext(ctx, `
			SELECT owner_scope, file_id, bytes, created_at, expires_at, filename, purpose
			FROM llm_file_index
			WHERE owner_scope = ? AND file_id = ?
		`, query.Scope, query.After))
		if errors.Is(err, sql.ErrNoRows) ||
			err == nil && query.Purpose != "" && cursor.Purpose != query.Purpose {
			return responsesstate.FilePage{}, responsesstate.ErrInvalidFileCursor
		}
		if err != nil {
			return responsesstate.FilePage{}, fmt.Errorf("resolve SQLite file cursor: %w", err)
		}
	}

	conditions := []string{
		"f.owner_scope = ?",
		"a.deleted_at IS NULL",
		"(a.expires_at_unix_ns IS NULL OR a.expires_at_unix_ns > ?)",
		"(f.expires_at IS NULL OR f.expires_at > ?)",
	}
	now := time.Now().UTC()
	arguments := []any{query.Scope, now.UnixNano(), now.Unix()}
	if query.Purpose != "" {
		conditions = append(conditions, "f.purpose = ?")
		arguments = append(arguments, query.Purpose)
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
		conditions = append(conditions,
			fmt.Sprintf("(f.created_at %s ? OR (f.created_at = ? AND f.file_id %s ?))", operator, operator),
		)
		arguments = append(arguments, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	arguments = append(arguments, query.Limit+1)
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
		LIMIT ?`
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return responsesstate.FilePage{}, fmt.Errorf("list SQLite files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	page := responsesstate.FilePage{Records: make([]responsesstate.FileRecord, 0, query.Limit)}
	for rows.Next() {
		record, scanErr := scanSQLiteFile(rows)
		if scanErr != nil {
			return responsesstate.FilePage{}, fmt.Errorf("scan SQLite file index: %w", scanErr)
		}
		page.Records = append(page.Records, record)
	}
	if err := rows.Err(); err != nil {
		return responsesstate.FilePage{}, fmt.Errorf("iterate SQLite file index: %w", err)
	}
	if len(page.Records) > query.Limit {
		page.HasMore = true
		page.Records = page.Records[:query.Limit]
	}
	return page, nil
}

type sqliteFileScanner interface {
	Scan(...any) error
}

func scanSQLiteFile(scanner sqliteFileScanner) (responsesstate.FileRecord, error) {
	var record responsesstate.FileRecord
	var expiresAt sql.NullInt64
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
	if expiresAt.Valid {
		record.ExpiresAt = expiresAt.Int64
	}
	return record, nil
}

var _ responsesstate.FileStore = (*Store)(nil)
