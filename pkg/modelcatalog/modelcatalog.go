// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package modelcatalog defines exact-name, tenant-scoped virtual-model
// resolution. A Resolver is an external adapter; Directory is the mandatory
// validating, bounded, and coalescing boundary used by the gateway.
package modelcatalog

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/config"
)

const (
	DefaultLookupTimeout  = 2 * time.Second
	DefaultPositiveTTL    = 5 * time.Minute
	DefaultNegativeTTL    = 15 * time.Second
	DefaultMaxEntries     = 1_000
	DefaultMaxInFlight    = 128
	DefaultMaxProviders   = 8
	DefaultMaxDeployments = 32

	maxTenantIDBytes = 128
	maxModelIDBytes  = 256
	maxSourceIDBytes = 64
	maxRevisionBytes = 128
)

var (
	// ErrNotFound means the authoritative source has no exact entry for the
	// authenticated tenant and requested model.
	ErrNotFound = errors.New("tenant model catalog entry was not found")
	// ErrUnavailable means the source could not produce an authoritative answer.
	ErrUnavailable = errors.New("tenant model catalog is unavailable")
	// ErrInvalidEntry means an external entry failed the gateway-owned schema or
	// policy envelope and must not enter the data plane.
	ErrInvalidEntry = errors.New("tenant model catalog entry is invalid")
)

// Request contains the complete external lookup identity. TenantID must come
// from authenticated gateway identity, never a request body or untrusted
// attribution header.
type Request struct {
	TenantID       string `json:"tenant_id"`
	RequestedModel string `json:"requested_model"`
}

// Entry is the storage-neutral adapter result. Credential fields inside the
// provider and deployment definitions remain opaque references; adapters must
// never return resolved secret values.
type Entry struct {
	SourceID     string              `json:"source_id"`
	Revision     string              `json:"revision"`
	ValidUntil   time.Time           `json:"valid_until,omitempty"`
	VirtualModel config.VirtualModel `json:"virtual_model"`
	Providers    []config.Provider   `json:"providers"`
	Deployments  []config.Deployment `json:"deployments"`
}

// Resolver is implemented by an external catalog adapter. It performs one
// exact lookup and should classify authoritative absence with ErrNotFound.
type Resolver interface {
	Resolve(context.Context, Request) (Entry, error)
}

// Policy is the centrally configured maximum authority of one catalog source.
// External entries may select within this envelope but cannot expand it.
type Policy struct {
	AllowedProviderTypes     []string
	AllowedEndpointSchemes   []string
	AllowedEndpointHosts     []string
	AllowedCredentialSchemes []string
	DisallowedCapabilities   []config.Capability
	UnknownCapabilityDefault config.UnknownCapabilityPolicy
	AllowDynamicEndpoints    bool
	AllowGuardrails          bool
	AllowPrivacy             bool
	MaxProviders             int
	MaxDeployments           int
}

// Options configures the validating cache in front of a Resolver.
type Options struct {
	Resolver      Resolver
	SourceID      string
	Policy        Policy
	LookupTimeout time.Duration
	PositiveTTL   time.Duration
	NegativeTTL   time.Duration
	MaxEntries    int
	MaxInFlight   int
	Now           func() time.Time
}

type CacheStatus string

const (
	CacheMiss        CacheStatus = "miss"
	CachePositiveHit CacheStatus = "positive_hit"
	CacheNegativeHit CacheStatus = "negative_hit"
	CacheCoalesced   CacheStatus = "coalesced"
)

// ResolvedEntry is the validated immutable catalog result. Document decodes a
// private canonical representation so callers cannot mutate cached policy data.
type ResolvedEntry struct {
	SourceID   string
	Revision   string
	ValidUntil time.Time
	Digest     config.Version
	CacheUntil time.Time
	canonical  []byte
}

func (e ResolvedEntry) Document() (config.Document, error) {
	return config.Decode(e.canonical)
}

// ConfigRevision is a bounded provenance value suitable for the existing
// ledger, trace, telemetry, and prefix-affinity revision fields.
func (e ResolvedEntry) ConfigRevision() string {
	digest := string(e.Digest)
	if len(digest) > 16 {
		digest = digest[:16]
	}
	return "catalog:" + e.SourceID + ":" + e.Revision + ":" + digest
}

type Resolution struct {
	Entry ResolvedEntry
	Cache CacheStatus
}

