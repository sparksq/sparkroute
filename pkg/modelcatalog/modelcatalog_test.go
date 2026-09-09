// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package modelcatalog

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestDirectoryCachesExactTenantModelAndMaterializesStrictPolicy(t *testing.T) {
	t.Parallel()

	request := Request{TenantID: "tenant-a", RequestedModel: "tenant-model"}
	resolver := &countingResolver{entry: catalogTestEntry("tenant-model")}
	directory := catalogTestDirectory(t, resolver, Options{})
	first, err := directory.Lookup(t.Context(), request)
	if err != nil || first.Cache != CacheMiss {
		t.Fatalf("first Lookup() = %#v, %v", first, err)
	}
	second, err := directory.Lookup(t.Context(), request)
	if err != nil || second.Cache != CachePositiveHit {
		t.Fatalf("second Lookup() = %#v, %v", second, err)
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	document, err := second.Entry.Document()
	if err != nil {
		t.Fatalf("Document() error = %v", err)
	}
	if got := document.Deployments[0].CapabilityPolicy.Unknown; got != config.UnknownCapabilityReject {
		t.Fatalf("unknown capability policy = %q, want reject", got)
	}
	document.Deployments[0].Model = "mutated"
	immutable, err := second.Entry.Document()
	if err != nil || immutable.Deployments[0].Model != "upstream-model" {
		t.Fatalf("second Document() = %#v, %v", immutable.Deployments, err)
	}
	if got := second.Entry.ConfigRevision(); !strings.HasPrefix(got, "catalog:") {
		t.Fatalf("ConfigRevision() = %q", got)
	}
	stats := directory.Stats()
	if stats.Lookups != 2 || stats.Misses != 1 || stats.PositiveHits != 1 || stats.Resolved != 1 {
		t.Fatalf("Stats() = %#v", stats)
	}
}

func TestDirectoryClosesStatefulResolver(t *testing.T) {
	t.Parallel()
	resolver := &closingResolver{}
	directory := catalogTestDirectory(t, resolver, Options{})
	if err := directory.Close(); err != nil || resolver.closed.Load() != 1 {
		t.Fatalf("Close() = %v, calls = %d", err, resolver.closed.Load())
	}
}

func TestDirectoryNegativeCacheIsTenantScopedAndExpires(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	resolver := NewMemoryResolver(nil)
	directory := catalogTestDirectory(t, resolver, Options{
		Now:         func() time.Time { return now },
		NegativeTTL: time.Minute,
	})
	request := Request{TenantID: "tenant-a", RequestedModel: "tenant-model"}
	if _, err := directory.Lookup(t.Context(), request); !errors.Is(err, ErrNotFound) {
		t.Fatalf("first Lookup() error = %v", err)
	}
	resolver.Set(request, catalogTestEntry("tenant-model"))
	if result, err := directory.Lookup(t.Context(), request); !errors.Is(err, ErrNotFound) ||
		result.Cache != CacheNegativeHit {
		t.Fatalf("cached Lookup() = %#v, %v", result, err)
	}
	now = now.Add(time.Minute)
	if result, err := directory.Lookup(t.Context(), request); err != nil || result.Cache != CacheMiss {
		t.Fatalf("expired Lookup() = %#v, %v", result, err)
	}
	otherTenant := Request{TenantID: "tenant-b", RequestedModel: "tenant-model"}
	if _, err := directory.Lookup(t.Context(), otherTenant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other tenant Lookup() error = %v", err)
	}
}

func TestDirectoryCoalescesConcurrentColdLookups(t *testing.T) {
	t.Parallel()

	resolver := &blockingResolver{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		entry:   catalogTestEntry("tenant-model"),
	}
	directory := catalogTestDirectory(t, resolver, Options{})
	request := Request{TenantID: "tenant-a", RequestedModel: "tenant-model"}
	const callers = 24
	errorsSeen := make(chan error, callers)
	var wait sync.WaitGroup
	wait.Add(callers)
	for range callers {
		go func() {
			defer wait.Done()
			_, err := directory.Lookup(context.Background(), request)
			errorsSeen <- err
		}()
	}
	<-resolver.entered
	deadline := time.Now().Add(time.Second)
	for directory.Stats().Coalesced != callers-1 {
		if time.Now().After(deadline) {
			t.Fatalf("coalesced lookups = %d, want %d", directory.Stats().Coalesced, callers-1)
		}
		runtime.Gosched()
	}
	close(resolver.release)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("Lookup() error = %v", err)
		}
	}
	if calls := resolver.calls.Load(); calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	if got := directory.Stats().Coalesced; got != callers-1 {
		t.Fatalf("coalesced lookups = %d, want %d", got, callers-1)
	}
}

func TestDirectoryBoundsDistinctConcurrentLookups(t *testing.T) {
	t.Parallel()

	resolver := &blockingResolver{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		entry:   catalogTestEntry("model-a"),
	}
	directory := catalogTestDirectory(t, resolver, Options{MaxInFlight: 1})
	firstDone := make(chan error, 1)
	go func() {
		_, err := directory.Lookup(context.Background(), Request{
			TenantID: "tenant-a", RequestedModel: "model-a",
		})
		firstDone <- err
	}()
	<-resolver.entered
	_, err := directory.Lookup(t.Context(), Request{
		TenantID: "tenant-a", RequestedModel: "model-b",
	})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("saturated Lookup() error = %v", err)
	}
	close(resolver.release)
	if err := <-firstDone; err != nil {
		t.Fatalf("first Lookup() error = %v", err)
	}
	stats := directory.Stats()
	if stats.Rejected != 1 || stats.Unavailable != 1 {
		t.Fatalf("Stats() = %#v", stats)
	}
}

