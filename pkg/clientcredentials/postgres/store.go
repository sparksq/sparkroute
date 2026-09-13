// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres persists multi-tenant gateway client credentials.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sparksq/sparkroute/pkg/clientcredentials"
)

const (
	defaultPostgresMajor = 17
	migrationLockID      = int64(0x4c4c4d4757434352)
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Options struct {
	URL                   string
	AutoMigrate           bool
	ExpectedPostgresMajor int
	MaxConnections        int32
}

type Store struct {
	pool      *pgxpool.Pool
	closeOnce sync.Once
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.URL) == "" {
		return nil, fmt.Errorf("PostgreSQL client credential URL is required")
	}
	if options.ExpectedPostgresMajor == 0 {
		options.ExpectedPostgresMajor = defaultPostgresMajor
	}
	poolConfig, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL client credential configuration: %w", err)
	}
	if options.MaxConnections > 0 {
		poolConfig.MaxConns = options.MaxConnections
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL client credential pool: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping PostgreSQL client credential store: %w", err))
	}
	if err := validatePostgresMajor(ctx, pool, options.ExpectedPostgresMajor); err != nil {
		return closeOnError(err)
	}
	if options.AutoMigrate {
		if err := runMigrations(ctx, pool); err != nil {
			return closeOnError(err)
		}
	}
	if err := validateSchema(ctx, pool); err != nil {
		return closeOnError(err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Create(ctx context.Context, record clientcredentials.Record, actor string) error {
	roles, allowed, fixed, err := encodePrincipal(record)
	if err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL client credential creation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `
		INSERT INTO gateway_client_credentials (
			credential_id, tenant_id, name, principal_id, principal_type,
			principal_subject, roles, allowed_attribution, fixed_attribution,
			secret_sha256, state, created_at, created_by, updated_at,
			updated_by, rotated_at, expires_at, revoked_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9::jsonb,
			$10, $11, $12, $13, $14, $15, $16, $17, $18)
	`, record.ID, record.TenantID, record.Name, record.PrincipalID,
		record.PrincipalType, record.PrincipalSubject, roles, allowed, fixed,
		record.SecretSHA256[:], record.State, record.CreatedAt, record.CreatedBy,
		record.UpdatedAt, record.UpdatedBy, record.RotatedAt, record.ExpiresAt,
		record.RevokedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return clientcredentials.ErrConflict
		}
		return fmt.Errorf("insert PostgreSQL client credential: %w", err)
	}
	if err := insertAudit(ctx, tx, record.ID, record.TenantID, "created", actor,
		map[string]any{"name": record.Name, "principal_id": record.PrincipalID, "state": record.State}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL client credential creation: %w", err)
	}
	return nil
}

func (s *Store) Get(ctx context.Context, id string) (clientcredentials.Record, error) {
	return scanRecord(s.pool.QueryRow(ctx, credentialSelect+" WHERE credential_id = $1", id))
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
	appendCondition := func(column string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf("%s = $%d", column, len(arguments)))
	}
	if query.TenantID != "" {
		appendCondition("tenant_id", query.TenantID)
	}
	if query.PrincipalID != "" {
		appendCondition("principal_id", query.PrincipalID)
	}
	if query.State != "" {
		appendCondition("state", query.State)
	}
	statement := credentialSelect
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	arguments = append(arguments, limit)
	statement += fmt.Sprintf(" ORDER BY created_at DESC, credential_id DESC LIMIT $%d", len(arguments))
	rows, err := s.pool.Query(ctx, statement, arguments...)
	if err != nil {
		return clientcredentials.Page{}, fmt.Errorf("list PostgreSQL client credentials: %w", err)
	}
	defer rows.Close()
	result := make([]clientcredentials.Credential, 0)
	for rows.Next() {
		record, err := scanRecord(rows)
		if err != nil {
			return clientcredentials.Page{}, err
		}
		result = append(result, record.Credential)
	}
	if err := rows.Err(); err != nil {
		return clientcredentials.Page{}, fmt.Errorf("iterate PostgreSQL client credentials: %w", err)
	}
	return clientcredentials.Page{Credentials: result}, nil
}

