// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres implements immutable PostgreSQL-backed gateway
// configuration revisions and activation.
package postgres

import (
	"context"
	"embed"
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
	"github.com/sparksq/sparkroute/pkg/config"
)

const (
	defaultPostgresMajor = 17
	migrationLockID      = int64(0x4c4c4d4757434647)
	activationLockID     = int64(0x4c4c4d4757434143)
	notificationChannel  = "sparkroute_config_changed"
)

var (
	ErrNoActiveRevision = errors.New("no active gateway configuration revision")
	ErrRevisionNotFound = errors.New("gateway configuration revision not found")
	ErrRevisionConflict = errors.New("active gateway configuration revision changed")
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
		return nil, fmt.Errorf("PostgreSQL configuration URL is required")
	}
	if options.ExpectedPostgresMajor == 0 {
		options.ExpectedPostgresMajor = defaultPostgresMajor
	}
	poolConfig, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL configuration source: %w", err)
	}
	if options.MaxConnections > 0 {
		poolConfig.MaxConns = options.MaxConnections
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL configuration pool: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping PostgreSQL configuration source: %w", err))
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

func validateSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, table := range []string{
		"llm_config_revisions",
		"llm_config_state",
		"llm_config_activations",
	} {
		var exists bool
		if err := pool.QueryRow(
			ctx,
			"SELECT to_regclass(current_schema() || '.' || $1) IS NOT NULL",
			table,
		).Scan(&exists); err != nil {
			return fmt.Errorf("validate PostgreSQL configuration table %s: %w", table, err)
		}
		if !exists {
			return fmt.Errorf(
				"PostgreSQL configuration table %s is missing; run migrations first",
				table,
			)
		}
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sparkroute_config_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create PostgreSQL configuration migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read PostgreSQL configuration migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read PostgreSQL configuration migration %s: %w", version, err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin PostgreSQL configuration migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock PostgreSQL configuration migration %s: %w", version, err)
		}
		var exists bool
		if err := tx.QueryRow(
			ctx,
			`SELECT EXISTS (
				SELECT 1 FROM sparkroute_config_schema_migrations
				WHERE version = $1
			)`,
			version,
		).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check PostgreSQL configuration migration %s: %w", version, err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("execute PostgreSQL configuration migration %s: %w", version, err)
			}
			if _, err := tx.Exec(
				ctx,
				"INSERT INTO sparkroute_config_schema_migrations (version) VALUES ($1)",
				version,
			); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("record PostgreSQL configuration migration %s: %w", version, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit PostgreSQL configuration migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) Load(
	ctx context.Context,
) (config.Document, config.Version, error) {
	var raw []byte
	var revision string
	err := s.pool.QueryRow(ctx, `
		SELECT revision.document_json, revision.revision_id
		FROM llm_config_state AS state
		JOIN llm_config_revisions AS revision
		  ON revision.revision_id = state.active_revision_id
		WHERE state.singleton = 1
	`).Scan(&raw, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return config.Document{}, "", ErrNoActiveRevision
	}
	if err != nil {
		return config.Document{}, "", fmt.Errorf("load active PostgreSQL configuration: %w", err)
	}
	document, err := config.Decode(raw)
	if err != nil {
		return config.Document{}, "", fmt.Errorf(
			"decode active PostgreSQL configuration revision %s: %w",
			revision,
			err,
		)
	}
	_, calculated, err := config.EncodeCanonical(document)
	if err != nil {
		return config.Document{}, "", err
	}
	if string(calculated) != revision {
		return config.Document{}, "", fmt.Errorf(
			"active PostgreSQL configuration revision failed integrity check",
		)
	}
	return document, config.Version(revision), nil
}

func (s *Store) Health(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("PostgreSQL configuration health: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.pool.Close()
	})
	return nil
}

type Revision struct {
	Version   config.Version `json:"revision"`
	CreatedAt time.Time      `json:"created_at"`
	CreatedBy string         `json:"created_by"`
	Reason    string         `json:"reason,omitempty"`
}

func (s *Store) GetRevision(
	ctx context.Context,
	version config.Version,
) (config.Document, Revision, error) {
	if err := validateVersion(version); err != nil {
		return config.Document{}, Revision{}, err
	}
	var raw []byte
	revision := Revision{Version: version}
	err := s.pool.QueryRow(ctx, `
		SELECT document_json, created_at, created_by, COALESCE(reason, '')
		FROM llm_config_revisions
		WHERE revision_id = $1
	`, version).Scan(
		&raw,
		&revision.CreatedAt,
		&revision.CreatedBy,
		&revision.Reason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return config.Document{}, Revision{}, fmt.Errorf(
			"%w: %s",
			ErrRevisionNotFound,
			version,
		)
	}
	if err != nil {
		return config.Document{}, Revision{}, fmt.Errorf(
			"read PostgreSQL configuration revision: %w",
			err,
		)
	}
	document, err := config.Decode(raw)
	if err != nil {
		return config.Document{}, Revision{}, fmt.Errorf(
			"decode PostgreSQL configuration revision %s: %w",
			version,
			err,
		)
	}
	_, calculated, err := config.EncodeCanonical(document)
	if err != nil {
		return config.Document{}, Revision{}, err
	}
	if calculated != version {
		return config.Document{}, Revision{}, fmt.Errorf(
			"PostgreSQL configuration revision %s failed integrity check",
			version,
		)
	}
	revision.CreatedAt = revision.CreatedAt.UTC()
	return document, revision, nil
}

func (s *Store) ListRevisions(
	ctx context.Context,
	limit int,
) ([]Revision, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return nil, fmt.Errorf("configuration revision limit must be between 1 and 500")
	}
	rows, err := s.pool.Query(ctx, `
		SELECT revision_id, created_at, created_by, COALESCE(reason, '')
		FROM llm_config_revisions
		ORDER BY created_at DESC, revision_id DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list PostgreSQL configuration revisions: %w", err)
	}
	defer rows.Close()
	revisions := make([]Revision, 0, limit)
	for rows.Next() {
		var revision Revision
		if err := rows.Scan(
			&revision.Version,
			&revision.CreatedAt,
			&revision.CreatedBy,
			&revision.Reason,
		); err != nil {
			return nil, fmt.Errorf("scan PostgreSQL configuration revision: %w", err)
		}
		revision.CreatedAt = revision.CreatedAt.UTC()
		revisions = append(revisions, revision)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate PostgreSQL configuration revisions: %w", err)
	}
	return revisions, nil
}

var (
	_ config.Source      = (*Store)(nil)
	_ config.WatchSource = (*Store)(nil)
)
