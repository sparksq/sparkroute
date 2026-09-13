// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
)

const (
	defaultActivationLimit = 100
	maxActivationLimit     = 500
)

type ActivationQuery struct {
	Revision         config.Version
	BeforeGeneration int64
	Limit            int
}

type ActivationPage struct {
	Activations          []Activation `json:"activations"`
	NextBeforeGeneration int64        `json:"next_before_generation,omitempty"`
}

func (s *Store) ListActivations(
	ctx context.Context,
	query ActivationQuery,
) (ActivationPage, error) {
	if query.Limit == 0 {
		query.Limit = defaultActivationLimit
	}
	if query.Limit < 1 || query.Limit > maxActivationLimit {
		return ActivationPage{}, fmt.Errorf(
			"configuration activation limit must be between 1 and %d",
			maxActivationLimit,
		)
	}
	if query.BeforeGeneration < 0 {
		return ActivationPage{}, fmt.Errorf("before generation must be positive")
	}
	if query.Revision != "" {
		if err := validateVersion(query.Revision); err != nil {
			return ActivationPage{}, err
		}
	}

	where := make([]string, 0, 2)
	arguments := make([]any, 0, 3)
	if query.Revision != "" {
		arguments = append(arguments, query.Revision)
		where = append(where, fmt.Sprintf("revision_id = $%d", len(arguments)))
	}
	if query.BeforeGeneration > 0 {
		arguments = append(arguments, query.BeforeGeneration)
		where = append(where, fmt.Sprintf("generation < $%d", len(arguments)))
	}
	arguments = append(arguments, query.Limit+1)
	statement := `
		SELECT generation, revision_id, COALESCE(previous_revision_id, ''),
		       activated_at, activated_by, COALESCE(reason, '')
		FROM llm_config_activations`
	if len(where) > 0 {
		statement += " WHERE " + strings.Join(where, " AND ")
	}
	statement += fmt.Sprintf(
		" ORDER BY generation DESC LIMIT $%d",
		len(arguments),
	)
	rows, err := s.pool.Query(ctx, statement, arguments...)
	if err != nil {
		return ActivationPage{}, fmt.Errorf(
			"list PostgreSQL configuration activations: %w",
			err,
		)
	}
	defer rows.Close()
	activations := make([]Activation, 0, query.Limit+1)
	for rows.Next() {
		var activation Activation
		if err := rows.Scan(
			&activation.Generation,
			&activation.Version,
			&activation.PreviousVersion,
			&activation.ActivatedAt,
			&activation.ActivatedBy,
			&activation.Reason,
		); err != nil {
			return ActivationPage{}, fmt.Errorf(
				"scan PostgreSQL configuration activation: %w",
				err,
			)
		}
		activation.ActivatedAt = activation.ActivatedAt.UTC()
		activations = append(activations, activation)
	}
	if err := rows.Err(); err != nil {
		return ActivationPage{}, fmt.Errorf(
			"iterate PostgreSQL configuration activations: %w",
			err,
		)
	}
	page := ActivationPage{Activations: activations}
	if len(page.Activations) > query.Limit {
		page.Activations = page.Activations[:query.Limit]
		page.NextBeforeGeneration = page.Activations[len(page.Activations)-1].Generation
	}
	return page, nil
}
