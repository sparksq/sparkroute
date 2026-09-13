// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres implements the SparkRoute PostgreSQL ledger with an
// optional TimescaleDB specialization.
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
	"github.com/jackc/pgx/v5/pgxpool"
	savedtracepostgres "github.com/sparksq/sparkroute/pkg/savedtrace/postgres"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

const (
	defaultPostgresMajor = 17
	minTimescaleMajor    = 2
	minTimescaleMinor    = 17
	migrationLockID      = int64(0x4c4c4d47574d4947)
)

//go:embed migrations/*.sql migrations/timescaledb/*.sql
var migrationFiles embed.FS

// Backend selects the PostgreSQL-compatible storage behavior used by the
// ledger. TimescaleDB extends the common PostgreSQL schema with hypertables and
// chunk-aware retention.
type Backend string

const (
	BackendPostgres    Backend = "postgres"
	BackendTimescaleDB Backend = "timescaledb"
)

type Options struct {
	URL                   string
	AutoMigrate           bool
	Backend               Backend
	ExpectedPostgresMajor int
	MaxConnections        int32
}

type Store struct {
	pool        *pgxpool.Pool
	backend     Backend
	savedTraces *savedtracepostgres.Store
	closeOnce   sync.Once
}

// ParseBackend validates a configured ledger backend. The empty value selects
// the portable PostgreSQL base implementation.
func ParseBackend(value string) (Backend, error) {
	backend := Backend(strings.ToLower(strings.TrimSpace(value)))
	switch backend {
	case "", BackendPostgres:
		return BackendPostgres, nil
	case BackendTimescaleDB:
		return BackendTimescaleDB, nil
	default:
		return "", fmt.Errorf("ledger backend must be postgres or timescaledb")
	}
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.URL) == "" {
		return nil, fmt.Errorf("PostgreSQL ledger URL is required")
	}
	backend, err := ParseBackend(string(options.Backend))
	if err != nil {
		return nil, err
	}
	if options.ExpectedPostgresMajor == 0 {
		options.ExpectedPostgresMajor = defaultPostgresMajor
	}
	poolConfig, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL ledger configuration: %w", err)
	}
	if options.MaxConnections > 0 {
		poolConfig.MaxConns = options.MaxConnections
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL ledger pool: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping PostgreSQL ledger: %w", err))
	}
	if err := validatePostgresMajor(ctx, pool, options.ExpectedPostgresMajor); err != nil {
		return closeOnError(err)
	}
	if options.AutoMigrate {
		if err := runMigrations(ctx, pool, backend); err != nil {
			return closeOnError(err)
		}
	}
	if err := validateSchema(ctx, pool); err != nil {
		return closeOnError(err)
	}
	if backend == BackendTimescaleDB {
		if err := validateTimescale(ctx, pool); err != nil {
			return closeOnError(err)
		}
	}
	savedTraces, err := savedtracepostgres.OpenPool(ctx, pool, options.AutoMigrate)
	if err != nil {
		return closeOnError(fmt.Errorf("open shared PostgreSQL saved-trace store: %w", err))
	}
	return &Store{pool: pool, backend: backend, savedTraces: savedTraces}, nil
}

func validatePostgresMajor(
	ctx context.Context,
	pool *pgxpool.Pool,
	expectedMajor int,
) error {
	var raw string
	if err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&raw); err != nil {
		return fmt.Errorf("read PostgreSQL server version: %w", err)
	}
	version, err := strconv.Atoi(raw)
	if err != nil {
		return fmt.Errorf("parse PostgreSQL server version %q: %w", raw, err)
	}
	if major := version / 10000; major != expectedMajor {
		return fmt.Errorf(
			"PostgreSQL major %d is required, server reports major %d",
			expectedMajor,
			major,
		)
	}
	return nil
}

