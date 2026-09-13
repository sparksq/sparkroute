// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres provides replica-shared dynamic endpoint registration and
// activation fencing for the SparkRoute cluster profile.
package postgres

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

const (
	defaultPostgresMajor = 17
	migrationLockID      = int64(0x4c4c4d4752544d45)
	maxClaimTTL          = 24 * time.Hour
	maxCursorBytes       = 2048
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
		return nil, fmt.Errorf("PostgreSQL runtime coordination URL is required")
	}
	if options.ExpectedPostgresMajor == 0 {
		options.ExpectedPostgresMajor = defaultPostgresMajor
	}
	poolConfig, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL runtime coordination configuration: %w", err)
	}
	if options.MaxConnections > 0 {
		poolConfig.MaxConns = options.MaxConnections
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL runtime coordination pool: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping PostgreSQL runtime coordination: %w", err))
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

func (s *Store) Close() error {
	s.closeOnce.Do(s.pool.Close)
	return nil
}

func (s *Store) Begin(
	ctx context.Context,
	key string,
	holder string,
	ttl time.Duration,
) (lifecycle.ActivationClaim, error) {
	if err := validateClaimInput(key, holder, ttl); err != nil {
		return lifecycle.ActivationClaim{}, err
	}
	var currentHolder string
	var fencingToken int64
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO llm_runtime_activation_claims (
			activation_key, holder, fencing_token, claim_state, expires_at, updated_at
		) VALUES (
			$1, $2, 1, 'active', clock_timestamp() + $3 * interval '1 microsecond', clock_timestamp()
		)
		ON CONFLICT (activation_key) DO UPDATE SET
			holder = CASE
				WHEN llm_runtime_activation_claims.claim_state <> 'active'
				  OR llm_runtime_activation_claims.expires_at <= clock_timestamp()
				THEN EXCLUDED.holder ELSE llm_runtime_activation_claims.holder END,
			fencing_token = CASE
				WHEN llm_runtime_activation_claims.claim_state <> 'active'
				  OR llm_runtime_activation_claims.expires_at <= clock_timestamp()
				THEN llm_runtime_activation_claims.fencing_token + 1
				ELSE llm_runtime_activation_claims.fencing_token END,
			claim_state = CASE
				WHEN llm_runtime_activation_claims.claim_state <> 'active'
				  OR llm_runtime_activation_claims.expires_at <= clock_timestamp()
				THEN 'active' ELSE llm_runtime_activation_claims.claim_state END,
			expires_at = CASE
				WHEN llm_runtime_activation_claims.claim_state <> 'active'
				  OR llm_runtime_activation_claims.expires_at <= clock_timestamp()
				THEN EXCLUDED.expires_at ELSE llm_runtime_activation_claims.expires_at END,
			updated_at = clock_timestamp()
		RETURNING holder, fencing_token, expires_at
	`, key, holder, ttl.Microseconds()).Scan(&currentHolder, &fencingToken, &expiresAt)
	if err != nil {
		return lifecycle.ActivationClaim{}, fmt.Errorf("begin PostgreSQL activation claim: %w", err)
	}
	disposition := lifecycle.ActivationObserver
	if currentHolder == holder {
		disposition = lifecycle.ActivationOwner
	}
	return lifecycle.ActivationClaim{
		Key: key, Holder: currentHolder, FencingToken: fencingToken,
		Disposition: disposition, ExpiresAt: expiresAt.UTC(),
	}, nil
}

func (s *Store) Renew(
	ctx context.Context,
	claim lifecycle.ActivationClaim,
	ttl time.Duration,
) (lifecycle.ActivationClaim, error) {
	if claim.Disposition != lifecycle.ActivationOwner ||
		validateClaimInput(claim.Key, claim.Holder, ttl) != nil || claim.FencingToken <= 0 {
		return lifecycle.ActivationClaim{}, lifecycle.ErrInvalidActivationClaim
	}
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, `
		UPDATE llm_runtime_activation_claims
		SET expires_at = clock_timestamp() + $4 * interval '1 microsecond',
			updated_at = clock_timestamp()
		WHERE activation_key = $1 AND holder = $2 AND fencing_token = $3
		  AND claim_state = 'active' AND expires_at > clock_timestamp()
		RETURNING expires_at
	`, claim.Key, claim.Holder, claim.FencingToken, ttl.Microseconds()).Scan(&expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return lifecycle.ActivationClaim{}, lifecycle.ErrActivationClaimLost
	}
	if err != nil {
		return lifecycle.ActivationClaim{}, fmt.Errorf("renew PostgreSQL activation claim: %w", err)
	}
	claim.ExpiresAt = expiresAt.UTC()
	return claim, nil
}

func (s *Store) Complete(ctx context.Context, claim lifecycle.ActivationClaim) error {
	if claim.Key == "" || claim.Holder == "" || claim.FencingToken <= 0 ||
		claim.Disposition != lifecycle.ActivationOwner {
		return lifecycle.ErrInvalidActivationClaim
	}
	command, err := s.pool.Exec(ctx, `
		UPDATE llm_runtime_activation_claims
		SET claim_state = 'completed', expires_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE activation_key = $1 AND holder = $2 AND fencing_token = $3
		  AND claim_state = 'active' AND expires_at > clock_timestamp()
	`, claim.Key, claim.Holder, claim.FencingToken)
	if err != nil {
		return fmt.Errorf("complete PostgreSQL activation claim: %w", err)
	}
	if command.RowsAffected() != 1 {
		return lifecycle.ErrActivationClaimLost
	}
	return nil
}

func validateClaimInput(key, holder string, ttl time.Duration) error {
	if key == "" || holder == "" || len(key) > 4096 || len(holder) > 1024 ||
		ttl < time.Microsecond || ttl > maxClaimTTL {
		return lifecycle.ErrInvalidActivationClaim
	}
	return nil
}

func (s *Store) Register(ctx context.Context, endpoint endpointregistry.Endpoint) error {
	if endpoint.State == "" {
		endpoint.State = endpointregistry.StateUnknown
	}
	if err := endpointregistry.ValidateEndpoint(endpoint); err != nil {
		return err
	}
	servedModels, err := json.Marshal(endpoint.ServedModels)
	if err != nil {
		return fmt.Errorf("encode endpoint served models: %w", err)
	}
	metadata, err := json.Marshal(endpoint.Metadata)
	if err != nil {
		return fmt.Errorf("encode endpoint metadata: %w", err)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin endpoint registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if endpoint.FencingToken > 0 {
		key := lifecycle.ActivationKey(lifecycle.Binding{
			Controller: endpoint.Controller, Revision: endpoint.BindingRevision,
		})
		var current bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM llm_runtime_activation_claims
				WHERE activation_key = $1 AND fencing_token = $2
				  AND (claim_state = 'completed' OR expires_at > clock_timestamp())
			)
		`, key, endpoint.FencingToken).Scan(&current); err != nil {
			return fmt.Errorf("validate endpoint fencing token: %w", err)
		}
		if !current {
			return lifecycle.ErrActivationClaimLost
		}
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO llm_runtime_endpoints (
			endpoint_id, target, base_url, protocol, served_models_json, controller,
			cluster_id, job_id, binding_revision, recipe_revision, fencing_token,
			lifecycle_state, active_requests, max_concurrency, registered_at,
			heartbeat_at, expires_at, metadata_json, updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, clock_timestamp()
		)
		ON CONFLICT (endpoint_id) DO UPDATE SET
			target = EXCLUDED.target, base_url = EXCLUDED.base_url,
			protocol = EXCLUDED.protocol, served_models_json = EXCLUDED.served_models_json,
			controller = EXCLUDED.controller, cluster_id = EXCLUDED.cluster_id,
			job_id = EXCLUDED.job_id, binding_revision = EXCLUDED.binding_revision,
			recipe_revision = EXCLUDED.recipe_revision, fencing_token = EXCLUDED.fencing_token,
			lifecycle_state = EXCLUDED.lifecycle_state,
			active_requests = EXCLUDED.active_requests,
			max_concurrency = EXCLUDED.max_concurrency,
			registered_at = EXCLUDED.registered_at, heartbeat_at = EXCLUDED.heartbeat_at,
			expires_at = EXCLUDED.expires_at, metadata_json = EXCLUDED.metadata_json,
			updated_at = clock_timestamp()
		WHERE llm_runtime_endpoints.fencing_token <= EXCLUDED.fencing_token
	`, endpoint.ID, endpoint.Target, endpoint.BaseURL, endpoint.Protocol, servedModels,
		endpoint.Controller, endpoint.ClusterID, endpoint.JobID, endpoint.BindingRevision,
		endpoint.RecipeRevision, endpoint.FencingToken, string(endpoint.State),
		endpoint.ActiveRequests, endpoint.MaxConcurrency, nullTime(endpoint.RegisteredAt),
		nullTime(endpoint.HeartbeatAt), nullTime(endpoint.ExpiresAt), metadata)
	if err != nil {
		return fmt.Errorf("register PostgreSQL runtime endpoint: %w", err)
	}
	if command.RowsAffected() != 1 {
		return lifecycle.ErrActivationClaimLost
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit endpoint registration: %w", err)
	}
	return nil
}

func (s *Store) Remove(ctx context.Context, endpointID string) error {
	if endpointID == "" || len(endpointID) > 1024 {
		return fmt.Errorf("endpoint ID is required and must not exceed 1024 bytes")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM llm_runtime_endpoints WHERE endpoint_id = $1`, endpointID); err != nil {
		return fmt.Errorf("remove PostgreSQL runtime endpoint: %w", err)
	}
	return nil
}

