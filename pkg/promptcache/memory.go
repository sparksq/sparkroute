// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package promptcache

import (
	"context"
	"sync"
	"time"
)

type MemoryOptions struct {
	MaxEntries int
}

type memoryKey struct {
	scope, model, operation, revision, prefix, deployment string
}

type memoryEntry struct {
	prefix      Prefix
	route       Route
	lastSuccess time.Time
	expiresAt   time.Time
}

type MemoryStore struct {
	mu         sync.Mutex
	maxEntries int
	entries    map[memoryKey]memoryEntry
}

func NewMemoryStore(options MemoryOptions) *MemoryStore {
	maximum := options.MaxEntries
	if maximum == 0 {
		maximum = 100_000
	}
	return &MemoryStore{
		maxEntries: maximum,
		entries:    make(map[memoryKey]memoryEntry),
	}
}

func (s *MemoryStore) Lookup(_ context.Context, query Query) (Match, bool, error) {
	now := query.Now
	if now.IsZero() {
		now = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpired(now)
	for _, prefix := range query.Prefixes {
		var best memoryEntry
		found := false
		for _, route := range query.Candidates {
			entry, exists := s.entries[memoryKey{
				scope: query.ScopeDigest, model: query.VirtualModel,
				operation: query.Operation, revision: query.ConfigRevision,
				prefix: prefix.Digest, deployment: route.Deployment,
			}]
			if !exists || entry.route != route || !entry.expiresAt.After(now) {
				continue
			}
			if !found || entry.lastSuccess.After(best.lastSuccess) {
				best, found = entry, true
			}
		}
		if found {
			return Match{
				Prefix: best.prefix, Route: best.route,
				LastSuccess: best.lastSuccess, ExpiresAt: best.expiresAt,
			}, true, nil
		}
	}
	return Match{}, false, nil
}

func (s *MemoryStore) Record(_ context.Context, observation Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeExpired(observation.LastSuccess)
	for _, prefix := range observation.Prefixes {
		key := memoryKey{
			scope: observation.ScopeDigest, model: observation.VirtualModel,
			operation: observation.Operation, revision: observation.ConfigRevision,
			prefix: prefix.Digest, deployment: observation.Route.Deployment,
		}
		previous, exists := s.entries[key]
		if !exists || !previous.lastSuccess.After(observation.LastSuccess) {
			s.entries[key] = memoryEntry{
				prefix: prefix, route: observation.Route,
				lastSuccess: observation.LastSuccess, expiresAt: observation.ExpiresAt,
			}
		}
	}
	for s.maxEntries > 0 && len(s.entries) > s.maxEntries {
		var oldestKey memoryKey
		var oldest time.Time
		first := true
		for key, entry := range s.entries {
			if first || entry.lastSuccess.Before(oldest) {
				oldestKey, oldest, first = key, entry.lastSuccess, false
			}
		}
		delete(s.entries, oldestKey)
	}
	return nil
}

func (s *MemoryStore) removeExpired(now time.Time) {
	if now.IsZero() {
		now = time.Now()
	}
	for key, entry := range s.entries {
		if !entry.expiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

var _ Backend = (*MemoryStore)(nil)
