// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package modelcatalog

import (
	"context"
	"sync"
)

// MemoryResolver is a small exact-match adapter for tests, embedding, and
// conformance suites. Directory still owns validation and caching.
type MemoryResolver struct {
	mu      sync.RWMutex
	entries map[Request]Entry
}

func NewMemoryResolver(entries map[Request]Entry) *MemoryResolver {
	resolver := &MemoryResolver{entries: make(map[Request]Entry, len(entries))}
	for request, entry := range entries {
		resolver.entries[request] = entry
	}
	return resolver
}

func (r *MemoryResolver) Resolve(ctx context.Context, request Request) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	r.mu.RLock()
	entry, exists := r.entries[request]
	r.mu.RUnlock()
	if !exists {
		return Entry{}, ErrNotFound
	}
	return entry, nil
}

func (r *MemoryResolver) Set(request Request, entry Entry) {
	r.mu.Lock()
	if r.entries == nil {
		r.entries = make(map[Request]Entry)
	}
	r.entries[request] = entry
	r.mu.Unlock()
}

func (r *MemoryResolver) Delete(request Request) {
	r.mu.Lock()
	delete(r.entries, request)
	r.mu.Unlock()
}

var _ Resolver = (*MemoryResolver)(nil)
