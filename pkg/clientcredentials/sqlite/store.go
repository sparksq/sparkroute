// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package sqlite persists gateway client credentials for the standalone profile.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/internal/privatepath"

	"github.com/sparksq/sparkroute/pkg/clientcredentials"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Options struct {
	Path        string
	BusyTimeout time.Duration
}

type Store struct {
	db        *sql.DB
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, fmt.Errorf("SQLite client credential path is required")
	}
	if options.BusyTimeout <= 0 {
		options.BusyTimeout = 5 * time.Second
	}
	path := options.Path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve SQLite client credential path: %w", err)
		}
		if err := ensurePrivateFile(absolute); err != nil {
			return nil, err
		}
		path = absolute
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite client credential store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA busy_timeout=%d", options.BusyTimeout.Milliseconds()),
	}
	if path != ":memory:" {
		pragmas = append(pragmas, "PRAGMA journal_mode=WAL")
	}
	for _, statement := range pragmas {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return closeOnError(fmt.Errorf("configure SQLite client credential store: %w", err))
		}
	}
	if err := runMigrations(ctx, db); err != nil {
		return closeOnError(err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Create(ctx context.Context, record clientcredentials.Record, actor string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	roles, allowed, fixed, err := encodePrincipal(record)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite client credential creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO gateway_client_credentials (
			credential_id, tenant_id, name, principal_id, principal_type,
			principal_subject, roles_json, allowed_attribution_json,
			fixed_attribution_json, secret_sha256, state, created_at,
			created_by, updated_at, updated_by, rotated_at, expires_at,
			revoked_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.ID, record.TenantID, record.Name, record.PrincipalID,
		record.PrincipalType, record.PrincipalSubject, roles, allowed, fixed,
		record.SecretSHA256[:], record.State, formatTime(record.CreatedAt),
		record.CreatedBy, formatTime(record.UpdatedAt), record.UpdatedBy,
		nullTime(record.RotatedAt), nullTime(record.ExpiresAt), nullTime(record.RevokedAt))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return clientcredentials.ErrConflict
		}
		return fmt.Errorf("insert SQLite client credential: %w", err)
	}
	if err := insertAudit(ctx, tx, record.ID, record.TenantID, "created", actor,
		map[string]any{"name": record.Name, "principal_id": record.PrincipalID, "state": record.State}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite client credential creation: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (clientcredentials.Record, error) {
	row := s.db.QueryRowContext(ctx, credentialSelect+" WHERE credential_id = ?", id)
	return scanRecord(row)
}

func (s *Store) List(
	ctx context.Context,
	query clientcredentials.ListQuery,
) (clientcredentials.Page, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	where := make([]string, 0, 3)
	arguments := make([]any, 0, 4)
	if query.TenantID != "" {
		where = append(where, "tenant_id = ?")
		arguments = append(arguments, query.TenantID)
	}
	if query.PrincipalID != "" {
		where = append(where, "principal_id = ?")
		arguments = append(arguments, query.PrincipalID)
	}
	if query.State != "" {
		where = append(where, "state = ?")
		arguments = append(arguments, query.State)
	}
	statement := credentialSelect
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY created_at DESC, credential_id DESC LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return clientcredentials.Page{}, fmt.Errorf("list SQLite client credentials: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]clientcredentials.Credential, 0)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return clientcredentials.Page{}, err
		}
		result = append(result, record.Credential)
	}
	if err := rows.Err(); err != nil {
		return clientcredentials.Page{}, fmt.Errorf("iterate SQLite client credentials: %w", err)
	}
	return clientcredentials.Page{Credentials: result}, nil
}

func (s *Store) Rotate(
	ctx context.Context,
	id string,
	digest [32]byte,
	actor string,
) (clientcredentials.Credential, error) {
	return s.mutate(ctx, id, actor, func(ctx context.Context, tx *sql.Tx, current clientcredentials.Record, now time.Time) error {
		if current.State == clientcredentials.StateRevoked {
			return clientcredentials.ErrRevoked
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE gateway_client_credentials
			SET secret_sha256 = ?, rotated_at = ?, updated_at = ?, updated_by = ?
			WHERE credential_id = ?
		`, digest[:], formatTime(now), formatTime(now), actor, id)
		if err != nil {
			return fmt.Errorf("rotate SQLite client credential: %w", err)
		}
		return insertAudit(ctx, tx, id, current.TenantID, "rotated", actor, nil)
	})
}

func (s *Store) SetState(
	ctx context.Context,
	id string,
	state clientcredentials.State,
	actor string,
) (clientcredentials.Credential, error) {
	return s.mutate(ctx, id, actor, func(ctx context.Context, tx *sql.Tx, current clientcredentials.Record, now time.Time) error {
		if current.State == clientcredentials.StateRevoked && state != clientcredentials.StateRevoked {
			return clientcredentials.ErrRevoked
		}
		revokedAt := current.RevokedAt
		if state == clientcredentials.StateRevoked && revokedAt == nil {
			revokedAt = &now
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE gateway_client_credentials
			SET state = ?, updated_at = ?, updated_by = ?, revoked_at = ?
			WHERE credential_id = ?
		`, state, formatTime(now), actor, nullTime(revokedAt), id)
		if err != nil {
			return fmt.Errorf("update SQLite client credential state: %w", err)
		}
		action := string(state)
		if state == clientcredentials.StateActive {
			action = "enabled"
		}
		return insertAudit(ctx, tx, id, current.TenantID, action, actor,
			map[string]any{"previous_state": current.State, "state": state})
	})
}

func (s *Store) mutate(
	ctx context.Context,
	id string,
	actor string,
	mutation func(context.Context, *sql.Tx, clientcredentials.Record, time.Time) error,
) (clientcredentials.Credential, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return clientcredentials.Credential{}, fmt.Errorf("begin SQLite client credential mutation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanRecord(tx.QueryRowContext(ctx, credentialSelect+" WHERE credential_id = ?", id))
	if err != nil {
		return clientcredentials.Credential{}, err
	}
	now := time.Now().UTC()
	if err := mutation(ctx, tx, current, now); err != nil {
		return clientcredentials.Credential{}, err
	}
	updated, err := scanRecord(tx.QueryRowContext(ctx, credentialSelect+" WHERE credential_id = ?", id))
	if err != nil {
		return clientcredentials.Credential{}, err
	}
	if err := tx.Commit(); err != nil {
		return clientcredentials.Credential{}, fmt.Errorf("commit SQLite client credential mutation: %w", err)
	}
	return updated.Credential, nil
}

func (s *Store) ListAudit(
	ctx context.Context,
	query clientcredentials.AuditQuery,
) (clientcredentials.AuditPage, error) {
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	where := make([]string, 0, 3)
	arguments := make([]any, 0, 4)
	if query.TenantID != "" {
		where = append(where, "tenant_id = ?")
		arguments = append(arguments, query.TenantID)
	}
	if query.CredentialID != "" {
		where = append(where, "credential_id = ?")
		arguments = append(arguments, query.CredentialID)
	}
	if query.Action != "" {
		where = append(where, "action = ?")
		arguments = append(arguments, query.Action)
	}
	statement := `SELECT event_id, credential_id, tenant_id, action, occurred_at, actor, details_json
		FROM gateway_client_credential_audit`
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += " ORDER BY event_id DESC LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := s.db.QueryContext(ctx, statement, arguments...)
	if err != nil {
		return clientcredentials.AuditPage{}, fmt.Errorf("list SQLite client credential audit: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]clientcredentials.AuditEvent, 0)
	for rows.Next() {
		var event clientcredentials.AuditEvent
		var occurredAt, details string
		if err := rows.Scan(&event.ID, &event.CredentialID, &event.TenantID,
			&event.Action, &occurredAt, &event.Actor, &details); err != nil {
			return clientcredentials.AuditPage{}, fmt.Errorf("scan SQLite client credential audit: %w", err)
		}
		if event.OccurredAt, err = parseTime(occurredAt); err != nil {
			return clientcredentials.AuditPage{}, err
		}
		if err := json.Unmarshal([]byte(details), &event.Details); err != nil {
			return clientcredentials.AuditPage{}, fmt.Errorf("decode SQLite client credential audit details: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return clientcredentials.AuditPage{}, fmt.Errorf("iterate SQLite client credential audit: %w", err)
	}
	return clientcredentials.AuditPage{Events: events}, nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

const credentialSelect = `SELECT credential_id, tenant_id, name, principal_id,
	principal_type, principal_subject, roles_json, allowed_attribution_json,
	fixed_attribution_json, secret_sha256, state, created_at, created_by,
	updated_at, updated_by, rotated_at, expires_at, revoked_at
	FROM gateway_client_credentials`

type scanner interface {
	Scan(...any) error
}

func scanRecord(row scanner) (clientcredentials.Record, error) {
	var record clientcredentials.Record
	var roles, allowed, fixed string
	var digest []byte
	var state string
	var createdAt, updatedAt string
	var rotatedAt, expiresAt, revokedAt sql.NullString
	if err := row.Scan(&record.ID, &record.TenantID, &record.Name,
		&record.PrincipalID, &record.PrincipalType, &record.PrincipalSubject,
		&roles, &allowed, &fixed, &digest, &state, &createdAt,
		&record.CreatedBy, &updatedAt, &record.UpdatedBy, &rotatedAt,
		&expiresAt, &revokedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return clientcredentials.Record{}, clientcredentials.ErrNotFound
		}
		return clientcredentials.Record{}, fmt.Errorf("scan SQLite client credential: %w", err)
	}
	if len(digest) != len(record.SecretSHA256) {
		return clientcredentials.Record{}, fmt.Errorf("SQLite client credential digest has invalid length")
	}
	copy(record.SecretSHA256[:], digest)
	record.State = clientcredentials.State(state)
	var err error
	if record.CreatedAt, err = parseTime(createdAt); err != nil {
		return clientcredentials.Record{}, err
	}
	if record.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return clientcredentials.Record{}, err
	}
	if record.RotatedAt, err = parseNullTime(rotatedAt); err != nil {
		return clientcredentials.Record{}, err
	}
	if record.ExpiresAt, err = parseNullTime(expiresAt); err != nil {
		return clientcredentials.Record{}, err
	}
	if record.RevokedAt, err = parseNullTime(revokedAt); err != nil {
		return clientcredentials.Record{}, err
	}
	if err := json.Unmarshal([]byte(roles), &record.Roles); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode SQLite client credential roles: %w", err)
	}
	if err := json.Unmarshal([]byte(allowed), &record.AllowedAttribution); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode SQLite client credential attribution: %w", err)
	}
	if err := json.Unmarshal([]byte(fixed), &record.FixedAttribution); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode SQLite client credential fixed attribution: %w", err)
	}
	return record, nil
}

func encodePrincipal(record clientcredentials.Record) (string, string, string, error) {
	roles, err := json.Marshal(record.Roles)
	if err != nil {
		return "", "", "", fmt.Errorf("encode client credential roles: %w", err)
	}
	allowed, err := json.Marshal(record.AllowedAttribution)
	if err != nil {
		return "", "", "", fmt.Errorf("encode client credential attribution: %w", err)
	}
	fixed, err := json.Marshal(record.FixedAttribution)
	if err != nil {
		return "", "", "", fmt.Errorf("encode client credential fixed attribution: %w", err)
	}
	return string(roles), string(allowed), string(fixed), nil
}

func insertAudit(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	tenant string,
	action string,
	actor string,
	details map[string]any,
) error {
	if details == nil {
		details = map[string]any{}
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return fmt.Errorf("encode SQLite client credential audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO gateway_client_credential_audit (
			credential_id, tenant_id, action, occurred_at, actor, details_json
		) VALUES (?, ?, ?, ?, ?, ?)
	`, id, tenant, action, formatTime(time.Now().UTC()), actor, string(raw)); err != nil {
		return fmt.Errorf("insert SQLite client credential audit: %w", err)
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse SQLite client credential timestamp: %w", err)
	}
	return parsed.UTC(), nil
}

func parseNullTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

func ensurePrivateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create SQLite client credential directory: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect SQLite client credential directory: %w", err)
	}
	if permissionErr := privatepath.Check(parent, parentInfo.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite client credential directory %q: %w", parent, permissionErr)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite client credential file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close SQLite client credential file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite client credential file: %w", err)
	}
	if permissionErr := privatepath.Check(path, info.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite client credential file %q: %w", path, permissionErr)
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS client_credential_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create SQLite client credential migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read SQLite client credential migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM client_credential_schema_migrations WHERE version = ?
			)
		`, version).Scan(&exists); err != nil {
			return fmt.Errorf("check SQLite client credential migration %s: %w", version, err)
		}
		if exists {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read SQLite client credential migration %s: %w", version, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin SQLite client credential migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute SQLite client credential migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO client_credential_schema_migrations (version, applied_at)
			VALUES (?, ?)
		`, version, formatTime(time.Now().UTC())); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record SQLite client credential migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit SQLite client credential migration %s: %w", version, err)
		}
	}
	return nil
}
