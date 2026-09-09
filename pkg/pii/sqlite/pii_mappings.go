// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	privacy "github.com/sparksq/sparkroute/pkg/pii"
)

func (s *Store) LoadPIIMappings(
	ctx context.Context,
	scope privacy.ConversationScope,
	now time.Time,
	limits privacy.ConversationLimits,
) ([]privacy.EncryptedMapping, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	limits, err := privacy.NormalizeConversationLimits(limits)
	if err != nil {
		return nil, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin SQLite PII mapping load: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	nowNS := now.UTC().UnixNano()
	if _, err = tx.ExecContext(ctx, `
		UPDATE llm_pii_conversation_mappings
		SET last_used_at_unix_ns = ?,
		    expires_at_unix_ns = min(absolute_expires_at_unix_ns, ?)
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ?
		  AND expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ?
	`, nowNS, now.Add(limits.SlidingTTL).UnixNano(), scope.Tenant, scope.Principal,
		scope.Conversation, nowNS, nowNS); err != nil {
		return nil, fmt.Errorf("touch SQLite PII mappings: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT entity, lookup_digest, token, key_id, nonce, ciphertext,
		       original_bytes, created_at_unix_ns, last_used_at_unix_ns,
		       expires_at_unix_ns, absolute_expires_at_unix_ns
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ?
		  AND expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ?
		ORDER BY created_at_unix_ns, token
		LIMIT ?
	`, scope.Tenant, scope.Principal, scope.Conversation, nowNS, nowNS,
		limits.MaximumMappings+1)
	if err != nil {
		return nil, fmt.Errorf("query SQLite PII mappings: %w", err)
	}
	records := make([]privacy.EncryptedMapping, 0)
	totalBytes := 0
	for rows.Next() {
		record := privacy.EncryptedMapping{Scope: scope}
		var createdNS, usedNS, expiresNS, absoluteNS int64
		if err := rows.Scan(
			&record.Entity, &record.LookupDigest, &record.Token, &record.KeyID,
			&record.Nonce, &record.Ciphertext, &record.OriginalBytes,
			&createdNS, &usedNS, &expiresNS, &absoluteNS,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan SQLite PII mapping: %w", err)
		}
		totalBytes += record.OriginalBytes
		if len(records) >= limits.MaximumMappings || totalBytes > limits.MaximumOriginalBytes {
			_ = rows.Close()
			return nil, privacy.ErrConversationLimit
		}
		record.CreatedAt = time.Unix(0, createdNS).UTC()
		record.LastUsedAt = time.Unix(0, usedNS).UTC()
		record.ExpiresAt = time.Unix(0, expiresNS).UTC()
		record.AbsoluteExpires = time.Unix(0, absoluteNS).UTC()
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate SQLite PII mappings: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite PII mappings: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit SQLite PII mapping load: %w", err)
	}
	return records, nil
}

func (s *Store) PutPIIMappingIfAbsent(
	ctx context.Context,
	record privacy.EncryptedMapping,
	now time.Time,
	limits privacy.ConversationLimits,
) (privacy.EncryptedMapping, error) {
	if err := validateEncryptedMapping(record); err != nil {
		return privacy.EncryptedMapping{}, err
	}
	limits, err := privacy.NormalizeConversationLimits(limits)
	if err != nil {
		return privacy.EncryptedMapping{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("begin SQLite PII mapping write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	nowNS := now.UTC().UnixNano()
	if _, err = tx.ExecContext(ctx, `
		DELETE FROM llm_pii_conversation_mappings
		WHERE (expires_at_unix_ns <= ? OR absolute_expires_at_unix_ns <= ?)
		  AND tenant_id = ? AND principal_id = ? AND conversation_id = ?
	`, nowNS, nowNS, record.Scope.Tenant, record.Scope.Principal,
		record.Scope.Conversation); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("remove expired SQLite PII mapping: %w", err)
	}
	existing, found, err := loadSQLiteMapping(ctx, tx, record.Scope, record.LookupDigest)
	if err != nil {
		return privacy.EncryptedMapping{}, err
	}
	if found {
		if err := touchSQLiteMapping(ctx, tx, existing, now, limits.SlidingTTL); err != nil {
			return privacy.EncryptedMapping{}, err
		}
		existing.LastUsedAt = now.UTC()
		existing.ExpiresAt = minTime(existing.AbsoluteExpires, now.Add(limits.SlidingTTL).UTC())
		if err := tx.Commit(); err != nil {
			return privacy.EncryptedMapping{}, fmt.Errorf("commit existing SQLite PII mapping: %w", err)
		}
		return existing, nil
	}
	var mappingCount, originalBytes int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(original_bytes), 0)
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ?
		  AND expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ?
	`, record.Scope.Tenant, record.Scope.Principal, record.Scope.Conversation,
		nowNS, nowNS).Scan(&mappingCount, &originalBytes); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("count SQLite PII mappings: %w", err)
	}
	if mappingCount >= limits.MaximumMappings ||
		record.OriginalBytes > limits.MaximumOriginalBytes-originalBytes {
		return privacy.EncryptedMapping{}, privacy.ErrConversationLimit
	}
	if mappingCount == 0 {
		var conversations int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM (
				SELECT 1 FROM llm_pii_conversation_mappings
				WHERE expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ?
				GROUP BY tenant_id, principal_id, conversation_id
			)
		`, nowNS, nowNS).Scan(&conversations); err != nil {
			return privacy.EncryptedMapping{}, fmt.Errorf("count SQLite PII conversations: %w", err)
		}
		if conversations >= limits.MaximumConversations {
			return privacy.EncryptedMapping{}, privacy.ErrConversationLimit
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO llm_pii_conversation_mappings (
			tenant_id, principal_id, conversation_id, entity, lookup_digest,
			token, key_id, nonce, ciphertext, original_bytes,
			created_at_unix_ns, last_used_at_unix_ns, expires_at_unix_ns,
			absolute_expires_at_unix_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, record.Scope.Tenant, record.Scope.Principal, record.Scope.Conversation,
		record.Entity, record.LookupDigest, record.Token, record.KeyID, record.Nonce,
		record.Ciphertext, record.OriginalBytes, record.CreatedAt.UnixNano(),
		record.LastUsedAt.UnixNano(), record.ExpiresAt.UnixNano(),
		record.AbsoluteExpires.UnixNano()); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("insert SQLite PII mapping: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("commit SQLite PII mapping: %w", err)
	}
	return record, nil
}

func loadSQLiteMapping(ctx context.Context, tx *sql.Tx, scope privacy.ConversationScope, digest []byte) (privacy.EncryptedMapping, bool, error) {
	record := privacy.EncryptedMapping{Scope: scope, LookupDigest: append([]byte(nil), digest...)}
	var createdNS, usedNS, expiresNS, absoluteNS int64
	err := tx.QueryRowContext(ctx, `
		SELECT entity, token, key_id, nonce, ciphertext, original_bytes,
		       created_at_unix_ns, last_used_at_unix_ns, expires_at_unix_ns,
		       absolute_expires_at_unix_ns
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ? AND lookup_digest = ?
	`, scope.Tenant, scope.Principal, scope.Conversation, digest).Scan(
		&record.Entity, &record.Token, &record.KeyID, &record.Nonce,
		&record.Ciphertext, &record.OriginalBytes, &createdNS, &usedNS,
		&expiresNS, &absoluteNS,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return privacy.EncryptedMapping{}, false, nil
	}
	if err != nil {
		return privacy.EncryptedMapping{}, false, fmt.Errorf("load SQLite PII mapping: %w", err)
	}
	record.CreatedAt = time.Unix(0, createdNS).UTC()
	record.LastUsedAt = time.Unix(0, usedNS).UTC()
	record.ExpiresAt = time.Unix(0, expiresNS).UTC()
	record.AbsoluteExpires = time.Unix(0, absoluteNS).UTC()
	return record, true, nil
}

func touchSQLiteMapping(ctx context.Context, tx *sql.Tx, record privacy.EncryptedMapping, now time.Time, sliding time.Duration) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE llm_pii_conversation_mappings
		SET last_used_at_unix_ns = ?, expires_at_unix_ns = min(absolute_expires_at_unix_ns, ?)
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ? AND lookup_digest = ?
	`, now.UnixNano(), now.Add(sliding).UnixNano(), record.Scope.Tenant,
		record.Scope.Principal, record.Scope.Conversation, record.LookupDigest)
	if err != nil {
		return fmt.Errorf("touch SQLite PII mapping: %w", err)
	}
	return nil
}

func (s *Store) RewrapPIIMapping(ctx context.Context, record privacy.EncryptedMapping, keyID string, nonce, ciphertext []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
		UPDATE llm_pii_conversation_mappings
		SET key_id = ?, nonce = ?, ciphertext = ?
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ?
		  AND lookup_digest = ? AND key_id = ? AND nonce = ? AND ciphertext = ?
	`, keyID, nonce, ciphertext, record.Scope.Tenant, record.Scope.Principal,
		record.Scope.Conversation, record.LookupDigest, record.KeyID, record.Nonce,
		record.Ciphertext)
	if err != nil {
		return fmt.Errorf("rewrap SQLite PII mapping: %w", err)
	}
	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("inspect SQLite PII mapping rewrap: %w", err)
	}
	return nil
}

func (s *Store) DeletePIIConversation(ctx context.Context, scope privacy.ConversationScope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM llm_pii_conversation_mappings
		WHERE tenant_id = ? AND principal_id = ? AND conversation_id = ?
	`, scope.Tenant, scope.Principal, scope.Conversation)
	if err != nil {
		return fmt.Errorf("delete SQLite PII conversation: %w", err)
	}
	return nil
}

