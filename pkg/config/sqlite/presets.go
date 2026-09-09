// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/sparksq/sparkroute/pkg/config/managed"
)

type presetExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func ensureDefaultPreset(ctx context.Context, db presetExecer) error {
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_config_presets (id, name, revision_id, document_json, updated_at)
		SELECT 'default', 'Default', revision_id, document_json, updated_at FROM gateway_config_managed_sets WHERE owner = 'operator'`); err != nil {
		return fmt.Errorf("initialize default configuration preset: %w", err)
	}
	_, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO gateway_config_preset_state (singleton, active_preset, revision)
		SELECT 1, 'default', 1 WHERE EXISTS (SELECT 1 FROM gateway_config_presets WHERE id = 'default')`)
	return err
}

func presetState(ctx context.Context, tx *sql.Tx) (string, int64, error) {
	var active string
	var revision int64
	err := tx.QueryRowContext(ctx, "SELECT active_preset, revision FROM gateway_config_preset_state WHERE singleton = 1").Scan(&active, &revision)
	return active, revision, err
}

func (s *Store) ListPresets(ctx context.Context) (managed.PresetCatalog, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return managed.PresetCatalog{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := readCurrent(ctx, tx)
	if err != nil {
		return managed.PresetCatalog{}, err
	}
	active, revision, err := presetState(ctx, tx)
	if err != nil {
		return managed.PresetCatalog{}, err
	}
	result := managed.PresetCatalog{ActivePreset: active, PresetsRevision: revision, ActiveRevision: current.Version, Presets: []managed.PresetMetadata{}}
	rows, err := tx.QueryContext(ctx, "SELECT id, name, revision_id, updated_at FROM gateway_config_presets ORDER BY id != 'default', name COLLATE NOCASE, id")
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item managed.PresetMetadata
		var updated string
		if err := rows.Scan(&item.ID, &item.Name, &item.Revision, &updated); err != nil {
			return result, err
		}
		item.UpdatedAt, err = parseTime(updated)
		if err != nil {
			return result, err
		}
		result.Presets = append(result.Presets, item)
	}
	return result, rows.Err()
}

func readPreset(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (managed.Preset, error) {
	var item managed.Preset
	var raw []byte
	var updated string
	err := query.QueryRowContext(ctx, "SELECT id, name, revision_id, document_json, updated_at FROM gateway_config_presets WHERE id = ?", id).
		Scan(&item.ID, &item.Name, &item.Revision, &raw, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return item, managed.ErrPresetNotFound
	}
	if err != nil {
		return item, err
	}
	item.UpdatedAt, err = parseTime(updated)
	if err == nil {
		item.Document, err = decodeFragment(raw, item.Revision)
	}
	return item, err
}

func (s *Store) GetPreset(ctx context.Context, id string) (managed.Preset, error) {
	return readPreset(ctx, s.db, id)
}

// Caller holds writeMu. Every preset edit checks both configuration and catalog
// revisions: selecting equal configurations must still invalidate stale edits.
func (s *Store) beginPresetMutation(ctx context.Context, options managed.ReplaceOptions) (*sql.Tx, string, error) {
	if err := validateMetadata(options.Actor, options.Reason); err != nil {
		return nil, "", err
	}
	if options.ExpectedActive == "" || options.ExpectedPresetsRevision == nil {
		return nil, "", fmt.Errorf("%w: configuration and presets revisions are required", managed.ErrInvalidPreset)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	fail := func(err error) (*sql.Tx, string, error) { _ = tx.Rollback(); return nil, "", err }
	current, err := readCurrent(ctx, tx)
	if err != nil {
		return fail(err)
	}
	if current.Version != options.ExpectedActive {
		return fail(ErrRevisionConflict)
	}
	active, revision, err := presetState(ctx, tx)
	if err != nil {
		return fail(err)
	}
	if revision != *options.ExpectedPresetsRevision {
		return fail(managed.ErrPresetConflict)
	}
	return tx, active, nil
}

func validateAvailablePresetName(ctx context.Context, tx *sql.Tx, name, exceptID string) error {
	if err := managed.ValidatePresetName(name); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM gateway_config_presets WHERE name = ? COLLATE NOCASE AND id != ?)", name, exceptID).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("%w: a preset with this name already exists", managed.ErrInvalidPreset)
	}
	return nil
}

