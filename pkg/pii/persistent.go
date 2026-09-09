// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultConversationSlidingTTL      = 24 * time.Hour
	DefaultConversationAbsoluteTTL     = 30 * 24 * time.Hour
	DefaultMaximumConversations        = 100_000
	DefaultMaximumConversationMappings = defaultMaximumMappings
	DefaultMaximumConversationBytes    = defaultMaximumOriginalBytes
)

var (
	ErrConversationLimit = errors.New("PII conversation mapping limit exceeded")
	ErrDecryptMapping    = errors.New("PII conversation mapping could not be decrypted")
)

// ConversationScope partitions durable substitutions. Every component comes
// from authenticated identity and trusted attribution, never request content.
type ConversationScope struct {
	Tenant       string
	Principal    string
	Conversation string
}

func (s ConversationScope) Validate() error {
	for name, value := range map[string]string{
		"tenant": s.Tenant, "principal": s.Principal, "conversation": s.Conversation,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("PII conversation %s is required", name)
		}
		if len(value) > 256 || !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("PII conversation %s is invalid", name)
		}
	}
	return nil
}

// EncryptedMapping is the storage-neutral database record. Ciphertext is the
// only reversible representation of Original and KeyID names keyring material
// without containing it.
type EncryptedMapping struct {
	Scope           ConversationScope
	Entity          Entity
	LookupDigest    []byte
	Token           string
	KeyID           string
	Nonce           []byte
	Ciphertext      []byte
	OriginalBytes   int
	CreatedAt       time.Time
	LastUsedAt      time.Time
	ExpiresAt       time.Time
	AbsoluteExpires time.Time
}

type ConversationLimits struct {
	MaximumMappings      int
	MaximumOriginalBytes int
	MaximumConversations int
	SlidingTTL           time.Duration
	AbsoluteTTL          time.Duration
}

func (l ConversationLimits) effective() (ConversationLimits, error) {
	if l.MaximumMappings == 0 {
		l.MaximumMappings = DefaultMaximumConversationMappings
	}
	if l.MaximumOriginalBytes == 0 {
		l.MaximumOriginalBytes = DefaultMaximumConversationBytes
	}
	if l.MaximumConversations == 0 {
		l.MaximumConversations = DefaultMaximumConversations
	}
	if l.SlidingTTL == 0 {
		l.SlidingTTL = DefaultConversationSlidingTTL
	}
	if l.AbsoluteTTL == 0 {
		l.AbsoluteTTL = DefaultConversationAbsoluteTTL
	}
	if l.MaximumMappings < 1 || l.MaximumOriginalBytes < 1 ||
		l.MaximumConversations < 1 || l.SlidingTTL <= 0 ||
		l.AbsoluteTTL <= 0 || l.SlidingTTL > l.AbsoluteTTL {
		return ConversationLimits{}, fmt.Errorf("PII conversation limits are invalid")
	}
	return l, nil
}

// NormalizeConversationLimits applies bounded defaults and validates a store
// call. Backends use it too so direct callers cannot bypass enforcement.
func NormalizeConversationLimits(l ConversationLimits) (ConversationLimits, error) {
	return l.effective()
}

// MappingBackend atomically stores already-encrypted substitutions. Load must
// return only live rows and extend their sliding expiry no later than the
// absolute expiry. PutIfAbsent must serialize bound checks with insertion and
// return the existing row when its lookup digest already exists.
type MappingBackend interface {
	LoadPIIMappings(context.Context, ConversationScope, time.Time, ConversationLimits) ([]EncryptedMapping, error)
	PutPIIMappingIfAbsent(context.Context, EncryptedMapping, time.Time, ConversationLimits) (EncryptedMapping, error)
	RewrapPIIMapping(context.Context, EncryptedMapping, string, []byte, []byte) error
	DeletePIIConversation(context.Context, ConversationScope) error
	PrunePIIMappings(context.Context, time.Time, int) (int64, error)
}

