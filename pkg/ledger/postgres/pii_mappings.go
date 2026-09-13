// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	privacy "github.com/sparksq/sparkroute/pkg/pii"
)

const piiMappingWriteLockID = int64(0x5049494d41505049)

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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin PostgreSQL PII mapping load: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `
		UPDATE llm_pii_conversation_mappings
		SET last_used_at = $1,
		    expires_at = LEAST(absolute_expires_at, $2)
		WHERE tenant_id = $3 AND principal_id = $4 AND conversation_id = $5
		  AND expires_at > $1 AND absolute_expires_at > $1
	`, now.UTC(), now.Add(limits.SlidingTTL).UTC(), scope.Tenant, scope.Principal,
		scope.Conversation); err != nil {
		return nil, fmt.Errorf("touch PostgreSQL PII mappings: %w", err)
	}
	rows, err := tx.Query(ctx, `
		SELECT entity, lookup_digest, token, key_id, nonce, ciphertext,
		       original_bytes, created_at, last_used_at, expires_at,
		       absolute_expires_at
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = $1 AND principal_id = $2 AND conversation_id = $3
		  AND expires_at > $4 AND absolute_expires_at > $4
		ORDER BY created_at, token
		LIMIT $5
	`, scope.Tenant, scope.Principal, scope.Conversation, now.UTC(),
		limits.MaximumMappings+1)
	if err != nil {
		return nil, fmt.Errorf("query PostgreSQL PII mappings: %w", err)
	}
	records := make([]privacy.EncryptedMapping, 0)
	totalBytes := 0
	for rows.Next() {
		record := privacy.EncryptedMapping{Scope: scope}
		if err := rows.Scan(
			&record.Entity, &record.LookupDigest, &record.Token, &record.KeyID,
			&record.Nonce, &record.Ciphertext, &record.OriginalBytes,
			&record.CreatedAt, &record.LastUsedAt, &record.ExpiresAt,
			&record.AbsoluteExpires,
		); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan PostgreSQL PII mapping: %w", err)
		}
		totalBytes += record.OriginalBytes
		if len(records) >= limits.MaximumMappings || totalBytes > limits.MaximumOriginalBytes {
			rows.Close()
			return nil, privacy.ErrConversationLimit
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate PostgreSQL PII mappings: %w", err)
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit PostgreSQL PII mapping load: %w", err)
	}
	return records, nil
}

func (s *Store) PutPIIMappingIfAbsent(
	ctx context.Context,
	record privacy.EncryptedMapping,
	now time.Time,
	limits privacy.ConversationLimits,
) (privacy.EncryptedMapping, error) {
	if err := validatePostgresEncryptedMapping(record); err != nil {
		return privacy.EncryptedMapping{}, err
	}
	limits, err := privacy.NormalizeConversationLimits(limits)
	if err != nil {
		return privacy.EncryptedMapping{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("begin PostgreSQL PII mapping write: %w", err)
	}
	defer tx.Rollback(ctx)
	// One narrow advisory lock makes the global conversation bound and each
	// scope's byte/mapping bounds exact across replicas.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", piiMappingWriteLockID); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("lock PostgreSQL PII mapping write: %w", err)
	}
	if _, err = tx.Exec(ctx, `
		DELETE FROM llm_pii_conversation_mappings
		WHERE (expires_at <= $1 OR absolute_expires_at <= $1)
		  AND tenant_id = $2 AND principal_id = $3 AND conversation_id = $4
	`, now.UTC(), record.Scope.Tenant, record.Scope.Principal,
		record.Scope.Conversation); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("remove expired PostgreSQL PII mapping: %w", err)
	}
	existing, found, err := loadPostgresPIIMapping(ctx, tx, record.Scope, record.LookupDigest)
	if err != nil {
		return privacy.EncryptedMapping{}, err
	}
	if found {
		expiry := earlierPIITime(existing.AbsoluteExpires, now.Add(limits.SlidingTTL).UTC())
		if _, err := tx.Exec(ctx, `
			UPDATE llm_pii_conversation_mappings
			SET last_used_at = $1, expires_at = $2
			WHERE tenant_id = $3 AND principal_id = $4 AND conversation_id = $5
			  AND lookup_digest = $6
		`, now.UTC(), expiry, existing.Scope.Tenant, existing.Scope.Principal,
			existing.Scope.Conversation, existing.LookupDigest); err != nil {
			return privacy.EncryptedMapping{}, fmt.Errorf("touch PostgreSQL PII mapping: %w", err)
		}
		existing.LastUsedAt = now.UTC()
		existing.ExpiresAt = expiry
		if err := tx.Commit(ctx); err != nil {
			return privacy.EncryptedMapping{}, fmt.Errorf("commit existing PostgreSQL PII mapping: %w", err)
		}
		return existing, nil
	}
	var mappingCount, originalBytes int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*), COALESCE(SUM(original_bytes), 0)
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = $1 AND principal_id = $2 AND conversation_id = $3
		  AND expires_at > $4 AND absolute_expires_at > $4
	`, record.Scope.Tenant, record.Scope.Principal, record.Scope.Conversation,
		now.UTC()).Scan(&mappingCount, &originalBytes); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("count PostgreSQL PII mappings: %w", err)
	}
	if mappingCount >= limits.MaximumMappings ||
		record.OriginalBytes > limits.MaximumOriginalBytes-originalBytes {
		return privacy.EncryptedMapping{}, privacy.ErrConversationLimit
	}
	if mappingCount == 0 {
		var conversations int
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM (
				SELECT 1 FROM llm_pii_conversation_mappings
				WHERE expires_at > $1 AND absolute_expires_at > $1
				GROUP BY tenant_id, principal_id, conversation_id
			) AS live_conversations
		`, now.UTC()).Scan(&conversations); err != nil {
			return privacy.EncryptedMapping{}, fmt.Errorf("count PostgreSQL PII conversations: %w", err)
		}
		if conversations >= limits.MaximumConversations {
			return privacy.EncryptedMapping{}, privacy.ErrConversationLimit
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO llm_pii_conversation_mappings (
			tenant_id, principal_id, conversation_id, entity, lookup_digest,
			token, key_id, nonce, ciphertext, original_bytes,
			created_at, last_used_at, expires_at, absolute_expires_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
	`, record.Scope.Tenant, record.Scope.Principal, record.Scope.Conversation,
		record.Entity, record.LookupDigest, record.Token, record.KeyID, record.Nonce,
		record.Ciphertext, record.OriginalBytes, record.CreatedAt, record.LastUsedAt,
		record.ExpiresAt, record.AbsoluteExpires); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("insert PostgreSQL PII mapping: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return privacy.EncryptedMapping{}, fmt.Errorf("commit PostgreSQL PII mapping: %w", err)
	}
	return record, nil
}

func loadPostgresPIIMapping(ctx context.Context, tx pgx.Tx, scope privacy.ConversationScope, digest []byte) (privacy.EncryptedMapping, bool, error) {
	record := privacy.EncryptedMapping{Scope: scope, LookupDigest: append([]byte(nil), digest...)}
	err := tx.QueryRow(ctx, `
		SELECT entity, token, key_id, nonce, ciphertext, original_bytes,
		       created_at, last_used_at, expires_at, absolute_expires_at
		FROM llm_pii_conversation_mappings
		WHERE tenant_id = $1 AND principal_id = $2 AND conversation_id = $3
		  AND lookup_digest = $4
	`, scope.Tenant, scope.Principal, scope.Conversation, digest).Scan(
		&record.Entity, &record.Token, &record.KeyID, &record.Nonce,
		&record.Ciphertext, &record.OriginalBytes, &record.CreatedAt,
		&record.LastUsedAt, &record.ExpiresAt, &record.AbsoluteExpires,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return privacy.EncryptedMapping{}, false, nil
	}
	if err != nil {
		return privacy.EncryptedMapping{}, false, fmt.Errorf("load PostgreSQL PII mapping: %w", err)
	}
	return record, true, nil
}

func (s *Store) RewrapPIIMapping(ctx context.Context, record privacy.EncryptedMapping, keyID string, nonce, ciphertext []byte) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE llm_pii_conversation_mappings
		SET key_id = $1, nonce = $2, ciphertext = $3
		WHERE tenant_id = $4 AND principal_id = $5 AND conversation_id = $6
		  AND lookup_digest = $7 AND key_id = $8 AND nonce = $9 AND ciphertext = $10
	`, keyID, nonce, ciphertext, record.Scope.Tenant, record.Scope.Principal,
		record.Scope.Conversation, record.LookupDigest, record.KeyID, record.Nonce,
		record.Ciphertext)
	if err != nil {
		return fmt.Errorf("rewrap PostgreSQL PII mapping: %w", err)
	}
	return nil
}

