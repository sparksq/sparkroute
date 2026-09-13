// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/config"
)

type PublishOptions struct {
	Actor  string
	Reason string
}

type ActivateOptions struct {
	Actor          string
	Reason         string
	ExpectedActive *config.Version
}

type Activation struct {
	Version         config.Version `json:"revision"`
	PreviousVersion config.Version `json:"previous_revision,omitempty"`
	Generation      int64          `json:"generation"`
	ActivatedAt     time.Time      `json:"activated_at"`
	ActivatedBy     string         `json:"activated_by,omitempty"`
	Reason          string         `json:"reason,omitempty"`
}

func (s *Store) Publish(
	ctx context.Context,
	document config.Document,
	options PublishOptions,
) (config.Version, bool, error) {
	if err := validateMetadata(options.Actor, options.Reason); err != nil {
		return "", false, err
	}
	raw, version, err := config.EncodeCanonical(document)
	if err != nil {
		return "", false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", false, fmt.Errorf("begin PostgreSQL configuration publish: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	created, err := publishTx(ctx, tx, raw, version, options)
	if err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, fmt.Errorf("commit PostgreSQL configuration publish: %w", err)
	}
	return version, created, nil
}

func (s *Store) PublishAndActivate(
	ctx context.Context,
	document config.Document,
	publishOptions PublishOptions,
	activateOptions ActivateOptions,
) (Activation, bool, error) {
	if err := validateMetadata(publishOptions.Actor, publishOptions.Reason); err != nil {
		return Activation{}, false, err
	}
	if err := validateMetadata(activateOptions.Actor, activateOptions.Reason); err != nil {
		return Activation{}, false, err
	}
	raw, version, err := config.EncodeCanonical(document)
	if err != nil {
		return Activation{}, false, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Activation{}, false, fmt.Errorf(
			"begin PostgreSQL configuration publish and activate: %w",
			err,
		)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	created, err := publishTx(ctx, tx, raw, version, publishOptions)
	if err != nil {
		return Activation{}, false, err
	}
	activation, err := activateTx(ctx, tx, version, activateOptions)
	if err != nil {
		return Activation{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Activation{}, false, fmt.Errorf(
			"commit PostgreSQL configuration publish and activate: %w",
			err,
		)
	}
	return activation, created, nil
}

func (s *Store) Activate(
	ctx context.Context,
	version config.Version,
	options ActivateOptions,
) (Activation, error) {
	if err := validateMetadata(options.Actor, options.Reason); err != nil {
		return Activation{}, err
	}
	if err := validateVersion(version); err != nil {
		return Activation{}, err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Activation{}, fmt.Errorf("begin PostgreSQL configuration activation: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()
	activation, err := activateTx(ctx, tx, version, options)
	if err != nil {
		return Activation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Activation{}, fmt.Errorf("commit PostgreSQL configuration activation: %w", err)
	}
	return activation, nil
}

func publishTx(
	ctx context.Context,
	tx pgx.Tx,
	raw []byte,
	version config.Version,
	options PublishOptions,
) (bool, error) {
	var inserted string
	err := tx.QueryRow(ctx, `
		INSERT INTO llm_config_revisions (
			revision_id, document_json, created_by, reason
		) VALUES ($1, $2::jsonb, $3, $4)
		ON CONFLICT (revision_id) DO NOTHING
		RETURNING revision_id
	`, version, raw, options.Actor, nullString(options.Reason)).Scan(&inserted)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	default:
		return false, fmt.Errorf("publish PostgreSQL configuration revision: %w", err)
	}
}

func activateTx(
	ctx context.Context,
	tx pgx.Tx,
	version config.Version,
	options ActivateOptions,
) (Activation, error) {
	if err := validateVersion(version); err != nil {
		return Activation{}, err
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", activationLockID); err != nil {
		return Activation{}, fmt.Errorf("lock PostgreSQL configuration activation: %w", err)
	}
	var revisionExists bool
	if err := tx.QueryRow(
		ctx,
		"SELECT EXISTS (SELECT 1 FROM llm_config_revisions WHERE revision_id = $1)",
		version,
	).Scan(&revisionExists); err != nil {
		return Activation{}, fmt.Errorf("find PostgreSQL configuration revision: %w", err)
	}
	if !revisionExists {
		return Activation{}, fmt.Errorf("%w: %s", ErrRevisionNotFound, version)
	}

	var current config.Version
	var generation int64
	var activatedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT active_revision_id, generation, activated_at
		FROM llm_config_state
		WHERE singleton = 1
		FOR UPDATE
	`).Scan(&current, &generation, &activatedAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Activation{}, fmt.Errorf("read active PostgreSQL configuration: %w", err)
	}
	if options.ExpectedActive != nil && *options.ExpectedActive != current {
		return Activation{}, fmt.Errorf(
			"%w: expected %q, found %q",
			ErrRevisionConflict,
			*options.ExpectedActive,
			current,
		)
	}
	if current == version {
		return Activation{
			Version:     version,
			Generation:  generation,
			ActivatedAt: activatedAt.UTC(),
		}, nil
	}

	previous := current
	err = tx.QueryRow(ctx, `
		INSERT INTO llm_config_state (
			singleton, active_revision_id, generation,
			activated_at, activated_by, reason
		) VALUES (1, $1, 1, now(), $2, $3)
		ON CONFLICT (singleton) DO UPDATE SET
			active_revision_id = EXCLUDED.active_revision_id,
			generation = llm_config_state.generation + 1,
			activated_at = EXCLUDED.activated_at,
			activated_by = EXCLUDED.activated_by,
			reason = EXCLUDED.reason
		RETURNING generation, activated_at
	`, version, options.Actor, nullString(options.Reason)).Scan(
		&generation,
		&activatedAt,
	)
	if err != nil {
		return Activation{}, fmt.Errorf("activate PostgreSQL configuration revision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_config_activations (
			generation, revision_id, previous_revision_id,
			activated_at, activated_by, reason
		) VALUES ($1, $2, $3, $4, $5, $6)
	`, generation, version, nullVersion(previous), activatedAt,
		options.Actor, nullString(options.Reason)); err != nil {
		return Activation{}, fmt.Errorf("record PostgreSQL configuration activation: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		"SELECT pg_notify($1, $2)",
		notificationChannel,
		string(version),
	); err != nil {
		return Activation{}, fmt.Errorf("notify PostgreSQL configuration activation: %w", err)
	}
	return Activation{
		Version:         version,
		PreviousVersion: previous,
		Generation:      generation,
		ActivatedAt:     activatedAt.UTC(),
		ActivatedBy:     options.Actor,
		Reason:          options.Reason,
	}, nil
}

func validateMetadata(actor, reason string) error {
	if actor == "" {
		return fmt.Errorf("configuration mutation actor is required")
	}
	if len(actor) > 512 {
		return fmt.Errorf("configuration mutation actor exceeds 512 bytes")
	}
	if len(reason) > 4096 {
		return fmt.Errorf("configuration mutation reason exceeds 4096 bytes")
	}
	return nil
}

func validateVersion(version config.Version) error {
	if len(version) != 64 {
		return fmt.Errorf("configuration revision must be a SHA-256 identifier")
	}
	if _, err := hex.DecodeString(string(version)); err != nil {
		return fmt.Errorf("configuration revision must be a SHA-256 identifier")
	}
	return nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullVersion(value config.Version) any {
	if value == "" {
		return nil
	}
	return value
}
