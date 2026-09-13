// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

func (s *Store) Watch(
	ctx context.Context,
) (<-chan config.Version, error) {
	connection, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire PostgreSQL configuration watch connection: %w", err)
	}
	if _, err := connection.Exec(
		ctx,
		"LISTEN "+notificationChannel,
	); err != nil {
		connection.Release()
		return nil, fmt.Errorf("listen for PostgreSQL configuration changes: %w", err)
	}
	changes := make(chan config.Version, 1)
	go func() {
		defer close(changes)
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, _ = connection.Exec(cleanupCtx, "UNLISTEN "+notificationChannel)
			connection.Release()
		}()
		for {
			notification, err := connection.Conn().WaitForNotification(ctx)
			if err != nil {
				return
			}
			version := config.Version(notification.Payload)
			if validateVersion(version) != nil {
				continue
			}
			select {
			case changes <- version:
			default:
				// Notifications are hints; coalesce while the consumer catches up.
			}
		}
	}()
	return changes, nil
}