func (s *Store) DeletePIIConversation(ctx context.Context, scope privacy.ConversationScope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	_, err := s.pool.Exec(ctx, `
		DELETE FROM llm_pii_conversation_mappings
		WHERE tenant_id = $1 AND principal_id = $2 AND conversation_id = $3
	`, scope.Tenant, scope.Principal, scope.Conversation)
	if err != nil {
		return fmt.Errorf("delete PostgreSQL PII conversation: %w", err)
	}
	return nil
}

func (s *Store) PrunePIIMappings(ctx context.Context, now time.Time, limit int) (int64, error) {
	if limit < 1 {
		return 0, fmt.Errorf("PostgreSQL PII prune limit must be positive")
	}
	result, err := s.pool.Exec(ctx, `
		DELETE FROM llm_pii_conversation_mappings WHERE ctid IN (
			SELECT ctid FROM llm_pii_conversation_mappings
			WHERE expires_at <= $1 OR absolute_expires_at <= $1
			ORDER BY LEAST(expires_at, absolute_expires_at)
			LIMIT $2
		)
	`, now.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("prune PostgreSQL PII mappings: %w", err)
	}
	return result.RowsAffected(), nil
}

func (s *Store) PIIMappingStatus(
	ctx context.Context,
	now time.Time,
) (privacy.MappingStoreStatus, error) {
	status := privacy.MappingStoreStatus{Backend: "postgres"}
	err := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE expires_at > $1 AND absolute_expires_at > $1),
			COALESCE(SUM(original_bytes) FILTER (WHERE expires_at > $1 AND absolute_expires_at > $1), 0),
			COUNT(*) FILTER (WHERE expires_at <= $1 OR absolute_expires_at <= $1),
			(SELECT COUNT(*) FROM (
				SELECT 1 FROM llm_pii_conversation_mappings
				WHERE expires_at > $1 AND absolute_expires_at > $1
				GROUP BY tenant_id, principal_id, conversation_id
			) AS live_conversations)
		FROM llm_pii_conversation_mappings
	`, now.UTC()).Scan(
		&status.LiveMappings, &status.LiveOriginalBytes,
		&status.ExpiredPendingPrune, &status.LiveConversations,
	)
	if err != nil {
		return status, fmt.Errorf("inspect PostgreSQL PII mappings: %w", err)
	}
	return status, nil
}

func validatePostgresEncryptedMapping(record privacy.EncryptedMapping) error {
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

func earlierPIITime(left, right time.Time) time.Time {
	if left.Before(right) {
		return left
	}
	return right
}

var _ privacy.MappingBackend = (*Store)(nil)
var _ privacy.MappingStatusSource = (*Store)(nil)
