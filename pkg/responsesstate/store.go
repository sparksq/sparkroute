// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package responsesstate stores content-free routing affinity for
// provider-owned OpenAI Responses state.
package responsesstate

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxResponseIDBytes  = 512
	MaxResourceIDBytes  = 512
	MaxScopeBytes       = 128
	MaxNameBytes        = 256
	DefaultMaxEntries   = 100_000
	DefaultTTL          = 30 * 24 * time.Hour
	DefaultTombstoneTTL = 30 * 24 * time.Hour
)

var (
	ErrConflict = errors.New("resource affinity conflicts with an existing binding")
	ErrNotFound = errors.New("resource affinity was not found")
)

type ResourceKind string

const (
	ResourceResponse     ResourceKind = "response"
	ResourceConversation ResourceKind = "conversation"
	ResourceItem         ResourceKind = "item"
	ResourceFile         ResourceKind = "file"
	ResourcePrompt       ResourceKind = "prompt"
)

type ResourceKey struct {
	Scope      string
	Kind       ResourceKind
	ResourceID string
}

func (k ResourceKey) Validate() error {
	if err := ValidateScope(k.Scope); err != nil {
		return err
	}
	if err := ValidateResourceKind(k.Kind); err != nil {
		return err
	}
	return ValidateResourceID(k.ResourceID)
}

// ResourceAffinity contains only routing identity. A zero ExpiresAt represents
// an active durable resource. Deleted resources are retained as bounded
// tombstones so IDs cannot be silently rebound to another route.
type ResourceAffinity struct {
	ResourceKey
	VirtualModel  string
	Provider      string
	Deployment    string
	UpstreamModel string
	BoundAt       time.Time
	DeletedAt     time.Time
	ExpiresAt     time.Time
}

func (a ResourceAffinity) Validate() error {
	if err := a.ResourceKey.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"virtual model":  a.VirtualModel,
		"provider":       a.Provider,
		"deployment":     a.Deployment,
		"upstream model": a.UpstreamModel,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !utf8.ValidString(value) || len(value) > MaxNameBytes {
			return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, MaxNameBytes)
		}
	}
	if a.BoundAt.IsZero() {
		return fmt.Errorf("bound time is required")
	}
	if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(a.BoundAt) {
		return fmt.Errorf("expiry must be after bound time")
	}
	if !a.DeletedAt.IsZero() {
		if a.DeletedAt.Before(a.BoundAt) {
			return fmt.Errorf("deleted time must not be before bound time")
		}
		if a.ExpiresAt.IsZero() || !a.ExpiresAt.After(a.DeletedAt) {
			return fmt.Errorf("deleted resource expiry must be after deleted time")
		}
	}
	return nil
}

func (a ResourceAffinity) Deleted() bool {
	return !a.DeletedAt.IsZero()
}

// Affinity contains only routing identity. It must never contain prompts,
// response content, credentials, or caller-controlled headers.
type Affinity struct {
	Scope         string
	ResponseID    string
	VirtualModel  string
	Provider      string
	Deployment    string
	UpstreamModel string
	BoundAt       time.Time
	ExpiresAt     time.Time
}

func (a Affinity) Validate() error {
	if err := ValidateScope(a.Scope); err != nil {
		return err
	}
	if err := ValidateResponseID(a.ResponseID); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"virtual model":  a.VirtualModel,
		"provider":       a.Provider,
		"deployment":     a.Deployment,
		"upstream model": a.UpstreamModel,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
		if !utf8.ValidString(value) || len(value) > MaxNameBytes {
			return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, MaxNameBytes)
		}
	}
	if a.BoundAt.IsZero() {
		return fmt.Errorf("bound time is required")
	}
	if a.ExpiresAt.IsZero() || !a.ExpiresAt.After(a.BoundAt) {
		return fmt.Errorf("expiry must be after bound time")
	}
	return nil
}

func ValidateScope(scope string) error {
	if scope == "" || !utf8.ValidString(scope) || len(scope) > MaxScopeBytes {
		return fmt.Errorf("scope must be valid UTF-8 and between 1 and %d bytes", MaxScopeBytes)
	}
	for _, value := range scope {
		if value <= 0x20 || value == 0x7f {
			return fmt.Errorf("scope must not contain whitespace or control characters")
		}
	}
	return nil
}

