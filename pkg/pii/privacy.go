// Package pii provides bounded, request-scoped PII substitution primitives.
// It deliberately separates deterministic span detection from protocol-aware
// request and response traversal, which remains the gateway's responsibility.
package pii

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

type Entity string

const (
	EntityEmail      Entity = "email"
	EntityPhone      Entity = "phone"
	EntitySSN        Entity = "ssn"
	EntityCreditCard Entity = "credit_card"
	EntityIPv4       Entity = "ipv4"
	EntityPerson     Entity = "person"
	EntityAddress    Entity = "address"
)

var defaultEntities = []Entity{
	EntityEmail,
	EntityPhone,
	EntitySSN,
	EntityCreditCard,
	EntityIPv4,
}

// DefaultEntities returns a copy of the entities supported by the built-in
// detector and enabled when a session does not specify an entity list.
func DefaultEntities() []Entity {
	return append([]Entity(nil), defaultEntities...)
}

const (
	defaultMaximumMappings      = 4096
	defaultMaximumOriginalBytes = 4 << 20
)

var ErrMappingLimit = errors.New("PII substitution mapping limit exceeded")

// Span uses UTF-8 byte offsets into the exact text supplied to Detector.
type Span struct {
	Start  int
	End    int
	Entity Entity
}

// Detector returns non-overlapping entity spans. Implementations must not
// retain input content after Detect returns.
type Detector interface {
	Detect(context.Context, string, []Entity) ([]Span, error)
	Supports(Entity) bool
}

type TokenSource func() (string, error)

// MappingResolver returns the stable opaque token for an entity value. It is
// called only for values not already present in the request-local session.
// Implementations may durably coordinate token creation across replicas.
type MappingResolver func(context.Context, Entity, string) (string, error)

// Mapping seeds one session with a previously created exact substitution.
type Mapping struct {
	Entity   Entity
	Original string
	Token    string
}

type SessionOptions struct {
	MaximumMappings      int
	MaximumOriginalBytes int
	TokenSource          TokenSource
	InitialMappings      []Mapping
	MappingResolver      MappingResolver
}

type Session struct {
	detector             Detector
	entities             []Entity
	maximumMappings      int
	maximumOriginalBytes int
	tokenSource          TokenSource
	mappingResolver      MappingResolver

	mu            sync.Mutex
	byOriginal    map[string]string
	byToken       map[string]string
	counts        map[Entity]int
	originalBytes int
	replacer      *strings.Replacer
}

func NewSession(detector Detector, entities []Entity, options SessionOptions) (*Session, error) {
	if detector == nil {
		return nil, fmt.Errorf("PII detector is required")
	}
	if len(entities) == 0 {
		entities = defaultEntities
	}
	entities = append([]Entity(nil), entities...)
	seen := make(map[Entity]struct{}, len(entities))
	for _, entity := range entities {
		if strings.TrimSpace(string(entity)) == "" {
			return nil, fmt.Errorf("PII entity must not be empty")
		}
		if _, exists := seen[entity]; exists {
			return nil, fmt.Errorf("duplicate PII entity %q", entity)
		}
		seen[entity] = struct{}{}
		if !detector.Supports(entity) {
			return nil, fmt.Errorf("PII detector does not support entity %q", entity)
		}
	}
	maximumMappings := options.MaximumMappings
	if maximumMappings == 0 {
		maximumMappings = defaultMaximumMappings
	}
	maximumOriginalBytes := options.MaximumOriginalBytes
	if maximumOriginalBytes == 0 {
		maximumOriginalBytes = defaultMaximumOriginalBytes
	}
	if maximumMappings < 1 || maximumOriginalBytes < 1 {
		return nil, fmt.Errorf("PII mapping limits must be positive")
	}
	tokenSource := options.TokenSource
	if tokenSource == nil {
		tokenSource = randomToken
	}
	session := &Session{
		detector:             detector,
		entities:             entities,
		maximumMappings:      maximumMappings,
		maximumOriginalBytes: maximumOriginalBytes,
		tokenSource:          tokenSource,
		mappingResolver:      options.MappingResolver,
		byOriginal:           make(map[string]string),
		byToken:              make(map[string]string),
		counts:               make(map[Entity]int),
	}
	for index, mapping := range options.InitialMappings {
		if err := session.addMapping(mapping.Entity, mapping.Original, mapping.Token); err != nil {
			return nil, fmt.Errorf("initial PII mapping %d: %w", index, err)
		}
	}
	return session, nil
}

func (s *Session) Substitute(ctx context.Context, text string) (string, error) {
	spans, err := s.detector.Detect(ctx, text, s.entities)
	if err != nil {
		return "", err
	}
	if err := validateSpans(text, spans); err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return text, nil
	}
	var output strings.Builder
	output.Grow(len(text) + len(spans)*32)
	position := 0
	for _, span := range spans {
		output.WriteString(text[position:span.Start])
		original := text[span.Start:span.End]
		token, err := s.tokenFor(ctx, span.Entity, original)
		if err != nil {
			return "", err
		}
		output.WriteString(token)
		position = span.End
	}
	output.WriteString(text[position:])
	return output.String(), nil
}

