// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

// Package providerauth owns renewable provider credentials and guided sign-in.
package providerauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	llmauth "github.com/scitrera/go-llm/auth"
	"github.com/sparksq/sparkroute/internal/privatepath"
	_ "modernc.org/sqlite"
)

// Store holds secrets separately from exportable configuration. A lifetime
// SQLite lock prevents two processes from racing renewable token replacement.
// SQLite supplies native OS locking on Linux, macOS, and Windows.
type Store struct {
	db   *sql.DB
	lock *sql.DB
}

func OpenStore(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("provider credential path is required")
	}
	if path != ":memory:" {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		if err := privateFile(path); err != nil {
			return nil, err
		}
		if err := privateFile(path + ".lock"); err != nil {
			return nil, err
		}
	}
	s := &Store{}
	fail := func(err error) (*Store, error) { _ = s.Close(); return nil, err }
	var err error
	if path != ":memory:" {
		s.lock, err = sql.Open("sqlite", path+".lock")
		if err != nil {
			return fail(err)
		}
		s.lock.SetMaxOpenConns(1)
		if _, err = s.lock.ExecContext(ctx, "BEGIN EXCLUSIVE"); err != nil {
			return fail(fmt.Errorf("provider credential store is already in use: %w", err))
		}
	}
	s.db, err = sql.Open("sqlite", path)
	if err != nil {
		return fail(err)
	}
	s.db.SetMaxOpenConns(1)
	// Rollback journaling keeps credential writes atomic; secure_delete erases
	// removed values from database pages rather than leaving reusable free cells.
	for _, statement := range []string{
		"PRAGMA secure_delete=ON",
		"PRAGMA synchronous=FULL",
		"CREATE TABLE IF NOT EXISTS provider_credentials (profile TEXT PRIMARY KEY, credential BLOB NOT NULL)",
	} {
		if _, err = s.db.ExecContext(ctx, statement); err != nil {
			return fail(err)
		}
	}
	return s, nil
}

func privateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Stat(parent)
	if err != nil {
		return err
	}
	if err := privatepath.Check(parent, info.Mode()); err != nil {
		return err
	}
	if existing, err := os.Lstat(path); err == nil && !existing.Mode().IsRegular() {
		return fmt.Errorf("provider credential path must be a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err = f.Stat()
	if err != nil {
		return err
	}
	return privatepath.Check(path, info.Mode())
}

func (s *Store) Load(ctx context.Context, profile string) (llmauth.Credential, error) {
	var raw []byte
	if err := s.db.QueryRowContext(ctx, "SELECT credential FROM provider_credentials WHERE profile = ?", profile).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return llmauth.Credential{}, llmauth.ErrCredentialNotFound
		}
		return llmauth.Credential{}, err
	}
	var result llmauth.Credential
	err := json.Unmarshal(raw, &result)
	return result, err
}

func (s *Store) Save(ctx context.Context, profile string, credential llmauth.Credential) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(credential)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO provider_credentials (profile, credential) VALUES (?, ?) ON CONFLICT(profile) DO UPDATE SET credential=excluded.credential", profile, raw)
	return err
}

func (s *Store) Delete(ctx context.Context, profile string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM provider_credentials WHERE profile = ?", profile)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n == 0 {
		return llmauth.ErrCredentialNotFound
	}
	return err
}

func (s *Store) Close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	if s.lock != nil {
		err = errors.Join(err, s.lock.Close())
	}
	return err
}
