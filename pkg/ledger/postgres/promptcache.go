// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sparksq/sparkroute/pkg/promptcache"
)

func (s *Store) Lookup(
	ctx context.Context,
	query promptcache.Query,
) (promptcache.Match, bool, error) {
	if len(query.Prefixes) == 0 || len(query.Candidates) == 0 {
		return promptcache.Match{}, false, nil
	}
	now := query.Now
	if now.IsZero() {
		now = time.Now()
	}
	digests := make([]string, len(query.Prefixes))
	for index, prefix := range query.Prefixes {
		digests[index] = prefix.Digest
	}
	deployments := make([]string, len(query.Candidates))
	allowed := make(map[promptcache.Route]struct{}, len(query.Candidates))
	for index, route := range query.Candidates {
		deployments[index] = route.Deployment
		allowed[route] = struct{}{}
	}
	rows, err := s.pool.Query(ctx, `
		SELECT prefix_digest, prefix_bytes, prefix_segments,
		       provider, deployment, upstream_model, upstream_protocol,
		       last_success_at, expires_at
		FROM llm_prompt_cache_affinities
		WHERE scope_digest = $1
		  AND virtual_model = $2
		  AND operation = $3
		  AND config_revision = $4
		  AND prefix_digest = ANY($5)
		  AND deployment = ANY($6)
		  AND expires_at > $7
		ORDER BY array_position($5::text[], prefix_digest), last_success_at DESC
	`, query.ScopeDigest, query.VirtualModel, query.Operation, query.ConfigRevision,
		digests, deployments, now)
	if err != nil {
		return promptcache.Match{}, false, fmt.Errorf("query prompt-cache affinities: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var match promptcache.Match
		if err := rows.Scan(
			&match.Prefix.Digest, &match.Prefix.Bytes, &match.Prefix.Segments,
			&match.Route.Provider, &match.Route.Deployment,
			&match.Route.UpstreamModel, &match.Route.UpstreamProtocol,
			&match.LastSuccess, &match.ExpiresAt,
		); err != nil {
			return promptcache.Match{}, false, fmt.Errorf("scan prompt-cache affinity: %w", err)
		}
		if _, exists := allowed[match.Route]; exists {
			return match, true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return promptcache.Match{}, false, fmt.Errorf("iterate prompt-cache affinities: %w", err)
	}
	return promptcache.Match{}, false, nil
}

func (s *Store) Record(ctx context.Context, observation promptcache.Observation) error {
	if len(observation.Prefixes) == 0 {
		return nil
	}
	batch := &pgx.Batch{}
	for _, prefix := range observation.Prefixes {
		batch.Queue(`
			INSERT INTO llm_prompt_cache_affinities (
				scope_digest, virtual_model, operation, config_revision,
				prefix_digest, prefix_bytes, prefix_segments,
				provider, deployment, upstream_model, upstream_protocol,
				last_success_at, expires_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
			ON CONFLICT (
				scope_digest, virtual_model, operation, config_revision,
				prefix_digest, deployment
			) DO UPDATE SET
				prefix_bytes = EXCLUDED.prefix_bytes,
				prefix_segments = EXCLUDED.prefix_segments,
				provider = EXCLUDED.provider,
				upstream_model = EXCLUDED.upstream_model,
				upstream_protocol = EXCLUDED.upstream_protocol,
				last_success_at = EXCLUDED.last_success_at,
				expires_at = EXCLUDED.expires_at
			WHERE llm_prompt_cache_affinities.last_success_at <= EXCLUDED.last_success_at
		`, observation.ScopeDigest, observation.VirtualModel, observation.Operation,
			observation.ConfigRevision, prefix.Digest, prefix.Bytes, prefix.Segments,
			observation.Route.Provider, observation.Route.Deployment,
			observation.Route.UpstreamModel, observation.Route.UpstreamProtocol,
			observation.LastSuccess, observation.ExpiresAt)
	}
	results := s.pool.SendBatch(ctx, batch)
	for range observation.Prefixes {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return fmt.Errorf("record prompt-cache affinity: %w", err)
		}
	}
	if err := results.Close(); err != nil {
		return fmt.Errorf("close prompt-cache affinity batch: %w", err)
	}
	// Keep cleanup bounded and opportunistic. Concurrent replicas may perform
	// the same deletion safely.
	if _, err := s.pool.Exec(ctx, `
		DELETE FROM llm_prompt_cache_affinities
		WHERE ctid IN (
			SELECT ctid FROM llm_prompt_cache_affinities
			WHERE expires_at <= $1
			LIMIT 1000
		)
	`, observation.LastSuccess); err != nil {
		return fmt.Errorf("prune prompt-cache affinities: %w", err)
	}
	return nil
}

var _ promptcache.Backend = (*Store)(nil)