func (s *Store) Rotate(
	ctx context.Context,
	id string,
	digest [32]byte,
	actor string,
) (clientcredentials.Credential, error) {
	return s.mutate(ctx, id, actor, func(ctx context.Context, tx pgx.Tx, current clientcredentials.Record, now time.Time) error {
		if current.State == clientcredentials.StateRevoked {
			return clientcredentials.ErrRevoked
		}
		if _, err := tx.Exec(ctx, `
			UPDATE gateway_client_credentials
			SET secret_sha256 = $1, rotated_at = $2, updated_at = $2, updated_by = $3
			WHERE credential_id = $4
		`, digest[:], now, actor, id); err != nil {
			return fmt.Errorf("rotate PostgreSQL client credential: %w", err)
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
	return s.mutate(ctx, id, actor, func(ctx context.Context, tx pgx.Tx, current clientcredentials.Record, now time.Time) error {
		if current.State == clientcredentials.StateRevoked && state != clientcredentials.StateRevoked {
			return clientcredentials.ErrRevoked
		}
		revokedAt := current.RevokedAt
		if state == clientcredentials.StateRevoked && revokedAt == nil {
			revokedAt = &now
		}
		if _, err := tx.Exec(ctx, `
			UPDATE gateway_client_credentials
			SET state = $1, updated_at = $2, updated_by = $3, revoked_at = $4
			WHERE credential_id = $5
		`, state, now, actor, revokedAt, id); err != nil {
			return fmt.Errorf("update PostgreSQL client credential state: %w", err)
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
	mutation func(context.Context, pgx.Tx, clientcredentials.Record, time.Time) error,
) (clientcredentials.Credential, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return clientcredentials.Credential{}, fmt.Errorf("begin PostgreSQL client credential mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanRecord(tx.QueryRow(ctx, credentialSelect+" WHERE credential_id = $1 FOR UPDATE", id))
	if err != nil {
		return clientcredentials.Credential{}, err
	}
	now := time.Now().UTC()
	if err := mutation(ctx, tx, current, now); err != nil {
		return clientcredentials.Credential{}, err
	}
	updated, err := scanRecord(tx.QueryRow(ctx, credentialSelect+" WHERE credential_id = $1", id))
	if err != nil {
		return clientcredentials.Credential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return clientcredentials.Credential{}, fmt.Errorf("commit PostgreSQL client credential mutation: %w", err)
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
	appendCondition := func(column string, value any) {
		arguments = append(arguments, value)
		where = append(where, fmt.Sprintf("%s = $%d", column, len(arguments)))
	}
	if query.TenantID != "" {
		appendCondition("tenant_id", query.TenantID)
	}
	if query.CredentialID != "" {
		appendCondition("credential_id", query.CredentialID)
	}
	if query.Action != "" {
		appendCondition("action", query.Action)
	}
	statement := `SELECT event_id, credential_id, tenant_id, action,
		occurred_at, actor, details FROM gateway_client_credential_audit`
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	arguments = append(arguments, limit)
	statement += fmt.Sprintf(" ORDER BY event_id DESC LIMIT $%d", len(arguments))
	rows, err := s.pool.Query(ctx, statement, arguments...)
	if err != nil {
		return clientcredentials.AuditPage{}, fmt.Errorf("list PostgreSQL client credential audit: %w", err)
	}
	defer rows.Close()
	events := make([]clientcredentials.AuditEvent, 0)
	for rows.Next() {
		var event clientcredentials.AuditEvent
		var details []byte
		if err := rows.Scan(&event.ID, &event.CredentialID, &event.TenantID,
			&event.Action, &event.OccurredAt, &event.Actor, &details); err != nil {
			return clientcredentials.AuditPage{}, fmt.Errorf("scan PostgreSQL client credential audit: %w", err)
		}
		event.OccurredAt = event.OccurredAt.UTC()
		if err := json.Unmarshal(details, &event.Details); err != nil {
			return clientcredentials.AuditPage{}, fmt.Errorf("decode PostgreSQL client credential audit details: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return clientcredentials.AuditPage{}, fmt.Errorf("iterate PostgreSQL client credential audit: %w", err)
	}
	return clientcredentials.AuditPage{Events: events}, nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.pool.Close() })
	return nil
}

const credentialSelect = `SELECT credential_id, tenant_id, name, principal_id,
	principal_type, principal_subject, roles, allowed_attribution,
	fixed_attribution, secret_sha256, state, created_at, created_by,
	updated_at, updated_by, rotated_at, expires_at, revoked_at
	FROM gateway_client_credentials`

type scanner interface {
	Scan(...any) error
}

func scanRecord(row scanner) (clientcredentials.Record, error) {
	var record clientcredentials.Record
	var roles, allowed, fixed []byte
	var digest []byte
	var state string
	var rotatedAt, expiresAt, revokedAt *time.Time
	if err := row.Scan(&record.ID, &record.TenantID, &record.Name,
		&record.PrincipalID, &record.PrincipalType, &record.PrincipalSubject,
		&roles, &allowed, &fixed, &digest, &state, &record.CreatedAt,
		&record.CreatedBy, &record.UpdatedAt, &record.UpdatedBy, &rotatedAt,
		&expiresAt, &revokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return clientcredentials.Record{}, clientcredentials.ErrNotFound
		}
		return clientcredentials.Record{}, fmt.Errorf("scan PostgreSQL client credential: %w", err)
	}
	if len(digest) != len(record.SecretSHA256) {
		return clientcredentials.Record{}, fmt.Errorf("PostgreSQL client credential digest has invalid length")
	}
	copy(record.SecretSHA256[:], digest)
	record.State = clientcredentials.State(state)
	record.CreatedAt = record.CreatedAt.UTC()
	record.UpdatedAt = record.UpdatedAt.UTC()
	record.RotatedAt = utcPointer(rotatedAt)
	record.ExpiresAt = utcPointer(expiresAt)
	record.RevokedAt = utcPointer(revokedAt)
	if err := json.Unmarshal(roles, &record.Roles); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode PostgreSQL client credential roles: %w", err)
	}
	if err := json.Unmarshal(allowed, &record.AllowedAttribution); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode PostgreSQL client credential attribution: %w", err)
	}
	if err := json.Unmarshal(fixed, &record.FixedAttribution); err != nil {
		return clientcredentials.Record{}, fmt.Errorf("decode PostgreSQL client credential fixed attribution: %w", err)
	}
	return record, nil
}

func encodePrincipal(record clientcredentials.Record) ([]byte, []byte, []byte, error) {
	roles, err := json.Marshal(record.Roles)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode client credential roles: %w", err)
	}
	allowed, err := json.Marshal(record.AllowedAttribution)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode client credential attribution: %w", err)
	}
	fixed, err := json.Marshal(record.FixedAttribution)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encode client credential fixed attribution: %w", err)
	}
	return roles, allowed, fixed, nil
}

func insertAudit(
	ctx context.Context,
	tx pgx.Tx,
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
		return fmt.Errorf("encode PostgreSQL client credential audit: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO gateway_client_credential_audit (
			credential_id, tenant_id, action, actor, details
		) VALUES ($1, $2, $3, $4, $5::jsonb)
	`, id, tenant, action, actor, raw); err != nil {
		return fmt.Errorf("insert PostgreSQL client credential audit: %w", err)
	}
	return nil
}

func utcPointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func validatePostgresMajor(ctx context.Context, pool *pgxpool.Pool, expected int) error {
	var raw string
	if err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return fmt.Errorf("read PostgreSQL server version: %w", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse PostgreSQL server version %q: %w", raw, err)
	}
	if major := version / 10000; major != expected {
		return fmt.Errorf("PostgreSQL major %d is required, server reports major %d", expected, major)
	}
	return nil
}

func validateSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{"gateway_client_credentials", "gateway_client_credential_audit"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL", table).Scan(&exists); err != nil {
			return fmt.Errorf("validate PostgreSQL client credential table %s: %w", table, err)
		}
		if !exists {
			return fmt.Errorf("PostgreSQL client credential table %s is missing; run migrations first", table)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS client_credential_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create PostgreSQL client credential migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read PostgreSQL client credential migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read PostgreSQL client credential migration %s: %w", version, err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin PostgreSQL client credential migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock PostgreSQL client credential migration %s: %w", version, err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM client_credential_schema_migrations WHERE version = $1
			)
		`, version).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check PostgreSQL client credential migration %s: %w", version, err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("execute PostgreSQL client credential migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO client_credential_schema_migrations (version) VALUES ($1)
			`, version); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("record PostgreSQL client credential migration %s: %w", version, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit PostgreSQL client credential migration %s: %w", version, err)
		}
	}
	return nil
}