// Redact replaces detected entities with typed, non-reconstitutable markers.
// It never adds values to the session mapping.
func (s *Session) Redact(ctx context.Context, text string) (string, error) {
	spans, err := s.detector.Detect(ctx, text, s.entities)
	if err != nil {
		return "", err
	}
	if err := validateSpans(text, spans); err != nil {
		return "", err
	}
	if len(spans) == 0 {
		return text, nil
	}
	var output strings.Builder
	output.Grow(len(text))
	position := 0
	for _, span := range spans {
		output.WriteString(text[position:span.Start])
		output.WriteString("[[SPARKROUTE_PII_")
		output.WriteString(strings.ToUpper(string(span.Entity)))
		output.WriteString("_REDACTED]]")
		position = span.End
	}
	output.WriteString(text[position:])
	return output.String(), nil
}

func (s *Session) Restore(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.byToken) == 0 {
		return text
	}
	if s.replacer == nil {
		pairs := make([]string, 0, len(s.byToken)*2)
		tokens := make([]string, 0, len(s.byToken))
		for token := range s.byToken {
			tokens = append(tokens, token)
		}
		sort.Strings(tokens)
		for _, token := range tokens {
			pairs = append(pairs, token, s.byToken[token])
		}
		s.replacer = strings.NewReplacer(pairs...)
	}
	return s.replacer.Replace(text)
}

func (s *Session) MappingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byToken)
}

func (s *Session) Counts() map[Entity]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make(map[Entity]int, len(s.counts))
	for entity, count := range s.counts {
		result[entity] = count
	}
	return result
}

func (s *Session) tokenFor(ctx context.Context, entity Entity, original string) (string, error) {
	key := string(entity) + "\x00" + original
	s.mu.Lock()
	defer s.mu.Unlock()
	if token, exists := s.byOriginal[key]; exists {
		return token, nil
	}
	if len(s.byToken) >= s.maximumMappings ||
		len(original) > s.maximumOriginalBytes-s.originalBytes {
		return "", ErrMappingLimit
	}
	var token string
	if s.mappingResolver != nil {
		var err error
		token, err = s.mappingResolver(ctx, entity, original)
		if err != nil {
			return "", err
		}
	} else {
		identifier, err := s.tokenSource()
		if err != nil {
			return "", fmt.Errorf("create PII substitution token: %w", err)
		}
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return "", fmt.Errorf("PII substitution token is empty")
		}
		token = "[[SPARKROUTE_PII_" + strings.ToUpper(string(entity)) + "_" + identifier + "]]"
	}
	if err := validateToken(entity, token); err != nil {
		return "", err
	}
	if _, collision := s.byToken[token]; collision {
		return "", fmt.Errorf("PII substitution token collision")
	}
	s.byOriginal[key] = token
	s.byToken[token] = original
	s.counts[entity]++
	s.originalBytes += len(original)
	s.replacer = nil
	return token, nil
}

func (s *Session) addMapping(entity Entity, original string, token string) error {
	// Previously persisted mappings may have been created by another virtual
	// model with a wider detector policy. They remain valid for exact response
	// restoration even when this request does not detect that entity type.
	if strings.TrimSpace(string(entity)) == "" {
		return fmt.Errorf("PII mapping entity is empty")
	}
	if original == "" {
		return fmt.Errorf("PII mapping original value is empty")
	}
	if err := validateToken(entity, token); err != nil {
		return err
	}
	key := string(entity) + "\x00" + original
	if existing, exists := s.byOriginal[key]; exists {
		if existing != token {
			return fmt.Errorf("PII original value has conflicting tokens")
		}
		return nil
	}
	if existing, exists := s.byToken[token]; exists && existing != original {
		return fmt.Errorf("PII substitution token collision")
	}
	if len(s.byToken) >= s.maximumMappings ||
		len(original) > s.maximumOriginalBytes-s.originalBytes {
		return ErrMappingLimit
	}
	s.byOriginal[key] = token
	s.byToken[token] = original
	s.counts[entity]++
	s.originalBytes += len(original)
	s.replacer = nil
	return nil
}

func validateToken(entity Entity, token string) error {
	prefix := "[[SPARKROUTE_PII_" + strings.ToUpper(string(entity)) + "_"
	if !strings.HasPrefix(token, prefix) || !strings.HasSuffix(token, "]]") ||
		len(token) <= len(prefix)+2 || strings.ContainsAny(token, "\r\n\x00") {
		return fmt.Errorf("PII substitution token has an invalid shape")
	}
	return nil
}

func validateSpans(text string, spans []Span) error {
	previousEnd := 0
	for index, span := range spans {
		if span.Start < 0 || span.End <= span.Start || span.End > len(text) {
			return fmt.Errorf("PII detector span %d is out of bounds", index)
		}
		if span.Start < previousEnd {
			return fmt.Errorf("PII detector spans overlap or are not ordered")
		}
		if span.Entity == "" {
			return fmt.Errorf("PII detector span %d has no entity", index)
		}
		if !utf8.RuneStart(text[span.Start]) ||
			(span.End < len(text) && !utf8.RuneStart(text[span.End])) {
			return fmt.Errorf("PII detector span %d splits UTF-8", index)
		}
		previousEnd = span.End
	}
	return nil
}

func randomToken() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:]), nil
}