func (s *Store) PrunePIIMappings(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, fmt.Errorf("SQLite PII prune limit must be positive")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM llm_pii_conversation_mappings WHERE rowid IN (
			SELECT rowid FROM llm_pii_conversation_mappings
			WHERE expires_at_unix_ns <= ? OR absolute_expires_at_unix_ns <= ?
			ORDER BY min(expires_at_unix_ns, absolute_expires_at_unix_ns)
			LIMIT ?
		)
	`, now.UnixNano(), now.UnixNano(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune SQLite PII mappings: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count pruned SQLite PII mappings: %w", err)
	}
	return count, nil
}

func (s *Store) PIIMappingStatus(
	ctx context.Context,
	now time.Time,
) (privacy.MappingStoreStatus, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	nowNS := now.UTC().UnixNano()
	status := privacy.MappingStoreStatus{Backend: "sqlite"}
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ? THEN original_bytes ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN expires_at_unix_ns <= ? OR absolute_expires_at_unix_ns <= ? THEN 1 ELSE 0 END), 0)
		FROM llm_pii_conversation_mappings
	`, nowNS, nowNS, nowNS, nowNS, nowNS, nowNS).Scan(
		&status.LiveMappings, &status.LiveOriginalBytes, &status.ExpiredPendingPrune,
	)
	if err != nil {
		return status, fmt.Errorf("inspect SQLite PII mappings: %w", err)
	}
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM (
			SELECT 1 FROM llm_pii_conversation_mappings
			WHERE expires_at_unix_ns > ? AND absolute_expires_at_unix_ns > ?
			GROUP BY tenant_id, principal_id, conversation_id
		)
	`, nowNS, nowNS).Scan(&status.LiveConversations)
	if err != nil {
		return status, fmt.Errorf("inspect SQLite PII conversations: %w", err)
	}
	return status, nil
}

func validateEncryptedMapping(record privacy.EncryptedMapping) error {
	if err := record.Scope.Validate(); err != nil {
		return err
	}
	if record.Entity == "" || len(record.LookupDigest) != 32 || record.Token == "" ||
		record.KeyID == "" || len(record.Nonce) < 12 || len(record.Ciphertext) == 0 ||
		record.OriginalBytes < 1 || record.CreatedAt.IsZero() ||
		record.LastUsedAt.Before(record.CreatedAt) || !record.ExpiresAt.After(record.CreatedAt) ||
		record.AbsoluteExpires.Before(record.ExpiresAt) {
		return fmt.Errorf("encrypted PII mapping is invalid")
	}
	return nil
}

func minTime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

var _ privacy.MappingBackend = (*Store)(nil)
var _ privacy.MappingStatusSource = (*Store)(nil)
