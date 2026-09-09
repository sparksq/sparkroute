// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package modelcatalog

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func BenchmarkRegressionMatrixCatalogPositiveCache(b *testing.B) {
	request := Request{TenantID: "tenant-a", RequestedModel: "tenant-model"}
	directory := benchmarkCatalogDirectory(b, benchmarkResolver(func(Request) (Entry, error) {
		return catalogTestEntry("tenant-model"), nil
	}))
	if _, err := directory.Lookup(context.Background(), request); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			result, err := directory.Lookup(context.Background(), request)
			if err != nil || result.Cache != CachePositiveHit {
				b.Errorf("Lookup() = %#v, %v", result, err)
				return
			}
		}
	})
}

func BenchmarkRegressionMatrixCatalogColdValidation(b *testing.B) {
	var identifier atomic.Uint64
	directory := benchmarkCatalogDirectory(b, benchmarkResolver(func(request Request) (Entry, error) {
		return catalogTestEntry(request.RequestedModel), nil
	}))
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			value := identifier.Add(1)
			model := fmt.Sprintf("tenant-model-%d", value)
			result, err := directory.Lookup(context.Background(), Request{
				TenantID: fmt.Sprintf("tenant-%d", value), RequestedModel: model,
			})
			if err != nil || result.Cache != CacheMiss {
				b.Errorf("Lookup() = %#v, %v", result, err)
				return
			}
		}
	})
}

type benchmarkResolver func(Request) (Entry, error)

func (resolve benchmarkResolver) Resolve(_ context.Context, request Request) (Entry, error) {
	return resolve(request)
}

func benchmarkCatalogDirectory(b *testing.B, resolver Resolver) *Directory {
	b.Helper()
	directory, err := NewDirectory(Options{
		Resolver: resolver, SourceID: "test", MaxEntries: 4096,
		Policy: Policy{
			AllowedProviderTypes:     []string{"openai_compatible"},
			AllowedEndpointSchemes:   []string{"https"},
			AllowedEndpointHosts:     []string{"models.example.test"},
			AllowedCredentialSchemes: []string{"k8s"},
			DisallowedCapabilities:   []config.Capability{config.CapabilityStoredCompletion},
			UnknownCapabilityDefault: config.UnknownCapabilityReject,
		},
	})
	if err != nil {
		b.Fatal(err)
	}
	return directory
}