func (s *Store) Ready(ctx context.Context, target string) ([]endpointregistry.Endpoint, error) {
	if target == "" || len(target) > 1024 {
		return nil, fmt.Errorf("endpoint target is required and must not exceed 1024 bytes")
	}
	rows, err := s.pool.Query(ctx, endpointSelect+`
		WHERE e.target = $1 AND e.lifecycle_state = 'ready'
		  AND (e.expires_at IS NULL OR e.expires_at > clock_timestamp())
		  AND (
			e.fencing_token = 0 OR EXISTS (
				SELECT 1 FROM llm_runtime_activation_claims c
				WHERE c.activation_key = octet_length(e.controller)::text || ':' || e.controller || e.binding_revision
				  AND c.fencing_token = e.fencing_token
				  AND (c.claim_state = 'completed' OR c.expires_at > clock_timestamp())
			)
		  )
		ORDER BY e.endpoint_id
	`, target)
	if err != nil {
		return nil, fmt.Errorf("query ready PostgreSQL runtime endpoints: %w", err)
	}
	defer rows.Close()
	return scanEndpoints(rows)
}

func (s *Store) List(
	ctx context.Context,
	query endpointregistry.Query,
) (endpointregistry.Page, error) {
	validated, err := endpointregistry.ValidateQuery(query)
	if err != nil {
		return endpointregistry.Page{}, err
	}
	afterID, err := decodeCursor(validated.Cursor)
	if err != nil {
		return endpointregistry.Page{}, err
	}
	rows, err := s.pool.Query(ctx, endpointSelect+`
		WHERE ($1 = '' OR e.target = $1)
		  AND ($2 = '' OR e.controller = $2)
		  AND ($3 = '' OR e.lifecycle_state = $3)
		  AND ($4 = '' OR e.endpoint_id > $4)
		ORDER BY e.endpoint_id
		LIMIT $5
	`, validated.Target, validated.Controller, string(validated.State), afterID, validated.Limit+1)
	if err != nil {
		return endpointregistry.Page{}, fmt.Errorf("list PostgreSQL runtime endpoints: %w", err)
	}
	defer rows.Close()
	endpoints, err := scanEndpoints(rows)
	if err != nil {
		return endpointregistry.Page{}, err
	}
	page := endpointregistry.Page{Endpoints: endpoints}
	if len(endpoints) > validated.Limit {
		page.Endpoints = endpoints[:validated.Limit]
		page.NextCursor, err = encodeCursor(page.Endpoints[len(page.Endpoints)-1].ID)
		if err != nil {
			return endpointregistry.Page{}, err
		}
	}
	return page, nil
}