func validateTimescale(ctx context.Context, pool *pgxpool.Pool) error {
	var version string
	err := pool.QueryRow(
		ctx,
		"SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'",
	).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("TimescaleDB extension is required")
	}
	if err != nil {
		return fmt.Errorf("read TimescaleDB extension version: %w", err)
	}
	major, minor, err := parseTimescaleVersion(version)
	if err != nil {
		return err
	}
	if major < minTimescaleMajor ||
		major == minTimescaleMajor && minor < minTimescaleMinor {
		return fmt.Errorf(
			"TimescaleDB >= %d.%d is required, server reports %s",
			minTimescaleMajor,
			minTimescaleMinor,
			version,
		)
	}
	var hypertables int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM timescaledb_information.hypertables
		WHERE hypertable_schema = current_schema()
		  AND hypertable_name = ANY($1)
	`, []string{"llm_requests", "llm_attempts", "model_runtime_events"}).Scan(&hypertables); err != nil {
		return fmt.Errorf("validate TimescaleDB hypertables: %w", err)
	}
	if hypertables != 3 {
		return fmt.Errorf("expected 3 ledger hypertables, found %d", hypertables)
	}
	return nil
}

func parseTimescaleVersion(version string) (int, int, error) {
	base := strings.SplitN(version, "-", 2)[0]
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("parse TimescaleDB extension version %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse TimescaleDB major version %q: %w", version, err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse TimescaleDB minor version %q: %w", version, err)
	}
	return major, minor, nil
}

func validateSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{
		"llm_requests",
		"llm_request_lookup",
		"llm_attempts",
		"llm_attempt_lookup",
		"model_runtime_events",
		"llm_response_affinities",
		"llm_resource_affinities",
		"llm_file_index",
		"llm_prompt_cache_affinities",
		"llm_pii_conversation_mappings",
	} {
		var exists bool
		if err := pool.QueryRow(
			ctx,
			"SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL",
			table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("validate PostgreSQL ledger table %s: %w", table, err)
		}
		if !exists {
			return fmt.Errorf(
				"PostgreSQL ledger table %s is missing; run migrations first",
				table,
			)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool, backend Backend) error {
	if err := runMigrationSet(
		ctx,
		pool,
		"migrations",
		"sparkroute_schema_migrations",
		"PostgreSQL",
	); err != nil {
		return err
	}
	if backend == BackendTimescaleDB {
		return runMigrationSet(
			ctx,
			pool,
			"migrations/timescaledb",
			"sparkroute_timescaledb_schema_migrations",
			"TimescaleDB",
		)
	}
	return nil
}

func runMigrationSet(
	ctx context.Context,
	pool *pgxpool.Pool,
	directory string,
	migrationTable string,
	label string,
) error {
	if _, err := pool.Exec(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %s (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`, migrationTable)); err != nil {
		return fmt.Errorf("create %s migration table: %w", label, err)
	}
	entries, err := migrationFiles.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read %s ledger migrations: %w", label, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		content, err := migrationFiles.ReadFile(directory + "/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read %s migration %s: %w", label, version, err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin %s migration %s: %w", label, version, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock %s migration %s: %w", label, version, err)
		}
		var exists bool
		if err := tx.QueryRow(
			ctx,
			fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s WHERE version = $1)", migrationTable),
			version,
		).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check %s migration %s: %w", label, version, err)
		}
		if exists {
			if err := tx.Commit(ctx); err != nil {
				return fmt.Errorf("commit %s migration check %s: %w", label, version, err)
			}
			continue
		}
		if _, err := tx.Exec(ctx, string(content)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("execute %s migration %s: %w", label, version, err)
		}
		if _, err := tx.Exec(
			ctx,
			fmt.Sprintf("INSERT INTO %s (version) VALUES ($1)", migrationTable),
			version,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record %s migration %s: %w", label, version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit %s migration %s: %w", label, version, err)
		}
	}
	return nil
}

func (s *Store) AppendBatch(ctx context.Context, records []ledger.Record) error {
	if len(records) == 0 {
		return nil
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL ledger batch: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	batch := &pgx.Batch{}
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("validate PostgreSQL ledger record: %w", err)
		}
		switch record.Kind {
		case ledger.KindRequest:
			started := normalizePostgresTime(record.Request.StartedAt)
			if err := ensureRequestKey(ctx, tx, record.Request.RequestID, started); err != nil {
				return err
			}
			args, err := requestArgs(*record.Request, started)
			if err != nil {
				return err
			}
			batch.Queue(insertRequestSQL, args...)
		case ledger.KindAttempt:
			started := normalizePostgresTime(record.Attempt.StartedAt)
			if err := ensureAttemptKey(
				ctx,
				tx,
				record.Attempt.RequestID,
				record.Attempt.Attempt,
				started,
			); err != nil {
				return err
			}
			batch.Queue(insertAttemptSQL, attemptArgs(*record.Attempt, started)...)
		case ledger.KindRuntimeEvent:
			batch.Queue(insertRuntimeEventSQL, runtimeEventArgs(*record.RuntimeEvent)...)
		default:
			return fmt.Errorf("unsupported ledger record kind %q", record.Kind)
		}
	}
	results := tx.SendBatch(ctx, batch)
	for range records {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("execute PostgreSQL ledger batch: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close PostgreSQL ledger batch: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL ledger batch: %w", err)
	}
	return nil
}