func ValidateResponseID(responseID string) error {
	if responseID == "" {
		return fmt.Errorf("response ID is required")
	}
	if !utf8.ValidString(responseID) || len(responseID) > MaxResponseIDBytes {
		return fmt.Errorf(
			"response ID must be valid UTF-8 and at most %d bytes",
			MaxResponseIDBytes,
		)
	}
	for _, value := range responseID {
		if value <= 0x20 || value == 0x7f {
			return fmt.Errorf("response ID must not contain whitespace or control characters")
		}
	}
	return nil
}

// Store provides conflict-safe affinity binding and lookup. A response ID may
// never be silently rebound to a different routing identity.
type Store interface {
	Resolve(context.Context, string, string) (Affinity, bool, error)
	Bind(context.Context, Affinity) error
}

// ResourceStore extends response-chain affinity with durable provider-owned
// resource kinds such as Conversations.
type ResourceStore interface {
	ResolveResource(context.Context, ResourceKey) (ResourceAffinity, bool, error)
	BindResource(context.Context, ResourceAffinity) error
	TombstoneResource(context.Context, ResourceKey, time.Time, time.Time) error
}

type MemoryOptions struct {
	MaxEntries int
	Now        func() time.Time
}

type memoryEntry struct {
	affinity ResourceAffinity
	element  *list.Element
}

// MemoryStore is the bounded standalone fallback used when no durable store is
// configured. Entries are expired lazily and least-recently-used entries are
// evicted when the configured bound is reached.
type MemoryStore struct {
	mu         sync.Mutex
	entries    map[string]*memoryEntry
	files      map[string]FileRecord
	order      *list.List
	maxEntries int
	now        func() time.Time
}

func NewMemoryStore(options MemoryOptions) *MemoryStore {
	maxEntries := options.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{
		entries:    make(map[string]*memoryEntry),
		files:      make(map[string]FileRecord),
		order:      list.New(),
		maxEntries: maxEntries,
		now:        now,
	}
}

func (s *MemoryStore) Resolve(
	ctx context.Context,
	scope string,
	responseID string,
) (Affinity, bool, error) {
	resource, found, err := s.ResolveResource(ctx, ResourceKey{
		Scope:      scope,
		Kind:       ResourceResponse,
		ResourceID: responseID,
	})
	if err != nil {
		return Affinity{}, false, err
	}
	if !found || resource.Deleted() {
		return Affinity{}, false, nil
	}
	return Affinity{
		Scope:         resource.Scope,
		ResponseID:    resource.ResourceID,
		VirtualModel:  resource.VirtualModel,
		Provider:      resource.Provider,
		Deployment:    resource.Deployment,
		UpstreamModel: resource.UpstreamModel,
		BoundAt:       resource.BoundAt,
		ExpiresAt:     resource.ExpiresAt,
	}, true, nil
}

func (s *MemoryStore) Bind(ctx context.Context, affinity Affinity) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	return s.BindResource(ctx, ResourceAffinity{
		ResourceKey: ResourceKey{
			Scope:      affinity.Scope,
			Kind:       ResourceResponse,
			ResourceID: affinity.ResponseID,
		},
		VirtualModel:  affinity.VirtualModel,
		Provider:      affinity.Provider,
		Deployment:    affinity.Deployment,
		UpstreamModel: affinity.UpstreamModel,
		BoundAt:       affinity.BoundAt,
		ExpiresAt:     affinity.ExpiresAt,
	})
}

