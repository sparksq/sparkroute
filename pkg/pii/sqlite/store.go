// Package sqlite implements private single-process persistence for
// encrypted PII conversation mappings.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/internal/privatepath"

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
	if options.Path == "" {
		return nil, fmt.Errorf("SQLite PII mapping path is required")
	}
	if options.BusyTimeout <= 0 {
		options.BusyTimeout = 5 * time.Second
	}
	path := options.Path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve SQLite PII mapping path: %w", err)
		}
		if err := ensurePrivateFile(absolute); err != nil {
			return nil, err
		}
		path = absolute
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite PII mapping store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys=ON"); err != nil {
		return closeOnError(fmt.Errorf("configure SQLite PII mapping store: %w", err))
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(
		"PRAGMA busy_timeout=%d", options.BusyTimeout.Milliseconds(),
	)); err != nil {
		return closeOnError(fmt.Errorf("configure SQLite PII mapping store: %w", err))
	}
	if path != ":memory:" {
		if _, err := db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			return closeOnError(fmt.Errorf("configure SQLite PII mapping WAL: %w", err))
		}
	}
	migration, err := migrationFiles.ReadFile("migrations/001_pii_conversation_mappings.sql")
	if err != nil {
		return closeOnError(fmt.Errorf("read SQLite PII mapping migration: %w", err))
	}
	if _, err := db.ExecContext(ctx, string(migration)); err != nil {
		return closeOnError(fmt.Errorf("migrate SQLite PII mapping store: %w", err))
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

func ensurePrivateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create SQLite PII mapping directory: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect SQLite PII mapping directory: %w", err)
	}
	if err := privatepath.Check(parent, parentInfo.Mode()); err != nil {
		return fmt.Errorf("SQLite PII mapping directory %q must not grant group or other permissions", parent)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite PII mapping file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close SQLite PII mapping file: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite PII mapping file: %w", err)
	}
	if err := privatepath.Check(path, info.Mode()); err != nil {
		return fmt.Errorf("SQLite PII mapping file %q must not grant group or other permissions", path)
	}
	return nil
}