func (s *Store) Resolve(
	ctx context.Context,
	scope string,
	responseID string,
) (responsesstate.Affinity, bool, error) {
	if err := responsesstate.ValidateScope(scope); err != nil {
		return responsesstate.Affinity{}, false, err
	}
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return responsesstate.Affinity{}, false, err
	}
	var affinity responsesstate.Affinity
	err := s.pool.QueryRow(ctx, `
		SELECT owner_scope, response_id, virtual_model, provider, deployment, upstream_model,
		       bound_at, expires_at
		FROM llm_response_affinities
		WHERE owner_scope = $1 AND response_id = $2 AND expires_at > now()
	`, scope, responseID).Scan(
		&affinity.Scope,
		&affinity.ResponseID,
		&affinity.VirtualModel,
		&affinity.Provider,
		&affinity.Deployment,
		&affinity.UpstreamModel,
		&affinity.BoundAt,
		&affinity.ExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return responsesstate.Affinity{}, false, nil
	}
	if err != nil {
		return responsesstate.Affinity{}, false, fmt.Errorf(
			"resolve PostgreSQL response affinity: %w",
			err,
		)
	}
	return affinity, true, nil
}

func (s *Store) Bind(ctx context.Context, affinity responsesstate.Affinity) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL response affinity bind: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if _, err := tx.Exec(
		ctx,
		"DELETE FROM llm_response_affinities WHERE expires_at <= now()",
	); err != nil {
		return fmt.Errorf("expire PostgreSQL response affinities: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_response_affinities (
			owner_scope, response_id, virtual_model, provider, deployment, upstream_model,
			bound_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (owner_scope, response_id) DO NOTHING
	`,
		affinity.Scope,
		affinity.ResponseID,
		affinity.VirtualModel,
		affinity.Provider,
		affinity.Deployment,
		affinity.UpstreamModel,
		affinity.BoundAt,
		affinity.ExpiresAt,
	); err != nil {
		return fmt.Errorf("insert PostgreSQL response affinity: %w", err)
	}
	var existing responsesstate.Affinity
	if err := tx.QueryRow(ctx, `
		SELECT owner_scope, response_id, virtual_model, provider, deployment, upstream_model
		FROM llm_response_affinities
		WHERE owner_scope = $1 AND response_id = $2
	`, affinity.Scope, affinity.ResponseID).Scan(
		&existing.Scope,
		&existing.ResponseID,
		&existing.VirtualModel,
		&existing.Provider,
		&existing.Deployment,
		&existing.UpstreamModel,
	); err != nil {
		return fmt.Errorf("verify PostgreSQL response affinity: %w", err)
	}
	if !responsesstate.SameRoute(existing, affinity) {
		return responsesstate.ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL response affinity bind: %w", err)
	}
	return nil
}

func (s *Store) ResolveResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
) (responsesstate.ResourceAffinity, bool, error) {
	if err := key.Validate(); err != nil {
		return responsesstate.ResourceAffinity{}, false, err
	}
	var affinity responsesstate.ResourceAffinity
	var kind string
	var deletedAt, expiresAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT owner_scope, resource_kind, resource_id, virtual_model, provider,
		       deployment, upstream_model, bound_at, deleted_at, expires_at
		FROM llm_resource_affinities
		WHERE owner_scope = $1 AND resource_kind = $2 AND resource_id = $3
		  AND (expires_at IS NULL OR expires_at > now())
	`, key.Scope, string(key.Kind), key.ResourceID).Scan(
		&affinity.Scope,
		&kind,
		&affinity.ResourceID,
		&affinity.VirtualModel,
		&affinity.Provider,
		&affinity.Deployment,
		&affinity.UpstreamModel,
		&affinity.BoundAt,
		&deletedAt,
		&expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return responsesstate.ResourceAffinity{}, false, nil
	}
	if err != nil {
		return responsesstate.ResourceAffinity{}, false, fmt.Errorf(
			"resolve PostgreSQL resource affinity: %w",
			err,
		)
	}
	affinity.Kind = responsesstate.ResourceKind(kind)
	if deletedAt != nil {
		affinity.DeletedAt = *deletedAt
	}
	if expiresAt != nil {
		affinity.ExpiresAt = *expiresAt
	}
	return affinity, true, nil
}