type keyMaterial struct {
	aead cipher.AEAD
}

// Keyring contains one active encryption key and zero or more decrypt-only old
// keys. Derived encryption and lookup keys are domain-separated.
type Keyring struct {
	active string
	keys   map[string]keyMaterial
	lookup [sha256.Size]byte
}

type keyringDocument struct {
	Active    string            `json:"active"`
	LookupKey string            `json:"lookup_key,omitempty"`
	Keys      map[string]string `json:"keys"`
}

// ParseKeyring accepts either a JSON keyring or one raw/base64 key. JSON keys
// are base64 values and the active member receives all new writes. A raw key's
// ID is a short non-secret fingerprint so restarts remain decryptable.
func ParseKeyring(value []byte) (*Keyring, error) {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return nil, fmt.Errorf("PII conversation key material is empty")
	}
	document := keyringDocument{}
	var lookupMaster []byte
	var err error
	defer func() { clear(lookupMaster) }()
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &document); err != nil {
			return nil, fmt.Errorf("decode PII conversation keyring: %w", err)
		}
		if strings.TrimSpace(document.Active) == "" || len(document.Keys) == 0 {
			return nil, fmt.Errorf("PII conversation keyring requires active and keys")
		}
		if document.LookupKey != "" {
			lookupMaster, err = decodeKey(document.LookupKey)
			if err != nil || len(lookupMaster) < 32 {
				clear(lookupMaster)
				return nil, fmt.Errorf("PII conversation lookup_key must contain at least 32 base64 bytes")
			}
		} else if len(document.Keys) > 1 {
			return nil, fmt.Errorf("PII conversation keyrings with multiple keys require lookup_key")
		}
	} else {
		raw := append([]byte(nil), value...)
		if decoded, err := decodeKey(trimmed); err == nil && len(decoded) >= 32 {
			clear(raw)
			raw = decoded
		}
		digest := sha256.Sum256(raw)
		id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:8]))
		document.Active = id
		document.Keys = map[string]string{id: base64.StdEncoding.EncodeToString(raw)}
		lookupMaster = append([]byte(nil), raw...)
		clear(raw)
	}
	result := &Keyring{active: document.Active, keys: make(map[string]keyMaterial, len(document.Keys))}
	for id, encoded := range document.Keys {
		if strings.TrimSpace(id) == "" || len(id) > 128 || strings.ContainsAny(id, "\r\n\x00") {
			return nil, fmt.Errorf("PII conversation key ID is invalid")
		}
		raw, err := decodeKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode PII conversation key %q: %w", id, err)
		}
		if len(raw) < 32 {
			clear(raw)
			return nil, fmt.Errorf("PII conversation key %q must contain at least 32 bytes", id)
		}
		if lookupMaster == nil {
			lookupMaster = append([]byte(nil), raw...)
		}
		result.keys[id], err = deriveKeyMaterial(raw)
		clear(raw)
		if err != nil {
			return nil, err
		}
	}
	if _, exists := result.keys[result.active]; !exists {
		clear(lookupMaster)
		return nil, fmt.Errorf("PII conversation active key %q is absent", result.active)
	}
	result.lookup = deriveKey(lookupMaster, "sparkroute/pii/lookup/v1")
	clear(lookupMaster)
	return result, nil
}

func decodeKey(value string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(strings.TrimSpace(value)); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("key is not valid base64")
}

func deriveKeyMaterial(master []byte) (keyMaterial, error) {
	encryption := deriveKey(master, "sparkroute/pii/encryption/v1")
	block, err := aes.NewCipher(encryption[:])
	clear(encryption[:])
	if err != nil {
		return keyMaterial{}, fmt.Errorf("construct PII mapping cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return keyMaterial{}, fmt.Errorf("construct PII mapping AEAD: %w", err)
	}
	return keyMaterial{aead: aead}, nil
}

func deriveKey(master []byte, label string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte(label))
	var result [sha256.Size]byte
	copy(result[:], mac.Sum(nil))
	return result
}