type Stats struct {
	Lookups      uint64
	PositiveHits uint64
	NegativeHits uint64
	Misses       uint64
	Coalesced    uint64
	Resolved     uint64
	NotFound     uint64
	Unavailable  uint64
	Invalid      uint64
	Rejected     uint64
	Evictions    uint64
	Entries      int
	InFlight     int
}

type counters struct {
	lookups      atomic.Uint64
	positiveHits atomic.Uint64
	negativeHits atomic.Uint64
	misses       atomic.Uint64
	coalesced    atomic.Uint64
	resolved     atomic.Uint64
	notFound     atomic.Uint64
	unavailable  atomic.Uint64
	invalid      atomic.Uint64
	rejected     atomic.Uint64
	evictions    atomic.Uint64
}

type cacheKey struct {
	tenant string
	model  string
}

type cacheEntry struct {
	key      cacheKey
	resolved ResolvedEntry
	notFound bool
	expires  time.Time
	element  *list.Element
}

type lookupCall struct {
	done       chan struct{}
	resolution Resolution
	err        error
}

// Directory is the only catalog object accepted by the gateway. It validates
// every adapter result before caching it, coalesces concurrent cold lookups,
// and keeps positive and negative state bounded by one LRU.
type Directory struct {
	resolver      Resolver
	sourceID      string
	policy        policySet
	lookupTimeout time.Duration
	positiveTTL   time.Duration
	negativeTTL   time.Duration
	maxEntries    int
	lookupSlots   chan struct{}
	now           func() time.Time

	mu       sync.Mutex
	entries  map[cacheKey]*cacheEntry
	order    *list.List
	inflight map[cacheKey]*lookupCall
	stats    counters
}

type policySet struct {
	providerTypes     map[string]struct{}
	endpointSchemes   map[string]struct{}
	endpointHosts     []string
	credentialSchemes map[string]struct{}
	disallowed        map[config.Capability]struct{}
	unknownDefault    config.UnknownCapabilityPolicy
	allowDynamic      bool
	allowGuardrails   bool
	allowPrivacy      bool
	maxProviders      int
	maxDeployments    int
}