func (s *Store) BindResource(
	ctx context.Context,
	affinity responsesstate.ResourceAffinity,
) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin PostgreSQL resource affinity bind: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	if err := bindResourceTx(ctx, tx, affinity); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit PostgreSQL resource affinity bind: %w", err)
	}
	return nil
}

func bindResourceTx(
	ctx context.Context,
	tx pgx.Tx,
	affinity responsesstate.ResourceAffinity,
) error {
	if _, err := tx.Exec(
		ctx,
		`DELETE FROM llm_resource_affinities
		 WHERE expires_at IS NOT NULL AND expires_at <= now()`,
	); err != nil {
		return fmt.Errorf("expire PostgreSQL resource affinities: %w", err)
	}
	var deletedAt, expiresAt any
	if !affinity.DeletedAt.IsZero() {
		deletedAt = affinity.DeletedAt
	}
	if !affinity.ExpiresAt.IsZero() {
		expiresAt = affinity.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_resource_affinities (
			owner_scope, resource_kind, resource_id, virtual_model, provider,
			deployment, upstream_model, bound_at, deleted_at, expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (owner_scope, resource_kind, resource_id) DO NOTHING
	`,
		affinity.Scope,
		string(affinity.Kind),
		affinity.ResourceID,
		affinity.VirtualModel,
		affinity.Provider,
		affinity.Deployment,
		affinity.UpstreamModel,
		affinity.BoundAt,
		deletedAt,
		expiresAt,
	); err != nil {
		return fmt.Errorf("insert PostgreSQL resource affinity: %w", err)
	}
	var existing responsesstate.ResourceAffinity
	var kind string
	var existingDeletedAt, existingExpiresAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT owner_scope, resource_kind, resource_id, virtual_model, provider,
		       deployment, upstream_model, deleted_at, expires_at
		FROM llm_resource_affinities
		WHERE owner_scope = $1 AND resource_kind = $2 AND resource_id = $3
		FOR UPDATE
	`, affinity.Scope, string(affinity.Kind), affinity.ResourceID).Scan(
		&existing.Scope,
		&kind,
		&existing.ResourceID,
		&existing.VirtualModel,
		&existing.Provider,
		&existing.Deployment,
		&existing.UpstreamModel,
		&existingDeletedAt,
		&existingExpiresAt,
	); err != nil {
		return fmt.Errorf("verify PostgreSQL resource affinity: %w", err)
	}
	existing.Kind = responsesstate.ResourceKind(kind)
	if existingDeletedAt != nil ||
		!responsesstate.SameResourceRoute(existing, affinity) {
		return responsesstate.ErrConflict
	}
	if existingExpiresAt != nil {
		existing.ExpiresAt = *existingExpiresAt
	}
	mergedExpiry := responsesstate.MergeResourceExpiry(
		existing.ExpiresAt,
		affinity.ExpiresAt,
	)
	var mergedExpiryValue any
	if !mergedExpiry.IsZero() {
		mergedExpiryValue = mergedExpiry
	}
	if _, err := tx.Exec(ctx, `
		UPDATE llm_resource_affinities
		SET expires_at = $1
		WHERE owner_scope = $2 AND resource_kind = $3 AND resource_id = $4
		  AND deleted_at IS NULL
	`,
		mergedExpiryValue,
		affinity.Scope,
		string(affinity.Kind),
		affinity.ResourceID,
	); err != nil {
		return fmt.Errorf("extend PostgreSQL resource affinity: %w", err)
	}
	return nil
}

func (s *Store) TombstoneResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
	deletedAt time.Time,
	expiresAt time.Time,
) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if deletedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(deletedAt) {
		return fmt.Errorf("tombstone expiry must be after deleted time")
	}
	result, err := s.pool.Exec(ctx, `
		UPDATE llm_resource_affinities
		SET deleted_at = COALESCE(deleted_at, $1),
		    expires_at = CASE WHEN deleted_at IS NULL THEN $2 ELSE expires_at END
		WHERE owner_scope = $3 AND resource_kind = $4 AND resource_id = $5
		  AND (expires_at IS NULL OR expires_at > now())
	`,
		deletedAt,
		expiresAt,
		key.Scope,
		string(key.Kind),
		key.ResourceID,
	)
	if err != nil {
		return fmt.Errorf("tombstone PostgreSQL resource affinity: %w", err)
	}
	if result.RowsAffected() == 0 {
		return responsesstate.ErrNotFound
	}
	return nil
}