func (s *MemoryStore) ResolveResource(
	ctx context.Context,
	key ResourceKey,
) (ResourceAffinity, bool, error) {
	if err := ctx.Err(); err != nil {
		return ResourceAffinity{}, false, err
	}
	if err := key.Validate(); err != nil {
		return ResourceAffinity{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[memoryKey(key)]
	if !exists {
		return ResourceAffinity{}, false, nil
	}
	if resourceExpired(entry.affinity, s.now()) {
		s.remove(entry)
		return ResourceAffinity{}, false, nil
	}
	s.order.MoveToBack(entry.element)
	return entry.affinity, true, nil
}

func (s *MemoryStore) BindResource(
	ctx context.Context,
	affinity ResourceAffinity,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := affinity.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindResourceLocked(affinity)
}

func (s *MemoryStore) bindResourceLocked(affinity ResourceAffinity) error {
	key := memoryKey(affinity.ResourceKey)
	if existing, exists := s.entries[key]; exists {
		if resourceExpired(existing.affinity, s.now()) {
			s.remove(existing)
		} else if existing.affinity.Deleted() {
			return ErrConflict
		} else if sameResourceRoute(existing.affinity, affinity) {
			existing.affinity.ExpiresAt = MergeResourceExpiry(
				existing.affinity.ExpiresAt,
				affinity.ExpiresAt,
			)
			s.order.MoveToBack(existing.element)
			return nil
		} else {
			return ErrConflict
		}
	}
	for len(s.entries) >= s.maxEntries {
		front := s.order.Front()
		if front == nil {
			break
		}
		s.remove(s.entries[front.Value.(string)])
	}
	element := s.order.PushBack(key)
	s.entries[key] = &memoryEntry{
		affinity: affinity,
		element:  element,
	}
	return nil
}

func (s *MemoryStore) TombstoneResource(
	ctx context.Context,
	key ResourceKey,
	deletedAt time.Time,
	expiresAt time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := key.Validate(); err != nil {
		return err
	}
	if deletedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(deletedAt) {
		return fmt.Errorf("tombstone expiry must be after deleted time")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, exists := s.entries[memoryKey(key)]
	if !exists || resourceExpired(entry.affinity, s.now()) {
		if exists {
			s.remove(entry)
		}
		return ErrNotFound
	}
	if !entry.affinity.Deleted() {
		entry.affinity.DeletedAt = deletedAt
		entry.affinity.ExpiresAt = expiresAt
	}
	s.order.MoveToBack(entry.element)
	return nil
}

func (s *MemoryStore) remove(entry *memoryEntry) {
	key := memoryKey(entry.affinity.ResourceKey)
	delete(s.entries, key)
	delete(s.files, key)
	s.order.Remove(entry.element)
}

func SameRoute(left, right Affinity) bool {
	return sameRoute(left, right)
}

func sameRoute(left, right Affinity) bool {
	return left.Scope == right.Scope &&
		left.ResponseID == right.ResponseID &&
		left.VirtualModel == right.VirtualModel &&
		left.Provider == right.Provider &&
		left.Deployment == right.Deployment &&
		left.UpstreamModel == right.UpstreamModel
}

func SameResourceRoute(left, right ResourceAffinity) bool {
	return sameResourceRoute(left, right)
}

// MergeResourceExpiry preserves the longest safe lifetime for an idempotent
// same-route binding. A zero expiry is durable and therefore wins.
func MergeResourceExpiry(left, right time.Time) time.Time {
	if left.IsZero() || right.IsZero() {
		return time.Time{}
	}
	if right.After(left) {
		return right
	}
	return left
}

func sameResourceRoute(left, right ResourceAffinity) bool {
	return left.ResourceKey == right.ResourceKey &&
		left.VirtualModel == right.VirtualModel &&
		left.Provider == right.Provider &&
		left.Deployment == right.Deployment &&
		left.UpstreamModel == right.UpstreamModel
}

func resourceExpired(affinity ResourceAffinity, now time.Time) bool {
	return !affinity.ExpiresAt.IsZero() && !now.Before(affinity.ExpiresAt)
}

func ValidateResourceKind(kind ResourceKind) error {
	switch kind {
	case ResourceResponse, ResourceConversation, ResourceItem, ResourceFile, ResourcePrompt:
		return nil
	default:
		return fmt.Errorf("unsupported resource kind %q", kind)
	}
}

func ValidateResourceID(resourceID string) error {
	if resourceID == "" {
		return fmt.Errorf("resource ID is required")
	}
	if !utf8.ValidString(resourceID) || len(resourceID) > MaxResourceIDBytes {
		return fmt.Errorf(
			"resource ID must be valid UTF-8 and at most %d bytes",
			MaxResourceIDBytes,
		)
	}
	for _, value := range resourceID {
		if value <= 0x20 || value == 0x7f {
			return fmt.Errorf("resource ID must not contain whitespace or control characters")
		}
	}
	return nil
}

func memoryKey(key ResourceKey) string {
	return string(key.Kind) + "\x00" + key.Scope + "\x00" + key.ResourceID
}