func (s *Store) SavePreset(ctx context.Context, name string, options managed.ReplaceOptions) (managed.PresetMetadata, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, _, err := s.beginPresetMutation(ctx, options)
	if err != nil {
		return managed.PresetMetadata{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := validateAvailablePresetName(ctx, tx, name, ""); err != nil {
		return managed.PresetMetadata{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM gateway_config_presets").Scan(&count); err != nil {
		return managed.PresetMetadata{}, err
	}
	if count >= managed.MaxPresets {
		return managed.PresetMetadata{}, fmt.Errorf("%w: at most %d presets are allowed", managed.ErrInvalidPreset, managed.MaxPresets)
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return managed.PresetMetadata{}, err
	}
	id := hex.EncodeToString(random[:])
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `INSERT INTO gateway_config_presets (id, name, revision_id, document_json, updated_at)
		SELECT ?, ?, revision_id, document_json, ? FROM gateway_config_managed_sets WHERE owner = 'operator'`, id, name, formatTime(now)); err != nil {
		return managed.PresetMetadata{}, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_config_preset_state SET active_preset = ?, revision = revision + 1 WHERE singleton = 1", id); err != nil {
		return managed.PresetMetadata{}, err
	}
	item, err := readPreset(ctx, tx, id)
	if err != nil {
		return managed.PresetMetadata{}, err
	}
	return item.PresetMetadata, tx.Commit()
}

func (s *Store) ActivatePreset(ctx context.Context, id string, options managed.ReplaceOptions) (managed.ReplaceResult, error) {
	if id == "" || options.ExpectedPresetsRevision == nil {
		return managed.ReplaceResult{}, fmt.Errorf("%w: preset and presets revision are required", managed.ErrInvalidPreset)
	}
	return s.replaceSet(ctx, managed.OwnerOperator, managed.EmptyDocument(), options, id)
}

func (s *Store) RenamePreset(ctx context.Context, id, name string, options managed.ReplaceOptions) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, _, err := s.beginPresetMutation(ctx, options)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if id == managed.DefaultPreset {
		return fmt.Errorf("%w: Default cannot be renamed", managed.ErrInvalidPreset)
	}
	if _, err := readPreset(ctx, tx, id); err != nil {
		return err
	}
	if err := validateAvailablePresetName(ctx, tx, name, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_config_presets SET name = ?, updated_at = ? WHERE id = ?", name, formatTime(time.Now().UTC()), id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_config_preset_state SET revision = revision + 1 WHERE singleton = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) DeletePreset(ctx context.Context, id string, options managed.ReplaceOptions) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, active, err := s.beginPresetMutation(ctx, options)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if id == managed.DefaultPreset || id == active {
		return fmt.Errorf("%w: Default and the active preset cannot be deleted; switch presets first", managed.ErrInvalidPreset)
	}
	if _, err := readPreset(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM gateway_config_presets WHERE id = ?", id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE gateway_config_preset_state SET revision = revision + 1 WHERE singleton = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

func syncActivePreset(ctx context.Context, tx *sql.Tx, fragment compiledFragment, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE gateway_config_presets SET revision_id = ?, document_json = ?, updated_at = ?
		WHERE id = (SELECT active_preset FROM gateway_config_preset_state WHERE singleton = 1) AND revision_id != ?`,
		fragment.version, fragment.raw, formatTime(now), fragment.version)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		_, err = tx.ExecContext(ctx, "UPDATE gateway_config_preset_state SET revision = revision + 1 WHERE singleton = 1")
	}
	return err
}
