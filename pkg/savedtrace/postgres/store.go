// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package postgres persists saved inference traces in ordinary PostgreSQL.
// It does not require TimescaleDB and can either own a dedicated connection
// pool or compose over the ledger's existing pool.
package postgres

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultPostgresMajor = 17
	migrationLockID      = int64(0x5350524b54524345)
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
	ownsPool  bool
	closeOnce sync.Once
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.URL) == "" {
		return nil, fmt.Errorf("PostgreSQL saved-trace URL is required")
	}
	if options.ExpectedPostgresMajor == 0 {
		options.ExpectedPostgresMajor = defaultPostgresMajor
	}
	poolConfig, err := pgxpool.ParseConfig(options.URL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL saved-trace configuration: %w", err)
	}
	if options.MaxConnections > 0 {
		poolConfig.MaxConns = options.MaxConnections
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL saved-trace pool: %w", err)
	}
	closeOnError := func(err error) (*Store, error) {
		pool.Close()
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping PostgreSQL saved-trace store: %w", err))
	}
	if err := validatePostgresMajor(ctx, pool, options.ExpectedPostgresMajor); err != nil {
		return closeOnError(err)
	}
	store, err := OpenPool(ctx, pool, options.AutoMigrate)
	if err != nil {
		return closeOnError(err)
	}
	store.ownsPool = true
	return store, nil
}

// OpenPool composes the saved-trace schema and operations over an existing
// pool. Closing the returned Store does not close the caller-owned pool.
func OpenPool(ctx context.Context, pool *pgxpool.Pool, autoMigrate bool) (*Store, error) {
	if pool == nil {
		return nil, fmt.Errorf("PostgreSQL saved-trace pool is required")
	}
	if autoMigrate {
		if err := runMigrations(ctx, pool); err != nil {
			return nil, err
		}
	}
	if err := validateSchema(ctx, pool); err != nil {
		return nil, err
	}
	return &Store{pool: pool}, nil
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
	var tracesExist, sessionsExist bool
	if err := pool.QueryRow(
		ctx,
		`SELECT to_regclass(current_schema() || '.llm_saved_traces') IS NOT NULL,
		        to_regclass(current_schema() || '.llm_saved_trace_capture_sessions') IS NOT NULL`,
	).Scan(&tracesExist, &sessionsExist); err != nil {
		return fmt.Errorf("validate PostgreSQL saved-trace table: %w", err)
	}
	if !tracesExist || !sessionsExist {
		return fmt.Errorf("PostgreSQL saved-trace tables are missing; run migrations first")
	}
	return nil
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS sparkroute_saved_trace_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create PostgreSQL saved-trace migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read PostgreSQL saved-trace migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read PostgreSQL saved-trace migration %s: %w", version, err)
		}
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return fmt.Errorf("begin PostgreSQL saved-trace migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("lock PostgreSQL saved-trace migration %s: %w", version, err)
		}
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM sparkroute_saved_trace_schema_migrations WHERE version = $1
			)
		`, version).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("check PostgreSQL saved-trace migration %s: %w", version, err)
		}
		if !exists {
			if _, err := tx.Exec(ctx, string(content)); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("execute PostgreSQL saved-trace migration %s: %w", version, err)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO sparkroute_saved_trace_schema_migrations (version) VALUES ($1)
			`, version); err != nil {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("record PostgreSQL saved-trace migration %s: %w", version, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit PostgreSQL saved-trace migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) Health(ctx context.Context) error {
	if err := s.pool.Ping(ctx); err != nil {
		return fmt.Errorf("PostgreSQL saved-trace health: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.ownsPool {
			s.pool.Close()
		}
	})
	return nil
}

func normalizePostgresTime(value time.Time) time.Time {
	return value.UTC().Truncate(time.Microsecond)
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