const endpointSelect = `
	SELECT e.endpoint_id, e.target, e.base_url, e.protocol, e.served_models_json,
		e.controller, e.cluster_id, e.job_id, e.binding_revision, e.recipe_revision,
		e.fencing_token, e.lifecycle_state, e.active_requests, e.max_concurrency,
		e.registered_at, e.heartbeat_at, e.expires_at, e.metadata_json
	FROM llm_runtime_endpoints e
`

type rowScanner interface {
	Scan(dest ...any) error
}

type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanEndpoints(rows rowsScanner) ([]endpointregistry.Endpoint, error) {
	result := make([]endpointregistry.Endpoint, 0)
	for rows.Next() {
		endpoint, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, endpoint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan PostgreSQL runtime endpoints: %w", err)
	}
	return result, nil
}

func scanEndpoint(row rowScanner) (endpointregistry.Endpoint, error) {
	var endpoint endpointregistry.Endpoint
	var state string
	var servedModels []byte
	var metadata []byte
	var registeredAt, heartbeatAt, expiresAt *time.Time
	if err := row.Scan(
		&endpoint.ID, &endpoint.Target, &endpoint.BaseURL, &endpoint.Protocol, &servedModels,
		&endpoint.Controller, &endpoint.ClusterID, &endpoint.JobID, &endpoint.BindingRevision,
		&endpoint.RecipeRevision, &endpoint.FencingToken, &state, &endpoint.ActiveRequests,
		&endpoint.MaxConcurrency, &registeredAt, &heartbeatAt, &expiresAt, &metadata,
	); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("scan PostgreSQL runtime endpoint: %w", err)
	}
	if err := json.Unmarshal(servedModels, &endpoint.ServedModels); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("decode endpoint served models: %w", err)
	}
	if err := json.Unmarshal(metadata, &endpoint.Metadata); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("decode endpoint metadata: %w", err)
	}
	endpoint.State = endpointregistry.State(state)
	endpoint.RegisteredAt = dereferenceTime(registeredAt)
	endpoint.HeartbeatAt = dereferenceTime(heartbeatAt)
	endpoint.ExpiresAt = dereferenceTime(expiresAt)
	if err := endpointregistry.ValidateEndpoint(endpoint); err != nil {
		return endpointregistry.Endpoint{}, fmt.Errorf("stored PostgreSQL runtime endpoint is invalid: %w", err)
	}
	return endpoint, nil
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func dereferenceTime(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return value.UTC()
}