type DirectoryOptions struct {
	Limits      ConversationLimits
	TokenSource TokenSource
	Now         func() time.Time
}

// Directory owns crypto and stable mapping semantics above a storage backend.
type Directory struct {
	backend     MappingBackend
	keyring     *Keyring
	limits      ConversationLimits
	tokenSource TokenSource
	now         func() time.Time
}

func NewDirectory(backend MappingBackend, keyring *Keyring, options DirectoryOptions) (*Directory, error) {
	if backend == nil || keyring == nil {
		return nil, fmt.Errorf("PII conversation backend and keyring are required")
	}
	limits, err := options.Limits.effective()
	if err != nil {
		return nil, err
	}
	tokenSource := options.TokenSource
	if tokenSource == nil {
		tokenSource = randomToken
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Directory{backend: backend, keyring: keyring, limits: limits, tokenSource: tokenSource, now: now}, nil
}

func (d *Directory) NewSession(
	ctx context.Context,
	detector Detector,
	entities []Entity,
	scope ConversationScope,
) (*Session, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	now := d.now().UTC()
	records, err := d.backend.LoadPIIMappings(ctx, scope, now, d.limits)
	if err != nil {
		return nil, fmt.Errorf("load PII conversation mappings: %w", err)
	}
	mappings := make([]Mapping, 0, len(records))
	for _, record := range records {
		original, decryptErr := d.decrypt(record)
		if decryptErr != nil {
			return nil, decryptErr
		}
		mappings = append(mappings, Mapping{Entity: record.Entity, Original: original, Token: record.Token})
		if record.KeyID != d.keyring.active {
			keyID, nonce, ciphertext, encryptErr := d.encrypt(record, original)
			if encryptErr != nil {
				return nil, encryptErr
			}
			if rewrapErr := d.backend.RewrapPIIMapping(ctx, record, keyID, nonce, ciphertext); rewrapErr != nil {
				return nil, fmt.Errorf("rotate PII conversation mapping: %w", rewrapErr)
			}
		}
	}
	return NewSession(detector, entities, SessionOptions{
		MaximumMappings: d.limits.MaximumMappings, MaximumOriginalBytes: d.limits.MaximumOriginalBytes,
		InitialMappings: mappings,
		MappingResolver: func(resolveCtx context.Context, entity Entity, original string) (string, error) {
			return d.resolve(resolveCtx, scope, entity, original)
		},
	})
}

func (d *Directory) resolve(ctx context.Context, scope ConversationScope, entity Entity, original string) (string, error) {
	digest := mappingDigest(d.keyring.lookup[:], scope, entity, original)
	identifier, err := d.tokenSource()
	if err != nil {
		return "", fmt.Errorf("create PII conversation token: %w", err)
	}
	token := "[[SPARKROUTE_PII_" + strings.ToUpper(string(entity)) + "_" + strings.TrimSpace(identifier) + "]]"
	if err := validateToken(entity, token); err != nil {
		return "", err
	}
	now := d.now().UTC()
	record := EncryptedMapping{
		Scope: scope, Entity: entity, LookupDigest: digest, Token: token,
		KeyID: d.keyring.active, OriginalBytes: len(original), CreatedAt: now, LastUsedAt: now,
		ExpiresAt: now.Add(d.limits.SlidingTTL), AbsoluteExpires: now.Add(d.limits.AbsoluteTTL),
	}
	record.KeyID, record.Nonce, record.Ciphertext, err = d.encrypt(record, original)
	if err != nil {
		return "", err
	}
	stored, err := d.backend.PutPIIMappingIfAbsent(ctx, record, now, d.limits)
	if err != nil {
		return "", fmt.Errorf("store PII conversation mapping: %w", err)
	}
	storedOriginal, err := d.decrypt(stored)
	if err != nil {
		return "", err
	}
	if stored.Entity != entity || !hmac.Equal(stored.LookupDigest, digest) || storedOriginal != original {
		return "", ErrDecryptMapping
	}
	return stored.Token, nil
}

func mappingDigest(key []byte, scope ConversationScope, entity Entity, original string) []byte {
	mac := hmac.New(sha256.New, key)
	for _, value := range []string{scope.Tenant, scope.Principal, scope.Conversation, string(entity), original} {
		_, _ = mac.Write([]byte{0})
		_, _ = mac.Write([]byte(value))
	}
	return mac.Sum(nil)
}

func (d *Directory) encrypt(record EncryptedMapping, original string) (string, []byte, []byte, error) {
	material := d.keyring.keys[d.keyring.active]
	nonce := make([]byte, material.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", nil, nil, fmt.Errorf("create PII mapping nonce: %w", err)
	}
	ciphertext := material.aead.Seal(nil, nonce, []byte(original), mappingAAD(record))
	return d.keyring.active, nonce, ciphertext, nil
}

func (d *Directory) decrypt(record EncryptedMapping) (string, error) {
	material, exists := d.keyring.keys[record.KeyID]
	if !exists || len(record.LookupDigest) != sha256.Size || record.OriginalBytes < 1 {
		return "", ErrDecryptMapping
	}
	plaintext, err := material.aead.Open(nil, record.Nonce, record.Ciphertext, mappingAAD(record))
	if err != nil || len(plaintext) != record.OriginalBytes || !utf8.Valid(plaintext) {
		clear(plaintext)
		return "", ErrDecryptMapping
	}
	result := string(plaintext)
	clear(plaintext)
	return result, nil
}

func mappingAAD(record EncryptedMapping) []byte {
	return []byte(strings.Join([]string{
		"sparkroute/pii/mapping/v1", record.Scope.Tenant, record.Scope.Principal,
		record.Scope.Conversation, string(record.Entity),
		base64.RawURLEncoding.EncodeToString(record.LookupDigest), record.Token,
	}, "\x00"))
}

func (d *Directory) DeleteConversation(ctx context.Context, scope ConversationScope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	return d.backend.DeletePIIConversation(ctx, scope)
}

func (d *Directory) Prune(ctx context.Context, limit int) (int64, error) {
	if limit < 1 {
		return 0, fmt.Errorf("PII prune limit must be positive")
	}
	return d.backend.PrunePIIMappings(ctx, d.now().UTC(), limit)
}

// Status returns content-free aggregate mapping and retention state.
func (d *Directory) Status(ctx context.Context) (DirectoryStatus, error) {
	source, ok := d.backend.(MappingStatusSource)
	if !ok {
		return DirectoryStatus{}, fmt.Errorf("PII conversation backend does not expose status")
	}
	backend, err := source.PIIMappingStatus(ctx, d.now().UTC())
	status := DirectoryStatus{
		MappingStoreStatus:   backend,
		MaximumMappings:      d.limits.MaximumMappings,
		MaximumOriginalBytes: d.limits.MaximumOriginalBytes,
		MaximumConversations: d.limits.MaximumConversations,
		SlidingTTLSeconds:    int64(d.limits.SlidingTTL / time.Second),
		AbsoluteTTLSeconds:   int64(d.limits.AbsoluteTTL / time.Second),
	}
	return status, err
}

// RunPruner removes expired rows in bounded batches until ctx is canceled.
func (d *Directory) RunPruner(
	ctx context.Context,
	interval time.Duration,
	batchSize int,
	onError func(error),
) {
	if interval <= 0 || batchSize < 1 {
		if onError != nil {
			onError(fmt.Errorf("PII conversation pruner options are invalid"))
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				count, err := d.Prune(ctx, batchSize)
				if err != nil {
					if onError != nil && ctx.Err() == nil {
						onError(err)
					}
					break
				}
				if count < int64(batchSize) {
					break
				}
			}
		}
	}
}