func TestDirectoryRejectsEntriesOutsideCentralPolicy(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*Entry){
		"source identity":  func(entry *Entry) { entry.SourceID = "other" },
		"missing revision": func(entry *Entry) { entry.Revision = "" },
		"expired": func(entry *Entry) {
			entry.ValidUntil = time.Unix(1, 0).UTC()
		},
		"different model": func(entry *Entry) { entry.VirtualModel.Name = "other" },
		"alias":           func(entry *Entry) { entry.VirtualModel.Aliases = []string{"alias"} },
		"endpoint host":   func(entry *Entry) { entry.Providers[0].BaseURL = "https://blocked.example/v1" },
		"credential scheme": func(entry *Entry) {
			entry.Providers[0].Auth = config.ProviderAuth{
				Type: config.AuthBearer, Credential: credentials.Ref("file:///secret"),
			}
		},
		"dynamic endpoint": func(entry *Entry) {
			entry.Deployments[0].EndpointSource = config.EndpointSource{
				Type: config.EndpointSourceDiscovered,
			}
		},
		"provider permissive capability default": func(entry *Entry) {
			entry.Providers[0].CapabilityDefaults.Unknown = config.UnknownCapabilityTry
		},
		"deployment permissive capability policy": func(entry *Entry) {
			entry.Deployments[0].CapabilityPolicy.Unknown = config.UnknownCapabilityTry
		},
		"stateful capability": func(entry *Entry) {
			entry.Deployments[0].Capabilities = append(
				entry.Deployments[0].Capabilities,
				config.CapabilityStoredCompletion,
			)
		},
		"guardrail": func(entry *Entry) {
			entry.VirtualModel.Guardrails.Pre = []config.Guardrail{{
				Name: "self", Model: entry.VirtualModel.Name,
			}}
		},
		"privacy": func(entry *Entry) {
			entry.VirtualModel.Privacy = &config.PrivacyPolicy{
				PII: &config.PIIPolicy{Entities: []config.PIIEntity{config.PIIEntityEmail}},
			}
		},
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entry := catalogTestEntry("tenant-model")
			mutate(&entry)
			directory := catalogTestDirectory(t, NewMemoryResolver(map[Request]Entry{
				{TenantID: "tenant-a", RequestedModel: "tenant-model"}: entry,
			}), Options{})
			_, err := directory.Lookup(t.Context(), Request{
				TenantID: "tenant-a", RequestedModel: "tenant-model",
			})
			if !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("Lookup() error = %v, want ErrInvalidEntry", err)
			}
		})
	}
}

func TestDirectoryAllowsPrivacyOnlyWhenCentrallyAuthorized(t *testing.T) {
	t.Parallel()

	entry := catalogTestEntry("tenant-model")
	entry.VirtualModel.Privacy = &config.PrivacyPolicy{
		PII: &config.PIIPolicy{Entities: []config.PIIEntity{config.PIIEntityEmail}},
	}
	directory := catalogTestDirectory(t, NewMemoryResolver(map[Request]Entry{
		{TenantID: "tenant-a", RequestedModel: "tenant-model"}: entry,
	}), Options{Policy: Policy{AllowPrivacy: true}})
	resolution, err := directory.Lookup(t.Context(), Request{
		TenantID: "tenant-a", RequestedModel: "tenant-model",
	})
	if err != nil {
		t.Fatalf("Lookup() = %#v, %v", resolution, err)
	}
	document, err := resolution.Entry.Document()
	if err != nil || document.VirtualModels[0].Privacy == nil {
		t.Fatalf("Document() = %#v, %v", document, err)
	}
}

type countingResolver struct {
	calls atomic.Int64
	entry Entry
}

type closingResolver struct{ closed atomic.Int64 }

func (*closingResolver) Resolve(context.Context, Request) (Entry, error) {
	return Entry{}, ErrNotFound
}

func (r *closingResolver) Close() error {
	r.closed.Add(1)
	return nil
}

func (r *countingResolver) Resolve(context.Context, Request) (Entry, error) {
	r.calls.Add(1)
	return r.entry, nil
}

type blockingResolver struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
	entry   Entry
}

func (r *blockingResolver) Resolve(ctx context.Context, _ Request) (Entry, error) {
	if r.calls.Add(1) == 1 {
		close(r.entered)
	}
	select {
	case <-ctx.Done():
		return Entry{}, ctx.Err()
	case <-r.release:
		return r.entry, nil
	}
}

func catalogTestDirectory(t *testing.T, resolver Resolver, override Options) *Directory {
	t.Helper()
	allowPrivacy := override.Policy.AllowPrivacy
	override.Resolver = resolver
	override.SourceID = "test"
	override.Policy = Policy{
		AllowedProviderTypes:     []string{"openai_compatible"},
		AllowedEndpointSchemes:   []string{"https"},
		AllowedEndpointHosts:     []string{"models.example.test"},
		AllowedCredentialSchemes: []string{"k8s"},
		DisallowedCapabilities:   []config.Capability{config.CapabilityStoredCompletion},
		UnknownCapabilityDefault: config.UnknownCapabilityReject,
		AllowPrivacy:             allowPrivacy,
	}
	directory, err := NewDirectory(override)
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	return directory
}

func catalogTestEntry(model string) Entry {
	return Entry{
		SourceID: "test",
		Revision: "revision-1",
		VirtualModel: config.VirtualModel{
			Name: model,
			Pools: []config.RoutingPool{{
				Priority: 0,
				Targets:  []config.WeightedTarget{{Deployment: "deployment", Weight: 1}},
			}},
		},
		Providers: []config.Provider{{
			Name: "provider", Type: "openai_compatible",
			BaseURL: "https://models.example.test/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "deployment", Provider: "provider", Model: "upstream-model",
		}},
	}
}