func NewDirectory(options Options) (*Directory, error) {
	if options.Resolver == nil {
		return nil, fmt.Errorf("model catalog resolver is required")
	}
	if err := validateBoundedID("source ID", options.SourceID, maxSourceIDBytes); err != nil {
		return nil, err
	}
	policy, err := compilePolicy(options.Policy)
	if err != nil {
		return nil, err
	}
	if options.LookupTimeout == 0 {
		options.LookupTimeout = DefaultLookupTimeout
	}
	if options.PositiveTTL == 0 {
		options.PositiveTTL = DefaultPositiveTTL
	}
	if options.NegativeTTL == 0 {
		options.NegativeTTL = DefaultNegativeTTL
	}
	if options.MaxEntries == 0 {
		options.MaxEntries = DefaultMaxEntries
	}
	if options.MaxInFlight == 0 {
		options.MaxInFlight = DefaultMaxInFlight
	}
	if options.LookupTimeout < 0 || options.PositiveTTL < 0 || options.NegativeTTL < 0 {
		return nil, fmt.Errorf("model catalog timeouts and TTLs must be positive")
	}
	if options.LookupTimeout == 0 || options.PositiveTTL == 0 || options.NegativeTTL == 0 {
		return nil, fmt.Errorf("model catalog timeouts and TTLs must not be zero")
	}
	if options.MaxEntries < 1 {
		return nil, fmt.Errorf("model catalog max entries must be positive")
	}
	if options.MaxInFlight < 1 {
		return nil, fmt.Errorf("model catalog max in-flight lookups must be positive")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Directory{
		resolver:      options.Resolver,
		sourceID:      options.SourceID,
		policy:        policy,
		lookupTimeout: options.LookupTimeout,
		positiveTTL:   options.PositiveTTL,
		negativeTTL:   options.NegativeTTL,
		maxEntries:    options.MaxEntries,
		lookupSlots:   make(chan struct{}, options.MaxInFlight),
		now:           options.Now,
		entries:       make(map[cacheKey]*cacheEntry),
		order:         list.New(),
		inflight:      make(map[cacheKey]*lookupCall),
	}, nil
}

func (d *Directory) MaxEntries() int {
	if d == nil {
		return 0
	}
	return d.maxEntries
}

// Close releases adapter-owned resources such as tenant connection caches.
// Stateless resolvers require no special handling.
func (d *Directory) Close() error {
	if d == nil {
		return nil
	}
	closer, ok := d.resolver.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

func (d *Directory) Lookup(ctx context.Context, request Request) (Resolution, error) {
	if d == nil {
		return Resolution{}, fmt.Errorf("%w: directory is not configured", ErrUnavailable)
	}
	if err := request.validate(); err != nil {
		return Resolution{}, fmt.Errorf("%w: %v", ErrInvalidEntry, err)
	}
	d.stats.lookups.Add(1)
	key := cacheKey{tenant: request.TenantID, model: request.RequestedModel}
	now := d.now().UTC()

	d.mu.Lock()
	if entry := d.entries[key]; entry != nil {
		if now.Before(entry.expires) {
			d.order.MoveToFront(entry.element)
			if entry.notFound {
				d.stats.negativeHits.Add(1)
				d.mu.Unlock()
				return Resolution{Cache: CacheNegativeHit}, ErrNotFound
			}
			d.stats.positiveHits.Add(1)
			resolved := entry.resolved
			d.mu.Unlock()
			return Resolution{Entry: resolved, Cache: CachePositiveHit}, nil
		}
		d.removeLocked(entry)
	}
	if call := d.inflight[key]; call != nil {
		d.stats.coalesced.Add(1)
		d.mu.Unlock()
		return waitForCall(ctx, call, CacheCoalesced)
	}
	select {
	case d.lookupSlots <- struct{}{}:
	default:
		d.stats.rejected.Add(1)
		d.stats.unavailable.Add(1)
		d.mu.Unlock()
		return Resolution{}, fmt.Errorf(
			"%w: too many catalog lookups are in progress",
			ErrUnavailable,
		)
	}
	call := &lookupCall{done: make(chan struct{})}
	d.inflight[key] = call
	d.stats.misses.Add(1)
	d.mu.Unlock()

	go d.resolveMiss(context.WithoutCancel(ctx), key, request, call)
	return waitForCall(ctx, call, CacheMiss)
}

func waitForCall(ctx context.Context, call *lookupCall, status CacheStatus) (Resolution, error) {
	select {
	case <-ctx.Done():
		return Resolution{}, ctx.Err()
	case <-call.done:
		result := call.resolution
		result.Cache = status
		return result, call.err
	}
}

func (d *Directory) resolveMiss(
	parent context.Context,
	key cacheKey,
	request Request,
	call *lookupCall,
) {
	defer func() { <-d.lookupSlots }()
	ctx, cancel := context.WithTimeout(parent, d.lookupTimeout)
	entry, err := d.resolver.Resolve(ctx, request)
	cancel()
	now := d.now().UTC()
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			d.stats.notFound.Add(1)
			d.finishLookup(key, call, Resolution{}, ErrNotFound, &cacheEntry{
				key: key, notFound: true, expires: now.Add(d.negativeTTL),
			})
			return
		}
		d.stats.unavailable.Add(1)
		if !errors.Is(err, ErrUnavailable) {
			err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		d.finishLookup(key, call, Resolution{}, err, nil)
		return
	}
	resolved, err := d.validateEntry(request, entry, now)
	if err != nil {
		d.stats.invalid.Add(1)
		d.finishLookup(key, call, Resolution{}, err, nil)
		return
	}
	d.stats.resolved.Add(1)
	d.finishLookup(key, call, Resolution{Entry: resolved}, nil, &cacheEntry{
		key: key, resolved: resolved, expires: resolved.CacheUntil,
	})
}

func (d *Directory) finishLookup(
	key cacheKey,
	call *lookupCall,
	resolution Resolution,
	err error,
	cache *cacheEntry,
) {
	d.mu.Lock()
	if cache != nil {
		if previous := d.entries[key]; previous != nil {
			d.removeLocked(previous)
		}
		cache.element = d.order.PushFront(cache)
		d.entries[key] = cache
		for len(d.entries) > d.maxEntries {
			oldest, _ := d.order.Back().Value.(*cacheEntry)
			d.removeLocked(oldest)
			d.stats.evictions.Add(1)
		}
	}
	call.resolution = resolution
	call.err = err
	delete(d.inflight, key)
	close(call.done)
	d.mu.Unlock()
}

func (d *Directory) removeLocked(entry *cacheEntry) {
	if entry == nil {
		return
	}
	delete(d.entries, entry.key)
	if entry.element != nil {
		d.order.Remove(entry.element)
	}
}

func (d *Directory) Stats() Stats {
	if d == nil {
		return Stats{}
	}
	d.mu.Lock()
	entries := len(d.entries)
	inflight := len(d.inflight)
	d.mu.Unlock()
	return Stats{
		Lookups:      d.stats.lookups.Load(),
		PositiveHits: d.stats.positiveHits.Load(),
		NegativeHits: d.stats.negativeHits.Load(),
		Misses:       d.stats.misses.Load(),
		Coalesced:    d.stats.coalesced.Load(),
		Resolved:     d.stats.resolved.Load(),
		NotFound:     d.stats.notFound.Load(),
		Unavailable:  d.stats.unavailable.Load(),
		Invalid:      d.stats.invalid.Load(),
		Rejected:     d.stats.rejected.Load(),
		Evictions:    d.stats.evictions.Load(),
		Entries:      entries,
		InFlight:     inflight,
	}
}

func (d *Directory) validateEntry(
	request Request,
	entry Entry,
	now time.Time,
) (ResolvedEntry, error) {
	invalid := func(format string, values ...any) (ResolvedEntry, error) {
		return ResolvedEntry{}, fmt.Errorf("%w: %s", ErrInvalidEntry, fmt.Sprintf(format, values...))
	}
	if entry.SourceID != d.sourceID {
		return invalid("source ID does not match the configured source")
	}
	if err := validateBoundedID("revision", entry.Revision, maxRevisionBytes); err != nil {
		return invalid("%v", err)
	}
	if !entry.ValidUntil.IsZero() && !entry.ValidUntil.After(now) {
		return invalid("entry has expired")
	}
	if entry.VirtualModel.Name != request.RequestedModel {
		return invalid("virtual model must exactly match the requested model")
	}
	if len(entry.VirtualModel.Aliases) != 0 {
		return invalid("tenant model aliases are not supported")
	}
	if entry.VirtualModel.Visibility.Effective() == config.ModelVisibilityInternal {
		return invalid("internal virtual models are not externally resolvable")
	}
	if !d.policy.allowGuardrails && (len(entry.VirtualModel.Guardrails.Pre) != 0 ||
		len(entry.VirtualModel.Guardrails.Post) != 0 ||
		entry.VirtualModel.Guardrails.Stream != nil) {
		return invalid("guardrails are outside this catalog policy")
	}
	if !d.policy.allowPrivacy && entry.VirtualModel.Privacy != nil {
		return invalid("privacy policy is outside this catalog policy")
	}
	if len(entry.Providers) == 0 || len(entry.Providers) > d.policy.maxProviders {
		return invalid("provider count must be between 1 and %d", d.policy.maxProviders)
	}
	if len(entry.Deployments) == 0 || len(entry.Deployments) > d.policy.maxDeployments {
		return invalid("deployment count must be between 1 and %d", d.policy.maxDeployments)
	}
	for _, capability := range entry.VirtualModel.RequiredCapabilities {
		if _, denied := d.policy.disallowed[capability]; denied {
			return invalid("capability %q is outside this catalog policy", capability)
		}
	}
	for _, provider := range entry.Providers {
		if _, allowed := d.policy.providerTypes[strings.ToLower(provider.Type)]; !allowed {
			return invalid("provider type %q is outside this catalog policy", provider.Type)
		}
		if d.policy.unknownDefault == config.UnknownCapabilityReject &&
			provider.CapabilityDefaults.Unknown == config.UnknownCapabilityTry {
			return invalid("provider %q broadens the unknown capability policy", provider.Name)
		}
		if provider.BaseURL != "" {
			parsed, err := url.Parse(provider.BaseURL)
			if err != nil {
				return invalid("provider %q base URL is invalid", provider.Name)
			}
			if _, allowed := d.policy.endpointSchemes[strings.ToLower(parsed.Scheme)]; !allowed {
				return invalid("provider %q endpoint scheme is outside this catalog policy", provider.Name)
			}
			if !hostAllowed(parsed.Hostname(), d.policy.endpointHosts) {
				return invalid("provider %q endpoint host is outside this catalog policy", provider.Name)
			}
		}
	}
	for _, deployment := range entry.Deployments {
		if d.policy.unknownDefault == config.UnknownCapabilityReject &&
			deployment.CapabilityPolicy.Unknown == config.UnknownCapabilityTry {
			return invalid("deployment %q broadens the unknown capability policy", deployment.Name)
		}
		if !d.policy.allowDynamic &&
			deployment.EndpointSource.Type.Effective() != config.EndpointSourceStatic {
			return invalid("deployment %q uses a dynamic endpoint source", deployment.Name)
		}
		for _, capability := range deployment.Capabilities {
			if _, denied := d.policy.disallowed[capability]; denied {
				return invalid("capability %q is outside this catalog policy", capability)
			}
		}
	}
	document := config.Document{
		Providers:     append([]config.Provider(nil), entry.Providers...),
		Deployments:   append([]config.Deployment(nil), entry.Deployments...),
		VirtualModels: []config.VirtualModel{entry.VirtualModel},
	}.ResolveCapabilityPolicies(d.policy.unknownDefault)
	for _, ref := range document.CredentialReferences() {
		scheme, _, _ := strings.Cut(string(ref), "://")
		if _, allowed := d.policy.credentialSchemes[strings.ToLower(scheme)]; !allowed {
			return invalid("credential reference scheme is outside this catalog policy")
		}
	}
	canonical, digest, err := config.EncodeCanonical(document)
	if err != nil {
		return invalid("configuration fragment failed validation: %v", err)
	}
	cacheUntil := now.Add(d.positiveTTL)
	if !entry.ValidUntil.IsZero() && entry.ValidUntil.Before(cacheUntil) {
		cacheUntil = entry.ValidUntil
	}
	return ResolvedEntry{
		SourceID:   entry.SourceID,
		Revision:   entry.Revision,
		ValidUntil: entry.ValidUntil.UTC(),
		Digest:     digest,
		CacheUntil: cacheUntil,
		canonical:  canonical,
	}, nil
}

func compilePolicy(policy Policy) (policySet, error) {
	result := policySet{
		providerTypes:     stringSet(policy.AllowedProviderTypes),
		endpointSchemes:   stringSet(policy.AllowedEndpointSchemes),
		endpointHosts:     normalizeHosts(policy.AllowedEndpointHosts),
		credentialSchemes: stringSet(policy.AllowedCredentialSchemes),
		disallowed:        make(map[config.Capability]struct{}, len(policy.DisallowedCapabilities)),
		unknownDefault:    policy.UnknownCapabilityDefault,
		allowDynamic:      policy.AllowDynamicEndpoints,
		allowGuardrails:   policy.AllowGuardrails,
		allowPrivacy:      policy.AllowPrivacy,
		maxProviders:      policy.MaxProviders,
		maxDeployments:    policy.MaxDeployments,
	}
	if len(result.providerTypes) == 0 {
		return policySet{}, fmt.Errorf("model catalog allowed provider types are required")
	}
	if len(result.endpointSchemes) == 0 {
		return policySet{}, fmt.Errorf("model catalog allowed endpoint schemes are required")
	}
	if result.unknownDefault == "" {
		result.unknownDefault = config.UnknownCapabilityReject
	}
	if result.unknownDefault != config.UnknownCapabilityReject &&
		result.unknownDefault != config.UnknownCapabilityTry {
		return policySet{}, fmt.Errorf("model catalog unknown capability default is invalid")
	}
	if result.maxProviders == 0 {
		result.maxProviders = DefaultMaxProviders
	}
	if result.maxDeployments == 0 {
		result.maxDeployments = DefaultMaxDeployments
	}
	if result.maxProviders < 1 || result.maxDeployments < 1 {
		return policySet{}, fmt.Errorf("model catalog provider and deployment limits must be positive")
	}
	for _, capability := range policy.DisallowedCapabilities {
		result.disallowed[capability] = struct{}{}
	}
	return result, nil
}

func (r Request) validate() error {
	if err := validateBoundedID("tenant ID", r.TenantID, maxTenantIDBytes); err != nil {
		return err
	}
	return validateBoundedID("requested model", r.RequestedModel, maxModelIDBytes)
}

func validateBoundedID(name, value string, maximum int) error {
	if value == "" || len(value) > maximum || strings.TrimSpace(value) != value {
		return fmt.Errorf("%s must be between 1 and %d bytes without surrounding whitespace", name, maximum)
	}
	for _, ch := range value {
		if unicode.IsSpace(ch) || unicode.IsControl(ch) {
			return fmt.Errorf("%s must not contain whitespace or control characters", name)
		}
	}
	return nil
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func normalizeHosts(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
		if value != "" {
			result = append(result, value)
		}
	}
	return result
}

func hostAllowed(host string, patterns []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, pattern := range patterns {
		if host == pattern {
			return true
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}