type endpointCursor struct {
	Version int    `json:"v"`
	AfterID string `json:"after_id"`
}

func encodeCursor(endpointID string) (string, error) {
	raw, err := json.Marshal(endpointCursor{Version: 1, AfterID: endpointID})
	if err != nil {
		return "", fmt.Errorf("encode endpoint cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > maxCursorBytes {
		return "", fmt.Errorf("invalid endpoint cursor")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint cursor")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var cursor endpointCursor
	if err := decoder.Decode(&cursor); err != nil {
		return "", fmt.Errorf("invalid endpoint cursor")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("invalid endpoint cursor")
	}
	if cursor.Version != 1 || cursor.AfterID == "" || len(cursor.AfterID) > 1024 {
		return "", fmt.Errorf("invalid endpoint cursor")
	}
	return cursor.AfterID, nil
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
	if actual := version / 10000; actual != expected {
		return fmt.Errorf("PostgreSQL major %d is required, server reports major %d", expected, actual)
	}
	return nil
}

func validateSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{"llm_runtime_activation_claims", "llm_runtime_endpoints"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL", table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("validate PostgreSQL runtime table %s: %w", table, err)
		}
		if !exists {
			return fmt.Errorf("PostgreSQL runtime table %s is missing; run migrations first", table)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sparkroute_runtime_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
		)
	`); err != nil {
		return fmt.Errorf("create PostgreSQL runtime migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read PostgreSQL runtime migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read PostgreSQL runtime migration %s: %w", version, err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin PostgreSQL runtime migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock PostgreSQL runtime migration %s: %w", version, err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM sparkroute_runtime_schema_migrations WHERE version = $1
			)
		`, version).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check PostgreSQL runtime migration %s: %w", version, err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("apply PostgreSQL runtime migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx,
				"INSERT INTO sparkroute_runtime_schema_migrations (version) VALUES ($1)", version,
			); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("record PostgreSQL runtime migration %s: %w", version, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit PostgreSQL runtime migration %s: %w", version, err)
		}
	}
	return nil
}

var (
	_ lifecycle.ActivationCoordinator = (*Store)(nil)
	_ endpointregistry.Registry       = (*Store)(nil)
	_ endpointregistry.Inspector      = (*Store)(nil)
)