func ensureRequestKey(
	ctx context.Context,
	tx pgx.Tx,
	requestID string,
	started time.Time,
) error {
	var actual time.Time
	if err := tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO llm_request_lookup (request_id, started_at)
			VALUES ($1, $2)
			ON CONFLICT (request_id) DO NOTHING
			RETURNING started_at
		)
		SELECT started_at FROM inserted
		UNION ALL
		SELECT started_at FROM llm_request_lookup WHERE request_id = $1
		LIMIT 1
	`, requestID, started).Scan(&actual); err != nil {
		return fmt.Errorf("reserve PostgreSQL request key: %w", err)
	}
	if !actual.Equal(started) {
		return fmt.Errorf("request ID %q was already used with a different start time", requestID)
	}
	return nil
}

func ensureAttemptKey(
	ctx context.Context,
	tx pgx.Tx,
	requestID string,
	attempt int,
	started time.Time,
) error {
	var actual time.Time
	if err := tx.QueryRow(ctx, `
		WITH inserted AS (
			INSERT INTO llm_attempt_lookup (request_id, attempt_no, attempt_started_at)
			VALUES ($1, $2, $3)
			ON CONFLICT (request_id, attempt_no) DO NOTHING
			RETURNING attempt_started_at
		)
		SELECT attempt_started_at FROM inserted
		UNION ALL
		SELECT attempt_started_at
		FROM llm_attempt_lookup
		WHERE request_id = $1 AND attempt_no = $2
		LIMIT 1
	`, requestID, attempt, started).Scan(&actual); err != nil {
		return fmt.Errorf("reserve PostgreSQL attempt key: %w", err)
	}
	if !actual.Equal(started) {
		return fmt.Errorf(
			"request %q attempt %d was already used with a different start time",
			requestID,
			attempt,
		)
	}
	return nil
}

func (s *Store) Health(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("PostgreSQL ledger health: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.pool.Close()
	})
	return nil
}

func requestArgs(record ledger.RequestRecord, started time.Time) ([]any, error) {
	usage := usageArgs(record.Usage)
	principalRoles, err := jsonValue(record.PrincipalRoles, len(record.PrincipalRoles) > 0)
	if err != nil {
		return nil, fmt.Errorf("encode PostgreSQL principal roles: %w", err)
	}
	attribution, err := jsonValue(record.Attribution, len(record.Attribution) > 0)
	if err != nil {
		return nil, fmt.Errorf("encode PostgreSQL attribution: %w", err)
	}
	return []any{
		started,
		record.RequestID,
		normalizePostgresTime(record.CompletedAt),
		nullString(record.PrincipalID),
		nullString(record.PrincipalType),
		nullString(record.PrincipalSubject),
		principalRoles,
		nullString(record.TenantID),
		attribution,
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
		string(record.Outcome),
		nullString(record.FailureClass),
		record.Latency.Nanoseconds(),
		nullDuration(record.TimeToFirstByte),
		usage[0], usage[1], usage[2], usage[3], usage[4],
		usage[5], usage[6], usage[7], usage[8], usage[9],
		usage[10], string(record.Usage.Completeness),
		nullString(record.Usage.NormalizationVersion),
	}, nil
}

func jsonValue(value any, present bool) (any, error) {
	if !present {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func attemptArgs(record ledger.AttemptRecord, started time.Time) []any {
	usage := usageArgs(record.Usage)
	return []any{
		started,
		record.RequestID,
		record.Attempt,
		nullTime(record.FirstByteAt),
		normalizePostgresTime(record.CompletedAt),
		record.Provider,
		record.Deployment,
		record.UpstreamModel,
		record.PoolPriority,
		nullInt(record.HTTPStatus),
		nullString(record.UpstreamRequestID),
		string(record.Outcome),
		nullString(record.FailureClass),
		record.Retried,
		record.Latency.Nanoseconds(),
		usage[0], usage[1], usage[2], usage[3], usage[4],
		usage[5], usage[6], usage[7], usage[8], usage[9],
		usage[10], string(record.Usage.Completeness),
		nullString(record.Usage.NormalizationVersion),
	}
}

func runtimeEventArgs(record ledger.RuntimeEventRecord) []any {
	return []any{
		normalizePostgresTime(record.OccurredAt),
		record.EventID,
		record.Controller,
		nullString(record.BindingRevision),
		nullString(record.VirtualModel),
		nullString(record.Deployment),
		nullString(record.EndpointInstance),
		nullString(record.ClusterID),
		nullString(record.JobID),
		nullString(record.RecipeRevision),
		nullString(record.PriorState),
		record.NewState,
		nullString(record.Reason),
		nullDuration(record.Latency),
		record.QueueDepth,
		record.WaiterCount,
		string(record.Outcome),
		nil,
	}
}

func usageArgs(usage ledger.TokenUsage) [11]any {
	var result [11]any
	result[0] = nullToken(usage.InputTokens)
	result[1] = nullToken(usage.OutputTokens)
	result[2] = nullToken(usage.TotalTokens)
	result[3] = nullToken(usage.CachedInputTokens)
	result[4] = nullToken(usage.CacheCreationTokens)
	result[5] = nullToken(usage.ReasoningTokens)
	result[6] = nullToken(usage.ToolUsePromptTokens)
	result[7] = nullToken(usage.AcceptedPredictionTokens)
	result[8] = nullToken(usage.RejectedPredictionTokens)
	if len(usage.ProviderComponents) > 0 {
		result[9] = usage.ProviderComponents
	}
	if len(usage.Raw) > 0 {
		result[10] = json.RawMessage(usage.Raw)
	}
	return result
}

func normalizePostgresTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return normalizePostgresTime(*value)
}

func nullDuration(value *time.Duration) any {
	if value == nil {
		return nil
	}
	return value.Nanoseconds()
}

func nullToken(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const insertRequestSQL = `
	INSERT INTO llm_requests (
		started_at, request_id, completed_at,
		principal_id, principal_type, principal_subject, principal_roles_json,
		tenant_id, attribution_json,
		protocol, operation, stream,
		requested_model, virtual_model, response_presented_model, config_revision,
		final_provider, final_deployment, final_upstream_model, attempt_count,
		http_status, outcome, failure_class, latency_ns, time_to_first_byte_ns,
		input_tokens, output_tokens, total_tokens, cached_input_tokens,
		cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
		accepted_prediction_tokens, rejected_prediction_tokens,
		provider_components_json, raw_usage_json, usage_completeness,
		normalization_version
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38
	)
	ON CONFLICT (started_at, request_id) DO NOTHING
`

const insertAttemptSQL = `
	INSERT INTO llm_attempts (
		attempt_started_at, request_id, attempt_no, first_byte_at, completed_at,
		provider, deployment, upstream_model, pool_priority, http_status,
		upstream_request_id, outcome, failure_class, retried, latency_ns,
		input_tokens, output_tokens, total_tokens, cached_input_tokens,
		cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
		accepted_prediction_tokens, rejected_prediction_tokens,
		provider_components_json, raw_usage_json, usage_completeness,
		normalization_version
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26,
		$27, $28
	)
	ON CONFLICT (attempt_started_at, request_id, attempt_no) DO NOTHING
`

const insertRuntimeEventSQL = `
	INSERT INTO model_runtime_events (
		occurred_at, event_id, controller, binding_revision, virtual_model,
		deployment, endpoint_instance, cluster_id, job_id, recipe_revision,
		prior_state, new_state, reason, latency_ns, queue_depth, waiter_count,
		outcome, metadata_json
	) VALUES (
		$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14,
		$15, $16, $17, $18
	)
	ON CONFLICT (occurred_at, event_id) DO NOTHING
`

var _ ledger.Store = (*Store)(nil)
